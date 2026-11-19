// Package proxy is the router's HTTP layer: it resolves the requested model,
// injects configured fields, waits for the scheduler and streams the request
// to the right upstream.
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/masood09/model-router/internal/auth"
	"github.com/masood09/model-router/internal/backend"
	"github.com/masood09/model-router/internal/config"
	"github.com/masood09/model-router/internal/metrics"
	"github.com/masood09/model-router/internal/scheduler"
)

// route is where a resolved request goes.
type route struct {
	name    string // catalog name, or "" for an unknown model
	backend *backend.Backend
	group   *scheduler.Group // nil for static backends
	key     scheduler.Key
	inject  map[string]json.RawMessage
}

type ctxKey struct{}

// Options wires a Router.
type Options struct {
	Config       *config.Config
	Backends     map[string]*backend.Backend
	Groups       map[string]*scheduler.Group
	Transport    http.RoundTripper
	ClientTokens *auth.Tokens
	AdminTokens  *auth.Tokens
	Metrics      *metrics.Registry
	Logger       *slog.Logger
}

// Router is the http.Handler of the service.
type Router struct {
	cfg        *config.Config
	backends   map[string]*backend.Backend
	groups     map[string]*scheduler.Group
	routes     map[string]*route
	modelNames []string
	modelPaths map[string]bool
	proxies    map[string]*httputil.ReverseProxy
	client     *auth.Tokens
	admin      *auth.Tokens
	log        *slog.Logger
	draining   atomic.Bool
	created    int64 // reported as each model's "created" in /v1/models

	mReg      *metrics.Registry
	mRequests *metrics.Counter
	mDuration *metrics.Summary
	mHeld     *metrics.Counter
}

// New builds the router. It panics on wiring errors that validated
// configuration cannot produce.
func New(o Options) *Router {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	r := &Router{
		cfg:        o.Config,
		backends:   o.Backends,
		groups:     o.Groups,
		routes:     map[string]*route{},
		modelPaths: map[string]bool{},
		proxies:    map[string]*httputil.ReverseProxy{},
		client:     o.ClientTokens,
		admin:      o.AdminTokens,
		log:        log,
		mReg:       o.Metrics,
		created:    time.Now().Unix(),
	}
	for _, p := range o.Config.ModelPaths {
		r.modelPaths[p] = true
	}
	for name, m := range o.Config.Models {
		b := o.Backends[m.Backend]
		rt := &route{name: name, backend: b, key: scheduler.Key{Backend: m.Backend, Variant: m.Variant}, inject: m.Inject}
		if !b.Static {
			rt.group = o.Groups[b.Group]
		}
		r.routes[name] = rt
		r.modelNames = append(r.modelNames, name)
	}
	sort.Strings(r.modelNames)
	for name, b := range o.Backends {
		r.proxies[name] = r.newReverseProxy(b, o.Transport)
	}
	if r.mReg != nil {
		r.mRequests = r.mReg.NewCounter("model_router_requests_total", "Requests handled, by catalog model and HTTP status.", "model", "code")
		r.mDuration = r.mReg.NewSummary("model_router_request_duration_seconds", "Request duration including queueing, by catalog model.", "model")
		r.mHeld = r.mReg.NewCounter("model_router_held_total", "Requests refused because their backend was held.", "backend")
	}
	return r
}

// SetDraining makes readyz report not-ready, ahead of shutdown.
func (r *Router) SetDraining(v bool) { r.draining.Store(v) }

