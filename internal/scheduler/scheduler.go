// Package scheduler serialises model switches within a resource group.
//
// A group runs one (backend, variant) at a time. Requests for the active,
// ready variant pass straight through. A request for anything else joins a
// FIFO queue; once nothing is in flight the scheduler switches to the queue
// head's variant and releases every waiter for it. While a switch is pending,
// new requests for the current variant queue too, so a steady stream of them
// cannot starve the waiting variant. In-flight requests are never cut off.
//
// A backend parked by a hold (see Holder) is never switched to: the waiters
// for it fail with ErrHeld, and the group's state is left as it was.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Key identifies a backend variant.
type Key struct {
	Backend string
	Variant string
}

// String renders the key for logs and metrics.
func (k Key) String() string {
	switch {
	case k.Backend == "":
		return "none"
	case k.Variant == "":
		return k.Backend
	default:
		return k.Backend + "/" + k.Variant
	}
}

// Lifecycle is what the scheduler needs from a backend.
type Lifecycle interface {
	Activate(ctx context.Context, variant string) error
	Deactivate(ctx context.Context) error
	WaitReady(ctx context.Context) error
	Ready(ctx context.Context) bool
	Current() (string, error)
}

// Runner is implemented by lifecycles that can tell whether the backend's
// process is up or starting (for example a systemd unit that is active or
// activating). It lets the scheduler wait for a backend that is merely still
// loading instead of restarting it. ok is false when the state is unknown.
type Runner interface {
	Running(ctx context.Context) (running, ok bool)
}

// StartupGracer is implemented by Runners that bound how long a backend that
// is up but not ready is waited for before it is restarted. A process can be
// up yet never become ready on its own, such as a server whose engine has
// stopped and that only reloads it when a request reaches it; the router
// sends none while it waits. Without it the wait lasts as long as WaitReady.
type StartupGracer interface {
	StartupGrace() time.Duration
}

// Holder is implemented by lifecycles that another process can park. While
// Held is true the scheduler does not activate, restart or wait for the
// backend.
type Holder interface {
	Held() bool
}

// Errors returned by Acquire.
var (
	ErrQueueFull = errors.New("too many requests waiting for a model switch")
	ErrTimeout   = errors.New("timed out waiting for the model to become active")
	ErrSwitch    = errors.New("model switch failed")
	ErrHeld      = errors.New("backend is held")
)

// Observer receives scheduler events, for metrics. Any field may be nil.
type Observer struct {
	Switch    func(group string, from, to Key, d time.Duration, err error)
	QueueWait func(group string, d time.Duration)
}

// Options configures a Group.
type Options struct {
	Name       string
	MaxWaiters int
	MaxWait    time.Duration
	Refresh    time.Duration
	Backends   map[string]Lifecycle
	Observer   Observer
	Logger     *slog.Logger
	// Context bounds switches: cancelling it aborts one in progress (its
	// waiters fail). Defaults to context.Background().
	Context context.Context
}

type ticket struct {
	key  Key
	done chan error // buffered(1); nil = granted (inflight already counted)
}

// Group is the scheduler of one resource group.
type Group struct {
	name       string
	maxWaiters int
	maxWait    time.Duration
	refresh    time.Duration
	backends   map[string]Lifecycle
	names      []string // backend names, sorted, for deterministic detection
	obs        Observer
	log        *slog.Logger
	ctx        context.Context

	mu        sync.Mutex
	active    Key
	ready     bool
	switching bool
	inflight  int
	queue     []*ticket
}

// New creates a group scheduler. Call Detect before serving and Run to keep
// the readiness state fresh.
func New(o Options) *Group {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	ctx := o.Context
	if ctx == nil {
		ctx = context.Background()
	}
	names := make([]string, 0, len(o.Backends))
	for n := range o.Backends {
		names = append(names, n)
	}
	sort.Strings(names)
	return &Group{
		name:       o.Name,
		maxWaiters: o.MaxWaiters,
		maxWait:    o.MaxWait,
		refresh:    o.Refresh,
		backends:   o.Backends,
		names:      names,
		obs:        o.Observer,
		log:        log.With("group", o.Name),
		ctx:        ctx,
	}
}

// State is a snapshot of a group.
type State struct {
	Active    Key
	Ready     bool
	Switching bool
	InFlight  int
	Waiting   int
}

// State returns a snapshot without probing anything.
func (g *Group) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return State{Active: g.active, Ready: g.ready, Switching: g.switching, InFlight: g.inflight, Waiting: len(g.queue)}
}

