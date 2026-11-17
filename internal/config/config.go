// Package config defines model-router's JSON configuration, its defaults and
// its validation rules.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Driver kinds.
const (
	DriverSystemd = "systemd"
	DriverCommand = "command"
	DriverStatic  = "static"
)

// Unknown-model policies.
const (
	UnknownLoaded  = "loaded"
	UnknownDefault = "default"
	UnknownReject  = "reject"
)

// Passthrough policies for requests that carry no model.
const (
	PassthroughDefault = "default"
	PassthroughOff     = "off"
)

// VariantPlaceholder is replaced by the variant in command driver arguments.
const VariantPlaceholder = "{variant}"

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	variantRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	unitRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:-]{0,200}\.(service|target)$`)
	modelRe   = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,256}$`)
)

// Config is the root of the configuration file.
type Config struct {
	// Listen is the TCP address the router listens on.
	Listen string `json:"listen"`
	// RouterPrefix is the path prefix of the router's own endpoints
	// (healthz, readyz, metrics). It must start and end with "/".
	RouterPrefix string `json:"routerPrefix"`
	// DefaultModel is a key of Models. Requests without a model, unknown
	// models under the "default" policy, and passthrough requests use it.
	DefaultModel string `json:"defaultModel"`
	// UnknownModel decides what happens to a model name not in Models:
	// "loaded" (serve with whatever is active in the default model's group,
	// never switching), "default" (switch to DefaultModel) or "reject" (404).
	UnknownModel string `json:"unknownModel"`
	// Passthrough decides where requests to paths other than ModelPaths go:
	// "default" (the default model's backend, without switching) or "off" (404).
	Passthrough string `json:"passthrough"`
	// ModelPaths are the POST paths whose JSON body carries a "model" field.
	ModelPaths []string `json:"modelPaths"`

	Limits   Limits             `json:"limits"`
	Timeouts Timeouts           `json:"timeouts"`
	Auth     Auth               `json:"auth"`
	Log      Log                `json:"log"`
	Groups   map[string]Group   `json:"groups"`
	Backends map[string]Backend `json:"backends"`
	Models   map[string]Model   `json:"models"`
}

// Limits bounds request sizes.
type Limits struct {
	MaxBodyBytes   int64 `json:"maxBodyBytes"`
	MaxHeaderBytes int   `json:"maxHeaderBytes"`
}

// Timeouts for the HTTP server and the upstream transport.
type Timeouts struct {
	ReadHeader             Duration `json:"readHeader"`
	Idle                   Duration `json:"idle"`
	ShutdownGrace          Duration `json:"shutdownGrace"`
	UpstreamDial           Duration `json:"upstreamDial"`
	UpstreamResponseHeader Duration `json:"upstreamResponseHeader"`
}

// Auth configures optional bearer-token authentication. Tokens are read
// from a file (one token per line, "#" comments allowed) or from an
// environment variable (comma-separated). Both may be set; the union is used.
type Auth struct {
	ClientTokensFile string `json:"clientTokensFile"`
	ClientTokensEnv  string `json:"clientTokensEnv"`
	AdminTokensFile  string `json:"adminTokensFile"`
	AdminTokensEnv   string `json:"adminTokensEnv"`
}

// Log configures logging.
type Log struct {
	// Level is one of "debug", "info", "warn", "error".
	Level string `json:"level"`
}

// Group configures the switching queue of a resource group.
type Group struct {
	// MaxWaiters is the largest number of requests that may wait for a switch.
	MaxWaiters int `json:"maxWaiters"`
	// MaxWait is the longest a request may wait for its model to be active.
	MaxWait Duration `json:"maxWait"`
	// StateRefresh is how often the active backend's readiness is re-probed.
	StateRefresh Duration `json:"stateRefresh"`
}

