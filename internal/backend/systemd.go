package backend

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/masood09/model-router/internal/config"
)

// Systemd switches variants by writing a selection file that the unit reads
// at start, then restarting the unit.
type Systemd struct {
	unit          string
	selectionFile string
	defaultVar    string
	systemctl     string
	timeout       time.Duration

	mu sync.Mutex // serialises selection writes and unit actions
}

// NewSystemd builds a systemd driver from validated configuration.
func NewSystemd(c config.SystemdDriver) *Systemd {
	return &Systemd{
		unit:          c.Unit,
		selectionFile: c.SelectionFile,
		defaultVar:    c.DefaultVariant,
		systemctl:     c.Systemctl,
		timeout:       c.ActionTimeout.D(),
	}
}

// Activate records variant in the selection file and restarts the unit.
// systemctl restart blocks until the unit's start job finishes, so the old
// process is gone before readiness is probed.
func (s *Systemd) Activate(ctx context.Context, variant string) error {
	if !config.ValidVariant(variant) {
		return fmt.Errorf("invalid variant %q", variant)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.selectionFile != "" {
		if err := writeFileAtomic(s.selectionFile, []byte(variant+"\n"), 0o644); err != nil {
			return fmt.Errorf("write selection: %w", err)
		}
	}
	return run(ctx, s.timeout, []string{s.systemctl, "--no-ask-password", "restart", s.unit})
}

// Deactivate stops the unit.
func (s *Systemd) Deactivate(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return run(ctx, s.timeout, []string{s.systemctl, "--no-ask-password", "stop", s.unit})
}

// Running reports whether the unit is up or on its way up (systemd's
// "active", "activating", "reloading" or "refreshing"). ok is false when the
// state could not be read. It only queries state, so it needs no polkit rule.
func (s *Systemd) Running(ctx context.Context) (running, ok bool) {
	state, err := output(ctx, s.timeout, []string{s.systemctl, "--no-ask-password", "is-active", s.unit})
	if err != nil {
		return false, false
	}
	switch state {
	case "active", "activating", "reloading", "refreshing":
		return true, true
	case "inactive", "failed", "deactivating", "maintenance":
		return false, true
	default:
		return false, false
	}
}

// Current returns the variant in the selection file, or the default variant
// if the file does not exist yet.
func (s *Systemd) Current() (string, error) {
	if s.selectionFile == "" {
		return s.defaultVar, nil
	}
	data, err := os.ReadFile(s.selectionFile)
	if errors.Is(err, fs.ErrNotExist) {
		return s.defaultVar, nil
	}
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(data))
	if !config.ValidVariant(v) {
		return "", fmt.Errorf("selection file holds an invalid variant")
	}
	return v, nil
}

// writeFileAtomic writes data to a temporary file in the same directory,
// syncs it and renames it over path, so readers never see a partial file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".selection-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
