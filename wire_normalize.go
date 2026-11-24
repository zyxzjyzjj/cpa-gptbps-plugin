package main

import "bytes"

// CPA 8522538 drops a cross-format stream frame if a valid native translator
// returns byte-identical output, treating it as a registry fallback. Codex to
// Responses is intentionally a pass-through for most events. Mark only this
// plugin's C-ABI stream frames, then remove the marker after native translation.
// This makes successful native translation distinguishable without changing
// upstream input, client-visible JSON, or any built-in provider's response.
var wireMarker = []byte(`"_cpa_codex_bps_wire":true,`)

func tagWireFrame(frame []byte) []byte {
	prefix := []byte("data: {")
	if !bytes.HasPrefix(frame, prefix) {
		return frame
	}
	out := make([]byte, 0, len(frame)+len(wireMarker))
	out = append(out, prefix...)
	out = append(out, wireMarker...)
	out = append(out, frame[len(prefix):]...)
	return out
}

func normalizeWireResponse(raw []byte) (any, error) {
	var req struct {
		FromFormat string
		ToFormat   string
		Stream     bool
		Body       []byte
	}
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	body := req.Body
	if req.Stream && req.FromFormat == "codex" && req.ToFormat == "openai-response" {
		brace := bytes.IndexByte(body, '{')
		if brace >= 0 && bytes.HasPrefix(body[brace+1:], wireMarker) {
			out := make([]byte, 0, len(body)-len(wireMarker))
			out = append(out, body[:brace+1]...)
			out = append(out, body[brace+1+len(wireMarker):]...)
			body = out
		}
	}
	return map[string]any{"Body": body}, nil
}
