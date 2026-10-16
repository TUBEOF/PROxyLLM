package openai_test

// Contract tests derived from the golden fixtures in
// testdata/golden/openai/decisions-*.json (provenance:
// https://developers.openai.com/api/docs/guides/decisions, accessed
// 2026-10-16). The fixtures are the specification: every assertion reads
// the documented shape, never this codec's output.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/openai"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

func TestDecisionsCodec_DecodeRequest_Fixture(t *testing.T) {
	raw := testutil.LoadGolden(t, "openai/decisions-request.json")
	req, err := openai.DecisionsCodec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.Model != "gpt-6-luna" {
		t.Errorf("Model = %q, want gpt-6-luna", req.Model)
	}
	if req.State != "Our checkout has returned 500 errors since 9am." {
		t.Errorf("State = %q, want the fixture's input text", req.State)
	}
	if len(req.Questions) != 3 {
		t.Fatalf("Questions = %d, want 3", len(req.Questions))
	}
	if req.Questions[0].Type != ir.DecisionPredicate || req.Questions[0].Name != "visible_damage" {
		t.Errorf("Questions[0] = %+v, want the predicate question", req.Questions[0])
	}
	dept := req.Questions[1]
	if dept.Type != ir.DecisionChoice {
		t.Fatalf("Questions[1] = %+v, want the choice question", dept)
	}
	gotValues := make([]string, 0, len(dept.Choices))
	for _, c := range dept.Choices {
		gotValues = append(gotValues, c.Value)
	}
	if !reflect.DeepEqual(gotValues, []string{"billing", "bug", "account"}) {
		t.Errorf("choice order = %v, want the fixture's array order", gotValues)
	}
	sev := req.Questions[2]
	if sev.Type != ir.DecisionScore || len(sev.Levels) != 3 {
		t.Fatalf("Questions[2] = %+v, want the three-level score question", sev)
	}
	if sev.Levels[1].Label != "Workaround available" || sev.Levels[1].Description != "A workaround exists" {
		t.Errorf("level[1] = %+v, want label and description kept", sev.Levels[1])
	}
}

func TestDecisionsCodec_DecodeInput_ImagesAndTextParts(t *testing.T) {
	png := "iVBORw0KGgo="
	raw := []byte(`{"model":"gpt-6-luna","input":[{"role":"user","content":[` +
		`{"type":"input_text","text":"read this"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,` + png + `"}]}],` +
		`"questions":[{"type":"predicate","name":"q","instructions":"i"}]}`)
	req, err := openai.DecisionsCodec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.State != "read this" {
		t.Errorf("State = %q, want the text part", req.State)
	}
	if len(req.Images) != 1 || req.Images[0].MediaType != "image/png" || req.Images[0].Data != png {
		t.Errorf("Images = %+v, want the data URL split into media type and payload", req.Images)
	}
}

func TestDecisionsCodec_DecodeInput_RejectsHostedURLs(t *testing.T) {
	raw := []byte(`{"model":"gpt-6-luna","input":[{"role":"user","content":[` +
		`{"type":"input_image","image_url":"https://example.com/a.png"}]}],` +
		`"questions":[{"type":"predicate","name":"q","instructions":"i"}]}`)
	_, err := openai.DecisionsCodec{}.DecodeRequest(raw)
	if err == nil {
		t.Fatal("hosted URLs are not supported by this API; DecodeRequest must reject them")
	}
	if !strings.Contains(err.Error(), "data URL") {
		t.Errorf("err = %v, want it to name the data-URL contract", err)
	}
}