// Backend is one upstream inference server and how to manage it.
type Backend struct {
	// Group is the resource group. Backends in a group are mutually exclusive.
	// Static backends must not have a group.
	Group string `json:"group"`
	// Upstream is the base URL requests are proxied to.
	Upstream string `json:"upstream"`
	// PreserveHost sends the client's Host header upstream instead of the
	// upstream's own host.
	PreserveHost bool `json:"preserveHost"`
	// ForwardAuthorization passes the client's Authorization and x-api-key
	// headers upstream. By default they are removed.
	ForwardAuthorization bool `json:"forwardAuthorization"`
	// UpstreamTokenFile, if set, holds a bearer token sent to the upstream.
	UpstreamTokenFile string `json:"upstreamTokenFile"`

	Driver    Driver    `json:"driver"`
	Readiness Readiness `json:"readiness"`

	// Hold lets another process park the backend. See Hold.
	Hold Hold `json:"hold"`
}

// Hold lets another process park a backend, for example a controller that
// hands the backend's GPU to a different workload. While File exists, every
// request for the backend is refused with 503 and Retry-After, and the
// scheduler never switches to it, so the router cannot restart the backend
// behind that controller's back. The router only reads the file.
type Hold struct {
	// File is the marker path. Empty disables the hold.
	File string `json:"file"`
	// RetryAfter is sent with each refusal.
	RetryAfter Duration `json:"retryAfter"`
}

// Driver selects and configures how a backend is started, stopped and switched.
type Driver struct {
	Kind    string         `json:"kind"`
	Systemd *SystemdDriver `json:"systemd,omitempty"`
	Command *CommandDriver `json:"command,omitempty"`
}

// SystemdDriver switches variants by writing a selection file and restarting
// a systemd unit.
type SystemdDriver struct {
	// Unit is the systemd unit to restart and stop.
	Unit string `json:"unit"`
	// SelectionFile, if set, receives the variant name before each restart.
	SelectionFile string `json:"selectionFile"`
	// DefaultVariant is assumed when the selection file does not exist yet.
	DefaultVariant string `json:"defaultVariant"`
	// Systemctl is the systemctl executable.
	Systemctl string `json:"systemctl"`
	// ActionTimeout bounds each systemctl invocation.
	ActionTimeout Duration `json:"actionTimeout"`
}

// CommandDriver runs configured commands to activate and deactivate variants.
type CommandDriver struct {
	// Activate is the argv run to activate a variant. "{variant}" in any
	// argument is replaced by the variant name.
	Activate []string `json:"activate"`
	// Deactivate is the argv run to deactivate the backend (optional).
	Deactivate []string `json:"deactivate"`
	// DefaultVariant is assumed active at startup (optional).
	DefaultVariant string `json:"defaultVariant"`
	// ActionTimeout bounds each command.
	ActionTimeout Duration `json:"actionTimeout"`
}

// Readiness configures how the router decides a backend is ready to serve.
type Readiness struct {
	// Path is requested with GET on the upstream.
	Path string `json:"path"`
	// ExpectStatus is the HTTP status that counts as ready.
	ExpectStatus int `json:"expectStatus"`
	// BodyContains, if set, must appear in the response body.
	BodyContains string `json:"bodyContains"`
	// Interval between probes while waiting.
	Interval Duration `json:"interval"`
	// Timeout is how long to wait for readiness after an activation.
	Timeout Duration `json:"timeout"`
	// RequestTimeout bounds each probe request.
	RequestTimeout Duration `json:"requestTimeout"`
}

// Model maps a public model name to a backend variant.
type Model struct {
	Backend string `json:"backend"`
	Variant string `json:"variant"`
	// Inject adds these top-level JSON fields to the request body when the
	// client did not set them.
	Inject map[string]json.RawMessage `json:"inject"`
}

// Defaults applied to unset fields.
var (
	DefaultListen                    = "127.0.0.1:8080"
	DefaultRouterPrefix              = "/_router/"
	DefaultModelPaths                = []string{"/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/messages"}
	DefaultMaxBodyBytes        int64 = 64 << 20
	DefaultMaxHeaderBytes            = 1 << 20
	DefaultReadHeader                = 10 * time.Second
	DefaultIdle                      = 120 * time.Second
	DefaultShutdownGrace             = 60 * time.Second
	DefaultUpstreamDial              = 10 * time.Second
	DefaultUpstreamRespHdr           = 30 * time.Minute
	DefaultMaxWaiters                = 1024
	DefaultMaxWait                   = 20 * time.Minute
	DefaultStateRefresh              = 15 * time.Second
	DefaultActionTimeout             = 5 * time.Minute
	DefaultReadyPath                 = "/v1/models"
	DefaultReadyStatus               = 200
	DefaultReadyInterval             = 2 * time.Second
	DefaultReadyTimeout              = 10 * time.Minute
	DefaultReadyRequestTimeout       = 5 * time.Second
	DefaultHoldRetryAfter            = 60 * time.Second
)

