package systemone_test

// Contract tests derived from the golden fixtures in
// testdata/golden/systemone/ (provenance: https://docs.ollama.com/api/systemone,
// accessed 2026-10-16). The fixtures are the specification: every assertion
// reads the documented shape, never this codec's output.

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/tubeof/proxyllm/internal/translate/ir"
	"github.com/tubeof/proxyllm/internal/translate/systemone"
	"github.com/tubeof/proxyllm/internal/translate/testutil"
)

func TestDecodeRequest_Fixture(t *testing.T) {
	raw := testutil.LoadGolden(t, "systemone/request.json")
	req, err := systemone.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.Model != "clef:cloud" {
		t.Errorf("Model = %q, want clef:cloud", req.Model)
	}
	if req.State != "Our checkout has returned 500 errors since 9am." {
		t.Errorf("State = %q, want the fixture's state string", req.State)
	}
	if len(req.Questions) != 3 {
		t.Fatalf("Questions = %d, want 3", len(req.Questions))
	}

	// The criteria object's ORDER is payload on this wire: ties follow the
	// model's option order, and the fixture lists billing, bug, account.
	label := req.Questions[0]
	if label.Name != "label" || label.Type != ir.DecisionChoice {
		t.Fatalf("Questions[0] = %+v, want the choice question %q", label, "label")
	}
	gotChoices := make([]string, 0, len(label.Choices))
	for _, c := range label.Choices {
		gotChoices = append(gotChoices, c.Value)
	}
	if !reflect.DeepEqual(gotChoices, []string{"billing", "bug", "account"}) {
		t.Errorf("choice order = %v, want the fixture's object order [billing bug account]", gotChoices)
	}
	if label.Choices[1].Description != "Software errors" {
		t.Errorf("bug description = %q, want Software errors", label.Choices[1].Description)
	}

	urgent := req.Questions[1]
	if urgent.Name != "urgent" || urgent.Type != ir.DecisionPredicate {
		t.Errorf("Questions[1] = %+v, want the noul question as a predicate", urgent)
	}

	severity := req.Questions[2]
	if severity.Name != "severity" || severity.Type != ir.DecisionScore {
		t.Fatalf("Questions[2] = %+v, want the score question", severity)
	}
	gotLevels := make([]string, 0, len(severity.Levels))
	for _, l := range severity.Levels {
		gotLevels = append(gotLevels, l.Label)
	}
	if !reflect.DeepEqual(gotLevels, []string{"No impact", "Workaround available", "Feature broken"}) {
		t.Errorf("score levels = %v, want the fixture's criteria array in order", gotLevels)
	}
}

func TestDecodeRequest_StateObjectRidesAsJSONText(t *testing.T) {
	// "A nonempty string, or an object or array serialized as JSON text":
	// an object state decodes to its JSON text and re-encodes as an object.
	raw := []byte(`{"model":"clef","state":{"ticket":"t-1"},"questions":{"q":{"type":"noul","instructions":"blocking?"}}}`)
	req, err := systemone.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if req.State != `{"ticket":"t-1"}` {
		t.Errorf("State = %q, want the serialized object text", req.State)
	}
	out, err := systemone.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if !strings.Contains(string(out), `"state":{"ticket":"t-1"}`) {
		t.Errorf("encoded state should ride as the object, got %s", out)
	}
}

func TestEncodeRequest_KeepsQuestionAndCriteriaOrder(t *testing.T) {
	raw := testutil.LoadGolden(t, "systemone/request.json")
	req, err := systemone.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	out, err := systemone.Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	billing := strings.Index(string(out), `"billing"`)
	bug := strings.Index(string(out), `"bug"`)
	account := strings.Index(string(out), `"account"`)
	if billing < 0 || bug < 0 || account < 0 || !(billing < bug && bug < account) {
		t.Errorf("criteria order not preserved in the encoded body: %s", out)
	}
	// Round trip: decode(encode(req)) equals req.
	again, err := systemone.Codec{}.DecodeRequest(out)
	if err != nil {
		t.Fatalf("DecodeRequest(encoded): %v", err)
	}
	if !reflect.DeepEqual(req, again) {
		t.Errorf("round trip diverged:\n want %+v\n got  %+v", req, again)
	}
}

