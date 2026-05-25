package utils

import "bytes"

// LastSSEDataLine returns the last non-empty, non-[DONE] data event payload.
func LastSSEDataLine(body []byte) []byte {
	lines := ScanSSEDataLines(body)
	if len(lines) == 0 {
		return nil
	}
	return lines[len(lines)-1]
}

// LastSSEOrBody returns the last SSE data payload for streaming responses, or the
// whole body for plain JSON (non-streaming) responses. Returns nil when there is
// nothing to parse. Used by passthrough usage extractors that accept either form.
func LastSSEOrBody(body []byte) []byte {
	if line := LastSSEDataLine(body); line != nil {
		return line
	}
	if len(body) == 0 {
		return nil
	}
	// SSE body with no usable data payload (e.g. only [DONE] or comment/event lines):
	// don't hand a raw event envelope to JSON extractors.
	if bytes.HasPrefix(body, []byte("data:")) || bytes.Contains(body, []byte("\ndata:")) {
		return nil
	}
	return body
}

// ScanSSEDataLines returns all non-empty, non-[DONE] data event payloads in order.
// Parses line-by-line so each payload is clean JSON without trailing event declarations.
func ScanSSEDataLines(body []byte) [][]byte {
	lines := bytes.Split(body, []byte("\n"))
	out := make([][]byte, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimRight(line, "\r")
		// Per the SSE spec a "data:" field may omit the leading space (data:foo == data: foo).
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		out = append(out, data)
	}
	return out
}