// Detect works out which backend variant is currently serving by probing
// each backend, in name order, and adopting the first that is ready. If none
// is ready, the group starts with nothing active and the first request
// triggers a switch.
func (g *Group) Detect(ctx context.Context) {
	for _, name := range g.names {
		b := g.backends[name]
		if !b.Ready(ctx) {
			continue
		}
		v, err := b.Current()
		if err != nil || v == "" {
			continue
		}
		g.mu.Lock()
		if !g.switching {
			g.active, g.ready = Key{name, v}, true
		}
		g.mu.Unlock()
		g.log.Info("detected active model", "active", Key{name, v}.String())
		return
	}
	// None ready: adopt one that is still starting (its unit is up and it
	// knows its variant), not ready yet. A first request for that variant
	// then waits for the load instead of restarting the backend, which is
	// what happens when the router and its backend restart together.
	for _, name := range g.names {
		b := g.backends[name]
		r, ok := b.(Runner)
		if !ok {
			continue
		}
		if running, known := r.Running(ctx); !known || !running {
			continue
		}
		v, err := b.Current()
		if err != nil || v == "" {
			continue
		}
		g.mu.Lock()
		if !g.switching && g.active.Backend == "" {
			g.active, g.ready = Key{name, v}, false
		}
		g.mu.Unlock()
		g.log.Info("detected a model still starting", "active", Key{name, v}.String())
		return
	}
	g.log.Info("no model active at startup")
}

// Run re-probes the active backend every refresh interval until ctx ends.
func (g *Group) Run(ctx context.Context) {
	t := time.NewTicker(g.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.refreshOnce(ctx)
		}
	}
}

func (g *Group) refreshOnce(ctx context.Context) {
	g.mu.Lock()
	if g.switching {
		g.mu.Unlock()
		return
	}
	snap := g.active
	g.mu.Unlock()
	if snap.Backend == "" {
		g.Detect(ctx)
		return
	}
	ok := g.backends[snap.Backend].Ready(ctx)
	g.mu.Lock()
	if !g.switching && g.active == snap && g.ready != ok {
		g.ready = ok
		g.log.Info("readiness changed", "active", snap.String(), "ready", ok)
		if ok {
			g.kick()
		}
	}
	g.mu.Unlock()
}

// MarkUnready records that the upstream of key failed a request, so the next
// request re-checks it instead of assuming it is up.
func (g *Group) MarkUnready(key Key) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.switching && g.active == key {
		g.ready = false
	}
}

// Acquire waits until key is the group's active, ready variant and counts
// the caller as in flight. The returned release must be called exactly once
// when the request is finished. It also returns how long the caller waited.
func (g *Group) Acquire(ctx context.Context, key Key) (release func(), waited time.Duration, err error) {
	if _, ok := g.backends[key.Backend]; !ok {
		return nil, 0, fmt.Errorf("backend %q is not in group %q", key.Backend, g.name)
	}
	g.mu.Lock()
	if !g.switching && len(g.queue) == 0 && g.ready && g.active == key {
		g.inflight++
		g.mu.Unlock()
		return g.releaser(), 0, nil
	}
	if len(g.queue) >= g.maxWaiters {
		g.mu.Unlock()
		return nil, 0, ErrQueueFull
	}
	t := &ticket{key: key, done: make(chan error, 1)}
	g.queue = append(g.queue, t)
	g.kick()
	g.mu.Unlock()

	start := time.Now()
	timer := time.NewTimer(g.maxWait)
	defer timer.Stop()
	defer func() {
		waited = time.Since(start)
		if g.obs.QueueWait != nil {
			g.obs.QueueWait(g.name, waited)
		}
	}()

	select {
	case err := <-t.done:
		if err != nil {
			return nil, 0, err
		}
		return g.releaser(), 0, nil
	case <-ctx.Done():
		return nil, 0, g.abandon(t, ctx.Err())
	case <-timer.C:
		return nil, 0, g.abandon(t, ErrTimeout)
	}
}

// abandon removes a waiting ticket. If it was granted concurrently, the
// grant is returned (released) so the in-flight count stays correct.
func (g *Group) abandon(t *ticket, cause error) error {
	g.mu.Lock()
	for i, q := range g.queue {
		if q == t {
			g.queue = append(g.queue[:i], g.queue[i+1:]...)
			g.kick()
			g.mu.Unlock()
			return cause
		}
	}
	g.mu.Unlock()
	if err := <-t.done; err == nil {
		g.releaser()()
	}
	return cause
}

func (g *Group) releaser() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.inflight--
			if g.inflight == 0 {
				g.kick()
			}
			g.mu.Unlock()
		})
	}
}

