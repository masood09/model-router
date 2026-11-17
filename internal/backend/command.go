package backend

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/masood09/model-router/internal/config"
)

// Command runs configured programs to activate and deactivate variants.
// The variant is substituted into arguments as plain text; no shell is
// involved, so it can never be interpreted as syntax.
type Command struct {
	activate   []string
	deactivate []string
	timeout    time.Duration

	mu      sync.Mutex
	current string
}

// NewCommand builds a command driver from validated configuration.
func NewCommand(c config.CommandDriver) *Command {
	return &Command{
		activate:   append([]string(nil), c.Activate...),
		deactivate: append([]string(nil), c.Deactivate...),
		timeout:    c.ActionTimeout.D(),
		current:    c.DefaultVariant,
	}
}

// Activate runs the activate command for variant.
func (c *Command) Activate(ctx context.Context, variant string) error {
	if !config.ValidVariant(variant) {
		return fmt.Errorf("invalid variant %q", variant)
	}
	argv := make([]string, len(c.activate))
	argv[0] = c.activate[0]
	for i, a := range c.activate[1:] {
		argv[i+1] = strings.ReplaceAll(a, config.VariantPlaceholder, variant)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := run(ctx, c.timeout, argv); err != nil {
		c.current = ""
		return err
	}
	c.current = variant
	return nil
}

// Deactivate runs the deactivate command, if one is configured.
func (c *Command) Deactivate(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = ""
	if len(c.deactivate) == 0 {
		return nil
	}
	return run(ctx, c.timeout, c.deactivate)
}

// Current returns the variant most recently activated successfully.
func (c *Command) Current() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current, nil
}
