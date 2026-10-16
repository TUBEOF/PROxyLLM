// Package systemone implements the System One / Jev codec for Ollama's
// POST /v1/systemone: typed decision questions scored against a shared
// state in one forward pass. The codec translates between the wire shape
// and the IR decision operation in both directions — the client surface
// and the upstream transport share this pair (ADR-004's rule applied to
// decisions).
//
// Wire facts pinned from https://docs.ollama.com/api/systemone (accessed
// 2026-10-16): one non-streaming JSON response ("streaming, video, tools,
// and generation controls are not supported"); request fields
// model/state/images/questions/keep_alive; question types choice (criteria
// object, ORDER is payload — ties follow the model's option order), noul
// (criteria object limited to false/true), score (criteria array, lowest
// first); answers keyed by question name with probabilities objects
// (0–1, normalized) and confidence (1 - H(p)/ln(N), "not calibrated
// correctness"); usage carries input_tokens and output_tokens.
package systemone

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// Codec is the systemone codec pair (request and response, both
// directions).
type Codec struct{}

// --- request ---

type requestWire struct {
	Model     string          `json:"model"`
	State     json.RawMessage `json:"state"`
	Images    []string        `json:"images"`
	Questions orderedObject   `json:"questions"`
	KeepAlive json.RawMessage `json:"keep_alive"`
}

type questionWire struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria"`
}

// DecodeRequest translates a systemone request body into the IR decision
// operation. Validation is the documented one: model, a nonempty state and
// at least one question are required; option counts are checked where the
// reference states a floor.
func (Codec) DecodeRequest(raw []byte) (ir.DecisionRequest, error) {
	var w requestWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.DecisionRequest{}, fmt.Errorf("systemone: decode request: %w", err)
	}
	if strings.TrimSpace(w.Model) == "" {
		return ir.DecisionRequest{}, fmt.Errorf("systemone: model is required")
	}
	if len(w.State) == 0 || strings.TrimSpace(contentText(w.State)) == "" {
		return ir.DecisionRequest{}, fmt.Errorf("systemone: state is required and must be nonempty")
	}
	out := ir.DecisionRequest{Model: w.Model, State: contentText(w.State), KeepAlive: w.KeepAlive}
	for _, img := range w.Images {
		out.Images = append(out.Images, ir.DecisionImage{MediaType: sniffBase64MediaType(img), Data: img})
	}
	if len(w.Questions) == 0 {
		return ir.DecisionRequest{}, fmt.Errorf("systemone: at least one question is required")
	}
	for _, q := range w.Questions {
		var qw questionWire
		if err := json.Unmarshal(q.Value, &qw); err != nil {
			return ir.DecisionRequest{}, fmt.Errorf("systemone: question %q: %w", q.Key, err)
		}
		question := ir.DecisionQuestion{Name: q.Key, Instructions: contentText(qw.Instructions)}
		switch qw.Type {
		case "choice":
			criteria, err := decodeCriteriaObject(qw.Criteria)
			if err != nil {
				return ir.DecisionRequest{}, fmt.Errorf("systemone: question %q: %w", q.Key, err)
			}
			if len(criteria) < 2 {
				return ir.DecisionRequest{}, fmt.Errorf("systemone: question %q: choice needs at least 2 options", q.Key)
			}
			question.Type = ir.DecisionChoice
			for _, c := range criteria {
				// null is "a bare label": a key without a description.
				var desc string
				if len(c.Value) > 0 {
					_ = json.Unmarshal(c.Value, &desc)
				}
				question.Choices = append(question.Choices, ir.DecisionOption{Value: c.Key, Description: desc})
			}
		case "noul":
			question.Type = ir.DecisionPredicate
		case "score":
			var levels []string
			if err := json.Unmarshal(qw.Criteria, &levels); err != nil {
				return ir.DecisionRequest{}, fmt.Errorf("systemone: question %q: score criteria must be an array of strings: %w", q.Key, err)
			}
			if len(levels) < 2 {
				return ir.DecisionRequest{}, fmt.Errorf("systemone: question %q: score needs at least 2 levels", q.Key)
			}
			question.Type = ir.DecisionScore
			for _, l := range levels {
				// systemone holds one string per level (the level's name
				// on the scale); the IR keeps it as the label.
				question.Levels = append(question.Levels, ir.DecisionLevel{Label: l})
			}
		default:
			return ir.DecisionRequest{}, fmt.Errorf("systemone: question %q: unknown type %q (choice, noul, score)", q.Key, qw.Type)
		}
		out.Questions = append(out.Questions, question)
	}
	return out, nil
}

