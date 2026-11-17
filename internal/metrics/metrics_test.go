package metrics

import (
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExposition(t *testing.T) {
	r := New()
	c := r.NewCounter("mr_requests_total", "Requests handled.", "model", "code")
	s := r.NewSummary("mr_switch_seconds", "Switch duration.", "group")
	r.GaugeFunc("mr_inflight", "In-flight requests.", []string{"group"}, func() []Sample {
		return []Sample{{[]string{"gpu1"}, 1}, {[]string{"gpu0"}, 3}}
	})
	c.Inc("a", "200")
	c.Inc("a", "200")
	c.Inc(`we"ird\`+"\n", "503")
	s.Observe(1500*time.Millisecond, "gpu0")
	s.Observe(500*time.Millisecond, "gpu0")

	var b strings.Builder
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# TYPE mr_requests_total counter\n",
		`mr_requests_total{model="a",code="200"} 2` + "\n",
		`mr_requests_total{model="we\"ird\\\n",code="503"} 1` + "\n",
		"# TYPE mr_switch_seconds summary\n",
		`mr_switch_seconds_sum{group="gpu0"} 2` + "\n",
		`mr_switch_seconds_count{group="gpu0"} 2` + "\n",
		"# TYPE mr_inflight gauge\n",
		`mr_inflight{group="gpu0"} 3` + "\n" + `mr_inflight{group="gpu1"} 1` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestLabelCountMismatchPanics(t *testing.T) {
	r := New()
	c := r.NewCounter("x_total", "x", "a")
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	c.Inc("1", "2")
}

func TestDuplicatePanics(t *testing.T) {
	r := New()
	r.NewCounter("x_total", "x")
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	r.NewCounter("x_total", "x")
}

func TestConcurrentUse(t *testing.T) {
	r := New()
	c := r.NewCounter("c_total", "c", "k")
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				c.Inc("v")
			}
			var b strings.Builder
			_ = r.Write(&b)
		}()
	}
	wg.Wait()
	var b strings.Builder
	_ = r.Write(&b)
	if !strings.Contains(b.String(), `c_total{k="v"} 5000`) {
		t.Fatalf("lost increments:\n%s", b.String())
	}
}

func TestNum(t *testing.T) {
	if num(math.Inf(1)) != "+Inf" || num(math.Inf(-1)) != "-Inf" || num(math.NaN()) != "NaN" || num(0.25) != "0.25" {
		t.Fatal("num formatting")
	}
}
