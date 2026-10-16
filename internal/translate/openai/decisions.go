package openai

// DecisionsCodec is the codec pair for the OpenAI Decisions API
// (POST /v1/decisions): typed questions against shared evidence, answered
// ~10x faster than the Responses API. It translates between the wire shape
// and the IR decision operation in both directions.
//
// Wire facts pinned from https://developers.openai.com/api/docs/guides/decisions
// (accessed 2026-10-16): public beta, `gpt-6-luna` the only model; request
// fields model/input/questions; question types predicate (→ probability),
// choice (→ choice + probabilities + confidence), score (→
// probability-weighted mean of the level indices + per-level probabilities
// + confidence); answers is an ARRAY whose entries carry type+name, and may
// be a `refusal`; images "must be inline base64 data URLs" (hosted URLs and
// file_id unsupported); billing is input-only; the page documents no usage
// object and no streaming — a usage object is therefore parsed leniently
// when present and never invented.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/tubeof/proxyllm/internal/translate/ir"
)

// DecisionsCodec translates the OpenAI Decisions wire shape.
type DecisionsCodec struct{}

// --- request ---

type decisionsRequestWire struct {
	Model     string                  `json:"model"`
	Input     json.RawMessage         `json:"input"`
	Questions []decisionsQuestionWire `json:"questions"`
}

type decisionsQuestionWire struct {
	Type         string                `json:"type"`
	Name         string                `json:"name"`
	Instructions string                `json:"instructions"`
	Choices      []decisionsChoiceWire `json:"choices,omitempty"`
	Levels       []decisionsLevelWire  `json:"levels,omitempty"`
}

type decisionsChoiceWire struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type decisionsLevelWire struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type decisionsMessageWire struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type decisionsPartWire struct {
	Type     string `json:"type"` // input_text | input_image
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// DecodeRequest translates a Decisions request body into the IR decision
// operation. Validation is the documented one: model, input and at least
// one question are required; a question needs a name and one of the three
// documented types.
func (DecisionsCodec) DecodeRequest(raw []byte) (ir.DecisionRequest, error) {
	var w decisionsRequestWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.DecisionRequest{}, fmt.Errorf("decisions: decode request: %w", err)
	}
	if strings.TrimSpace(w.Model) == "" {
		return ir.DecisionRequest{}, fmt.Errorf("decisions: model is required")
	}
	state, images, err := decodeDecisionsInput(w.Input)
	if err != nil {
		return ir.DecisionRequest{}, err
	}
	if strings.TrimSpace(state) == "" {
		return ir.DecisionRequest{}, fmt.Errorf("decisions: input is required and must carry text")
	}
	out := ir.DecisionRequest{Model: w.Model, State: state, Images: images}
	if len(w.Questions) == 0 {
		return ir.DecisionRequest{}, fmt.Errorf("decisions: at least one question is required")
	}
	for _, q := range w.Questions {
		if strings.TrimSpace(q.Name) == "" {
			return ir.DecisionRequest{}, fmt.Errorf("decisions: every question needs a name")
		}
		question := ir.DecisionQuestion{Name: q.Name, Instructions: q.Instructions}
		switch q.Type {
		case "predicate":
			question.Type = ir.DecisionPredicate
		case "choice":
			if len(q.Choices) < 2 {
				return ir.DecisionRequest{}, fmt.Errorf("decisions: question %q: choice needs at least 2 options", q.Name)
			}
			question.Type = ir.DecisionChoice
			for _, c := range q.Choices {
				question.Choices = append(question.Choices, ir.DecisionOption{Value: c.Value, Description: c.Description})
			}
		case "score":
			if len(q.Levels) < 2 {
				return ir.DecisionRequest{}, fmt.Errorf("decisions: question %q: score needs at least 2 levels", q.Name)
			}
			question.Type = ir.DecisionScore
			for _, l := range q.Levels {
				question.Levels = append(question.Levels, ir.DecisionLevel{Label: l.Label, Description: l.Description})
			}
		default:
			return ir.DecisionRequest{}, fmt.Errorf("decisions: question %q: unknown type %q (predicate, choice, score)", q.Name, q.Type)
		}
		out.Questions = append(out.Questions, question)
	}
	return out, nil
}