func TestDecisionsCodec_EncodeRequest_ImagesNeedAMediaType(t *testing.T) {
	base := ir.DecisionRequest{
		Model:     "gpt-6-luna",
		State:     "read this",
		Questions: []ir.DecisionQuestion{{Name: "q", Type: ir.DecisionPredicate, Instructions: "i"}},
	}
	blind := base
	blind.Images = []ir.DecisionImage{{Data: "AAAA"}}
	if _, err := (openai.DecisionsCodec{}).EncodeRequest(blind); err == nil {
		t.Fatal("an image without a media type cannot become a data URL; EncodeRequest must fail")
	}
	typed := base
	typed.Images = []ir.DecisionImage{{MediaType: "image/png", Data: "AAAA"}}
	out, err := (openai.DecisionsCodec{}).EncodeRequest(typed)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"image_url":"data:image/png;base64,AAAA"`) {
		t.Errorf("encoded body should carry the data URL, got %s", out)
	}
}

func TestDecisionsCodec_EncodeRequest_RoundTrip(t *testing.T) {
	raw := testutil.LoadGolden(t, "openai/decisions-request.json")
	req, err := openai.DecisionsCodec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := openai.DecisionsCodec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	again, err := openai.DecisionsCodec{}.DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(encoded): %v", err)
	}
	if !reflect.DeepEqual(req, again) {
		t.Errorf("round trip diverged:\n want %+v\n got  %+v", req, again)
	}
}

func TestDecisionsCodec_DecodeResponse_Fixture(t *testing.T) {
	raw := testutil.LoadGolden(t, "openai/decisions-response.json")
	resp, err := openai.DecisionsCodec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	// The reference documents no usage object for this API: absent means
	// nil, never zeros.
	if resp.Usage != nil {
		t.Errorf("Usage = %+v, want nil (the fixture carries none)", resp.Usage)
	}
	if len(resp.Answers) != 3 {
		t.Fatalf("Answers = %d, want 3", len(resp.Answers))
	}
	if resp.Answers[0].Type != ir.DecisionPredicate || resp.Answers[0].Probability != 0.92 {
		t.Errorf("Answers[0] = %+v, want predicate 0.92", resp.Answers[0])
	}
	choice := resp.Answers[1]
	if choice.Type != ir.DecisionChoice || choice.Choice != "bug" || choice.Confidence != 0.8906 {
		t.Fatalf("Answers[1] = %+v, want choice bug / confidence 0.8906", choice)
	}
	if len(choice.Probabilities) != 3 || choice.Probabilities[1].Value != "bug" {
		t.Errorf("choice probabilities = %+v, want the fixture's entries", choice.Probabilities)
	}
	score := resp.Answers[2]
	if score.Type != ir.DecisionScore || score.Score != 1.1 {
		t.Fatalf("Answers[2] = %+v, want score 1.1", score)
	}
	// Numeric level indices decode to their string spelling, labels kept.
	if len(score.Probabilities) != 3 || score.Probabilities[1].Value != "1" || score.Probabilities[1].Label != "Workaround available" {
		t.Errorf("score probabilities = %+v, want stringified indices with labels", score.Probabilities)
	}
}

func TestDecisionsCodec_DecodeResponse_Refusal(t *testing.T) {
	raw := []byte(`{"answers":[{"type":"refusal","name":"visible_damage"}]}`)
	resp, err := openai.DecisionsCodec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(resp.Answers) != 1 || resp.Answers[0].Type != ir.DecisionRefusal || resp.Answers[0].Name != "visible_damage" {
		t.Fatalf("Answers = %+v, want the refusal kept as such", resp.Answers)
	}
	// And it encodes back to the refusal entry it was.
	out, err := openai.DecisionsCodec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"type":"refusal"`) {
		t.Errorf("encoded body should carry the refusal entry, got %s", out)
	}
}

// A choice answer must not carry the fields of other types — the
// reference's examples show choice/probabilities/confidence only (a live
// E2E once rendered `"probability":0,"score":0` noise onto a choice).
func TestDecisionsCodec_EncodeResponse_OmitsAbsentAnswerFields(t *testing.T) {
	resp := ir.DecisionResponse{Answers: []ir.DecisionAnswer{{
		Name: "department", Type: ir.DecisionChoice, Choice: "bug", Confidence: 0.9,
		Probabilities: []ir.DecisionProbability{
			{Value: "billing", Probability: 0.1},
			{Value: "bug", Probability: 0.9},
		},
	}}}
	out, err := openai.DecisionsCodec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	// Structural check: the answer object must own only its type's fields
	// (the probabilities' own "probability" keys are fine — the answer's
	// are not there).
	var parsed struct {
		Answers []map[string]any `json:"answers"`
		Usage   any              `json:"usage"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("decode encoded body: %v", err)
	}
	if len(parsed.Answers) != 1 {
		t.Fatalf("answers = %d, want 1", len(parsed.Answers))
	}
	ans := parsed.Answers[0]
	for _, absent := range []string{"probability", "score"} {
		if _, ok := ans[absent]; ok {
			t.Errorf("choice answer must not carry %q: %s", absent, out)
		}
	}
	if ans["confidence"] != 0.9 {
		t.Errorf("choice answer must carry its confidence: %s", out)
	}
	if parsed.Usage != nil {
		t.Errorf("no usage in the IR must stay absent on the wire, got %v", parsed.Usage)
	}
	again, err := openai.DecisionsCodec{}.DecodeResponse(out)
	if err != nil {
		t.Fatalf("DecodeResponse(encoded): %v", err)
	}
	if !reflect.DeepEqual(resp, again) {
		t.Errorf("round trip diverged:\n want %+v\n got  %+v", resp, again)
	}
}

func TestDecisionsCodec_EncodeResponse_ScoreIndicesRideAsNumbers(t *testing.T) {
	resp := ir.DecisionResponse{Answers: []ir.DecisionAnswer{{
		Name: "severity", Type: ir.DecisionScore, Score: 1.1, Confidence: 0.55,
		Probabilities: []ir.DecisionProbability{
			{Value: "0", Label: "No impact", Probability: 0.1},
			{Value: "1", Label: "Workaround available", Probability: 0.7},
		},
	}}}
	out, err := openai.DecisionsCodec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !strings.Contains(string(out), `"value":0`) || !strings.Contains(string(out), `"value":1`) {
		t.Errorf("score level values must ride as numbers per the reference, got %s", out)
	}
	again, err := openai.DecisionsCodec{}.DecodeResponse(out)
	if err != nil {
		t.Fatalf("DecodeResponse(encoded): %v", err)
	}
	if !reflect.DeepEqual(resp, again) {
		t.Errorf("round trip diverged:\n want %+v\n got  %+v", resp, again)
	}
}
