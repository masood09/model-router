// Command model-router is an OpenAI-compatible proxy that switches which
// model a local inference server has loaded based on each request.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/masood09/model-router/internal/config"
	"github.com/masood09/model-router/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("model-router", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", os.Getenv("MODEL_ROUTER_CONFIG"), "path to the JSON config file (env MODEL_ROUTER_CONFIG)")
	checkOnly := fs.Bool("check-config", false, "validate the config file and exit")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, "model-router", version)
		return 0
	}
	if *configPath == "" {
		fmt.Fprintln(stderr, "model-router: -config (or MODEL_ROUTER_CONFIG) is required")
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "model-router:", err)
		return 1
	}
	if *checkOnly {
		fmt.Fprintf(stdout, "config OK: %d models, %d backends, %d groups\n", len(cfg.Models), len(cfg.Backends), len(cfg.Groups))
		return 0
	}

	log := newLogger(stderr, cfg.Log.Level)
	log.Info("starting", "version", version)
	srv, err := server.New(cfg, log)
	if err != nil {
		log.Error("startup failed", "error", err.Error())
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.ListenAndServe(ctx); err != nil {
		log.Error("server failed", "error", err.Error())
		return 1
	}
	log.Info("stopped")
	return 0
}

func newLogger(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}
