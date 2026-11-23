# lb

> Part of the **Strata** platform.

L4 load balancer with a decoupled registry control plane and a TCP proxy data plane. Kubernetes endpoints, static bootstrap entries, and Guardian-deployed workloads publish intent to the registry; the proxy opens listeners on demand and balances connections across healthy backends.

## Architecture

```mermaid
flowchart LR
    K8S[Kubernetes Endpoints] --> AGENT[lb k8s-agent]
    STATIC[LB_BOOTSTRAP static routes] --> EDGE[lb-edge]
    AGENT -->|gRPC RegisterService| REG[registry]
    EDGE --> REG
    REG -->|GET /services| PROXY[proxy]
    PROXY -->|TCP round robin| B1[backend A]
    PROXY -->|TCP round robin| B2[backend B]
    PROXY -->|optional PROXY v1| B3[backend C]
```

| Component | Binary | Responsibility |
|---|---|---|
| Registry | `cmd/registry` | gRPC `DiscoveryRegistry` (`RegisterService`, `DeregisterService`, `ListServices`), HTTP `/services` snapshot, `/healthz`, and a service dashboard. `cmd/registry` requires a bearer token (health checks exempt) unless `-allow-anonymous`/`LB_ALLOW_ANONYMOUS=true`; the edge binary does **not** fail closed by default — set `-auth-token`/`LB_REGISTRY_TOKEN` explicitly. |
| Proxy | `cmd/proxy` | Polls the registry snapshot, opens listeners per external port, round-robins across healthy backends, passive circuit breaking, draining, optional PROXY protocol v1 |
| Kubernetes agent | `cmd/k8s-agent` | Watches Endpoints objects and registers/deregisters their addresses from annotations |
| Edge | `cmd/edge` | All-in-one registry plus proxy used as the container entrypoint |

### Data flow

1. A producer registers one or more backends for an external port. The registry assigns a port from the configured range when the intent does not pin one and stores the resulting route; the proxy tracks backend health and draining state.
2. The proxy syncs `GET /services` every `--sync-every` (default `2s`). It opens a listener for new external ports and closes listeners that no longer have backends.
3. Connections are balanced round-robin across active backends. Three consecutive dial failures trip a passive circuit breaker for that backend. Draining backends stop receiving new connections and are removed once their active connections reach zero.
4. Metrics are exported in Prometheus format on `:9090` for the standalone proxy and `:19090` for edge mode.

### Kubernetes intent

The agent reads the following annotations from the **Endpoints** object, falling back to the parent **Service**:

| Annotation | Meaning |
|---|---|
| `guardian.intent/expose` | `"true"` registers the endpoint |
| `guardian.intent/account` | Account/tenant, defaults to the namespace |
| `guardian.intent/service-name` | Service name, defaults to the Endpoints name |
| `guardian.intent/external-port` | Optional pinned external port |

When a Service exposes several ports, the agent selects the Service port whose
port number equals the pinned external port and registers only the Endpoints
port that has the same name, so one external port is never balanced across
unrelated receivers. A single-port Service resolves to that port. If no Service
port matches, or the selected name is absent from the Endpoints object, the
agent logs the ambiguity and falls back to every Endpoint port; an unnamed
selected Service port also falls back, without a log line.

## Quick start

```bash
go run ./cmd/registry -allow-anonymous
# in another shell
go run ./cmd/proxy -registry-url http://127.0.0.1:8081
```

The standalone registry listens on gRPC `:50051` and HTTP `:8081`; the proxy
exports metrics on `:9090`. The standalone registry requires a bearer token by
default (`-auth-token` or `LB_REGISTRY_TOKEN`); `-allow-anonymous` is the local
development opt-out. The standalone `cmd/proxy` has no token flag, so it can
only poll an anonymous registry.

`lb-edge` (the container default) has **no fail-closed auth**: `-auth-token`
defaults empty and is not validated. Set `LB_REGISTRY_TOKEN` (or `-auth-token`)
when exposing the edge registry; the same token is used for its registry API and
the local proxy's `/services` poll.

## Edge mode

The container image defaults to `lb-edge`, which runs a local registry and proxy
pair. Use `LB_BOOTSTRAP` to seed static routes for existing systems:

```bash
LB_BOOTSTRAP="9090=router-a:9090,router-b:9090;8080=router-a:8080,router-b:8080" ./edge
```

Format: `<external_port>=host:port[,host:port][;<external_port>=...]`. An
extended listener spec is also supported: `name@protocol[description]:port`,
for example `grpc@grpc:9090` or `monofs-http[MonoFS UI]:8080`.

Edge defaults differ from the standalone binaries:

| Listener | Standalone | Edge |
|---|---|---|
| Registry gRPC | `:50051` | `0.0.0.0:15051` |
| Registry HTTP | `:8081` | `127.0.0.1:18081` |
| Proxy metrics | `:9090` | `:19090` |

Most edge flags also accept environment variables: `LB_BIND_ADDRESS`,
`LB_SERVICE_BASE`, `LB_ACCOUNT`, `LB_BOOTSTRAP`, `LB_METRICS_ADDR`,
`LB_REGISTRY_HTTP_ADDR`, `LB_REGISTRY_GRPC_ADDR`, `LB_SYNC_EVERY`,
`LB_DIAL_TIMEOUT`, `LB_PROXY_PROTOCOL`.

External-port allocation ranges: standalone `cmd/registry` defaults to
`10000–60000` (`--min-external-port`/`--max-external-port`); edge defaults to
`1–65535`. A pinned port may be any free port in `1–65535`.

### TLS, mTLS, and telemetry

All binaries resolve TLS through `internal/tlsutil`, which prefers the shared
`STRATA_TLS_*` contract and falls back to legacy flags:

| Purpose | Legacy flag | Env |
| --- | --- | --- |
| Registry / edge server cert | `--tls-cert-file` | `LB_TLS_CERT_FILE` |
| Server key | `--tls-key-file` | `LB_TLS_KEY_FILE` |
| Verify the registry from edge | `--registry-ca-file` | `LB_REGISTRY_CA_FILE` |
| Skip registry verification (dev only) | `--insecure-skip-verify` | `LB_INSECURE_SKIP_VERIFY` |

When `STRATA_TLS_MODE` is set, it overrides the legacy flags, peer certificates
are always verified, and `LB_INSECURE_SKIP_VERIFY` is ignored. Edge passes the
resolved server/client config to the proxy listeners via `SetDataPlaneTLS`, so
the proxied TCP data plane can also use TLS.

OTLP export is enabled with `LB_OTEL_ENDPOINT` or
`OTEL_EXPORTER_OTLP_ENDPOINT`; `LB_OTEL_SERVICE_NAME`/`OTEL_SERVICE_NAME` and
`LB_OTEL_METRIC_INTERVAL` tune it. The container image builds with `otelc`
instrumentation.

## Build and test

```bash
go build ./...
go test ./...
```

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
