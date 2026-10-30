package ollama

// Request-field coverage for the Ollama native controls: options, keep_alive
// and format round-trip, unknown fields pass through same-protocol, and the
// stream flag stays explicit.

import (
	"encoding/json"
	"testing"
)

func TestStreamDefaultsTrue(t *testing.T) {
	req, err := Codec{}.DecodeRequest([]byte(`{"model":"llama3","messages":[]}`))
	if err != nil {
		t.Fatalf("decode absent stream: %v", err)
	}
	if !req.Stream {
		t.Fatal("stream default = false, want true (Ollama streams by default)")
	}
	req, err = Codec{}.DecodeRequest([]byte(`{"model":"llama3","messages":[],"stream":false}`))
	if err != nil {
		t.Fatalf("decode explicit stream: %v", err)
	}
	if req.Stream {
		t.Fatal("stream = true, want explicit false")
	}
}

func TestRequestOptionsRoundTrip(t *testing.T) {
	raw := `{
		"model":"llama3",
		"messages":[{"role":"user","content":"hi"}],
		"stream":false,
		"options":{"temperature":0.2,"top_p":0.9,"top_k":40,"num_predict":128,"seed":7,"num_ctx":4096,"stop":["END"]},
		"keep_alive":"5m",
		"format":{"type":"object"},
		"custom_field":1
	}`
	req, err := Codec{}.DecodeRequest([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Temperature == nil || *req.Temperature != 0.2 {
		t.Errorf("Temperature = %v", req.Temperature)
	}
	if req.TopK == nil || *req.TopK != 40 {
		t.Errorf("TopK = %v", req.TopK)
	}
	if req.MaxTokens != 128 || req.Seed == nil || *req.Seed != 7 {
		t.Errorf("MaxTokens/Seed = %d/%v", req.MaxTokens, req.Seed)
	}
	if len(req.StopSequences) != 1 || req.StopSequences[0] != "END" {
		t.Errorf("StopSequences = %v", req.StopSequences)
	}

	out, err := Codec{}.EncodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got struct {
		Stream    *bool           `json:"stream"`
		Options   map[string]any  `json:"options"`
		KeepAlive string          `json:"keep_alive"`
		Format    json.RawMessage `json:"format"`
		Custom    int             `json:"custom_field"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode encoded body: %v", err)
	}
	if got.Stream == nil || *got.Stream {
		t.Errorf("stream = %v, want explicit false", got.Stream)
	}
	if got.Options["temperature"] != 0.2 || got.Options["top_k"] != float64(40) ||
		got.Options["num_predict"] != float64(128) || got.Options["seed"] != float64(7) ||
		got.Options["num_ctx"] != float64(4096) {
		t.Errorf("options = %v", got.Options)
	}
	if got.KeepAlive != "5m" {
		t.Errorf("keep_alive = %q", got.KeepAlive)
	}
	if string(got.Format) != `{"type":"object"}` {
		t.Errorf("format = %s", got.Format)
	}
	if got.Custom != 1 {
		t.Errorf("passthrough custom_field = %d", got.Custom)
	}
}
