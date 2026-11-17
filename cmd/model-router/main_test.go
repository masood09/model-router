package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimal = `{
  "defaultModel": "m",
  "backends": {"b": {"upstream": "http://127.0.0.1:9", "driver": {"kind": "static"}}},
  "models": {"m": {"backend": "b"}}
}`

func TestRunFlags(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(good, []byte(minimal), 0o600)
	_ = os.WriteFile(bad, []byte(`{"defaultModel": "x"}`), 0o600)

	cases := []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"-version"}, 0, "model-router dev"},
		{[]string{}, 2, "required"},
		{[]string{"-config", good, "-check-config"}, 0, "config OK: 1 models"},
		{[]string{"-config", bad, "-check-config"}, 1, "backend is required"},
		{[]string{"-config", filepath.Join(dir, "missing.json")}, 1, "read config"},
		{[]string{"-bogus"}, 2, "flag provided but not defined"},
	}
	for _, c := range cases {
		t.Setenv("MODEL_ROUTER_CONFIG", "")
		var stdout, stderr bytes.Buffer
		code := run(c.args, &stdout, &stderr)
		if code != c.code {
			t.Errorf("%v: code %d, want %d (stderr %q)", c.args, code, c.code, stderr.String())
		}
		if !strings.Contains(stdout.String()+stderr.String(), c.out) {
			t.Errorf("%v: output %q missing %q", c.args, stdout.String()+stderr.String(), c.out)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(p, []byte(minimal), 0o600)
	t.Setenv("MODEL_ROUTER_CONFIG", p)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-check-config"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
}

func TestNewLoggerLevels(t *testing.T) {
	var b bytes.Buffer
	l := newLogger(&b, "warn")
	l.Info("hidden")
	l.Warn("shown")
	if strings.Contains(b.String(), "hidden") || !strings.Contains(b.String(), "shown") {
		t.Fatalf("log output %q", b.String())
	}
}