// Load reads, parses and validates the configuration file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- path comes from the operator
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(b)
}

// Parse parses and validates configuration from JSON. Unknown fields are
// rejected so typos do not silently fall back to defaults.
func Parse(b []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if dec.More() {
		return nil, errors.New("parse config: trailing data after the JSON object")
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func setDur(d *Duration, v time.Duration) {
	if *d == 0 {
		*d = Duration(v)
	}
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.RouterPrefix == "" {
		c.RouterPrefix = DefaultRouterPrefix
	}
	if c.UnknownModel == "" {
		c.UnknownModel = UnknownLoaded
	}
	if c.Passthrough == "" {
		c.Passthrough = PassthroughDefault
	}
	if len(c.ModelPaths) == 0 {
		c.ModelPaths = append([]string(nil), DefaultModelPaths...)
	}
	if c.Limits.MaxBodyBytes == 0 {
		c.Limits.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.Limits.MaxHeaderBytes == 0 {
		c.Limits.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	setDur(&c.Timeouts.ReadHeader, DefaultReadHeader)
	setDur(&c.Timeouts.Idle, DefaultIdle)
	setDur(&c.Timeouts.ShutdownGrace, DefaultShutdownGrace)
	setDur(&c.Timeouts.UpstreamDial, DefaultUpstreamDial)
	setDur(&c.Timeouts.UpstreamResponseHeader, DefaultUpstreamRespHdr)
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Groups == nil {
		c.Groups = map[string]Group{}
	}
	// Every group referenced by a backend gets an entry.
	for _, b := range c.Backends {
		if b.Group != "" {
			if _, ok := c.Groups[b.Group]; !ok {
				c.Groups[b.Group] = Group{}
			}
		}
	}
	for name, g := range c.Groups {
		if g.MaxWaiters == 0 {
			g.MaxWaiters = DefaultMaxWaiters
		}
		setDur(&g.MaxWait, DefaultMaxWait)
		setDur(&g.StateRefresh, DefaultStateRefresh)
		c.Groups[name] = g
	}
	for name, b := range c.Backends {
		r := &b.Readiness
		if r.Path == "" {
			r.Path = DefaultReadyPath
		}
		if r.ExpectStatus == 0 {
			r.ExpectStatus = DefaultReadyStatus
		}
		setDur(&r.Interval, DefaultReadyInterval)
		setDur(&r.Timeout, DefaultReadyTimeout)
		setDur(&r.RequestTimeout, DefaultReadyRequestTimeout)
		setDur(&b.Hold.RetryAfter, DefaultHoldRetryAfter)
		if s := b.Driver.Systemd; s != nil {
			if s.Systemctl == "" {
				s.Systemctl = "systemctl"
			}
			setDur(&s.ActionTimeout, DefaultActionTimeout)
		}
		if cm := b.Driver.Command; cm != nil {
			setDur(&cm.ActionTimeout, DefaultActionTimeout)
		}
		c.Backends[name] = b
	}
}

// Validate checks the configuration for consistency. It assumes defaults
// have been applied.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Listen == "" {
		add("listen must not be empty")
	}
	if !strings.HasPrefix(c.RouterPrefix, "/") || !strings.HasSuffix(c.RouterPrefix, "/") || len(c.RouterPrefix) < 3 {
		add("routerPrefix %q must start and end with \"/\" and not be the root", c.RouterPrefix)
	}
	switch c.UnknownModel {
	case UnknownLoaded, UnknownDefault, UnknownReject:
	default:
		add("unknownModel must be %q, %q or %q, not %q", UnknownLoaded, UnknownDefault, UnknownReject, c.UnknownModel)
	}
	switch c.Passthrough {
	case PassthroughDefault, PassthroughOff:
	default:
		add("passthrough must be %q or %q, not %q", PassthroughDefault, PassthroughOff, c.Passthrough)
	}
	for _, p := range c.ModelPaths {
		if !strings.HasPrefix(p, "/") {
			add("modelPaths entry %q must start with \"/\"", p)
		}
		if strings.HasPrefix(p, c.RouterPrefix) {
			add("modelPaths entry %q is under routerPrefix %q", p, c.RouterPrefix)
		}
	}
	if c.Limits.MaxBodyBytes < 1024 {
		add("limits.maxBodyBytes must be at least 1024")
	}
	if c.Limits.MaxHeaderBytes < 4096 {
		add("limits.maxHeaderBytes must be at least 4096")
	}
	for _, t := range []struct {
		name string
		d    Duration
	}{
		{"timeouts.readHeader", c.Timeouts.ReadHeader},
		{"timeouts.idle", c.Timeouts.Idle},
		{"timeouts.upstreamDial", c.Timeouts.UpstreamDial},
		{"timeouts.upstreamResponseHeader", c.Timeouts.UpstreamResponseHeader},
	} {
		if t.d <= 0 {
			add("%s must be positive", t.name)
		}
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level must be debug, info, warn or error, not %q", c.Log.Level)
	}

	for name, g := range c.Groups {
		if !nameRe.MatchString(name) {
			add("group name %q is invalid", name)
		}
		if g.MaxWaiters < 1 {
			add("groups.%s.maxWaiters must be at least 1", name)
		}
		if g.MaxWait <= 0 || g.StateRefresh <= 0 {
			add("groups.%s: maxWait and stateRefresh must be positive", name)
		}
	}

	if len(c.Backends) == 0 {
		add("at least one backend is required")
	}
	for _, name := range sortedKeys(c.Backends) {
		errs = append(errs, c.validateBackend(name, c.Backends[name])...)
	}

	if len(c.Models) == 0 {
		add("at least one model is required")
	}
	for _, name := range sortedKeys(c.Models) {
		m := c.Models[name]
		if !modelRe.MatchString(name) {
			add("model name %q is invalid", name)
		}
		b, ok := c.Backends[m.Backend]
		if !ok {
			add("models.%s: backend %q is not defined", name, m.Backend)
			continue
		}
		if b.Driver.Kind == DriverStatic {
			if m.Variant != "" && !variantRe.MatchString(m.Variant) {
				add("models.%s: variant %q is invalid", name, m.Variant)
			}
		} else if !variantRe.MatchString(m.Variant) {
			add("models.%s: variant %q is invalid (letters, digits, '.', '_', '-'; max 64)", name, m.Variant)
		}
		for k, v := range m.Inject {
			if k == "" || k == "model" {
				add("models.%s.inject: key %q is not allowed", name, k)
			}
			if !json.Valid(v) {
				add("models.%s.inject.%s is not valid JSON", name, k)
			}
		}
	}
	if c.DefaultModel == "" {
		add("defaultModel is required")
	} else if _, ok := c.Models[c.DefaultModel]; !ok {
		add("defaultModel %q is not one of models", c.DefaultModel)
	}

	return errors.Join(errs...)
}

func (c *Config) validateBackend(name string, b Backend) []error {
	var errs []error
	add := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("backends.%s: "+format, append([]any{name}, a...)...))
	}
	if !nameRe.MatchString(name) {
		errs = append(errs, fmt.Errorf("backend name %q is invalid", name))
	}
	u, err := url.Parse(b.Upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("upstream %q must be an absolute http(s) URL", b.Upstream)
	} else if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		add("upstream %q must not contain credentials, a query or a fragment", b.Upstream)
	}
	if b.UpstreamTokenFile != "" && !filepath.IsAbs(b.UpstreamTokenFile) {
		add("upstreamTokenFile must be an absolute path")
	}
	if b.Hold.File != "" && (!filepath.IsAbs(b.Hold.File) || filepath.Clean(b.Hold.File) != b.Hold.File) {
		add("hold.file %q must be a clean absolute path", b.Hold.File)
	}
	if ra := b.Hold.RetryAfter.D(); ra < time.Second || ra > time.Hour {
		add("hold.retryAfter must be between 1s and 1h")
	}

	r := b.Readiness
	if !strings.HasPrefix(r.Path, "/") {
		add("readiness.path %q must start with \"/\"", r.Path)
	}
	if r.ExpectStatus < 100 || r.ExpectStatus > 599 {
		add("readiness.expectStatus %d is not an HTTP status", r.ExpectStatus)
	}
	if r.Interval <= 0 || r.Timeout <= 0 || r.RequestTimeout <= 0 {
		add("readiness interval, timeout and requestTimeout must be positive")
	}

	var actionTimeout Duration
	switch b.Driver.Kind {
	case DriverStatic:
		if b.Group != "" {
			add("static backends must not belong to a group")
		}
		if b.Driver.Systemd != nil || b.Driver.Command != nil {
			add("static driver takes no systemd or command settings")
		}
	case DriverSystemd:
		s := b.Driver.Systemd
		if s == nil {
			add("driver.systemd settings are required")
			break
		}
		if b.Driver.Command != nil {
			add("driver.command must not be set for a systemd driver")
		}
		if !unitRe.MatchString(s.Unit) {
			add("driver.systemd.unit %q is not a valid unit name", s.Unit)
		}
		if s.SelectionFile != "" && !filepath.IsAbs(s.SelectionFile) {
			add("driver.systemd.selectionFile must be an absolute path")
		}
		if s.DefaultVariant != "" && !variantRe.MatchString(s.DefaultVariant) {
			add("driver.systemd.defaultVariant %q is invalid", s.DefaultVariant)
		}
		if s.Systemctl == "" {
			add("driver.systemd.systemctl must not be empty")
		}
		actionTimeout = s.ActionTimeout
	case DriverCommand:
		cm := b.Driver.Command
		if cm == nil {
			add("driver.command settings are required")
			break
		}
		if b.Driver.Systemd != nil {
			add("driver.systemd must not be set for a command driver")
		}
		if len(cm.Activate) == 0 || cm.Activate[0] == "" {
			add("driver.command.activate must name a program")
		}
		if len(cm.Deactivate) > 0 && cm.Deactivate[0] == "" {
			add("driver.command.deactivate must name a program")
		}
		for _, a := range cm.Activate[min(1, len(cm.Activate)):] {
			if strings.Contains(a, "\x00") {
				add("driver.command arguments must not contain NUL")
			}
		}
		if len(cm.Activate) > 0 && strings.Contains(cm.Activate[0], VariantPlaceholder) {
			add("driver.command.activate: the program itself must not contain %s", VariantPlaceholder)
		}
		if cm.DefaultVariant != "" && !variantRe.MatchString(cm.DefaultVariant) {
			add("driver.command.defaultVariant %q is invalid", cm.DefaultVariant)
		}
		actionTimeout = cm.ActionTimeout
	default:
		add("driver.kind must be %q, %q or %q, not %q", DriverSystemd, DriverCommand, DriverStatic, b.Driver.Kind)
	}

	if b.Driver.Kind == DriverSystemd || b.Driver.Kind == DriverCommand {
		if b.Group == "" {
			add("a %s backend needs a group", b.Driver.Kind)
		} else if g, ok := c.Groups[b.Group]; ok {
			if actionTimeout <= 0 {
				add("driver actionTimeout must be positive")
			}
			if actionTimeout > g.MaxWait {
				add("driver actionTimeout (%s) exceeds groups.%s.maxWait (%s)", actionTimeout.D(), b.Group, g.MaxWait.D())
			}
			if r.Timeout > g.MaxWait {
				add("readiness.timeout (%s) exceeds groups.%s.maxWait (%s)", r.Timeout.D(), b.Group, g.MaxWait.D())
			}
		}
	}
	return errs
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ValidVariant reports whether s is an acceptable variant name.
func ValidVariant(s string) bool { return variantRe.MatchString(s) }
