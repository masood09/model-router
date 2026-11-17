package auth

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const (
	tokA = "token-aaaaaaaaaaaaaaaa"
	tokB = "token-bbbbbbbbbbbbbbbb"
)

func TestLoadNoneDisables(t *testing.T) {
	tk, err := Load("", "")
	if err != nil || tk.Enabled() {
		t.Fatalf("Load = %v, %v", tk, err)
	}
	if !tk.Check(httptest.NewRequest("GET", "/", nil)) {
		t.Fatal("disabled auth rejected a request")
	}
}

func TestLoadFileAndEnv(t *testing.T) {
	f := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(f, []byte("# comment\n\n  "+tokA+"  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MR_TEST_TOKENS", " "+tokB+" , ")
	tk, err := Load(f, "MR_TEST_TOKENS")
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{tokA, tokB} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		if !tk.Check(r) {
			t.Errorf("%s rejected", tok)
		}
	}
}

func TestCheckForms(t *testing.T) {
	t.Setenv("MR_TEST_TOKENS", tokA)
	tk, err := Load("", "MR_TEST_TOKENS")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		header, value string
		want          bool
	}{
		{"Authorization", "Bearer " + tokA, true},
		{"Authorization", "bearer " + tokA, true},
		{"X-Api-Key", tokA, true},
		{"Authorization", "Basic " + tokA, false},
		{"Authorization", "Bearer " + tokB, false},
		{"Authorization", "Bearer", false},
		{"X-Api-Key", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if c.header != "" {
			r.Header.Set(c.header, c.value)
		}
		if got := tk.Check(r); got != c.want {
			t.Errorf("%s=%q: Check = %v, want %v", c.header, c.value, got, c.want)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing"), ""); err == nil {
		t.Error("missing file accepted")
	}
	t.Setenv("MR_EMPTY", " , ")
	if _, err := Load("", "MR_EMPTY"); err == nil {
		t.Error("configured but empty source accepted")
	}
	t.Setenv("MR_SHORT", "short")
	if _, err := Load("", "MR_SHORT"); err == nil {
		t.Error("short token accepted")
	}
}
