package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/masood09/model-router/internal/backend"
	"github.com/masood09/model-router/internal/config"
)

// env is an end-to-end fixture: a fake systemctl that "restarts" a unit by
// copying the selection file into a serving-state file, and an upstream
// that answers according to that state, like a real single-model server.
type env struct {
	dir       string
	selection string
	hold      string
	serving   string
	// active, while it exists, makes `systemctl is-active` report the unit
	// up. engineDown, while it exists, makes the upstream list no loaded
	// model, like a server whose engine stopped; a restart removes it.
	active     string
	engineDown string
	upstream   *httptest.Server
	url        string
	cancel     context.CancelFunc
	done       chan error
}

func newEnv(t *testing.T, extra string) *env {
	t.Helper()
	return newEnvWith(t, extra, nil)
}

// newEnvWith is newEnv with a hook that runs before the router starts, to
// set the backend's state the router finds at startup.
func newEnvWith(t *testing.T, extra string, setup func(*env)) *env {
	t.Helper()
	e := &env{dir: t.TempDir()}
	e.selection = filepath.Join(e.dir, "selection")
	e.serving = filepath.Join(e.dir, "serving")
	e.hold = filepath.Join(e.dir, "held")
	e.active = filepath.Join(e.dir, "active")
	e.engineDown = filepath.Join(e.dir, "engine-down")
	if err := os.WriteFile(e.serving, []byte("large\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The driver runs `systemctl --no-ask-password <verb> <unit>`, so the
	// verb is $2.
	systemctl := filepath.Join(e.dir, "systemctl")
	script := fmt.Sprintf(`#!/bin/sh
case "$2" in
  restart) sleep 0.05; cp '%[1]s' '%[2]s'; rm -f '%[4]s' ;;
  stop) rm -f '%[2]s' ;;
  is-active) if [ -e '%[3]s' ]; then echo active; else echo unknown; fi ;;
esac
`, e.selection, e.serving, e.active, e.engineDown)
	if err := os.WriteFile(systemctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	e.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(e.serving)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		variant := strings.TrimSpace(string(data))
		if r.URL.Path == "/v1/models" {
			if _, err := os.Stat(e.engineDown); err == nil {
				fmt.Fprint(w, `{"data":[]}`)
				return
			}
			fmt.Fprintf(w, `{"data":[{"id":%q,"status":{"value":"loaded"}}]}`, variant)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := range 5 {
				fmt.Fprintf(w, "data: {\"variant\":%q,\"n\":%d}\n\n", variant, i)
				w.(http.Flusher).Flush()
				if body["abort"] == true {
					panic(http.ErrAbortHandler) // the engine dies mid-stream
				}
				time.Sleep(100 * time.Millisecond)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"variant": variant, "projection": body["projection"]})
	}))
	t.Cleanup(e.upstream.Close)

	cfgJSON := fmt.Sprintf(`{
  "defaultModel": "m-large",
  %s
  "timeouts": {"shutdownGrace": "5s"},
  "groups": {"gpu0": {"maxWait": "20s", "stateRefresh": "1h"}},
  "backends": {
    "llm": {
      "group": "gpu0",
      "upstream": %q,
      "driver": {"kind": "systemd", "systemd": {
        "unit": "llm-backend.service", "selectionFile": %q, "defaultVariant": "large",
        "systemctl": %q, "actionTimeout": "10s"}},
      "readiness": {"bodyContains": "loaded", "interval": "20ms", "timeout": "10s", "startupGrace": "300ms"},
      "hold": {"file": %q, "retryAfter": "30s"}
    }
  },
  "models": {
    "m-large": {"backend": "llm", "variant": "large", "inject": {"projection": false}},
    "m-large-uncensored": {"backend": "llm", "variant": "large", "inject": {"projection": true}},
    "m-small": {"backend": "llm", "variant": "small", "inject": {"projection": false}}
  }
}`, extra, e.upstream.URL, e.selection, systemctl, e.hold)
	if setup != nil {
		setup(e)
	}
	cfg, err := config.Parse([]byte(cfgJSON))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	srv, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.url = "http://" + l.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.done = make(chan error, 1)
	go func() { e.done <- srv.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		<-e.done
	})
	return e
}

