# model-router

An OpenAI-compatible HTTP proxy that switches which model a local inference
server has loaded, based on the `model` field of each request.

Local inference servers often hold one model at a time: the GPU, or RAM, only
fits one. `model-router` sits in front of them and gives clients one endpoint
with many model names. When a request asks for a model that isn't loaded, the
router waits for in-flight requests to finish, switches the backend, and then
serves the request. Requests for the loaded model pass straight through.

- **Any OpenAI-compatible backend**, and Anthropic-style `/v1/messages` too.
- **Pluggable lifecycle drivers.** `systemd` (selection file + unit restart),
  `command` (your own argv), and `static` (always-on upstreams).
- **Resource groups.** Backends that share a GPU are mutually exclusive, and
  switches across them are serialised.
- **Request shaping.** Inject top-level fields per model name, for example two
  names for the same weights with different defaults. No reload is needed.
- **Streams** (SSE) token by token, with no buffering.
- **Production defaults.** No third-party dependencies, bounded bodies and
  timeouts everywhere, constant-time token checks, graceful draining,
  Prometheus metrics and structured logs that never contain prompts.

## How switching works

Every backend belongs to at most one **group**, and each group has an
**active** `(backend, variant)`.

1. A request for the active, ready variant passes straight through. That's a
   lock-light fast path with no body re-encoding unless a field is injected.
2. A request for any other variant joins the group's FIFO queue.
3. While a switch is pending, new requests for the current variant also queue,
   so a busy client can't starve the waiting one.
4. Once nothing is in flight, the router deactivates the previous backend (if
   it differs), activates the new variant, and polls readiness. Then it
   releases every waiter for that variant at once.
5. A waiter leaves the queue if its client disconnects, or after the group's
   `maxWait`. A failed switch fails only its own waiters, with a `503` and a
   `Retry-After` header.

In-flight requests are never interrupted. Model names that share a variant
(for example `local-large` and `local-large-variant-b`) never cause a switch
between each other.

**Backends that restart on their own** (a deploy, an operator, a crash):
- **A dropped connection:** if a request finds its backend refusing
  connections, nothing was sent. The router marks the backend not ready, waits
  for it like any queued request and retries once. Any other upstream failure
  returns `502` at once.
- **A variant that looks down:** when it's the one already selected, the
  router asks the driver whether the process is up or starting. The `systemd`
  driver uses `systemctl is-active`. If it is up or starting, the router waits
  for readiness instead of restarting it. Only a stopped or failed backend is
  restarted.
- **The router's own start:** if it starts while a backend is still loading,
  it adopts that backend's selected variant as not ready yet, so the first
  request waits rather than restarting it.

**Holds: lending a backend's hardware to something else.** A backend can name
a `hold.file`. While that file exists, the backend is *held*:
- Every request for it gets an immediate `503 backend_held` with
  `Retry-After: hold.retryAfter`. Nothing is queued.
- The scheduler never activates, restarts or waits for it. Requests that were
  already queued are turned away the same way, if the hold appears before
  their switch runs.
- `/v1/models` reports its models as `held`.

The router only reads the file. Whatever creates and removes it, such as a
controller handing the GPU to a game session or a maintenance window, owns the
backend's lifecycle meanwhile, including stopping it. Requests already running
when the hold appears are not cut off by the router.

Only a confirmed absence releases a hold: if the marker can't be checked (for
example, the router can't search its directory), the backend stays held. Make
the directory searchable by the router's user.

## Quick start

### Nix (flake)

```nix
{
  inputs.model-router.url = "github:masood09/model-router";

  outputs = { nixpkgs, model-router, ... }: {
    nixosConfigurations.host = nixpkgs.lib.nixosSystem {
      modules = [
        model-router.nixosModules.default
        {
          services.model-router = {
            enable = true;
            polkit.units = [ "llm-backend.service" ];  # units the router may restart/stop
            settings = builtins.fromJSON (builtins.readFile ./model-router.json);
          };
        }
      ];
    };
  };
}
```

