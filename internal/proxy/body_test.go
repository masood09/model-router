package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseBodyAndInject(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	b, err := parseBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b.model() != "m" {
		t.Fatalf("model = %q", b.model())
	}
	out, _ := b.bytes()
	if string(out) != string(raw) {
		t.Fatal("untouched body was re-encoded")
	}
	if !b.inject(map[string]json.RawMessage{"x": json.RawMessage(`1`), "model": json.RawMessage(`"other"`)}) {
		t.Fatal("inject reported no change")
	}
	out, _ = b.bytes()
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["x"] != float64(1) || m["model"] != "m" {
		t.Fatalf("after inject: %v", m)
	}
}

func TestParseBodyRejects(t *testing.T) {
	for _, s := range []string{"", "   ", "null", "[]", `"x"`, "{", `{"a":}`} {
		if _, err := parseBody([]byte(s)); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestParseBodyRejectsDuplicateKeys(t *testing.T) {
	for _, s := range []string{
		`{"model":"a","model":"b"}`,
		`{"stream":true,"x":1,"stream":false}`,
	} {
		if _, err := parseBody([]byte(s)); !errors.Is(err, errDuplicateKey) {
			t.Errorf("%s: err = %v, want errDuplicateKey", s, err)
		}
	}
	// Duplicates inside nested values are the upstream's business.
	if _, err := parseBody([]byte(`{"messages":[{"a":1,"a":2}]}`)); err != nil {
		t.Errorf("nested duplicate rejected: %v", err)
	}
	if _, err := parseBody([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Error("trailing object accepted")
	}
}

func TestModelMustBeString(t *testing.T) {
	b, _ := parseBody([]byte(`{"model": 42}`))
	if b.model() != "" {
		t.Fatal("non-string model accepted")
	}
}

func FuzzParseBody(f *testing.F) {
	f.Add([]byte(`{"model":"m","stream":true}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"model":{"nested":1}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := parseBody(data)
		if err != nil {
			return
		}
		_ = b.model()
		b.inject(map[string]json.RawMessage{"k": json.RawMessage(`true`)})
		out, err := b.bytes()
		if err != nil {
			return
		}
		if !json.Valid(out) {
			t.Fatalf("re-encoded body is invalid JSON: %q", out)
		}
	})
}

func BenchmarkFastPathNoInject(b *testing.B) {
	benchProxy(b, `{"model":"big-noinject","messages":[{"role":"user","content":"hello"}]}`)
}

func BenchmarkFastPathInject(b *testing.B) {
	benchProxy(b, `{"model":"big","messages":[{"role":"user","content":"hello"}]}`)
}

func benchProxy(b *testing.B, body string) {
	h := newHarness(b, "")
	h.router.routes["big-noinject"] = &route{name: "big-noinject", backend: h.router.routes["big"].backend,
		group: h.router.routes["big"].group, key: h.router.routes["big"].key}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.router.ServeHTTP(w, req)
		if w.Code != 200 {
			b.Fatalf("status %d", w.Code)
		}
	}
}
