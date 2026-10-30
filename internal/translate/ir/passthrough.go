package ir

// Passthrough helpers: a codec captures the top-level wire fields it does not
// model, and a same-protocol encoder re-emits them verbatim. Crossing
// protocols deliberately do not forward unknown fields — a field name from one
// wire family is not a field on another.

import "encoding/json"

// UnknownFields returns the top-level members of raw whose names are not in
// known. It returns nil when there are none (the common case), so the
// encoder-side merge stays a no-op.
func UnknownFields(raw []byte, known map[string]bool) map[string]json.RawMessage {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil
	}
	var out map[string]json.RawMessage
	for name, value := range all {
		if known[name] {
			continue
		}
		if out == nil {
			out = make(map[string]json.RawMessage)
		}
		out[name] = value
	}
	return out
}

// MergePassthrough re-emits extra top-level fields onto an encoded body. A
// field the encoder already wrote wins; passthrough never overwrites a
// translated field.
func MergePassthrough(base []byte, extra map[string]json.RawMessage) ([]byte, error) {
	if len(extra) == 0 {
		return base, nil
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(base, &all); err != nil {
		return nil, err
	}
	for name, value := range extra {
		if _, exists := all[name]; !exists {
			all[name] = value
		}
	}
	return json.Marshal(all)
}

// PassthroughFor returns the captured unknowns when the request came from the
// same codec family as the encoder.
func PassthroughFor(req Request, protocol string) map[string]json.RawMessage {
	if req.SourceProtocol != protocol {
		return nil
	}
	return req.Passthrough
}
