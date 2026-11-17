package config

import "testing"

// FuzzParse checks that arbitrary input never panics the parser and that
// anything it accepts passes validation again.
func FuzzParse(f *testing.F) {
	f.Add([]byte(validJSON))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"backends": {"x": {"driver": {"kind": "static"}}}}`))
	f.Add([]byte(`[`))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Parse(data)
		if err != nil {
			return
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("accepted config fails re-validation: %v", err)
		}
	})
}
