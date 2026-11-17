package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fake is a controllable Lifecycle.
type fake struct {
	mu          sync.Mutex
	current     string
	up          bool
	activations []string
	deacts      int
	activateErr error
	gate        chan struct{} // if non-nil, Activate blocks until closed or a value is sent
}

func (f *fake) Activate(ctx context.Context, v string) error {
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activations = append(f.activations, v)
	if f.activateErr != nil {
		f.up = false
		return f.activateErr
	}
	f.current, f.up = v, true
	return nil
}

func (f *fake) Deactivate(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deacts++
	f.up = false
	return nil
}

func (f *fake) WaitReady(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.up {
		return errors.New("not ready")
	}
	return nil
}

func (f *fake) Ready(context.Context) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.up }

func (f *fake) Current() (string, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.current, nil }

func (f *fake) acts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.activations...)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newGroup(backends map[string]Lifecycle, maxWait time.Duration) *Group {
	return New(Options{Name: "gpu0", MaxWaiters: 16, MaxWait: maxWait, Refresh: time.Hour, Backends: backends, Logger: quiet()})
}

func TestDetectAndFastPath(t *testing.T) {
	a := &fake{current: "large", up: true}
	g := newGroup(map[string]Lifecycle{"a": a}, time.Second)
	g.Detect(context.Background())
	if s := g.State(); s.Active != (Key{"a", "large"}) || !s.Ready {
		t.Fatalf("state = %+v", s)
	}
	rel, waited, err := g.Acquire(context.Background(), Key{"a", "large"})
	if err != nil || waited != 0 {
		t.Fatalf("Acquire = %v, waited %s", err, waited)
	}
	if g.State().InFlight != 1 {
		t.Fatal("in-flight not counted")
	}
	rel()
	rel() // second call is a no-op
	if g.State().InFlight != 0 {
		t.Fatal("in-flight not released")
	}
	if len(a.acts()) != 0 {
		t.Fatalf("fast path activated: %v", a.acts())
	}
}

func TestSwitchWaitsForInFlight(t *testing.T) {
	a := &fake{current: "large", up: true}
	g := newGroup(map[string]Lifecycle{"a": a}, 5*time.Second)
	g.Detect(context.Background())

	relLarge, _, err := g.Acquire(context.Background(), Key{"a", "large"})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		rel, _, err := g.Acquire(context.Background(), Key{"a", "small"})
		if err == nil {
			rel()
		}
		got <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if len(a.acts()) != 0 {
		t.Fatal("switched while a request was in flight")
	}
	// A new request for the current variant must queue behind the switch.
	sameDone := make(chan error, 1)
	go func() {
		rel, _, err := g.Acquire(context.Background(), Key{"a", "large"})
		if err == nil {
			rel()
		}
		sameDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-sameDone:
		t.Fatal("request for the current variant jumped the pending switch")
	default:
	}
	relLarge()
	if err := <-got; err != nil {
		t.Fatalf("small: %v", err)
	}
	if err := <-sameDone; err != nil {
		t.Fatalf("large after switch back: %v", err)
	}
	if acts := a.acts(); len(acts) != 2 || acts[0] != "small" || acts[1] != "large" {
		t.Fatalf("activations = %v, want [small large]", acts)
	}
}

