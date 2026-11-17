package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/masood09/model-router/internal/auth"
	"github.com/masood09/model-router/internal/backend"
	"github.com/masood09/model-router/internal/config"
	"github.com/masood09/model-router/internal/metrics"
	"github.com/masood09/model-router/internal/scheduler"
)

// upstream records what it receives and answers like a tiny OpenAI server.
type upstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recorded
}

type recorded struct {
	path, host string
	header     http.Header
	body       map[string]any
}

func newUpstream(t testing.TB) *upstream {
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.reqs = append(u.reqs, recorded{r.URL.Path, r.Host, r.Header.Clone(), body})
		u.mu.Unlock()
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for i := range 3 {
				fmt.Fprintf(w, "data: {\"n\":%d}\n\n", i)
				fl.Flush()
				time.Sleep(60 * time.Millisecond)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) last(t *testing.T) recorded {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatal("upstream received nothing")
	}
	return u.reqs[len(u.reqs)-1]
}

func (u *upstream) count() int { u.mu.Lock(); defer u.mu.Unlock(); return len(u.reqs) }

// lifecycle is a fake scheduler.Lifecycle.
type lifecycle struct {
	mu          sync.Mutex
	current     string
	up          bool
	activations []string
	fail        error
	delay       time.Duration
}

func (l *lifecycle) Activate(ctx context.Context, v string) error {
	if l.delay > 0 {
		select {
		case <-time.After(l.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.activations = append(l.activations, v)
	if l.fail != nil {
		return l.fail
	}
	l.current, l.up = v, true
	return nil
}
func (l *lifecycle) Deactivate(context.Context) error { return nil }
func (l *lifecycle) WaitReady(context.Context) error  { return nil }
func (l *lifecycle) Ready(context.Context) bool       { l.mu.Lock(); defer l.mu.Unlock(); return l.up }
func (l *lifecycle) Current() (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.current, nil
}
func (l *lifecycle) acts() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.activations...)
}

type harness struct {
	router *Router
	srv    *httptest.Server
	up     *upstream
	life   *lifecycle
	group  *scheduler.Group
}

func baseConfig(upstreamURL string, extra string) string {
	return `{
  "defaultModel": "big",
  ` + extra + `
  "backends": {
    "llm": {"group": "gpu0", "upstream": "` + upstreamURL + `",
      "driver": {"kind": "systemd", "systemd": {"unit": "llm-backend.service", "actionTimeout": "1s"}},
      "readiness": {"timeout": "1s"}}
  },
  "groups": {"gpu0": {"maxWait": "2s"}},
  "models": {
    "big": {"backend": "llm", "variant": "large", "inject": {"flag": false}},
    "big-alt": {"backend": "llm", "variant": "large", "inject": {"flag": true}},
    "small": {"backend": "llm", "variant": "small"}
  }
}`
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newHarness(t testing.TB, extraConfig string, opts ...func(*Options)) *harness {
	t.Helper()
	up := newUpstream(t)
	cfg, err := config.Parse([]byte(baseConfig(up.srv.URL, extraConfig)))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	u, _ := url.Parse(up.srv.URL)
	b := &backend.Backend{Name: "llm", Group: "gpu0", Upstream: u}
	life := &lifecycle{current: "large", up: true}
	g := scheduler.New(scheduler.Options{Name: "gpu0", MaxWaiters: 100, MaxWait: 2 * time.Second, Refresh: time.Hour,
		Backends: map[string]scheduler.Lifecycle{"llm": life}, Logger: quiet()})
	g.Detect(context.Background())
	o := Options{
		Config: cfg, Backends: map[string]*backend.Backend{"llm": b},
		Groups:    map[string]*scheduler.Group{"gpu0": g},
		Transport: http.DefaultTransport, Metrics: metrics.New(), Logger: quiet(),
	}
	for _, f := range opts {
		f(&o)
	}
	r := New(o)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &harness{router: r, srv: srv, up: up, life: life, group: g}
}

func (h *harness) post(t *testing.T, path, body string, hdr ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestInjectsOnlyAbsentFields(t *testing.T) {
	h := newHarness(t, "")
	resp := h.post(t, "/v1/chat/completions", `{"model":"big-alt","messages":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := h.up.last(t).body["flag"]; got != true {
		t.Fatalf("flag = %v, want injected true", got)
	}
	h.post(t, "/v1/chat/completions", `{"model":"big-alt","flag":false}`)
	if got := h.up.last(t).body["flag"]; got != false {
		t.Fatalf("client-set flag overwritten: %v", got)
	}
	if len(h.life.acts()) != 0 {
		t.Fatal("same-variant names caused a switch")
	}
}

func TestSwitchesForAnotherVariant(t *testing.T) {
	h := newHarness(t, "")
	resp := h.post(t, "/v1/chat/completions", `{"model":"small"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if acts := h.life.acts(); len(acts) != 1 || acts[0] != "small" {
		t.Fatalf("activations = %v", acts)
	}
	if st := h.group.State(); st.Active.Variant != "small" || st.InFlight != 0 {
		t.Fatalf("state = %+v", st)
	}
}

func TestUnknownModelPolicies(t *testing.T) {
	t.Run("loaded", func(t *testing.T) {
		h := newHarness(t, "")
		h.post(t, "/v1/chat/completions", `{"model":"small"}`)
		resp := h.post(t, "/v1/chat/completions", `{"model":"something-else"}`)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if acts := h.life.acts(); len(acts) != 1 {
			t.Fatalf("unknown model switched: %v", acts)
		}
		if _, ok := h.up.last(t).body["flag"]; ok {
			t.Fatal("unknown model got catalog injections")
		}
	})
	t.Run("reject", func(t *testing.T) {
		h := newHarness(t, `"unknownModel": "reject",`)
		if resp := h.post(t, "/v1/chat/completions", `{"model":"nope"}`); resp.StatusCode != 404 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
	t.Run("default", func(t *testing.T) {
		h := newHarness(t, `"unknownModel": "default",`)
		h.post(t, "/v1/chat/completions", `{"model":"small"}`)
		h.post(t, "/v1/chat/completions", `{"model":"nope"}`)
		if acts := h.life.acts(); len(acts) != 2 || acts[1] != "large" {
			t.Fatalf("activations = %v", acts)
		}
	})
}

func TestStreamsIncrementally(t *testing.T) {
	h := newHarness(t, "")
	resp := h.post(t, "/v1/chat/completions", `{"model":"big","stream":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	start := time.Now()
	rd := bufio.NewReader(resp.Body)
	line, err := rd.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "data: {\"n\":0}") {
		t.Fatalf("first line %q, %v", line, err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("first event was buffered until the stream ended")
	}
	rest, _ := io.ReadAll(rd)
	if !strings.Contains(string(rest), "[DONE]") {
		t.Fatal("stream truncated")
	}
}

func TestHeaders(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "up")
	_ = os.WriteFile(tokFile, []byte("upstream-secret"), 0o600)
	t.Setenv("MR_CLIENT", "client-token-0123456789")
	client, _ := auth.Load("", "MR_CLIENT")
	h := newHarness(t, "", func(o *Options) {
		o.ClientTokens = client
		o.Backends["llm"].UpstreamToken = "upstream-secret"
	})
	if resp := h.post(t, "/v1/chat/completions", `{"model":"big"}`); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated status %d", resp.StatusCode)
	}
	resp := h.post(t, "/v1/chat/completions", `{"model":"big"}`, "Authorization", "Bearer client-token-0123456789", "X-Request-Id", "abc-123")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-Id") != "abc-123" {
		t.Fatal("request id not echoed")
	}
	got := h.up.last(t)
	if got.header.Get("Authorization") != "Bearer upstream-secret" {
		t.Fatalf("upstream Authorization = %q", got.header.Get("Authorization"))
	}
	u, _ := url.Parse(h.up.srv.URL)
	if got.host != u.Host {
		t.Fatalf("upstream Host = %q, want %q", got.host, u.Host)
	}
	if got.header.Get("X-Forwarded-For") == "" {
		t.Fatal("X-Forwarded-For not set")
	}
	if resp := h.post(t, "/v1/chat/completions", `{"model":"big"}`, "X-Api-Key", "client-token-0123456789"); resp.StatusCode != 200 {
		t.Fatalf("x-api-key status %d", resp.StatusCode)
	}
	if h.up.last(t).header.Get("X-Api-Key") != "" {
		t.Fatal("client x-api-key forwarded upstream")
	}
}

func TestPreserveHost(t *testing.T) {
	h := newHarness(t, "", func(o *Options) { o.Backends["llm"].PreserveHost = true })
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"big"}`))
	req.Host = "api.example.org"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := h.up.last(t).host; got != "api.example.org" {
		t.Fatalf("Host = %q", got)
	}
}

func TestBadRequests(t *testing.T) {
	h := newHarness(t, `"limits": {"maxBodyBytes": 2048},`)
	if resp := h.post(t, "/v1/chat/completions", `not json`); resp.StatusCode != 400 {
		t.Fatalf("invalid JSON: %d", resp.StatusCode)
	}
	if resp := h.post(t, "/v1/chat/completions", `["array"]`); resp.StatusCode != 400 {
		t.Fatalf("array body: %d", resp.StatusCode)
	}
	big := `{"model":"big","pad":"` + strings.Repeat("x", 4096) + `"}`
	if resp := h.post(t, "/v1/chat/completions", big); resp.StatusCode != 413 {
		t.Fatalf("oversized body: %d", resp.StatusCode)
	}
	if h.up.count() != 0 {
		t.Fatal("bad requests reached the upstream")
	}
}

func TestModelsList(t *testing.T) {
	h := newHarness(t, "")
	resp, err := http.Get(h.srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Data []modelEntry `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	got := map[string]string{}
	for _, m := range out.Data {
		got[m.ID] = m.Status.Value
	}
	want := map[string]string{"big": "loaded", "big-alt": "loaded", "small": "unloaded"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if h.up.count() != 0 {
		t.Fatal("/v1/models was proxied upstream")
	}
}

func TestRouterEndpoints(t *testing.T) {
	t.Setenv("MR_ADMIN", "admin-token-0123456789")
	admin, _ := auth.Load("", "MR_ADMIN")
	h := newHarness(t, "", func(o *Options) { o.AdminTokens = admin })
	get := func(path string, hdr ...string) int {
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get("/_router/healthz"); c != 200 {
		t.Errorf("healthz %d", c)
	}
	if c := get("/_router/readyz"); c != 200 {
		t.Errorf("readyz %d", c)
	}
	h.router.SetDraining(true)
	if c := get("/_router/readyz"); c != 503 {
		t.Errorf("readyz while draining %d", c)
	}
	if c := get("/_router/metrics"); c != 401 {
		t.Errorf("metrics without token %d", c)
	}
	if c := get("/_router/metrics", "Authorization", "Bearer admin-token-0123456789"); c != 200 {
		t.Errorf("metrics with token %d", c)
	}
	if c := get("/_router/state", "Authorization", "Bearer admin-token-0123456789"); c != 200 {
		t.Errorf("state %d", c)
	}
	if c := get("/_router/nope"); c != 404 {
		t.Errorf("unknown router path %d", c)
	}
}

func TestMetricsCountRequests(t *testing.T) {
	h := newHarness(t, "")
	h.post(t, "/v1/chat/completions", `{"model":"big"}`)
	var b strings.Builder
	_ = h.router.mReg.Write(&b)
	if !strings.Contains(b.String(), `model_router_requests_total{model="big",code="200"} 1`) {
		t.Fatalf("metrics:\n%s", b.String())
	}
}

func TestUpstreamDownGives502AndMarksUnready(t *testing.T) {
	h := newHarness(t, "")
	h.up.srv.Close()
	resp := h.post(t, "/v1/chat/completions", `{"model":"big"}`)
	if resp.StatusCode != 502 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if h.group.State().Ready {
		t.Fatal("group still marked ready after an upstream failure")
	}
}

func TestSwitchFailureGives503(t *testing.T) {
	h := newHarness(t, "")
	h.life.mu.Lock()
	h.life.fail = errors.New("boom")
	h.life.mu.Unlock()
	resp := h.post(t, "/v1/chat/completions", `{"model":"small"}`)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	var e apiError
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Code != "model_unavailable" || strings.Contains(e.Error.Message, "boom") {
		t.Fatalf("error body = %+v (internal error leaked?)", e)
	}
}

func TestPassthrough(t *testing.T) {
	h := newHarness(t, "")
	resp, err := http.Get(h.srv.URL + "/v1/props")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || h.up.last(t).path != "/v1/props" {
		t.Fatalf("passthrough status %d", resp.StatusCode)
	}
	off := newHarness(t, `"passthrough": "off",`)
	resp, _ = http.Get(off.srv.URL + "/v1/props")
	resp.Body.Close()
	if resp.StatusCode != 404 || off.up.count() != 0 {
		t.Fatalf("passthrough off: status %d", resp.StatusCode)
	}
}

func TestRequestIDSanitised(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Request-Id", "bad id\nwith newline")
	if id := requestID(r); strings.ContainsAny(id, " \n") || len(id) != 24 {
		t.Fatalf("id = %q", id)
	}
}
