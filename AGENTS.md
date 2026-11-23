# Agent notes

## Project shape

Single Go module (`go 1.26.10`, no workspace). Four main binaries under `cmd/`:

- `cmd/registry` — gRPC control plane + HTTP `/services` state endpoint.
- `cmd/proxy` — TCP data plane; syncs routes from registry HTTP and listens dynamically.
- `cmd/k8s-agent` — watches Kubernetes Endpoints and registers/deregisters backends via gRPC.
- `cmd/edge` — local all-in-one: registry + proxy in one process. This is the Docker default entrypoint.

## Everyday commands

```bash
# Compile every package (the multi-package pattern writes no binaries)
go build ./...

# Run tests
go test ./...

# Local dev quickstart
go run ./cmd/registry -allow-anonymous   # gRPC :50051, HTTP :8081
go run ./cmd/proxy -registry-url http://127.0.0.1:8081  # metrics :9090

# All-in-one
go run ./cmd/edge
```

GitHub Actions CI (`.github/workflows/tests.yml`) runs `go test ./...`. No Makefile, linter, or formatter config exists — `go build` / `go test` are the primary verification steps.

## Generated code

`pkg/pb/discovery.pb.go` and `pkg/pb/discovery_grpc.pb.go` are generated from `pkg/pb/discovery.proto`. Regenerate with:

```bash
protoc --go_out=. --go_opt=paths=source_relative \
       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
       pkg/pb/discovery.proto
```

## Registry API

- gRPC: `DiscoveryRegistry` service (`RegisterService`, `DeregisterService`, `ListServices`). Reflection is enabled.
- HTTP: `GET /services` returns JSON snapshot; `/healthz` returns `ok`; `/` renders a service dashboard UI.

## Proxy behavior

- Polls registry `/services` every `-sync-every` (default 2s) and opens/closes listeners on demand.
- Round-robin across active backends; 3 consecutive dial failures trips a passive circuit breaker.
- Draining backends receive no new connections and are removed once active connections hit zero.
- Optional PROXY protocol v1 via `-proxy-protocol` / `LB_PROXY_PROTOCOL=true`.

## Kubernetes agent

Watches Endpoints in one namespace (default all). Intent is read from annotations on the **Endpoints** object, falling back to the parent **Service**:

- `guardian.intent/expose: "true"` — required to register.
- `guardian.intent/account` — defaults to namespace.
- `guardian.intent/service-name` — defaults to Endpoints name.
- `guardian.intent/external-port` — optional pinned external port. On a
  multi-port Service, the Service port whose number equals that external port
  is the only one registered for it; an unmatched or unnamed port falls back to
  every Endpoints port (the unmatched case logs it).

Uses in-cluster config if available, otherwise `$KUBECONFIG`/`~/.kube/config`.

## Edge mode / Docker

The container image defaults to `lb-edge`:

```bash
# build image
docker build -t lb .

# run with static bootstrap routes
LB_BOOTSTRAP="9090=router-a:9090,router-b:9090;8080=router-a:8080,router-b:8080" ./edge
```

Bootstrap format: `<external_port>=host:port[,host:port][;<external_port>=...]`. An extended listener spec is also supported: `name@protocol[description]:port`, e.g. `grpc@grpc:9090` or `monofs-http[MonoFS UI]:8080`.

Edge default ports differ from standalone registry/proxy:

- registry gRPC: `0.0.0.0:15051`
- registry HTTP: `127.0.0.1:18081`
- metrics: `:19090`

## Environment / flags

Most edge flags also accept env vars: `LB_BIND_ADDRESS`, `LB_SERVICE_BASE`, `LB_ACCOUNT`, `LB_BOOTSTRAP`, `LB_METRICS_ADDR`, `LB_REGISTRY_HTTP_ADDR`, `LB_REGISTRY_GRPC_ADDR`, `LB_SYNC_EVERY`, `LB_DIAL_TIMEOUT`, `LB_PROXY_PROTOCOL`.

## Gotchas

- `.gitignore` ignores the four root binary names exactly (`/edge`, `/proxy`, `/registry`, `/k8s-agent`). Pre-built binaries in the repo are artifacts and should not be committed.
- The proxy fetches state over HTTP, but the k8s-agent writes state over gRPC; both must reach the registry.
- The registry requires a bearer token by default (`-auth-token` / `LB_REGISTRY_TOKEN`) and refuses to start without one unless `-allow-anonymous` / `LB_ALLOW_ANONYMOUS=true` is set. This applies to standalone `cmd/registry`. The `lb-edge` image does **not** fail closed: `-auth-token` defaults empty and is not validated. The standalone `cmd/proxy` and `cmd/k8s-agent` send no token, so they only work against an anonymous registry.
- All binaries resolve TLS through `internal/tlsutil` (`STRATA_TLS_*`, with `LB_TLS_CERT_FILE`/`LB_TLS_KEY_FILE`/`LB_REGISTRY_CA_FILE` as fallbacks). Edge applies the resolved server/client config to proxy listeners via `SetDataPlaneTLS`. `STRATA_TLS_MODE` overrides the legacy flags and makes `LB_INSECURE_SKIP_VERIFY` a no-op.
- OTLP export uses `LB_OTEL_ENDPOINT` or `OTEL_EXPORTER_OTLP_ENDPOINT`; the image builds with `otelc`.
- Standalone `cmd/proxy` defaults its registry URL to `http://127.0.0.1:8081`, matching standalone `cmd/registry` defaults.