func TestWaitersForSameVariantShareOneSwitch(t *testing.T) {
	a := &fake{current: "large", up: true, gate: make(chan struct{})}
	g := newGroup(map[string]Lifecycle{"a": a}, 5*time.Second)
	g.Detect(context.Background())

	var wg sync.WaitGroup
	var ok atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, _, err := g.Acquire(context.Background(), Key{"a", "small"})
			if err == nil {
				ok.Add(1)
				time.Sleep(10 * time.Millisecond)
				rel()
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(a.gate)
	wg.Wait()
	if ok.Load() != 8 {
		t.Fatalf("%d of 8 granted", ok.Load())
	}
	if acts := a.acts(); len(acts) != 1 {
		t.Fatalf("activations = %v, want one", acts)
	}
}

func TestSwitchAcrossBackendsDeactivatesPrevious(t *testing.T) {
	a := &fake{current: "x", up: true}
	b := &fake{}
	g := newGroup(map[string]Lifecycle{"a": a, "b": b}, 5*time.Second)
	g.Detect(context.Background())
	rel, _, err := g.Acquire(context.Background(), Key{"b", "y"})
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if a.deacts != 1 || len(b.acts()) != 1 {
		t.Fatalf("a deactivations=%d, b activations=%v", a.deacts, b.acts())
	}
}

func TestSwitchFailureFailsWaitersAndRecovers(t *testing.T) {
	a := &fake{current: "large", up: true, activateErr: errors.New("boom")}
	var switches atomic.Int32
	g := New(Options{Name: "gpu0", MaxWaiters: 4, MaxWait: time.Second, Refresh: time.Hour,
		Backends: map[string]Lifecycle{"a": a}, Logger: quiet(),
		Observer: Observer{Switch: func(string, Key, Key, time.Duration, error) { switches.Add(1) }}})
	g.Detect(context.Background())
	_, _, err := g.Acquire(context.Background(), Key{"a", "small"})
	if !errors.Is(err, ErrSwitch) {
		t.Fatalf("err = %v, want ErrSwitch", err)
	}
	if s := g.State(); s.Ready || s.Switching || s.Waiting != 0 {
		t.Fatalf("state after failure = %+v", s)
	}
	// The next request retries the switch.
	a.mu.Lock()
	a.activateErr = nil
	a.mu.Unlock()
	rel, _, err := g.Acquire(context.Background(), Key{"a", "small"})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	rel()
	if switches.Load() != 2 {
		t.Fatalf("observer saw %d switches", switches.Load())
	}
}

func TestQueueTimeout(t *testing.T) {
	a := &fake{current: "large", up: true, gate: make(chan struct{})}
	defer close(a.gate)
	g := newGroup(map[string]Lifecycle{"a": a}, 60*time.Millisecond)
	g.Detect(context.Background())
	_, _, err := g.Acquire(context.Background(), Key{"a", "small"})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if g.State().Waiting != 0 {
		t.Fatal("timed-out waiter left in the queue")
	}
}

func TestCancelledWaiterLeavesQueue(t *testing.T) {
	a := &fake{current: "large", up: true}
	g := newGroup(map[string]Lifecycle{"a": a}, 5*time.Second)
	g.Detect(context.Background())
	relLarge, _, _ := g.Acquire(context.Background(), Key{"a", "large"})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, _, err := g.Acquire(ctx, Key{"a", "small"})
		errc <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	relLarge()
	time.Sleep(30 * time.Millisecond)
	if acts := a.acts(); len(acts) != 0 {
		t.Fatalf("switched for an abandoned request: %v", acts)
	}
	if s := g.State(); s.Waiting != 0 || s.InFlight != 0 {
		t.Fatalf("state = %+v", s)
	}
}

func TestQueueFull(t *testing.T) {
	a := &fake{current: "large", up: true, gate: make(chan struct{})}
	defer close(a.gate)
	g := New(Options{Name: "g", MaxWaiters: 1, MaxWait: time.Second, Refresh: time.Hour,
		Backends: map[string]Lifecycle{"a": a}, Logger: quiet()})
	g.Detect(context.Background())
	go func() { _, _, _ = g.Acquire(context.Background(), Key{"a", "small"}) }()
	time.Sleep(30 * time.Millisecond)
	if _, _, err := g.Acquire(context.Background(), Key{"a", "medium"}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
}

func TestUnknownBackend(t *testing.T) {
	g := newGroup(map[string]Lifecycle{"a": &fake{}}, time.Second)
	if _, _, err := g.Acquire(context.Background(), Key{"zzz", "v"}); err == nil {
		t.Fatal("unknown backend accepted")
	}
}

func TestMarkUnreadyRechecksWithoutRestart(t *testing.T) {
	a := &fake{current: "large", up: true}
	g := newGroup(map[string]Lifecycle{"a": a}, time.Second)
	g.Detect(context.Background())
	g.MarkUnready(Key{"a", "large"})
	if g.State().Ready {
		t.Fatal("MarkUnready ignored")
	}
	rel, _, err := g.Acquire(context.Background(), Key{"a", "large"})
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if acts := a.acts(); len(acts) != 0 {
		t.Fatalf("restarted a backend that was actually up: %v", acts)
	}
}

func TestDownBackendIsRestarted(t *testing.T) {
	a := &fake{current: "large", up: false}
	g := newGroup(map[string]Lifecycle{"a": a}, time.Second)
	g.Detect(context.Background())
	if g.State().Active.Backend != "" {
		t.Fatal("down backend detected as active")
	}
	rel, _, err := g.Acquire(context.Background(), Key{"a", "large"})
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if acts := a.acts(); len(acts) != 1 || acts[0] != "large" {
		t.Fatalf("activations = %v", acts)
	}
}

func TestRefreshTracksReadiness(t *testing.T) {
	a := &fake{current: "large", up: true}
	g := newGroup(map[string]Lifecycle{"a": a}, time.Second)
	g.Detect(context.Background())
	a.mu.Lock()
	a.up = false
	a.mu.Unlock()
	g.refreshOnce(context.Background())
	if g.State().Ready {
		t.Fatal("refresh did not notice the backend went down")
	}
	a.mu.Lock()
	a.up = true
	a.mu.Unlock()
	g.refreshOnce(context.Background())
	if !g.State().Ready {
		t.Fatal("refresh did not notice the backend came back")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	a := &fake{current: "large", up: true}
	g := New(Options{Name: "g", MaxWaiters: 1, MaxWait: time.Second, Refresh: 5 * time.Millisecond,
		Backends: map[string]Lifecycle{"a": a}, Logger: quiet()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
	if !g.State().Ready {
		t.Fatal("Run never detected the active backend")
	}
}

func TestConcurrentMixedLoad(t *testing.T) {
	a := &fake{current: "v0", up: true}
	g := New(Options{Name: "g", MaxWaiters: 1000, MaxWait: 10 * time.Second, Refresh: time.Hour,
		Backends: map[string]Lifecycle{"a": a}, Logger: quiet()})
	g.Detect(context.Background())
	var wg sync.WaitGroup
	var maxInflightWrong atomic.Bool
	variants := []string{"v0", "v1", "v2"}
	for i := range 300 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := Key{"a", variants[i%3]}
			rel, _, err := g.Acquire(context.Background(), k)
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			if cur, _ := a.Current(); cur != k.Variant {
				maxInflightWrong.Store(true)
			}
			time.Sleep(time.Millisecond)
			rel()
		}(i)
	}
	wg.Wait()
	if maxInflightWrong.Load() {
		t.Fatal("a request ran while a different variant was active")
	}
	if s := g.State(); s.InFlight != 0 || s.Waiting != 0 || s.Switching {
		t.Fatalf("final state = %+v", s)
	}
}

func TestKeyString(t *testing.T) {
	if (Key{}).String() != "none" || (Key{"a", "b"}).String() != "a/b" {
		t.Fatal("Key.String")
	}
}
