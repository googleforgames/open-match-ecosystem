# Standalone Game Server Director (`examples/standalone/gsdirector`)

This application is the backend half of the [standalone Open Match 2 example](../../README.md). Each matchmaking cycle, it queries the mock game server manager (`MockAgonesIntegration`) to discover what matches are needed, sends profiles to `om-core` via `InvokeMatchmakingFunctions`, evaluates returned match proposals, allocates game servers for accepted matches, and publishes roster assignments to Google Cloud Pub/Sub for the standalone [`mmqueue`](../mmqueue/README.md) instances to deliver to players. It runs the same shared [`internal/gsdirector`](../../../internal/gsdirector) package as the [all-in-one `matchmaker` harness](../../matchmaker/README.md), separated into its own binary for production-style deployments. Run a single instance (or one instance per fleet partition), because it keeps match and assignment collision state in memory.

On startup, `gsdirector` checks the connection to `OM_CORE_ADDR` once (with a 10-second limit) and exits with an error if `om-core` cannot be reached or rejects the request (for example, a wrong URL, DNS failure, timeout, or a `401`/`403` from Cloud Run). The process also exits cleanly on its own when the director stops (after `NUM_MM_CYCLES` cycles or `NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT` consecutive empty cycles).

## Run Locally

Start `om-core` (port `8080` by default) and the standalone [`mmf`](../../mmf/README.md) server (port `50090` by default), and create a Google Cloud Pub/Sub topic for assignments. Then run `gsdirector` from `v2/examples/standalone/gsdirector/`:

```bash
LOGGING_FORMAT=text LOGGING_LEVEL=debug OTEL_SIDECAR=false \
  GCP_PROJECT_ID=<your-project-id> ASSIGNMENT_TOPIC_ID=<your-topic-id> go run .
```

