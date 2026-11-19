package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validJSON = `{
  "listen": "127.0.0.1:9000",
  "defaultModel": "m-large",
  "backends": {
    "llm": {
      "group": "gpu0",
      "upstream": "http://127.0.0.1:8001",
      "driver": {
        "kind": "systemd",
        "systemd": {
          "unit": "llm-backend.service",
          "selectionFile": "/var/lib/model-router/llm-variant",
          "defaultVariant": "large",
          "actionTimeout": "2m"
        }
      },
      "readiness": {"path": "/v1/models", "bodyContains": "loaded", "timeout": "5m"}
    },
    "remote": {
      "upstream": "https://api.example.org",
      "driver": {"kind": "static"}
    }
  },
  "models": {
    "m-large": {"backend": "llm", "variant": "large", "inject": {"flag": false}},
    "m-large-alt": {"backend": "llm", "variant": "large", "inject": {"flag": true}},
    "m-small": {"backend": "llm", "variant": "small"},
    "remote-model": {"backend": "remote"}
  }
}`

func TestParseValidAppliesDefaults(t *testing.T) {
	c, err := Parse([]byte(validJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.RouterPrefix != DefaultRouterPrefix {
		t.Errorf("RouterPrefix = %q", c.RouterPrefix)
	}
	if c.UnknownModel != UnknownLoaded || c.Passthrough != PassthroughDefault {
		t.Errorf("policies = %q/%q", c.UnknownModel, c.Passthrough)
	}
	if len(c.ModelPaths) != len(DefaultModelPaths) {
		t.Errorf("ModelPaths = %v", c.ModelPaths)
	}
	g := c.Groups["gpu0"]
	if g.MaxWaiters != DefaultMaxWaiters || g.MaxWait.D() != DefaultMaxWait || g.StateRefresh.D() != DefaultStateRefresh {
		t.Errorf("group defaults = %+v", g)
	}
	b := c.Backends["llm"]
	if b.Driver.Systemd.Systemctl != "systemctl" {
		t.Errorf("systemctl default = %q", b.Driver.Systemd.Systemctl)
	}
	if b.Driver.Systemd.ActionTimeout.D() != 2*time.Minute {
		t.Errorf("actionTimeout = %s", b.Driver.Systemd.ActionTimeout.D())
	}
	if b.Readiness.ExpectStatus != 200 || b.Readiness.Interval.D() != DefaultReadyInterval {
		t.Errorf("readiness defaults = %+v", b.Readiness)
	}
	if b.Readiness.StartupGrace != b.Readiness.Timeout {
		t.Errorf("startupGrace = %s, want the readiness timeout %s", b.Readiness.StartupGrace.D(), b.Readiness.Timeout.D())
	}
	if c.Limits.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Errorf("MaxBodyBytes = %d", c.Limits.MaxBodyBytes)
	}
	if b.Hold.File != "" || b.Hold.RetryAfter.D() != DefaultHoldRetryAfter {
		t.Errorf("hold defaults = %+v", b.Hold)
	}
}

func TestParseAcceptsHold(t *testing.T) {
	s := strings.Replace(validJSON, `"readiness": {"path"`, `"hold": {"file": "/run/console/held", "retryAfter": "90s"}, "readiness": {"path"`, 1)
	c, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if h := c.Backends["llm"].Hold; h.File != "/run/console/held" || h.RetryAfter.D() != 90*time.Second {
		t.Fatalf("hold = %+v", h)
	}
}

func TestParseAcceptsStartupGrace(t *testing.T) {
	s := strings.Replace(validJSON, `"timeout": "5m"`, `"timeout": "5m", "startupGrace": "90s"`, 1)
	c, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if g := c.Backends["llm"].Readiness.StartupGrace.D(); g != 90*time.Second {
		t.Fatalf("startupGrace = %s", g)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{"unknown field", func(s string) string { return strings.Replace(s, `"listen"`, `"lisen"`, 1) }, "unknown field"},
		{"trailing data", func(s string) string { return s + `{}` }, "trailing data"},
		{"missing default model", func(s string) string {
			return strings.Replace(s, `"defaultModel": "m-large"`, `"defaultModel": "nope"`, 1)
		}, "defaultModel"},
		{"bad unit", func(s string) string { return strings.Replace(s, "llm-backend.service", "llm backend; rm", 1) }, "valid unit"},
		{"relative selection file", func(s string) string {
			return strings.Replace(s, "/var/lib/model-router/llm-variant", "llm-variant", 1)
		}, "absolute path"},
		{"bad variant", func(s string) string { return strings.Replace(s, `"variant": "small"`, `"variant": "../etc"`, 1) }, "variant"},
		{"inject model key", func(s string) string { return strings.Replace(s, `{"flag": true}`, `{"model": "x"}`, 1) }, "not allowed"},
		{"static in group", func(s string) string {
			return strings.Replace(s, `"upstream": "https://api.example.org",`, `"group": "gpu0", "upstream": "https://api.example.org",`, 1)
		}, "static backends must not belong"},
		{"upstream with credentials", func(s string) string {
			return strings.Replace(s, "https://api.example.org", "https://user:pw@api.example.org", 1)
		}, "credentials"},
		{"upstream not http", func(s string) string { return strings.Replace(s, "http://127.0.0.1:8001", "ftp://127.0.0.1", 1) }, "absolute http(s)"},
		{"readiness exceeds maxWait", func(s string) string { return strings.Replace(s, `"timeout": "5m"`, `"timeout": "2h"`, 1) }, "exceeds groups.gpu0.maxWait"},
		{"startupGrace exceeds readiness timeout", func(s string) string {
			return strings.Replace(s, `"timeout": "5m"`, `"timeout": "5m", "startupGrace": "6m"`, 1)
		}, "readiness.startupGrace"},
		{"action exceeds maxWait", func(s string) string { return strings.Replace(s, `"actionTimeout": "2m"`, `"actionTimeout": "3h"`, 1) }, "actionTimeout"},
		{"negative duration", func(s string) string { return strings.Replace(s, `"actionTimeout": "2m"`, `"actionTimeout": "-1m"`, 1) }, "negative"},
		{"bad unknown policy", func(s string) string {
			return strings.Replace(s, `"listen"`, `"unknownModel": "maybe", "listen"`, 1)
		}, "unknownModel"},
		{"bad driver kind", func(s string) string { return strings.Replace(s, `"kind": "systemd"`, `"kind": "docker"`, 1) }, "driver.kind"},
		{"router prefix root", func(s string) string {
			return strings.Replace(s, `"listen"`, `"routerPrefix": "/", "listen"`, 1)
		}, "routerPrefix"},
		{"relative hold file", func(s string) string {
			return strings.Replace(s, `"readiness": {"path"`, `"hold": {"file": "run/held"}, "readiness": {"path"`, 1)
		}, "hold.file"},
		{"unclean hold file", func(s string) string {
			return strings.Replace(s, `"readiness": {"path"`, `"hold": {"file": "/run/x/../held"}, "readiness": {"path"`, 1)
		}, "hold.file"},
		{"hold retryAfter too short", func(s string) string {
			return strings.Replace(s, `"readiness": {"path"`, `"hold": {"file": "/run/held", "retryAfter": "500ms"}, "readiness": {"path"`, 1)
		}, "hold.retryAfter"},
		{"hold retryAfter too long", func(s string) string {
			return strings.Replace(s, `"readiness": {"path"`, `"hold": {"file": "/run/held", "retryAfter": "2h"}, "readiness": {"path"`, 1)
		}, "hold.retryAfter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.mutate(validJSON)))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestCommandDriverValidation(t *testing.T) {
	base := `{
  "defaultModel": "a",
  "backends": {"b": {"group": "g", "upstream": "http://127.0.0.1:1",
    "driver": {"kind": "command", "command": %s}}},
  "models": {"a": {"backend": "b", "variant": "v1"}}
}`
	ok := `{"activate": ["/usr/bin/docker", "restart", "llm-{variant}"], "deactivate": ["/usr/bin/docker", "stop", "llm"]}`
	if _, err := Parse([]byte(strings.Replace(base, "%s", ok, 1))); err != nil {
		t.Fatalf("valid command driver rejected: %v", err)
	}
	bad := map[string]string{
		"empty activate":       `{"activate": []}`,
		"placeholder program":  `{"activate": ["{variant}", "x"]}`,
		"empty deactivate arg": `{"activate": ["/bin/true"], "deactivate": [""]}`,
	}
	for name, d := range bad {
		if _, err := Parse([]byte(strings.Replace(base, "%s", d, 1))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(validJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("Load of a missing file succeeded")
	}
}

func TestDurationRoundTrip(t *testing.T) {
	var d Duration
	if err := d.UnmarshalJSON([]byte(`"90s"`)); err != nil || d.D() != 90*time.Second {
		t.Fatalf("got %v, %v", d.D(), err)
	}
	b, err := d.MarshalJSON()
	if err != nil || string(b) != `"1m30s"` {
		t.Fatalf("marshal = %s, %v", b, err)
	}
	if err := d.UnmarshalJSON([]byte(`90`)); err == nil {
		t.Fatal("numeric duration accepted")
	}
	if err := d.UnmarshalJSON([]byte(`"soon"`)); err == nil {
		t.Fatal("invalid duration accepted")
	}
}

func TestValidVariant(t *testing.T) {
	for _, v := range []string{"q3_k_s", "q4.k-m", "A1"} {
		if !ValidVariant(v) {
			t.Errorf("%q rejected", v)
		}
	}
	for _, v := range []string{"", "-x", "a b", "a/b", "a;b", strings.Repeat("a", 65)} {
		if ValidVariant(v) {
			t.Errorf("%q accepted", v)
		}
	}
}