// EncodeRequest translates the IR decision operation into a Decisions
// request body. Images without a known media type fail the encode: the
// API requires data URLs, and a type cannot be guessed.
func (DecisionsCodec) EncodeRequest(req ir.DecisionRequest) ([]byte, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("decisions: model is required")
	}
	if strings.TrimSpace(req.State) == "" {
		return nil, fmt.Errorf("decisions: input is required and must carry text")
	}
	if len(req.Questions) == 0 {
		return nil, fmt.Errorf("decisions: at least one question is required")
	}
	w := decisionsRequestWire{Model: req.Model, Questions: make([]decisionsQuestionWire, 0, len(req.Questions))}
	if len(req.Images) == 0 {
		input, err := json.Marshal(req.State)
		if err != nil {
			return nil, fmt.Errorf("decisions: encode input: %w", err)
		}
		w.Input = input
	} else {
		parts := make([]decisionsPartWire, 0, len(req.Images)+1)
		parts = append(parts, decisionsPartWire{Type: "input_text", Text: req.State})
		for i, img := range req.Images {
			if img.MediaType == "" {
				return nil, fmt.Errorf("decisions: image %d has no recognizable media type — the Decisions API takes base64 data URLs and the type is not guessable", i)
			}
			parts = append(parts, decisionsPartWire{Type: "input_image", ImageURL: "data:" + img.MediaType + ";base64," + img.Data})
		}
		partsRaw, err := json.Marshal(parts)
		if err != nil {
			return nil, fmt.Errorf("decisions: encode input parts: %w", err)
		}
		msgsRaw, err := json.Marshal([]decisionsMessageWire{{Role: "user", Content: partsRaw}})
		if err != nil {
			return nil, fmt.Errorf("decisions: encode input messages: %w", err)
		}
		w.Input = msgsRaw
	}
	for _, q := range req.Questions {
		qw := decisionsQuestionWire{Name: q.Name, Instructions: q.Instructions}
		switch q.Type {
		case ir.DecisionPredicate:
			qw.Type = "predicate"
		case ir.DecisionChoice:
			if len(q.Choices) < 2 {
				return nil, fmt.Errorf("decisions: question %q: choice needs at least 2 options", q.Name)
			}
			qw.Type = "choice"
			for _, c := range q.Choices {
				qw.Choices = append(qw.Choices, decisionsChoiceWire{Value: c.Value, Description: c.Description})
			}
		case ir.DecisionScore:
			if len(q.Levels) < 2 {
				return nil, fmt.Errorf("decisions: question %q: score needs at least 2 levels", q.Name)
			}
			qw.Type = "score"
			for _, l := range q.Levels {
				label := l.Label
				if label == "" {
					label = l.Description
				}
				desc := l.Description
				if desc == label {
					desc = ""
				}
				qw.Levels = append(qw.Levels, decisionsLevelWire{Label: label, Description: desc})
			}
		default:
			return nil, fmt.Errorf("decisions: question %q: unsupported type %q", q.Name, q.Type)
		}
		w.Questions = append(w.Questions, qw)
	}
	return json.Marshal(w)
}

// --- response ---

type decisionsResponseWire struct {
	Model   string                  `json:"model"`
	Answers []decisionsAnswerInWire `json:"answers"`
	Usage   *decisionsUsageWire     `json:"usage"`
}

type decisionsUsageWire struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type decisionsAnswerInWire struct {
	Type          string  `json:"type"`
	Name          string  `json:"name"`
	Probability   float64 `json:"probability"`
	Choice        string  `json:"choice"`
	Score         float64 `json:"score"`
	Confidence    float64 `json:"confidence"`
	Probabilities []struct {
		Value       json.RawMessage `json:"value"` // string (choice) or number (score level index)
		Label       string          `json:"label"`
		Probability float64         `json:"probability"`
	} `json:"probabilities"`
}

// DecodeResponse translates a Decisions response body into the IR decision
// response. Answers arrive as an array; a `refusal` entry maps to the IR's
// refusal marker rather than being dropped.
func (DecisionsCodec) DecodeResponse(raw []byte) (ir.DecisionResponse, error) {
	var w decisionsResponseWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return ir.DecisionResponse{}, fmt.Errorf("decisions: decode response: %w", err)
	}
	out := ir.DecisionResponse{Model: w.Model}
	if w.Usage != nil {
		out.Usage = &ir.Usage{InputTokens: w.Usage.InputTokens, OutputTokens: w.Usage.OutputTokens}
	}
	for _, a := range w.Answers {
		ans := ir.DecisionAnswer{Name: a.Name, Confidence: a.Confidence}
		switch a.Type {
		case "predicate":
			ans.Type = ir.DecisionPredicate
			ans.Probability = a.Probability
		case "choice":
			ans.Type = ir.DecisionChoice
			ans.Choice = a.Choice
			for _, p := range a.Probabilities {
				ans.Probabilities = append(ans.Probabilities, ir.DecisionProbability{
					Value: rawValueString(p.Value), Probability: p.Probability,
				})
			}
		case "score":
			ans.Type = ir.DecisionScore
			ans.Score = a.Score
			for _, p := range a.Probabilities {
				ans.Probabilities = append(ans.Probabilities, ir.DecisionProbability{
					Value: rawValueString(p.Value), Label: p.Label, Probability: p.Probability,
				})
			}
		case "refusal":
			ans.Type = ir.DecisionRefusal
		default:
			return ir.DecisionResponse{}, fmt.Errorf("decisions: answer %q: unknown type %q", a.Name, a.Type)
		}
		out.Answers = append(out.Answers, ans)
	}
	return out, nil
}