func TestDecodeResponse_Fixture(t *testing.T) {
	raw := testutil.LoadGolden(t, "systemone/response.json")
	resp, err := systemone.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Model != "clef:cloud" {
		t.Errorf("Model = %q, want clef:cloud", resp.Model)
	}
	if resp.Usage == nil || resp.Usage.InputTokens != 174 || resp.Usage.OutputTokens != 1 {
		t.Errorf("Usage = %+v, want input 174 / output 1 (the reference's example)", resp.Usage)
	}
	if len(resp.Answers) != 3 {
		t.Fatalf("Answers = %d, want 3", len(resp.Answers))
	}

	choice := resp.Answers[0]
	if choice.Name != "label" || choice.Type != ir.DecisionChoice || choice.Choice != "bug" {
		t.Fatalf("Answers[0] = %+v, want the choice answer bug", choice)
	}
	if choice.Confidence != 0.8906 {
		t.Errorf("confidence = %v, want 0.8906", choice.Confidence)
	}
	gotProbs := make([]string, 0, len(choice.Probabilities))
	for _, p := range choice.Probabilities {
		gotProbs = append(gotProbs, p.Value)
	}
	if !reflect.DeepEqual(gotProbs, []string{"billing", "bug", "account"}) {
		t.Errorf("probability order = %v, want the fixture's order", gotProbs)
	}

	noul := resp.Answers[1]
	if noul.Type != ir.DecisionPredicate || noul.Probability != 0.92 {
		t.Errorf("Answers[1] = %+v, want noul 0.92 as a predicate", noul)
	}

	score := resp.Answers[2]
	if score.Type != ir.DecisionScore || score.Score != 1.1 || score.Confidence != 0.55 {
		t.Fatalf("Answers[2] = %+v, want score 1.1 / confidence 0.55", score)
	}
	// The legend maps stringified zero-based indices to the level names.
	if len(score.Probabilities) != 3 || score.Probabilities[1].Label != "Workaround available" {
		t.Errorf("score probabilities = %+v, want the legend labels attached", score.Probabilities)
	}
}

func TestEncodeResponse_RoundTrip(t *testing.T) {
	raw := testutil.LoadGolden(t, "systemone/response.json")
	resp, err := systemone.Codec{}.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	out, err := systemone.Codec{}.EncodeResponse(resp)
	if err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	again, err := systemone.Codec{}.DecodeResponse(out)
	if err != nil {
		t.Fatalf("DecodeResponse(encoded): %v", err)
	}
	if !reflect.DeepEqual(resp, again) {
		t.Errorf("round trip diverged:\n want %+v\n got  %+v", resp, again)
	}
}

func TestEncodeResponse_RefusalFailsInsteadOfInventingAnAnswer(t *testing.T) {
	_, err := systemone.Codec{}.EncodeResponse(ir.DecisionResponse{
		Answers: []ir.DecisionAnswer{{Name: "label", Type: ir.DecisionRefusal}},
	})
	if err == nil {
		t.Fatal("a refusal has no systemone answer shape; EncodeResponse must fail")
	}
	if !strings.Contains(err.Error(), "refusal") {
		t.Errorf("err = %v, want it to name the refusal", err)
	}
}

func TestDecodeRequest_ImagesSniffTheirMediaType(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0})
	raw := []byte(`{"model":"clef","state":"s","images":["` + png + `","bm90YW5pbWFnZQ=="],` +
		`"questions":{"q":{"type":"noul","instructions":"i"}}}`)
	req, err := systemone.Codec{}.DecodeRequest(raw)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if len(req.Images) != 2 {
		t.Fatalf("Images = %d, want 2", len(req.Images))
	}
	if req.Images[0].MediaType != "image/png" {
		t.Errorf("media type = %q, want image/png sniffed from the signature", req.Images[0].MediaType)
	}
	if req.Images[1].MediaType != "" {
		t.Errorf("media type = %q, want the honest empty answer for unrecognizable bytes", req.Images[1].MediaType)
	}
}
