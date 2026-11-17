// Package server assembles a running model-router from its configuration.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/masood09/model-router/internal/auth"
	"github.com/masood09/model-router/internal/backend"
	"github.com/masood09/model-router/internal/config"
	"github.com/masood09/model-router/internal/metrics"
	"github.com/masood09/model-router/internal/proxy"
	"github.com/masood09/model-router/internal/scheduler"
)

// Server is a configured, not yet running, router.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	http    *http.Server
	router  *proxy.Router
	groups  map[string]*scheduler.Group
	metrics *metrics.Registry

	// bg scopes background work: readiness refresh and switches. It ends
	// when Serve returns, after the drain.
	bg     context.Context
	stopBG context.CancelFunc
}

// lifecycle adapts a backend to scheduler.Lifecycle.
type lifecycle struct{ b *backend.Backend }

func (l lifecycle) Activate(ctx context.Context, v string) error { return l.b.Driver.Activate(ctx, v) }
func (l lifecycle) Deactivate(ctx context.Context) error         { return l.b.Driver.Deactivate(ctx) }
func (l lifecycle) WaitReady(ctx context.Context) error          { return l.b.WaitReady(ctx) }
func (l lifecycle) Ready(ctx context.Context) bool               { return l.b.Ready(ctx) }
func (l lifecycle) Current() (string, error)                     { return l.b.Driver.Current() }

// Running forwards to drivers that can report their process state, so the
// scheduler waits for a backend that is still loading instead of
// restarting it.
func (l lifecycle) Running(ctx context.Context) (running, ok bool) {
	if r, isRunner := l.b.Driver.(interface {
		Running(context.Context) (bool, bool)
	}); isRunner {
		return r.Running(ctx)
	}
	return false, false
}

// NewTransport returns the shared upstream transport. It ignores proxy
// environment variables so upstream traffic always goes where it is
// configured to.
func NewTransport(c *config.Config) *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   c.Timeouts.UpstreamDial.D(),
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: c.Timeouts.UpstreamResponseHeader.D(),
	}
}

// New builds a server from validated configuration.
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	transport := NewTransport(cfg)
	probeClient := backend.NewProbeClient(transport)

	backends := map[string]*backend.Backend{}
	for name, bc := range cfg.Backends {
		b, err := backend.New(name, bc, probeClient)
		if err != nil {
			return nil, err
		}
		backends[name] = b
	}

	clientTokens, err := auth.Load(cfg.Auth.ClientTokensFile, cfg.Auth.ClientTokensEnv)
	if err != nil {
		return nil, fmt.Errorf("client tokens: %w", err)
	}
	adminTokens, err := auth.Load(cfg.Auth.AdminTokensFile, cfg.Auth.AdminTokensEnv)
	if err != nil {
		return nil, fmt.Errorf("admin tokens: %w", err)
	}

	reg := metrics.New()
	switches := reg.NewCounter("model_router_switches_total", "Model switches, by group and result.", "group", "result")
	switchDur := reg.NewSummary("model_router_switch_duration_seconds", "Time taken by model switches.", "group")
	queueWait := reg.NewSummary("model_router_queue_wait_seconds", "Time requests spent waiting for a switch.", "group")
	obs := scheduler.Observer{
		Switch: func(group string, _, _ scheduler.Key, d time.Duration, err error) {
			result := "ok"
			if err != nil {
				result = "error"
			}
			switches.Inc(group, result)
			switchDur.Observe(d, group)
		},
		QueueWait: func(group string, d time.Duration) { queueWait.Observe(d, group) },
	}

	bg, stopBG := context.WithCancel(context.Background())
	groups := map[string]*scheduler.Group{}
	for gname, gc := range cfg.Groups {
		members := map[string]scheduler.Lifecycle{}
		for name, b := range backends {
			if b.Group == gname {
				members[name] = lifecycle{b}
			}
		}
		if len(members) == 0 {
			continue
		}
		groups[gname] = scheduler.New(scheduler.Options{
			Name: gname, MaxWaiters: gc.MaxWaiters, MaxWait: gc.MaxWait.D(), Refresh: gc.StateRefresh.D(),
			Backends: members, Observer: obs, Logger: log, Context: bg,
		})
	}
	registerGauges(reg, groups)

	router := proxy.New(proxy.Options{
		Config: cfg, Backends: backends, Groups: groups, Transport: transport,
		ClientTokens: clientTokens, AdminTokens: adminTokens, Metrics: reg, Logger: log,
	})
	return &Server{
		cfg:     cfg,
		log:     log,
		router:  router,
		groups:  groups,
		metrics: reg,
		bg:      bg,
		stopBG:  stopBG,
		http: &http.Server{
			Addr:              cfg.Listen,
			Handler:           router,
			ReadHeaderTimeout: cfg.Timeouts.ReadHeader.D(),
			IdleTimeout:       cfg.Timeouts.Idle.D(),
			MaxHeaderBytes:    cfg.Limits.MaxHeaderBytes,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		},
	}, nil
}