The module creates a `model-router` system user, a hardened unit
(`ProtectSystem=strict`, no capabilities, a syscall filter), and
`/var/lib/model-router` for selection files (mode `0755` by default, so a
backend's own service user can read them). It also writes a polkit rule that
lets only that user `restart` and `stop` only the listed units. The config is
validated with `-check-config` before every start.

Run it directly with `nix run github:masood09/model-router -- -config config.json`.

### Docker

```sh
docker run --rm -p 8080:8080 \
  -v "$PWD/config.json:/etc/model-router/config.json:ro" \
  ghcr.io/masood09/model-router:latest
```

The image is distroless and runs as non-root. Inside a container, set
`"listen": "0.0.0.0:8080"`. A container can't restart host systemd units, so
containerised deployments suit `static` backends, or a `command` driver whose
program the image can actually run.

### Binary

Download a release from the Releases page, check it against `SHA256SUMS`, and
run:

```sh
model-router -check-config -config config.json   # validate
model-router -config config.json                 # serve
```

`MODEL_ROUTER_CONFIG` can be used instead of `-config`.

## Configuration

A single JSON file (see [`examples/config.example.json`](examples/config.example.json)).
Unknown fields are rejected. Durations are Go duration strings (`"30s"`, `"15m"`).
A zero value (`"0s"`, or `0` for numbers) means "use the default", not "none".

| Field | Default | Meaning |
|---|---|---|
| `listen` | `127.0.0.1:8080` | Listen address. |
| `defaultModel` | *(required)* | Catalog name used by the `default` policy, as the fallback for `loaded` while nothing is active, and for passthrough. |
| `unknownModel` | `loaded` | Names not in the catalog, including a missing or non-string `model`: `loaded` (serve with what is active, never switch, inject nothing), `default` (switch to `defaultModel`, with its injections), or `reject` (404). |
| `passthrough` | `default` | Requests to other paths: `default` (proxy to the default model's backend, no scheduling) or `off` (404). |
| `modelPaths` | chat, completions, responses, messages | POST paths whose body carries `model`. |
| `routerPrefix` | `/_router/` | Prefix of the router's own endpoints. |
| `limits.maxBodyBytes` | 64 MiB | Request body limit (`413` above it). |
| `limits.maxHeaderBytes` | 1 MiB | Request header limit. |
| `timeouts.readHeader` / `idle` | `10s` / `120s` | HTTP server timeouts. |
| `timeouts.shutdownGrace` | `60s` | How long in-flight requests may run after SIGTERM. |
| `timeouts.upstreamDial` / `upstreamResponseHeader` | `10s` / `30m` | Upstream transport timeouts. |
| `auth.clientTokensFile` / `clientTokensEnv` | — | Client tokens (file: one per line; env: comma-separated). Unset means no auth. |
| `auth.adminTokensFile` / `adminTokensEnv` | — | Tokens for `metrics` and `state`. Unset means open. |
| `log.level` | `info` | `debug`, `info`, `warn`, `error`. |
| `groups.<name>.maxWaiters` | 1024 | Queue length limit (`503` when full). |
| `groups.<name>.maxWait` | `20m` | Longest a request waits for its model. |
| `groups.<name>.stateRefresh` | `15s` | How often the active backend is re-probed. |

**Backends** (`backends.<name>`):

| Field | Meaning |
|---|---|
| `group` | Resource group (required for `systemd` and `command`; forbidden for `static`). |
| `upstream` | Base URL, `http(s)://host:port[/path]`, with no credentials or query. |
| `preserveHost` | Send the client's `Host` upstream. Default: the upstream's own host, which servers with DNS-rebinding checks require. |
| `forwardAuthorization` | Forward the client's `Authorization` and `x-api-key`. Default: stripped. |
| `upstreamTokenFile` | Bearer token to send upstream. |
| `hold.file` | Clean absolute path of a marker that holds the backend while it exists (see Holds above). Unset means no hold. |
| `hold.retryAfter` | `Retry-After` sent with each refusal while held: `60s`, between `1s` and `1h`. |
| `readiness.path` | Probe path (default `/v1/models`). |
| `readiness.expectStatus` / `bodyContains` | Ready when the status matches and the body contains the text. |
| `readiness.interval` / `timeout` / `requestTimeout` | `2s` / `10m` / `5s`. |
| `driver.kind` | `systemd`, `command` or `static`. |
| `driver.systemd.unit` | Unit to restart/stop. |
| `driver.systemd.selectionFile` | Absolute path. The variant is written here (atomically, `0644`) before each restart, for the unit to read at start. |
| `driver.systemd.defaultVariant` | Assumed before the selection file exists. |
| `driver.systemd.systemctl` / `actionTimeout` | `systemctl` from `PATH` / `5m`. |
| `driver.command.activate` / `deactivate` | argv arrays. `{variant}` is replaced inside arguments, never in the program. No shell is involved. |
| `driver.command.defaultVariant` / `actionTimeout` | — / `5m`. |

Each `actionTimeout` and `readiness.timeout` must be at most its group's
`maxWait`, so a wedged backend fails the switch instead of blocking the
group.

**Models** (`models.<name>`): `backend`, `variant` (letters, digits, `.`, `_`,
`-`; at most 64 characters), and an optional `inject` object of top-level
fields added when the client didn't set them. `model` can't be injected.

Under the `loaded` policy an unknown name gets no injections, even when it
lands on a variant that catalog entries inject for: several names can share
a variant with different injections (that is the point of them), so there is
no single right set to apply. Request bodies must be a JSON object without
duplicate top-level keys (`400 duplicate_key`), so the forwarded body can
never be read differently from the one the router routed on.

## Endpoints

| Path | Description |
|---|---|
| `POST` *model paths* | Routed by `model`, with switching as above. |
| `GET /v1/models` | The catalog, with `status.value` of `loaded`, `loading`, `unloaded` or `held` for each model, from cached state. |
| *anything else* | Passthrough to the default model's backend (or `404` with `passthrough: off`). |
| `GET /_router/healthz` | Liveness. |
| `GET /_router/readyz` | `503` while draining for shutdown. |
| `GET /_router/metrics` | Prometheus metrics (admin token if configured). |
| `GET /_router/state` | Group state as JSON (admin token if configured). |

Errors use the OpenAI shape: `{"error": {"message", "type", "code"}}`.

## Observability

Metrics:
- `model_router_requests_total{model,code}`
- `model_router_request_duration_seconds{model}`
- `model_router_queue_wait_seconds{group}`
- `model_router_switches_total{group,result}`
- `model_router_switch_duration_seconds{group}`
- `model_router_inflight_requests{group}`, `model_router_waiting_requests{group}`, `model_router_group_ready{group}`
- `model_router_active_model{group,active}`
- `model_router_held_total{backend}`: requests refused because their backend was held

Model labels are catalog names, and every unknown name is reported as
`unknown`, so cardinality stays bounded. The request counter and duration
cover model-path requests only; `/v1/models`, passthrough and the router's
own endpoints are logged but not counted.

Logs are JSON on stderr, with one line per request: id, method, path, model,
target, status, queued and total milliseconds. Bodies, tokens and
`Authorization` headers are never logged. A sane `X-Request-Id` from the
client is kept and echoed back.

## Security model

- Listens on loopback by default. Put TLS and network access control in front
  of it if you expose it.
- Optional client and admin bearer tokens (also accepted as `x-api-key`). They
  must be at least 16 characters, only their SHA-256 digests are kept, and
  they're compared in constant time.
- The router never runs a shell. Driver programs run with an argv array, a
  timeout and bounded captured output. Variants are validated against the
  config and re-validated before use, so request data can't reach argv.
- Upstream traffic ignores proxy environment variables. Client credentials are
  stripped unless forwarding is explicitly enabled.
- For the systemd driver, grant the router only `restart`/`stop` on the
  specific units, as the NixOS module's polkit rule does. Never run it as root.

## Development

```sh
nix develop                      # Go, gopls, staticcheck, govulncheck
go test -race ./...
go test -run '^$' -fuzz=FuzzParse -fuzztime=30s ./internal/config
go test -run '^$' -bench . ./internal/proxy
nix flake check                  # package, gofmt, NixOS VM test
```

The module uses only the Go standard library. See [AGENTS.md](AGENTS.md) for
conventions.

## License

MIT. See [LICENSE](LICENSE).