// kick advances the queue. It must be called with g.mu held.
func (g *Group) kick() {
	if g.switching || len(g.queue) == 0 {
		return
	}
	head := g.queue[0].key
	if head == g.active && g.ready {
		g.grantLocked(head, nil)
		return
	}
	if g.inflight > 0 {
		return // switch once the current requests finish
	}
	g.switching = true
	from := g.active
	go g.switchTo(from, head)
}

// grantLocked completes every waiter for key with err (nil = granted).
func (g *Group) grantLocked(key Key, err error) {
	kept := g.queue[:0]
	for _, t := range g.queue {
		if t.key != key {
			kept = append(kept, t)
			continue
		}
		if err == nil {
			g.inflight++
		}
		t.done <- err
	}
	for i := len(kept); i < len(g.queue); i++ {
		g.queue[i] = nil
	}
	g.queue = kept
}

func (g *Group) switchTo(from, to Key) {
	// Bounded by the drivers' and probe's own timeouts, and cancelled when
	// the group's context ends (shutdown).
	ctx := g.ctx
	start := time.Now()
	g.log.Info("switching model", "from", from.String(), "to", to.String())

	err := g.doSwitch(ctx, from, to)
	d := time.Since(start)

	g.mu.Lock()
	g.switching = false
	if errors.Is(err, ErrHeld) {
		// Nothing was touched, so the group's state stands; only the
		// waiters for the held backend are turned away.
		g.grantLocked(to, err)
		g.log.Warn("switch refused: backend is held", "to", to.String())
		g.kick()
		g.mu.Unlock()
		return
	}
	if err != nil {
		g.active, g.ready = to, false
		err = fmt.Errorf("%w: %s: %v", ErrSwitch, to, err)
		g.grantLocked(to, err)
		g.log.Error("model switch failed", "to", to.String(), "duration", d.String(), "error", err)
	} else {
		g.active, g.ready = to, true
		g.log.Info("model switched", "from", from.String(), "to", to.String(), "duration", d.String())
	}
	if g.obs.Switch != nil {
		g.obs.Switch(g.name, from, to, d, err)
	}
	g.kick()
	g.mu.Unlock()
}

func (g *Group) doSwitch(ctx context.Context, from, to Key) error {
	target := g.backends[to.Backend]
	// Checked here, as well as when a request is admitted, because a hold can
	// appear while requests are queued. A held backend is never started.
	if h, ok := target.(Holder); ok && h.Held() {
		return ErrHeld
	}
	if from == to {
		// The same variant that merely looked down: re-check before restarting.
		if target.Ready(ctx) {
			return nil
		}
		// Still starting or loading (e.g. right after a deploy restarted it):
		// wait for it instead of throwing the load away with a restart. If it
		// is still not ready when the wait ends, it is up but stuck, so it is
		// restarted below rather than failing the switch, which would leave
		// every later request waiting for it the same way.
		if r, ok := target.(Runner); ok {
			if running, known := r.Running(ctx); known && running {
				if err := g.awaitStarting(ctx, target, to); err == nil || ctx.Err() != nil {
					return err
				}
				// A hold can appear during a long wait: never restart into it.
				if h, ok := target.(Holder); ok && h.Held() {
					return ErrHeld
				}
			}
		}
	}
	if from.Backend != "" && from.Backend != to.Backend {
		if err := g.backends[from.Backend].Deactivate(ctx); err != nil {
			return fmt.Errorf("deactivate %s: %w", from.Backend, err)
		}
	}
	if err := target.Activate(ctx, to.Variant); err != nil {
		return fmt.Errorf("activate: %w", err)
	}
	return target.WaitReady(ctx)
}

// awaitStarting waits for a backend whose process is up to become ready,
// for at most its startup grace when it has one.
func (g *Group) awaitStarting(ctx context.Context, target Lifecycle, key Key) error {
	wctx := ctx
	attrs := []any{"active", key.String()}
	if s, ok := target.(StartupGracer); ok && s.StartupGrace() > 0 {
		var cancel context.CancelFunc
		wctx, cancel = context.WithTimeout(ctx, s.StartupGrace())
		defer cancel()
		attrs = append(attrs, "grace", s.StartupGrace().String())
	}
	g.log.Info("backend is starting; waiting for it instead of restarting", attrs...)
	err := target.WaitReady(wctx)
	if err != nil && ctx.Err() == nil {
		g.log.Warn("backend is up but did not become ready; restarting it",
			append(attrs, "error", err.Error())...)
	}
	return err
}
