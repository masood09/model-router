package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/masood09/model-router/internal/config"
)

// maxProbeBody bounds how much of a readiness response is read.
const maxProbeBody = 1 << 20

// ErrNotReady is returned when a backend does not become ready in time.
var ErrNotReady = errors.New("backend did not become ready")

// Probe checks whether an upstream is ready to serve.
type Probe struct {
	url            string
	expectStatus   int
	bodyContains   []byte
	interval       time.Duration
	timeout        time.Duration
	requestTimeout time.Duration
	client         *http.Client
}

// NewProbe builds a probe for upstream from its readiness settings.
func NewProbe(upstream *url.URL, r config.Readiness, client *http.Client) *Probe {
	u := *upstream
	u.Path = joinPath(u.Path, r.Path)
	p := &Probe{
		url:            u.String(),
		expectStatus:   r.ExpectStatus,
		interval:       r.Interval.D(),
		timeout:        r.Timeout.D(),
		requestTimeout: r.RequestTimeout.D(),
		client:         client,
	}
	if r.BodyContains != "" {
		p.bodyContains = []byte(r.BodyContains)
	}
	return p
}

// Check runs a single probe and returns nil if the upstream is ready.
func (p *Probe) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != p.expectStatus {
		return fmt.Errorf("readiness: status %d, want %d", resp.StatusCode, p.expectStatus)
	}
	if p.bodyContains != nil && !bytes.Contains(body, p.bodyContains) {
		return errors.New("readiness: expected text not in response")
	}
	return nil
}

// Wait polls until the probe succeeds, the readiness timeout passes or ctx
// ends.
func (p *Probe) Wait(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	var last error
	for {
		if last = p.Check(ctx); last == nil {
			return nil
		}
		if err := sleepCtx(ctx, p.interval); err != nil {
			return fmt.Errorf("%w within %s: %v", ErrNotReady, p.timeout, last)
		}
	}
}

func joinPath(base, p string) string {
	switch {
	case base == "" || base == "/":
		return p
	case base[len(base)-1] == '/':
		return base + p[1:]
	default:
		return base + p
	}
}

// NewProbeClient returns an HTTP client for readiness probes that never
// follows redirects.
func NewProbeClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