By default, `gsdirector` listens on port `8083` (`GET http://localhost:8083/` shows the latest cycle's status) and serves Prometheus metrics on port `2225` (`http://localhost:2225/metrics`). Run the standalone [`mmqueue`](../mmqueue/README.md) (port `8082`) to create and activate tickets.

Note that `SOLODUEL_ADDR` and `SOLODUEL_PORT` (and `DEBUGMMF_ADDR` / `DEBUGMMF_PORT` if enabled) are **not** called directly by the director—they are placed into `MmfRequest` protobufs and passed to `om-core`, so they must be reachable from `om-core`.

## HTTP Endpoints

`gsdirector` starts an HTTP server on `PORT` (default `8083` locally; Cloud Run sets `PORT` itself):

| Endpoint | Description |
| :--- | :--- |
| `GET /` | Returns a plain-text status summary of the latest completed matchmaking cycle: accepted/rejected matches and tickets, MMF feedback responses and failed `InvokeMatchmakingFunctions` calls, cycle duration, and adaptive contention backoff. |
| `/debug/pprof/` | Unauthenticated Go profiling endpoints (`Index`, `Cmdline`, `Profile`, `Symbol`, `Trace`). Deploy only with internal, authenticated ingress. |

Prometheus metrics are not served on `PORT`; when `OTEL_SIDECAR=false`, they are served on `OTEL_PROM_PORT` (`http://localhost:2225/metrics` by default), and when `OTEL_SIDECAR=true` they are exported via OTLP gRPC to the OpenTelemetry Collector sidecar.

## Environment Variables

All configuration is read via Viper from environment variables:

| Environment Variable | Default | Description |
| :--- | :--- | :--- |
| `PORT` | `8083` | HTTP port serving `GET /` and `/debug/pprof/` (Cloud Run sets `PORT` itself). |
| `OM_CORE_ADDR` | `"http://localhost:8080"` | Base URL of the `om-core` HTTP/gRPC-gateway service. |
| `OM_CORE_REQUEST_TIMEOUT_MS` | `5000` | Per-attempt timeout in milliseconds for unary `om-core` calls (`ActivateTickets` for ticket re-activation and the startup connection check). |
| `OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL` | `500` | Maximum number of ticket IDs batched into a single `ActivateTickets` call to `om-core`. Must be `<=` `om-core`'s `OM_MAX_STATE_UPDATES_PER_CALL` (default `500`), or `om-core` rejects every activation call. |
| `OM_CORE_MAX_CONCURRENT_ACTIVATION_CALLS` | `10` | Maximum concurrent `ActivateTickets` batch calls in flight for ticket re-activation. |
| `SOLODUEL_ADDR` | `"http://localhost"` | Address of the `fifo` (SoloDuel) MMF gRPC server as `om-core` should call it. On Cloud Run, use the MMF's `https://` service URL because `om-core` only authenticates to MMFs with an ID token from its own identity (which needs `roles/run.invoker` on the MMF) when the scheme is `https`. |
| `SOLODUEL_PORT` | `50090` | Port of the `fifo` (SoloDuel) MMF gRPC server (`443` on Cloud Run; matches the standalone [`mmf`](../../mmf/README.md)'s default local `PORT`). |
| `DEBUGMMF_ADDR` | `"none"` | Opt-in address of the `debug` MMF gRPC server (`"none"` or empty string disables it). |
| `DEBUGMMF_PORT` | `50081` | Port of the `debug` MMF gRPC server (`443` on Cloud Run). |
| `NUM_MM_CYCLES` | `2147483647` (`math.MaxInt32`) | Maximum number of matchmaking cycles `gsdirector` will run before stopping and exiting the process. |
| `MM_CYCLE_MIN_DURATION_MS` | `5000` | Base minimum duration of a matchmaking cycle in milliseconds, scaled up by adaptive backoff on cross-cycle collisions or deactivation timeouts. |
| `MMF_INVOCATION_TIMEOUT_SECS` | `600` | Director-side deadline in seconds for each cycle's `InvokeMatchmakingFunctions` calls (default matches `om-core`'s `OM_MMF_TIMEOUT_SECS`; `0` means no director-side deadline). |
| `NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT` | `2147483647` (`math.MaxInt32`) | Stops the director (and exits the process) if this many consecutive cycles produce zero match proposals. |
| `REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT` | `false` | When `true`, rejects any proposed match with `deactivation_timeout=true` in `Match.Extensions` and re-activates its unclaimed tickets. |
| `WAIT_FOR_BACKFILL_MMFS` | `false` | When `true`, invokes `MmfRequest`s whose profile name starts with `"backfill."` synchronously before launching standard concurrent MMF requests. |
| `RANDOM_MATCH_REJECT_PERCENT` | `1` | Percentage of proposed matches randomly rejected by the director to exercise the ticket re-activation path (`0` disables it). |
| `ASSIGNMENT_TTL_MS` | `1200000` | Time in milliseconds (20 minutes) to retain assigned ticket IDs in the director's `assignments` collision-tracking map. Keep at least as long as `om-core`'s `OM_CACHE_TICKET_TTL_MS` (default 10 minutes). |
| `ASSIGNMENT_DISTRIBUTION_PATH` | `""` | Assignment transport: any value other than `"channel"` (including the empty default) publishes to Google Cloud Pub/Sub. `"channel"` only works in-process with `mmqueue`, so in the standalone director it discards assignments. |
| `GCP_PROJECT_ID` | `"my-project-id"` | Google Cloud project ID containing the Pub/Sub assignment topic. |
| `ASSIGNMENT_TOPIC_ID` | `"replace-me"` | Existing Google Cloud Pub/Sub topic ID to publish assignments to (the same topic that [`mmqueue`](../mmqueue/README.md) instances subscribe to). |
| `LOGGING_FORMAT` | `"json"` | Log output format (`"json"` for structured Cloud Logging, `"text"` for local console output). |
| `LOGGING_LEVEL` | `"info"` | Log verbosity level (`"trace"`, `"debug"`, `"info"`, `"warn"`, `"error"`, `"fatal"`). |
| `LOG_CALLER` | `"false"` | When `"true"`, includes caller file and line number in log output. |
| `OTEL_SIDECAR` | `"true"` | When `"true"`, exports metrics over OTLP gRPC to a local OpenTelemetry Collector sidecar; when `"false"`, starts a local Prometheus HTTP exporter. |
| `OTEL_PROM_PORT` | `2225` | Local Prometheus scrape port used when `OTEL_SIDECAR=false`. |

## Deploy to Cloud Run

First, build the container image from `v2/` following [the matchmaker README's Build section](../../matchmaker/README.md#build) with `_GOOGLE_BUILDABLE=./examples/standalone/gsdirector`:

```bash
cd v2
gcloud builds submit --async \
  --substitutions=COMMIT_SHA=latest,_ARTIFACT_REGISTRY_URI=${REGION}-docker.pkg.dev/${PROJECT_ID}/open-match,_JOB_NAME=om-standalone-gsdirector,_GOOGLE_BUILDABLE=./examples/standalone/gsdirector
```

Then deploy the service from `v2/examples/standalone/gsdirector/deploy/` using [`deploy/cloudbuild.yaml`](deploy/cloudbuild.yaml), supplying substitutions for the `om-core` URL, `fifo` MMF URL, project ID, and Pub/Sub topic:

```bash
cd v2/examples/standalone/gsdirector/deploy
gcloud builds submit --async --no-source \
  --substitutions=_REGION=${REGION},_ARTIFACT_REGISTRY_REPO=open-match,_GSDIRECTOR_IMAGE_NAME=om-standalone-gsdirector:latest,_SERVICE_ACCOUNT=${GSDIRECTOR_SA_EMAIL},_OM_CORE_ADDR=${OM_URL},_SOLODUEL_ADDR=${FIFO_URL},_GCP_PROJECT_ID=${PROJECT_ID},_ASSIGNMENT_TOPIC_ID=${TOPIC_ID}
```

Key Cloud Run deployment requirements:

* **Exact `https://` `OM_CORE_ADDR` and IAM**: `_OM_CORE_ADDR` must be the exact `https://` service URL of `om-core`. When `K_REVISION` is set (which Cloud Run sets automatically), `omclient` authenticates every `om-core` call with a Google-signed ID token for that exact URL audience, and `gsdirector`'s service account needs `roles/run.invoker` on `om-core`.
* **HTTPS MMF URLs and `om-core` IAM**: `_SOLODUEL_ADDR` must include `https://` with `_SOLODUEL_PORT=443` (and likewise for `_DEBUGMMF_ADDR` / `_DEBUGMMF_PORT` if enabled), and `om-core`'s service account needs `roles/run.invoker` on the MMF Cloud Run service(s).
* **Pub/Sub permissions**: `gsdirector`'s service account needs `roles/pubsub.publisher` on the existing `ASSIGNMENT_TOPIC_ID` topic.
* **Singleton Gen2 configuration**: [`deploy/cloudbuild.yaml`](deploy/cloudbuild.yaml) pins one instance (`--min-instances=1` and `--max-instances=1`), disables CPU throttling (`--no-cpu-throttling`) so the background matchmaking loop runs continuously, restricts ingress to internal authenticated callers (`--ingress=internal --no-allow-unauthenticated`), and runs an `opentelemetry-collector` sidecar container.
* **Bounded shutdown**: On `SIGTERM`, shutdown is bounded to fit Cloud Run's roughly 10-second grace period: stop accepting HTTP requests, stop the director loop, flush OpenTelemetry metrics, and then flush and stop the Pub/Sub assignment publisher.
