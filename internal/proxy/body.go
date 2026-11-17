package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

var (
	errNotObject    = errors.New("request body must be a JSON object")
	errDuplicateKey = errors.New("request body has a duplicate top-level key")
)

// requestBody is a decoded JSON request body. Values stay raw, so fields the
// router does not touch are forwarded byte for byte.
type requestBody struct {
	fields map[string]json.RawMessage
	raw    []byte
}

// parseBody decodes a JSON object without interpreting its values, in one
// streaming pass. Duplicate top-level keys are rejected: the original bytes
// are forwarded when nothing is injected, and a re-encoding otherwise, and
// the two must never disagree about which duplicate wins.
func parseBody(raw []byte) (*requestBody, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errNotObject
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if _, err := dec.Token(); err != nil { // the opening '{'
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errNotObject
		}
		if _, dup := fields[key]; dup {
			return nil, errDuplicateKey
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		fields[key] = v
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON object")
	}
	return &requestBody{fields: fields, raw: raw}, nil
}

// model returns the "model" field if it is a JSON string.
func (b *requestBody) model() string {
	v, ok := b.fields["model"]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return s
}

// inject adds each field the client did not set. It reports whether the
// body changed; only then does bytes() re-encode it.
func (b *requestBody) inject(fields map[string]json.RawMessage) bool {
	changed := false
	for k, v := range fields {
		if _, present := b.fields[k]; !present {
			b.fields[k] = v
			changed = true
		}
	}
	if changed {
		b.raw = nil
	}
	return changed
}

// bytes returns the body to send upstream: the original bytes when nothing
// was injected, otherwise a fresh encoding.
func (b *requestBody) bytes() ([]byte, error) {
	if b.raw != nil {
		return b.raw, nil
	}
	out, err := json.Marshal(b.fields)
	if err != nil {
		return nil, err
	}
	b.raw = out
	return out, nil
}
