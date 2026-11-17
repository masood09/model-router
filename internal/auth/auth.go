// Package auth checks bearer tokens on incoming requests.
package auth

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Tokens is a set of accepted tokens. Only SHA-256 digests are kept in
// memory, and checks compare digests in constant time against every entry,
// so neither timing nor a memory dump reveals a token.
type Tokens struct {
	digests [][sha256.Size]byte
}

// Load reads tokens from file (one per line; blank lines and lines starting
// with "#" are ignored) and from the comma-separated environment variable
// env. Either may be empty. It returns nil when no tokens are configured,
// which disables the check.
func Load(file, env string) (*Tokens, error) {
	var raw []string
	if file != "" {
		data, err := os.ReadFile(file) // #nosec G304 -- operator-configured path
		if err != nil {
			return nil, fmt.Errorf("read token file: %w", err)
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			raw = append(raw, line)
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("read token file: %w", err)
		}
	}
	if env != "" {
		for _, tok := range strings.Split(os.Getenv(env), ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				raw = append(raw, tok)
			}
		}
	}
	if len(raw) == 0 {
		if file != "" || env != "" {
			return nil, fmt.Errorf("token source configured but no tokens found")
		}
		return nil, nil
	}
	t := &Tokens{}
	for _, tok := range raw {
		if len(tok) < 16 {
			return nil, fmt.Errorf("tokens must be at least 16 characters")
		}
		t.digests = append(t.digests, sha256.Sum256([]byte(tok)))
	}
	return t, nil
}

// Enabled reports whether any token is configured.
func (t *Tokens) Enabled() bool { return t != nil && len(t.digests) > 0 }

// Check reports whether r carries an accepted token, either as
// "Authorization: Bearer <token>" or as an "x-api-key" header. A nil or
// empty set accepts everything.
func (t *Tokens) Check(r *http.Request) bool {
	if !t.Enabled() {
		return true
	}
	tok := Presented(r)
	if tok == "" {
		return false
	}
	d := sha256.Sum256([]byte(tok))
	ok := 0
	for i := range t.digests {
		ok |= subtle.ConstantTimeCompare(d[:], t.digests[i][:])
	}
	return ok == 1
}

// Presented extracts the token a request presents, or "".
func Presented(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, rest, found := strings.Cut(h, " ")
		if found && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(rest)
		}
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}