func registerGauges(reg *metrics.Registry, groups map[string]*scheduler.Group) {
	names := make([]string, 0, len(groups))
	for n := range groups {
		names = append(names, n)
	}
	sort.Strings(names)
	collect := func(f func(scheduler.State) float64) func() []metrics.Sample {
		return func() []metrics.Sample {
			out := make([]metrics.Sample, 0, len(names))
			for _, n := range names {
				out = append(out, metrics.Sample{LabelValues: []string{n}, Value: f(groups[n].State())})
			}
			return out
		}
	}
	b2f := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	reg.GaugeFunc("model_router_inflight_requests", "Requests being served, by group.", []string{"group"},
		collect(func(s scheduler.State) float64 { return float64(s.InFlight) }))
	reg.GaugeFunc("model_router_waiting_requests", "Requests waiting for a switch, by group.", []string{"group"},
		collect(func(s scheduler.State) float64 { return float64(s.Waiting) }))
	reg.GaugeFunc("model_router_group_ready", "Whether the group's active model is ready.", []string{"group"},
		collect(func(s scheduler.State) float64 { return b2f(s.Ready && !s.Switching) }))
	reg.GaugeFunc("model_router_active_model", "The active backend variant of each group (value 1).",
		[]string{"group", "active"}, func() []metrics.Sample {
			out := make([]metrics.Sample, 0, len(names))
			for _, n := range names {
				out = append(out, metrics.Sample{LabelValues: []string{n, groups[n].State().Active.String()}, Value: 1})
			}
			return out
		})
}

// Handler returns the HTTP handler, for embedding and tests.
func (s *Server) Handler() http.Handler { return s.router }

// ListenAndServe listens on the configured address and serves until ctx
// ends, then drains.
func (s *Server) ListenAndServe(ctx context.Context) error {
	l, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	return s.Serve(ctx, l)
}

// Serve serves on l until ctx ends. Shutdown first marks the router as
// draining (readyz fails), stops accepting connections, then waits up to
// the configured grace period for in-flight requests, including streams.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	// Switches still running when the drain ends are cancelled with bg.
	defer s.stopBG()
	for _, g := range s.groups {
		g.Detect(s.bg)
		go g.Run(s.bg)
	}

	errc := make(chan error, 1)
	go func() { errc <- s.http.Serve(l) }()
	s.log.Info("listening", "addr", l.Addr().String(), "models", len(s.cfg.Models), "groups", len(s.groups))

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "grace", s.cfg.Timeouts.ShutdownGrace.D().String())
	s.router.SetDraining(true)
	sctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeouts.ShutdownGrace.D())
	defer cancel()
	if err := s.http.Shutdown(sctx); err != nil {
		s.log.Warn("grace period ended with requests still open; closing them", "error", err.Error())
		_ = s.http.Close()
	}
	<-errc
	return nil
}