// EncodeRequest translates the IR decision operation into a systemone
// request body. Fields are assembled in order rather than marshalled from
// a map: the questions object and the criteria objects carry their order
// into the wire (ties and probability listings follow it).
func (Codec) EncodeRequest(req ir.DecisionRequest) ([]byte, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("systemone: model is required")
	}
	if strings.TrimSpace(req.State) == "" {
		return nil, fmt.Errorf("systemone: state is required and must be nonempty")
	}
	if len(req.Questions) == 0 {
		return nil, fmt.Errorf("systemone: at least one question is required")
	}
	var b bytes.Buffer
	b.WriteString(`{"model":`)
	b.WriteString(jsonString(req.Model))
	b.WriteString(`,"state":`)
	b.WriteString(stateJSON(req.State))
	if len(req.Images) > 0 {
		b.WriteString(`,"images":[`)
		for i, img := range req.Images {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsonString(img.Data))
		}
		b.WriteByte(']')
	}
	b.WriteString(`,"questions":{`)
	for i, q := range req.Questions {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonString(q.Name))
		b.WriteByte(':')
		qb, err := encodeQuestion(q)
		if err != nil {
			return nil, err
		}
		b.Write(qb)
	}
	b.WriteString(`}`)
	if len(req.KeepAlive) > 0 {
		b.WriteString(`,"keep_alive":`)
		b.Write(req.KeepAlive)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// --- response ---

type responseWire struct {
	Model   string        `json:"model"`
	Answers orderedObject `json:"answers"`
	Usage   *usageWire    `json:"usage"`
}

type usageWire struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type answerWire struct {
	Type          string        `json:"type"`
	Choice        string        `json:"choice"`
	Probabilities orderedObject `json:"probabilities"`
	Noul          float64       `json:"noul"`
	Score         float64       `json:"score"`
	Legend        orderedObject `json:"legend"`
	Confidence    float64       `json:"confidence"`
}

// DecodeResponse translates a systemone response body into the IR decision
// response.
func (Codec) DecodeResponse(raw []byte) (ir.DecisionResponse, error) {
	var w responseWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.DecisionResponse{}, fmt.Errorf("systemone: decode response: %w", err)
	}
	out := ir.DecisionResponse{Model: w.Model}
	if w.Usage != nil {
		out.Usage = &ir.Usage{InputTokens: w.Usage.InputTokens, OutputTokens: w.Usage.OutputTokens}
	}
	for _, a := range w.Answers {
		var aw answerWire
		if err := json.Unmarshal(a.Value, &aw); err != nil {
			return ir.DecisionResponse{}, fmt.Errorf("systemone: answer %q: %w", a.Key, err)
		}
		ans := ir.DecisionAnswer{Name: a.Key, Confidence: aw.Confidence}
		switch aw.Type {
		case "choice":
			ans.Type = ir.DecisionChoice
			ans.Choice = aw.Choice
			for _, p := range aw.Probabilities {
				ans.Probabilities = append(ans.Probabilities, ir.DecisionProbability{
					Value: p.Key, Probability: floatOf(p.Value),
				})
			}
		case "noul":
			// "Probability of true among the false and true candidates.
			// This is a number, not a Boolean."
			ans.Type = ir.DecisionPredicate
			ans.Probability = aw.Noul
		case "score":
			ans.Type = ir.DecisionScore
			ans.Score = aw.Score
			legend := make(map[string]string, len(aw.Legend))
			for _, l := range aw.Legend {
				var s string
				_ = json.Unmarshal(l.Value, &s)
				legend[l.Key] = s
			}
			for _, p := range aw.Probabilities {
				ans.Probabilities = append(ans.Probabilities, ir.DecisionProbability{
					Value: p.Key, Label: legend[p.Key], Probability: floatOf(p.Value),
				})
			}
		default:
			return ir.DecisionResponse{}, fmt.Errorf("systemone: answer %q: unknown type %q", a.Key, aw.Type)
		}
		out.Answers = append(out.Answers, ans)
	}
	return out, nil
}