func (e *env) chat(t *testing.T, model string) map[string]any {
	t.Helper()
	resp, err := http.Post(e.url+"/v1/chat/completions", "application/json",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[]}`, model)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: status %d: %s", model, resp.StatusCode, b)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func TestEndToEndSwitching(t *testing.T) {
	e := newEnv(t, "")
	if got := e.chat(t, "m-large")["variant"]; got != "large" {
		t.Fatalf("initial variant %v", got)
	}
	if got := e.chat(t, "m-large-uncensored"); got["variant"] != "large" || got["projection"] != true {
		t.Fatalf("uncensored = %v", got)
	}
	if got := e.chat(t, "m-small")["variant"]; got != "small" {
		t.Fatalf("after switch variant %v", got)
	}
	sel, _ := os.ReadFile(e.selection)
	if strings.TrimSpace(string(sel)) != "small" {
		t.Fatalf("selection file %q", sel)
	}
	resp, err := http.Get(e.url + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID     string `json:"id"`
			Status struct{ Value string }
		}
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	for _, m := range list.Data {
		want := "unloaded"
		if m.ID == "m-small" {
			want = "loaded"
		}
		if m.Status.Value != want {
			t.Errorf("%s status %q, want %q", m.ID, m.Status.Value, want)
		}
	}
}

func TestEndToEndConcurrentRequestsNeverHitTheWrongVariant(t *testing.T) {
	e := newEnv(t, "")
	models := map[string]string{"m-large": "large", "m-small": "small", "m-large-uncensored": "large"}
	names := []string{"m-large", "m-small", "m-large-uncensored"}
	var wg sync.WaitGroup
	errs := make(chan error, 60)
	for i := range 60 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := names[i%3]
			resp, err := http.Post(e.url+"/v1/chat/completions", "application/json",
				strings.NewReader(fmt.Sprintf(`{"model":%q}`, name)))
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			var out map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&out)
			if resp.StatusCode != 200 || out["variant"] != models[name] {
				errs <- fmt.Errorf("%s served by %v (status %d)", name, out["variant"], resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestEndToEndMetricsAndState(t *testing.T) {
	e := newEnv(t, "")
	e.chat(t, "m-small")
	resp, err := http.Get(e.url + "/_router/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	wants := []string{
		`model_router_switches_total{group="gpu0",result="ok"} 1`,
		`model_router_requests_total{model="m-small",code="200"} 1`,
		`model_router_active_model{group="gpu0",active="llm/small"} 1`,
	}
	// Request metrics are recorded as the handler returns, which can trail
	// the client reading the response, so poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(e.url + "/_router/metrics")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var missing []string
		for _, w := range wants {
			if !strings.Contains(string(body), w) {
				missing = append(missing, w)
			}
		}
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics missing %q", missing)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGracefulShutdownDrainsStreams(t *testing.T) {
	e := newEnv(t, "")
	resp, err := http.Post(e.url+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m-large","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	if _, err := rd.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	e.cancel() // begin shutdown mid-stream
	rest, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("stream cut during drain: %v", err)
	}
	if !strings.Contains(string(rest), "[DONE]") {
		t.Fatal("stream did not complete during the grace period")
	}
	select {
	case err := <-e.done:
		e.done <- err // let Cleanup's receive succeed
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after draining")
	}
	if _, err := http.Get(e.url + "/_router/healthz"); err == nil {
		t.Fatal("server still accepting connections after shutdown")
	}
}

// A backend that dies mid-stream must not leave the group thinking a request
// is still in flight: that blocks every later switch or restart, and each
// request waits out maxWait and fails with 503.
func TestEndToEndStreamCutByUpstreamDoesNotBlockSwitching(t *testing.T) {
	e := newEnv(t, "")
	resp, err := http.Post(e.url+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m-large","stream":true,"abort":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("stream ended cleanly; want it cut off")
	}
	resp.Body.Close()

	// Well under the group's 20s maxWait, so a leaked slot fails here rather
	// than as a slow 503.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err = client.Post(e.url+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m-small"}`))
	if err != nil {
		t.Fatalf("switch after the cut stream: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 || out["variant"] != "small" {
		t.Fatalf("status %d, variant %v; want 200 from small", resp.StatusCode, out["variant"])
	}
}

