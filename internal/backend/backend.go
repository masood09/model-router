// Package backend manages upstream inference servers: how a model variant is
// activated or deactivated (drivers) and how readiness is detected (probe).
package backend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/masood09/model-router/internal/config"
)

// Driver changes which variant a backend serves.
type Driver interface {
	// Activate makes variant the one the backend serves. It returns once the
	// lifecycle action has been issued; readiness is checked separately.
	Activate(ctx context.Context, variant string) error
	// Deactivate stops the backend so that another backend in its group can
	// use the shared resource.
	Deactivate(ctx context.Context) error
	// Current reports the variant the driver believes is selected, or "" if
	// unknown.
	Current() (string, error)
}

// Backend is a configured upstream with its lifecycle driver and probe.
type Backend struct {
	Name     string
	Group    string
	Upstream *url.URL
	// Static backends have no lifecycle and are never scheduled.
	Static bool

	PreserveHost         bool
	ForwardAuthorization bool
	// UpstreamToken, if non-empty, is sent as a bearer token upstream.
	UpstreamToken string

	Driver Driver
	probe  *Probe
}

// New builds a Backend from its configuration. client is used for readiness
// probes.
func New(name string, c config.Backend, client *http.Client) (*Backend, error) {
	u, err := url.Parse(c.Upstream)
	if err != nil {
		return nil, fmt.Errorf("backend %s: upstream: %w", name, err)
	}
	b := &Backend{
		Name:                 name,
		Group:                c.Group,
		Upstream:             u,
		PreserveHost:         c.PreserveHost,
		ForwardAuthorization: c.ForwardAuthorization,
		probe:                NewProbe(u, c.Readiness, client),
	}
	if c.UpstreamTokenFile != "" {
		tok, err := readToken(c.UpstreamTokenFile)
		if err != nil {
			return nil, fmt.Errorf("backend %s: %w", name, err)
		}
		b.UpstreamToken = tok
	}
	switch c.Driver.Kind {
	case config.DriverStatic:
		b.Static = true
		b.Driver = Static{}
	case config.DriverSystemd:
		b.Driver = NewSystemd(*c.Driver.Systemd)
	case config.DriverCommand:
		b.Driver = NewCommand(*c.Driver.Command)
	default:
		return nil, fmt.Errorf("backend %s: unknown driver kind %q", name, c.Driver.Kind)
	}
	return b, nil
}

// Ready runs one readiness probe.
func (b *Backend) Ready(ctx context.Context) bool { return b.probe.Check(ctx) == nil }

// WaitReady polls the readiness probe until it succeeds, the configured
// readiness timeout passes, or ctx ends.
func (b *Backend) WaitReady(ctx context.Context) error { return b.probe.Wait(ctx) }

func readToken(path string) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- operator-configured path
	if err != nil {
		return "", fmt.Errorf("read upstream token: %w", err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", errors.New("upstream token file is empty")
	}
	return tok, nil
}

// Static is the driver of an always-on upstream: nothing to activate.
type Static struct{}

// Activate is a no-op.
func (Static) Activate(context.Context, string) error { return nil }

// Deactivate is a no-op.
func (Static) Deactivate(context.Context) error { return nil }

// Current returns "" because a static backend serves every variant.
func (Static) Current() (string, error) { return "", nil }

// sleepCtx waits for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