// EncodeResponse translates the IR decision response into a systemone
// response body. The reference declares usage required; a response whose
// upstream reported none (the OpenAI Decisions shape documents none) omits
// the object rather than inventing zeros — a documented deviation, see
// docs/reference/translation-matrix.md. A refusal has no systemone shape
// and fails the encode instead of being invented into an answer.
func (Codec) EncodeResponse(resp ir.DecisionResponse) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"model":`)
	b.WriteString(jsonString(resp.Model))
	b.WriteString(`,"answers":{`)
	for i, a := range resp.Answers {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonString(a.Name))
		b.WriteByte(':')
		ab, err := encodeAnswer(a)
		if err != nil {
			return nil, err
		}
		b.Write(ab)
	}
	b.WriteString(`}`)
	if resp.Usage != nil {
		b.WriteString(`,"usage":{"input_tokens":`)
		b.WriteString(strconv.Itoa(resp.Usage.InputTokens))
		b.WriteString(`,"output_tokens":`)
		b.WriteString(strconv.Itoa(resp.Usage.OutputTokens))
		b.WriteByte('}')
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// --- helpers ---

func encodeQuestion(q ir.DecisionQuestion) ([]byte, error) {
	var b bytes.Buffer
	switch q.Type {
	case ir.DecisionChoice:
		if len(q.Choices) < 2 {
			return nil, fmt.Errorf("systemone: question %q: choice needs at least 2 options", q.Name)
		}
		b.WriteString(`{"type":"choice","instructions":`)
		b.WriteString(jsonString(q.Instructions))
		b.WriteString(`,"criteria":{`)
		for i, c := range q.Choices {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsonString(c.Value))
			b.WriteByte(':')
			if c.Description == "" {
				b.WriteString("null") // a bare label
			} else {
				b.WriteString(jsonString(c.Description))
			}
		}
		b.WriteString(`}}`)
	case ir.DecisionPredicate:
		b.WriteString(`{"type":"noul","instructions":`)
		b.WriteString(jsonString(q.Instructions))
		b.WriteByte('}')
	case ir.DecisionScore:
		if len(q.Levels) < 2 {
			return nil, fmt.Errorf("systemone: question %q: score needs at least 2 levels", q.Name)
		}
		b.WriteString(`{"type":"score","instructions":`)
		b.WriteString(jsonString(q.Instructions))
		b.WriteString(`,"criteria":[`)
		for i, l := range q.Levels {
			if i > 0 {
				b.WriteByte(',')
			}
			// One string per level on this wire: the label when present,
			// the description otherwise.
			text := l.Label
			if text == "" {
				text = l.Description
			}
			b.WriteString(jsonString(text))
		}
		b.WriteString(`]}`)
	default:
		return nil, fmt.Errorf("systemone: question %q: unsupported type %q", q.Name, q.Type)
	}
	return b.Bytes(), nil
}

func encodeAnswer(a ir.DecisionAnswer) ([]byte, error) {
	var b bytes.Buffer
	switch a.Type {
	case ir.DecisionChoice:
		b.WriteString(`{"type":"choice","choice":`)
		b.WriteString(jsonString(a.Choice))
		b.WriteString(`,"probabilities":`)
		b.WriteString(probabilitiesObject(a.Probabilities))
		b.WriteString(`,"confidence":`)
		b.WriteString(formatFloat(a.Confidence))
		b.WriteByte('}')
	case ir.DecisionPredicate:
		// The noul answer carries the probability and nothing else — the
		// reference lists no confidence for it.
		b.WriteString(`{"type":"noul","noul":`)
		b.WriteString(formatFloat(a.Probability))
		b.WriteByte('}')
	case ir.DecisionScore:
		b.WriteString(`{"type":"score","score":`)
		b.WriteString(formatFloat(a.Score))
		if len(a.Probabilities) > 0 {
			b.WriteString(`,"legend":{`)
			wrote := false
			for _, p := range a.Probabilities {
				if p.Label == "" {
					continue
				}
				if wrote {
					b.WriteByte(',')
				}
				b.WriteString(jsonString(p.Value))
				b.WriteByte(':')
				b.WriteString(jsonString(p.Label))
				wrote = true
			}
			b.WriteByte('}')
		}
		b.WriteString(`,"probabilities":`)
		b.WriteString(probabilitiesObject(a.Probabilities))
		b.WriteString(`,"confidence":`)
		b.WriteString(formatFloat(a.Confidence))
		b.WriteByte('}')
	case ir.DecisionRefusal:
		return nil, fmt.Errorf("systemone: a refusal has no systemone answer shape")
	default:
		return nil, fmt.Errorf("systemone: unsupported answer type %q", a.Type)
	}
	return b.Bytes(), nil
}

// probabilitiesObject renders the candidate probabilities as a JSON object
// keyed by candidate, in candidate order.
func probabilitiesObject(ps []ir.DecisionProbability) string {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range ps {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonString(p.Value))
		b.WriteByte(':')
		b.WriteString(formatFloat(p.Probability))
	}
	b.WriteByte('}')
	return b.String()
}

// contentText renders SystemOneContent as the IR's string: a JSON string
// rides as-is; an object or array rides as its serialized JSON text.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// stateJSON renders the IR state: a string that parses as a JSON object or
// array rides raw (the reference accepts "an object or array serialized as
// JSON text"); everything else is a plain string.
func stateJSON(state string) string {
	trimmed := strings.TrimSpace(state)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid([]byte(trimmed)) {
		return trimmed
	}
	return jsonString(state)
}

func decodeCriteriaObject(raw json.RawMessage) (orderedObject, error) {
	var o orderedObject
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("criteria must be an object: %w", err)
	}
	return o, nil
}

func floatOf(raw json.RawMessage) float64 {
	var f float64
	_ = json.Unmarshal(raw, &f)
	return f
}

// formatFloat renders a float the way the reference examples show it,
// without a trailing exponent surprise for whole numbers.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// orderedObject decodes a JSON object preserving key order. Order is
// payload on this wire: choice criteria follow the model's option order
// (ties), and probability listings follow the candidate order.
type orderedObject []member

type member struct {
	Key   string
	Value json.RawMessage
}

// UnmarshalJSON walks the object token by token so the member order
// survives — encoding/json's map decoding would sort it away.
func (o *orderedObject) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("expected a JSON object")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("object key is not a string")
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return err
		}
		*o = append(*o, member{Key: key, Value: val})
	}
	_, err = dec.Token() // closing '}'
	return err
}

// sniffBase64MediaType derives an image media type from the base64
// payload's magic bytes. The systemone wire carries bare base64 with no
// type; a translation onto a surface that needs one (data URLs) requires
// it, and guessing a type would be worse than the honest empty answer.
func sniffBase64MediaType(data string) string {
	head := data
	if len(head) > 24 {
		head = head[:24] // 24 base64 chars = 18 bytes: enough for every signature below
	}
	if rem := len(head) % 4; rem != 0 {
		head = head[:len(head)-rem]
	}
	raw, err := base64.StdEncoding.DecodeString(head)
	if err != nil {
		return ""
	}
	switch {
	case bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png"
	case bytes.HasPrefix(raw, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(raw, []byte("GIF87a")), bytes.HasPrefix(raw, []byte("GIF89a")):
		return "image/gif"
	case len(raw) >= 12 && bytes.HasPrefix(raw, []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}