// The router starts (or restarts) while its backend's unit is up but its
// engine has stopped and only reloads when a request reaches it. Waiting for
// readiness alone would deadlock; after the startup grace the router must
// restart the backend and serve the request.
func TestEndToEndUpButNeverReadyBackendIsRestarted(t *testing.T) {
	e := newEnvWith(t, "", func(e *env) {
		for _, f := range []string{e.active, e.engineDown} {
			if err := os.WriteFile(f, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})
	start := time.Now()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(e.url+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m-large"}`))
	if err != nil {
		t.Fatalf("request to a stuck backend: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 || out["variant"] != "large" {
		t.Fatalf("status %d, variant %v; want 200 from large", resp.StatusCode, out["variant"])
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("answered after %s, before the startup grace: the backend was not waited for", d)
	}
	if _, err := os.Stat(e.engineDown); err == nil {
		t.Fatal("backend was never restarted")
	}
	if sel, _ := os.ReadFile(e.selection); strings.TrimSpace(string(sel)) != "large" {
		t.Fatalf("selection file %q, want large", sel)
	}
}

func TestNewRejectsBadTokenSources(t *testing.T) {
	cfg, err := config.Parse([]byte(`{
  "defaultModel": "m",
  "auth": {"clientTokensFile": "/nonexistent/tokens"},
  "backends": {"b": {"upstream": "http://127.0.0.1:9", "driver": {"kind": "static"}}},
  "models": {"m": {"backend": "b"}}
}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("missing token file accepted")
	}
}

// A held backend is never restarted, even for a request that would switch it:
// the selection file and the served variant stay untouched until the marker
// goes, and then the switch happens as usual.
func TestEndToEndHoldBlocksSwitching(t *testing.T) {
	e := newEnv(t, "")
	if err := os.WriteFile(e.hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(e.url+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m-small"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "30" || !strings.Contains(string(body), "backend_held") {
		t.Fatalf("held: status %d, Retry-After %q, body %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	if _, err := os.Stat(e.selection); !os.IsNotExist(err) {
		t.Fatal("a held backend's selection file was written")
	}
	if got, _ := os.ReadFile(e.serving); strings.TrimSpace(string(got)) != "large" {
		t.Fatalf("a held backend was restarted: serving %q", got)
	}

	if err := os.Remove(e.hold); err != nil {
		t.Fatal(err)
	}
	if got := e.chat(t, "m-small")["variant"]; got != "small" {
		t.Fatalf("after release variant %v", got)
	}
}

func TestLifecycleForwardsHeld(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "held")
	l := lifecycle{&backend.Backend{Driver: backend.Static{}, HoldFile: marker}}
	if l.Held() {
		t.Fatal("held without a marker")
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !l.Held() {
		t.Fatal("marker not forwarded")
	}
}

// runningDriver is a backend.Driver that reports a process state.
type runningDriver struct {
	backend.Static
	running, known bool
}

func (d runningDriver) Running(context.Context) (bool, bool) { return d.running, d.known }

func TestLifecycleForwardsRunning(t *testing.T) {
	for _, d := range []runningDriver{{running: true, known: true}, {running: false, known: true}, {known: false}} {
		running, known := lifecycle{&backend.Backend{Driver: d}}.Running(context.Background())
		if running != d.running || known != d.known {
			t.Errorf("Running = %v, %v; want %v, %v", running, known, d.running, d.known)
		}
	}
	// Drivers that cannot tell report "unknown", so the scheduler keeps its
	// old behaviour for them.
	if running, known := (lifecycle{&backend.Backend{Driver: backend.Static{}}}).Running(context.Background()); running || known {
		t.Fatalf("static driver: Running = %v, %v; want false, false", running, known)
	}
}

func TestNewTransportSettings(t *testing.T) {
	cfg, _ := config.Parse([]byte(`{
  "defaultModel": "m",
  "timeouts": {"upstreamResponseHeader": "7m"},
  "backends": {"b": {"upstream": "http://127.0.0.1:9", "driver": {"kind": "static"}}},
  "models": {"m": {"backend": "b"}}
}`))
	tr := NewTransport(cfg)
	if tr.Proxy != nil {
		t.Fatal("transport honours proxy environment variables")
	}
	if tr.ResponseHeaderTimeout != 7*time.Minute {
		t.Fatalf("ResponseHeaderTimeout = %s", tr.ResponseHeaderTimeout)
	}
}