func (r *Router) newReverseProxy(b *backend.Backend, transport http.RoundTripper) *httputil.ReverseProxy {
	target := b.Upstream
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			if b.PreserveHost {
				pr.Out.Host = pr.In.Host
			}
			if !b.ForwardAuthorization {
				pr.Out.Header.Del("Authorization")
				pr.Out.Header.Del("X-Api-Key")
			}
			if b.UpstreamToken != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+b.UpstreamToken)
			}
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorLog:      slog.NewLogLogger(r.log.Handler(), slog.LevelWarn),
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return // the client went away
			}
			if rt, ok := req.Context().Value(ctxKey{}).(*route); ok && rt.group != nil {
				rt.group.MarkUnready(rt.key)
			}
			// The connection was refused or never made, so the request was
			// not sent: let serveModel wait for the backend and retry, instead
			// of failing a request that only raced a restart.
			if a, ok := req.Context().Value(attemptKey{}).(*attempt); ok && a.retryable && isDialError(err) {
				a.dialFailed = true
				r.log.Info("upstream not accepting connections; waiting to retry", "backend", b.Name)
				return
			}
			r.log.Warn("upstream error", "backend", b.Name, "error", err.Error())
			writeError(w, http.StatusBadGateway, "upstream_error", "bad_gateway", "the model server could not be reached", 5)
		},
	}
}

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); requestIDRe.MatchString(id) {
		return id
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ServeHTTP implements http.Handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	id := requestID(req)
	w.Header().Set("X-Request-Id", id)
	rec := &recorder{ResponseWriter: w}
	var (
		label  = "-"
		target = ""
		queued time.Duration
	)
	defer func() {
		code := rec.code()
		if code == 0 {
			code = 499 // client closed the request before a response
		}
		d := time.Since(start)
		if r.mRequests != nil && label != "-" {
			r.mRequests.Inc(label, strconv.Itoa(code))
			r.mDuration.Observe(d, label)
		}
		r.log.Info("request",
			"id", id, "method", req.Method, "path", req.URL.Path,
			"model", label, "target", target, "status", code,
			"queued_ms", queued.Milliseconds(), "duration_ms", d.Milliseconds())
	}()

	p := req.URL.Path
	if len(p) >= len(r.cfg.RouterPrefix) && p[:len(r.cfg.RouterPrefix)] == r.cfg.RouterPrefix {
		r.serveRouter(rec, req, p[len(r.cfg.RouterPrefix):])
		return
	}
	if !r.client.Check(req) {
		writeError(rec, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "a valid API key is required", 0)
		return
	}
	if req.Method == http.MethodGet && p == "/v1/models" {
		r.serveModels(rec)
		return
	}
	if req.Method == http.MethodPost && r.modelPaths[p] {
		label, target, queued = r.serveModel(rec, req)
		return
	}
	r.servePassthrough(rec, req)
}

// serveModel handles a request whose body names a model. It returns the
// metrics label, the resolved target and the time spent queued.
func (r *Router) serveModel(w *recorder, req *http.Request) (label, target string, queued time.Duration) {
	label = "unknown"
	raw, err := io.ReadAll(http.MaxBytesReader(w, req.Body, r.cfg.Limits.MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", "request body is too large", 0)
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "bad_body", "could not read the request body", 0)
		}
		return label, "", 0
	}
	body, err := parseBody(raw)
	if errors.Is(err, errDuplicateKey) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "duplicate_key", "request body has a duplicate top-level key", 0)
		return label, "", 0
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "request body must be a JSON object", 0)
		return label, "", 0
	}

	rt, ok := r.resolve(body.model())
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", "the requested model is not available", 0)
		return label, "", 0
	}
	if rt.name != "" {
		label = rt.name
	}
	target = rt.key.String()
	// Refused before it can queue: queueing would ask the scheduler to start
	// a backend that another process has parked.
	if rt.backend.Held() {
		r.refuseHeld(w, rt.backend)
		return label, target, 0
	}
	body.inject(rt.inject)
	out, err := body.bytes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "encode_failed", "could not prepare the request", 0)
		return label, target, 0
	}

	req.ContentLength = int64(len(out))
	req.Header.Set("Content-Length", strconv.Itoa(len(out)))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(out)), nil }
	base := context.WithValue(req.Context(), ctxKey{}, rt)

	// A scheduled backend can be mid-restart (by a deploy, or anyone) before
	// the router's state has caught up. A refused connection means nothing
	// was sent, so the request waits for the backend like any queued request
	// and is retried once; any other upstream failure is reported at once.
	for try := 0; ; try++ {
		var release func()
		if rt.group != nil {
			var waited time.Duration
			var err error
			release, waited, err = rt.group.Acquire(req.Context(), rt.key)
			queued += waited
			if err != nil {
				r.acquireError(w, req, rt.backend, err)
				return label, target, queued
			}
		}
		a := &attempt{retryable: rt.group != nil && try == 0}
		attemptReq := req.WithContext(context.WithValue(base, attemptKey{}, a))
		attemptReq.Body = io.NopCloser(bytes.NewReader(out))
		r.forward(rt.key.Backend, w, attemptReq, release)
		if !a.dialFailed {
			return label, target, queued
		}
	}
}

// forward proxies one attempt to backend and then runs release (if not nil).
// release is deferred because ReverseProxy aborts the handler with
// panic(http.ErrAbortHandler) when the upstream fails after the response has
// started (an engine dying mid-stream). Skipping it there would leave the
// request counted as in flight for good, and the group then never switches
// or restarts the backend again.
func (r *Router) forward(backend string, w http.ResponseWriter, req *http.Request, release func()) {
	if release != nil {
		defer release()
	}
	r.proxies[backend].ServeHTTP(w, req)
}

// attempt records, per proxied attempt, whether it may be retried and
// whether it failed before reaching the upstream.
type attempt struct {
	retryable  bool
	dialFailed bool
}

type attemptKey struct{}

// isDialError reports whether err happened while connecting, so the request
// was never sent upstream.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

