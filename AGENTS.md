# AGENTS.md

Guidance for coding agents (and humans) working in this repository.

## What this is

`model-router` is an OpenAI-compatible HTTP proxy that sits in front of one or
more local inference servers and switches which model is loaded based on the
`model` field of each request. Backends that share a resource (for example one
GPU) are put in the same *group*; only one model per group is active at a time,
and the router serialises switches so in-flight requests are never interrupted.

## Layout

```
cmd/model-router/      entry point: flags, signal handling
internal/config/       JSON config schema, defaults, validation
internal/backend/      backend lifecycle drivers (systemd, command, static) and readiness probe
internal/scheduler/    per-group switching queue
internal/proxy/        HTTP routing, field injection, streaming relay, /v1/models
internal/auth/         bearer-token checks
internal/metrics/      Prometheus text-format metrics
internal/server/       wires config into a running server; integration tests live here
nix/module.nix         generic NixOS module
examples/              example configuration
```

## Commands

```sh
go build ./...                       # build
go test -race ./...                  # full test suite
go test -run Fuzz -fuzz=FuzzParse -fuzztime=30s ./internal/config   # fuzz one target
go test -bench . -run '^$' ./internal/proxy                          # benchmarks
go vet ./...
gofmt -l .                           # must print nothing
nix build                            # package
nix flake check                      # tests + NixOS module evaluation
docker build -t model-router .       # container image
```

## Rules

- **Standard library only.** Do not add third-party Go modules. This keeps the
  supply chain minimal and the Nix build (`vendorHash = null`) simple. If a
  dependency is ever unavoidable, discuss it first.
- **Nothing environment-specific in the repository.** This is a public project:
  no real hostnames, domains, IP addresses (other than loopback and documentation
  ranges such as `192.0.2.0/24`), usernames, home paths, or personal details in
  code, docs, examples, tests or commit messages. Use `example.org`, `127.0.0.1`
  and placeholder unit names such as `llm-backend.service`.
- **Configurable, not hard-coded.** Ports, paths, unit names, model names,
  timeouts and limits come from the config file. Defaults must be safe
  (loopback listen address, bounded body sizes, finite timeouts).
- **Security first.**
  - Never invoke a shell. External commands run via `exec.CommandContext` with
    an argv array; user-controlled values (variants) are validated against the
    config and substituted as whole arguments only.
  - Never log request or response bodies, tokens or `Authorization` headers.
  - Compare secrets with `crypto/subtle`.
  - Every network and process action has a timeout.
- **Performance.** The non-switching request path must not take a global lock
  or re-encode the body unless a field is injected. Responses are streamed
  (`FlushInterval: -1`); never buffer a full upstream response.
- **Tests accompany changes.** New behaviour needs unit tests; anything touching
  request flow needs an integration test in `internal/server`. Run with `-race`.

## Commit style

Conventional Commits: `<type>(<scope>): <summary>`, imperative, at most 72
characters, with a body that explains why. One logical change per commit.
Types: `feat`, `fix`, `refactor`, `perf`, `test`, `docs`, `build`, `ci`, `chore`.