// EncodeResponse translates the IR decision response into a Decisions
// response body. The usage object is emitted only when the IR carries one —
// the reference documents none for this API, and zeros would be a claim.
func (DecisionsCodec) EncodeResponse(resp ir.DecisionResponse) ([]byte, error) {
	out := decisionsResponseWire{Model: resp.Model, Answers: make([]decisionsAnswerInWire, 0, len(resp.Answers))}
	for _, a := range resp.Answers {
		w := decisionsAnswerInWire{Name: a.Name}
		switch a.Type {
		case ir.DecisionPredicate:
			w.Type = "predicate"
			p := a.Probability
			w.Probability = p
		case ir.DecisionChoice:
			w.Type = "choice"
			w.Choice = a.Choice
			c := a.Confidence
			w.Confidence = c
			for _, pr := range a.Probabilities {
				v, err := json.Marshal(pr.Value)
				if err != nil {
					return nil, fmt.Errorf("decisions: encode probability value: %w", err)
				}
				w.Probabilities = append(w.Probabilities, struct {
					Value       json.RawMessage `json:"value"`
					Label       string          `json:"label"`
					Probability float64         `json:"probability"`
				}{Value: v, Probability: pr.Probability})
			}
		case ir.DecisionScore:
			w.Type = "score"
			s := a.Score
			w.Score = s
			c := a.Confidence
			w.Confidence = c
			for _, pr := range a.Probabilities {
				// Level indices ride as numbers on this wire; a value that
				// is not a zero-based index stays a string rather than
				// being coerced.
				v := json.RawMessage(strconv.Quote(pr.Value))
				if n, err := strconv.Atoi(pr.Value); err == nil {
					v = json.RawMessage(strconv.Itoa(n))
				}
				w.Probabilities = append(w.Probabilities, struct {
					Value       json.RawMessage `json:"value"`
					Label       string          `json:"label"`
					Probability float64         `json:"probability"`
				}{Value: v, Label: pr.Label, Probability: pr.Probability})
			}
		case ir.DecisionRefusal:
			w.Type = "refusal"
		default:
			return nil, fmt.Errorf("decisions: unsupported answer type %q", a.Type)
		}
		out.Answers = append(out.Answers, w)
	}
	if resp.Usage != nil {
		out.Usage = &decisionsUsageWire{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	}
	return json.Marshal(out)
}

// --- helpers ---

// decodeDecisionsInput splits the `input` field: a plain text string, or
// user messages whose parts are flattened (text concatenated, images
// decoded from their data URLs — "Images must be inline base64 data URLs").
func decodeDecisionsInput(raw json.RawMessage) (string, []ir.DecisionImage, error) {
	if len(raw) == 0 {
		return "", nil, fmt.Errorf("decisions: input is required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil, nil
	}
	var msgs []decisionsMessageWire
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return "", nil, fmt.Errorf("decisions: input must be a string or an array of messages")
	}
	var texts []string
	var images []ir.DecisionImage
	for _, m := range msgs {
		var str string
		if err := json.Unmarshal(m.Content, &str); err == nil {
			if str != "" {
				texts = append(texts, str)
			}
			continue
		}
		var parts []decisionsPartWire
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			continue
		}
		for _, p := range parts {
			switch p.Type {
			case "input_text":
				texts = append(texts, p.Text)
			case "input_image":
				media, data, err := splitDataURL(p.ImageURL)
				if err != nil {
					return "", nil, err
				}
				images = append(images, ir.DecisionImage{MediaType: media, Data: data})
			}
		}
	}
	return strings.Join(texts, "\n"), images, nil
}

// splitDataURL splits "data:<media>;base64,<payload>". Hosted URLs are
// rejected with the reference's own contract as the reason.
func splitDataURL(url string) (string, string, error) {
	if !strings.HasPrefix(url, "data:") {
		return "", "", fmt.Errorf("decisions: images must be inline base64 data URLs — hosted URLs are not supported by this API")
	}
	rest := strings.TrimPrefix(url, "data:")
	i := strings.Index(rest, ",")
	if i < 0 || !strings.HasSuffix(rest[:i], ";base64") {
		return "", "", fmt.Errorf("decisions: image data URL must be base64-encoded")
	}
	media := strings.TrimSuffix(rest[:i], ";base64")
	return media, rest[i+1:], nil
}

// rawValueString renders a probability value from either wire spelling: a
// choice value string, or a numeric level index.
func rawValueString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return strings.Trim(string(raw), `"`)
}
