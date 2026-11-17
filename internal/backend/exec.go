package backend

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// maxCommandOutput bounds how much of a command's output is kept for errors.
const maxCommandOutput = 4096

// limitedBuffer keeps at most max bytes and drops the rest.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}

// run executes argv with a timeout. It never uses a shell; arguments are
// passed to the program exactly as given. Combined output is captured
// (bounded) and included in the error on failure.
func run(ctx context.Context, timeout time.Duration, argv []string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// #nosec G204 -- argv comes from validated configuration, never a shell.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out := &limitedBuffer{max: maxCommandOutput}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = nil
	// If the process leaves children holding the output pipes, do not hang.
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%s: timed out after %s", argv[0], timeout)
	}
	msg := strings.TrimSpace(out.buf.String())
	if msg == "" {
		return fmt.Errorf("%s: %w", argv[0], err)
	}
	return fmt.Errorf("%s: %w: %s", argv[0], err, msg)
}