func (r *Router) acquireError(w http.ResponseWriter, req *http.Request, b *backend.Backend, err error) {
	switch {
	case req.Context().Err() != nil:
		// The client disconnected while waiting; nothing to send.
	case errors.Is(err, scheduler.ErrHeld):
		// The hold appeared while the request was queued.
		r.refuseHeld(w, b)
	case errors.Is(err, scheduler.ErrQueueFull):
		writeError(w, http.StatusServiceUnavailable, "server_busy", "queue_full", "too many requests are waiting for a model switch", 10)
	case errors.Is(err, scheduler.ErrTimeout):
		writeError(w, http.StatusServiceUnavailable, "server_busy", "switch_timeout", "the requested model did not become available in time", 30)
	case errors.Is(err, scheduler.ErrSwitch):
		r.log.Warn("switch failed for request", "error", err.Error())
		writeError(w, http.StatusServiceUnavailable, "server_error", "model_unavailable", "the requested model could not be loaded", 30)
	default:
		writeError(w, http.StatusServiceUnavailable, "server_error", "model_unavailable", "the requested model is not available", 30)
	}
}

// refuseHeld answers a request whose backend is held.
func (r *Router) refuseHeld(w http.ResponseWriter, b *backend.Backend) {
	if r.mHeld != nil {
		r.mHeld.Inc(b.Name)
	}
	writeError(w, http.StatusServiceUnavailable, "server_busy", "backend_held",
		"the requested model is paused while its hardware is in use elsewhere", int(b.HoldRetryAfter.Seconds()))
}

// resolve maps a requested model name to a route according to the
// unknown-model policy.
func (r *Router) resolve(name string) (*route, bool) {
	if rt, ok := r.routes[name]; ok {
		return rt, true
	}
	def := r.routes[r.cfg.DefaultModel]
	switch r.cfg.UnknownModel {
	case config.UnknownReject:
		return nil, false
	case config.UnknownDefault:
		return &route{backend: def.backend, group: def.group, key: def.key, inject: def.inject}, true
	default: // loaded: whatever the default model's group is serving, without switching
		if def.group == nil {
			return &route{backend: def.backend, key: def.key}, true
		}
		st := def.group.State()
		if st.Active.Backend == "" {
			// Nothing is active yet: fall back to the default model.
			return &route{backend: def.backend, group: def.group, key: def.key, inject: def.inject}, true
		}
		return &route{backend: r.backends[st.Active.Backend], group: def.group, key: st.Active}, true
	}
}

// servePassthrough forwards any other request to the default model's
// backend, without scheduling.
func (r *Router) servePassthrough(w http.ResponseWriter, req *http.Request) {
	if r.cfg.Passthrough == config.PassthroughOff {
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "not found", 0)
		return
	}
	def := r.routes[r.cfg.DefaultModel]
	r.proxies[def.backend.Name].ServeHTTP(w, req)
}

type modelEntry struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	OwnedBy string      `json:"owned_by"`
	Status  modelStatus `json:"status"`
}

type modelStatus struct {
	Value string `json:"value"`
}

// serveModels lists the catalog with each model's state, from the
// schedulers' cached state (no probing per request).
func (r *Router) serveModels(w http.ResponseWriter) {
	states := map[*scheduler.Group]scheduler.State{}
	data := make([]modelEntry, 0, len(r.modelNames))
	for _, name := range r.modelNames {
		rt := r.routes[name]
		status := "loaded"
		if rt.backend.Held() {
			status = "held"
		} else if rt.group != nil {
			st, ok := states[rt.group]
			if !ok {
				st = rt.group.State()
				states[rt.group] = st
			}
			switch {
			case st.Active == rt.key && st.Ready && !st.Switching:
				status = "loaded"
			case st.Switching:
				status = "loading"
			default:
				status = "unloaded"
			}
		}
		data = append(data, modelEntry{ID: name, Object: "model", Created: r.created, OwnedBy: rt.backend.Name, Status: modelStatus{status}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// serveRouter handles the router's own endpoints under RouterPrefix.
func (r *Router) serveRouter(w http.ResponseWriter, req *http.Request, sub string) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed", 0)
		return
	}
	switch sub {
	case "healthz":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case "readyz":
		if r.draining.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	case "metrics":
		if !r.admin.Check(req) {
			writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "a valid admin token is required", 0)
			return
		}
		if r.mReg == nil {
			writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "metrics are disabled", 0)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = r.mReg.Write(w)
	case "state":
		if !r.admin.Check(req) {
			writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "a valid admin token is required", 0)
			return
		}
		out := map[string]any{}
		for name, g := range r.groups {
			st := g.State()
			out[name] = map[string]any{
				"active": st.Active.String(), "ready": st.Ready, "switching": st.Switching,
				"inFlight": st.InFlight, "waiting": st.Waiting,
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"groups": out})
	default:
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "not found", 0)
	}
}
