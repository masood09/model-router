package backend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/masood09/model-router/internal/config"
)

func readiness(timeout time.Duration) config.Readiness {
	return config.Readiness{
		Path:           "/v1/models",
		ExpectStatus:   200,
		BodyContains:   `"loaded"`,
		Interval:       config.Duration(10 * time.Millisecond),
		Timeout:        config.Duration(timeout),
		RequestTimeout: config.Duration(time.Second),
	}
}

func TestProbeCheck(t *testing.T) {
	var body atomic.Value
	body.Store(`{"status":"loading"}`)
	var status atomic.Int32
	status.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("probe path = %s", r.URL.Path)
		}
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	p := NewProbe(u, readiness(time.Second), NewProbeClient(http.DefaultTransport))
	if err := p.Check(context.Background()); err == nil {
		t.Fatal("not-yet-loaded body passed")
	}
	body.Store(`{"status":"loaded"}`)
	if err := p.Check(context.Background()); err != nil {
		t.Fatalf("loaded body failed: %v", err)
	}
	status.Store(503)
	if err := p.Check(context.Background()); err == nil {
		t.Fatal("503 passed")
	}
}

func TestProbeWaitBecomesReady(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`"loaded"`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	p := NewProbe(u, readiness(2*time.Second), NewProbeClient(http.DefaultTransport))
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if calls.Load() < 3 {
		t.Fatalf("probe ran %d times", calls.Load())
	}
}

func TestProbeWaitTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	p := NewProbe(u, readiness(80*time.Millisecond), NewProbeClient(http.DefaultTransport))
	start := time.Now()
	err := p.Wait(context.Background())
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Wait overran its timeout")
	}
}

func TestProbeDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`"loaded"`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	p := NewProbe(u, readiness(time.Second), NewProbeClient(http.DefaultTransport))
	if err := p.Check(context.Background()); err == nil {
		t.Fatal("redirect was followed")
	}
}

func TestJoinPath(t *testing.T) {
	cases := map[[2]string]string{
		{"", "/health"}:          "/health",
		{"/", "/health"}:         "/health",
		{"/api", "/health"}:      "/api/health",
		{"/api/", "/health"}:     "/api/health",
		{"/api/v2", "/v1/model"}: "/api/v2/v1/model",
	}
	for in, want := range cases {
		if got := joinPath(in[0], in[1]); got != want {
			t.Errorf("joinPath(%q,%q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestNewStaticBackend(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tok, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := New("remote", config.Backend{
		Upstream:          "https://api.example.org",
		UpstreamTokenFile: tok,
		Driver:            config.Driver{Kind: config.DriverStatic},
		Readiness:         readiness(time.Second),
	}, NewProbeClient(http.DefaultTransport))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !b.Static || b.UpstreamToken != "secret-token" {
		t.Fatalf("backend = %+v", b)
	}
	if err := b.Driver.Activate(context.Background(), "anything"); err != nil {
		t.Fatal(err)
	}
	if v, _ := b.Driver.Current(); v != "" {
		t.Fatalf("static Current = %q", v)
	}
}

func TestNewRejectsEmptyToken(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(tok, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New("remote", config.Backend{
		Upstream:          "https://api.example.org",
		UpstreamTokenFile: tok,
		Driver:            config.Driver{Kind: config.DriverStatic},
		Readiness:         readiness(time.Second),
	}, NewProbeClient(http.DefaultTransport))
	if err == nil {
		t.Fatal("empty token accepted")
	}
}
