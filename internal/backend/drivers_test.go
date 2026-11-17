package backend

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/masood09/model-router/internal/config"
)

// fakeProgram writes an executable shell script that appends its arguments
// to a log file, so tests can see exactly what argv the driver passed.
// The script is the program under test's stand-in; the driver itself never
// uses a shell.
func fakeProgram(t *testing.T, body string) (prog, logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	prog = filepath.Join(dir, "fake")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + logFile + "'; done\necho --- >> '" + logFile + "'\n" + body
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return prog, logFile
}

func calls(t *testing.T, logFile string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	var cur []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "---" {
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return out
}

func TestSystemdActivateWritesSelectionThenRestarts(t *testing.T) {
	prog, log := fakeProgram(t, "exit 0\n")
	sel := filepath.Join(t.TempDir(), "variant")
	d := NewSystemd(config.SystemdDriver{
		Unit: "llm-backend.service", SelectionFile: sel, DefaultVariant: "large",
		Systemctl: prog, ActionTimeout: config.Duration(5 * time.Second),
	})

	if v, err := d.Current(); err != nil || v != "large" {
		t.Fatalf("Current before any write = %q, %v", v, err)
	}
	if err := d.Activate(context.Background(), "small"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	data, err := os.ReadFile(sel)
	if err != nil || string(data) != "small\n" {
		t.Fatalf("selection = %q, %v", data, err)
	}
	if fi, _ := os.Stat(sel); fi.Mode().Perm() != 0o644 {
		t.Fatalf("selection mode = %v", fi.Mode().Perm())
	}
	if v, _ := d.Current(); v != "small" {
		t.Fatalf("Current = %q", v)
	}
	if err := d.Deactivate(context.Background()); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	got := calls(t, log)
	want := [][]string{
		{"--no-ask-password", "restart", "llm-backend.service"},
		{"--no-ask-password", "stop", "llm-backend.service"},
	}
	if len(got) != len(want) {
		t.Fatalf("calls = %v", got)
	}
	for i := range want {
		if strings.Join(got[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestSystemdRejectsInvalidVariant(t *testing.T) {
	prog, log := fakeProgram(t, "exit 0\n")
	d := NewSystemd(config.SystemdDriver{Unit: "x.service", Systemctl: prog, ActionTimeout: config.Duration(time.Second)})
	for _, v := range []string{"", "a b", "$(id)", "../x", "-x"} {
		if err := d.Activate(context.Background(), v); err == nil {
			t.Errorf("variant %q accepted", v)
		}
	}
	if c := calls(t, log); len(c) != 0 {
		t.Fatalf("systemctl was called: %v", c)
	}
}

func TestSystemdFailureIncludesOutput(t *testing.T) {
	prog, _ := fakeProgram(t, "echo 'Unit not found.' >&2; exit 5\n")
	d := NewSystemd(config.SystemdDriver{Unit: "x.service", Systemctl: prog, ActionTimeout: config.Duration(5 * time.Second)})
	err := d.Activate(context.Background(), "v")
	if err == nil || !strings.Contains(err.Error(), "Unit not found") {
		t.Fatalf("err = %v", err)
	}
}

func TestSystemdTimeout(t *testing.T) {
	prog, _ := fakeProgram(t, "sleep 10\n")
	d := NewSystemd(config.SystemdDriver{Unit: "x.service", Systemctl: prog, ActionTimeout: config.Duration(100 * time.Millisecond)})
	start := time.Now()
	err := d.Deactivate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 7*time.Second {
		t.Fatalf("timeout not enforced: %s", time.Since(start))
	}
}

func TestSystemdRunning(t *testing.T) {
	cases := []struct {
		print       string
		exit        int
		running, ok bool
	}{
		{"active", 0, true, true},
		{"activating", 3, true, true},
		{"reloading", 0, true, true},
		{"inactive", 3, false, true},
		{"failed", 3, false, true},
		{"deactivating", 3, false, true},
		{"", 4, false, false},
		{"something-new", 3, false, false},
	}
	for _, c := range cases {
		prog, log := fakeProgram(t, "echo '"+c.print+"'; exit "+strconv.Itoa(c.exit)+"\n")
		d := NewSystemd(config.SystemdDriver{Unit: "llm-backend.service", Systemctl: prog, ActionTimeout: config.Duration(5 * time.Second)})
		running, ok := d.Running(context.Background())
		if running != c.running || ok != c.ok {
			t.Errorf("%q (exit %d): Running = %v, %v; want %v, %v", c.print, c.exit, running, ok, c.running, c.ok)
		}
		if got := calls(t, log); len(got) != 1 || strings.Join(got[0], " ") != "--no-ask-password is-active llm-backend.service" {
			t.Errorf("argv = %v", got)
		}
	}
}

func TestOutputCapturesStderr(t *testing.T) {
	// stdout is the answer; stderr noise must not reach it (or the process's
	// own stderr, which is the router's JSON log).
	prog, _ := fakeProgram(t, "echo 'Warning: unit file changed on disk' >&2; echo active\n")
	if got, err := output(context.Background(), 5*time.Second, []string{prog}); err != nil || got != "active" {
		t.Fatalf("output = %q, %v", got, err)
	}
	// With no stdout, stderr becomes the error's context.
	prog, _ = fakeProgram(t, "echo 'Failed to connect to bus' >&2; exit 1\n")
	_, err := output(context.Background(), 5*time.Second, []string{prog})
	if err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatalf("err = %v", err)
	}
}

func TestSystemdCurrentRejectsGarbage(t *testing.T) {
	sel := filepath.Join(t.TempDir(), "variant")
	if err := os.WriteFile(sel, []byte("rm -rf /\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := NewSystemd(config.SystemdDriver{Unit: "x.service", SelectionFile: sel, Systemctl: "systemctl"})
	if _, err := d.Current(); err == nil {
		t.Fatal("garbage selection accepted")
	}
}

func TestCommandSubstitutesVariantAsWholeArgument(t *testing.T) {
	prog, log := fakeProgram(t, "exit 0\n")
	d := NewCommand(config.CommandDriver{
		Activate:      []string{prog, "restart", "llm-{variant}", "--tag={variant}"},
		Deactivate:    []string{prog, "stop", "llm"},
		ActionTimeout: config.Duration(5 * time.Second),
	})
	if v, _ := d.Current(); v != "" {
		t.Fatalf("initial Current = %q", v)
	}
	if err := d.Activate(context.Background(), "q4_k_m"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if v, _ := d.Current(); v != "q4_k_m" {
		t.Fatalf("Current = %q", v)
	}
	if err := d.Deactivate(context.Background()); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if v, _ := d.Current(); v != "" {
		t.Fatalf("Current after deactivate = %q", v)
	}
	got := calls(t, log)
	if len(got) != 2 || strings.Join(got[0], "|") != "restart|llm-q4_k_m|--tag=q4_k_m" || strings.Join(got[1], "|") != "stop|llm" {
		t.Fatalf("calls = %q", got)
	}
}

func TestCommandFailureClearsCurrent(t *testing.T) {
	prog, _ := fakeProgram(t, "exit 1\n")
	d := NewCommand(config.CommandDriver{Activate: []string{prog}, DefaultVariant: "a", ActionTimeout: config.Duration(5 * time.Second)})
	if err := d.Activate(context.Background(), "b"); err == nil {
		t.Fatal("failing command succeeded")
	}
	if v, _ := d.Current(); v != "" {
		t.Fatalf("Current after failure = %q", v)
	}
}

func TestCommandWithoutDeactivate(t *testing.T) {
	d := NewCommand(config.CommandDriver{Activate: []string{"/bin/true"}, DefaultVariant: "a", ActionTimeout: config.Duration(time.Second)})
	if err := d.Deactivate(context.Background()); err != nil {
		t.Fatalf("Deactivate without a command: %v", err)
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := &limitedBuffer{max: 4}
	n, _ := b.Write([]byte("abcdef"))
	if n != 6 || b.buf.String() != "abcd" {
		t.Fatalf("n=%d buf=%q", n, b.buf.String())
	}
	_, _ = b.Write([]byte("gh"))
	if b.buf.String() != "abcd" {
		t.Fatalf("buf grew past max: %q", b.buf.String())
	}
}

func TestWriteFileAtomicLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sel")
	for _, v := range []string{"one", "two"} {
		if err := writeFileAtomic(p, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1", len(entries))
	}
	if err := writeFileAtomic(filepath.Join(dir, "missing", "sel"), []byte("x"), 0o644); err == nil {
		t.Fatal("write into a missing directory succeeded")
	}
}

func TestNewBuildsEachDriver(t *testing.T) {
	r := readiness(time.Second)
	sd, err := New("s", config.Backend{Group: "g", Upstream: "http://127.0.0.1:1", Readiness: r,
		Driver: config.Driver{Kind: config.DriverSystemd, Systemd: &config.SystemdDriver{Unit: "x.service", Systemctl: "systemctl"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sd.Driver.(*Systemd); !ok {
		t.Fatalf("driver = %T", sd.Driver)
	}
	cd, err := New("c", config.Backend{Group: "g", Upstream: "http://127.0.0.1:1", Readiness: r,
		Driver: config.Driver{Kind: config.DriverCommand, Command: &config.CommandDriver{Activate: []string{"/bin/true"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cd.Driver.(*Command); !ok {
		t.Fatalf("driver = %T", cd.Driver)
	}
	if _, err := New("x", config.Backend{Upstream: "http://127.0.0.1:1", Readiness: r, Driver: config.Driver{Kind: "nope"}}, nil); err == nil {
		t.Fatal("unknown driver accepted")
	}
}
