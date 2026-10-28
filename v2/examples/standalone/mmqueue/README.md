# Standalone Matchmaking Queue (`examples/standalone/mmqueue`)

This application is the game-client-facing frontend half of the [standalone Open Match 2 example](../../README.md). It receives player matchmaking requests, creates and activates tickets in `om-core`, and receives the roster assignments that the standalone [`gsdirector`](../gsdirector/README.md) publishes over Google Cloud Pub/Sub. It runs the same shared [`internal/mmqueue`](../../../internal/mmqueue) package as the [all-in-one `matchmaker` harness](../../matchmaker/README.md), packaged on its own so it can scale horizontally across multiple instances.

On startup, `mmqueue` checks the connection to `OM_CORE_ADDR` once (with a 10-second limit) and exits with an error if `om-core` cannot be reached or rejects the request; only after that check succeeds does it create its Pub/Sub subscription on `ASSIGNMENT_TOPIC_ID`.

## Run Locally

Start `om-core` (port `8080` by default) and create a Google Cloud Pub/Sub topic for assignments, then run `mmqueue` from `v2/examples/standalone/mmqueue/`:

```bash
LOGGING_FORMAT=text LOGGING_LEVEL=debug OTEL_SIDECAR=false \
  GCP_PROJECT_ID=<your-project-id> ASSIGNMENT_TOPIC_ID=<your-topic-id> go run .
```

By default, `mmqueue` listens on port `8082` and serves Prometheus metrics on port `2224` (`http://localhost:2224/metrics`). Run the standalone [`gsdirector`](../gsdirector/README.md) (port `8083`) and [`mmf`](../../mmf/README.md) (port `50090`) to form matches and publish assignments. You can submit tickets against local port `8082`:

```bash
# Create and activate one ticket
curl -i -X POST http://localhost:8082/tickets

# Generate 100 simulated tickets per cycle
curl -i -X POST http://localhost:8082/tpc/100
```

## HTTP Endpoints

`mmqueue` starts an HTTP server on `PORT` (default `8082` locally; Cloud Run sets `PORT` itself):

| Endpoint | Description |
| :--- | :--- |
| `POST /tickets` | Creates one mock ticket with `gameclient.Simple` and waits until `om-core` has created and activated it. Responds with `200 OK` when active, `502 Bad Gateway` if `om-core`'s `CreateTicket` or `ActivateTickets` failed, or `503 Service Unavailable` while shutting down. On client disconnect, no response is written and the queue skips creation or ensures the ticket ends up inactive in `om-core` (counted in `mmqueue.ticket.abandonments`). Other HTTP methods receive `405 Method Not Allowed`. |
| `/tpc/{tixPerCycle}` | Sets the simulated player arrival rate (`GET` or `POST`) for the background test generator (`0` to `10000` tickets per `TICKET_CREATION_CYCLE_DURATION_MS` window; `0` stops automatic generation; `400 Bad Request` otherwise). |
| `/debug/pprof/` | Unauthenticated Go profiling endpoints (`Index`, `Cmdline`, `Profile`, `Symbol`, `Trace`). Deploy only with internal, authenticated ingress. |

Prometheus metrics are served on `OTEL_PROM_PORT` (`http://localhost:2224/metrics` by default) when `OTEL_SIDECAR=false`, and exported over OTLP gRPC to the OpenTelemetry Collector sidecar otherwise.

## Environment Variables

All configuration is read via Viper from environment variables:

| Environment Variable | Default | Description |
| :--- | :--- | :--- |
| `PORT` | `8082` | HTTP port serving `POST /tickets`, `/tpc/{tixPerCycle}`, and `/debug/pprof/` (Cloud Run sets `PORT` itself). |
| `OM_CORE_ADDR` | `"http://localhost:8080"` | Base URL of the `om-core` HTTP/gRPC-gateway service. |
| `OM_CORE_REQUEST_TIMEOUT_MS` | `5000` | Per-attempt timeout in milliseconds for `CreateTicket`, `ActivateTickets`, `DeactivateTickets`, and the startup connection check. |
| `OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL` | `500` | Maximum number of ticket IDs batched into a single `ActivateTickets` call to `om-core`. Must be `<=` `om-core`'s `OM_MAX_STATE_UPDATES_PER_CALL` (default `500`), or `om-core` rejects every activation call. |
| `OM_CORE_MAX_CONCURRENT_ACTIVATION_CALLS` | `10` | Maximum concurrent `ActivateTickets` batch calls in flight; when reached, backpressure propagates to ticket creation. |
| `MAX_CONCURRENT_TICKET_CREATIONS` | `500` | Maximum number of concurrent `CreateTicket` calls in flight. |
| `MMQUEUE_TICKET_TTL_MS` | `600000` | How long (in milliseconds) the queue tracks a ticket before treating it as expired; keep equal to `om-core`'s `OM_CACHE_TICKET_TTL_MS`. |
| `INITIAL_TPC` | `0` | Initial simulated tickets-per-cycle rate (`0` to `10000`; `0` means off until `/tpc/{tixPerCycle}` is called). |
| `TICKET_CREATION_CYCLE_DURATION_MS` | `1000` | Duration of each simulated ticket creation cycle in milliseconds. |
| `TICKET_CREATION_CYCLES` | `2147483647` (`math.MaxInt32`) | Maximum number of ticket generation cycles to execute before the generator exits. |
| `TICKET_ARRIVAL_SEED` | `0` | PRNG seed for the Poisson arrival schedule (`0` picks a random seed each run; non-zero reproduces the schedule). |
| `ASSIGNMENT_DISTRIBUTION_PATH` | `"pubsub"` | Assignment transport (`"pubsub"` subscribes to Google Cloud Pub/Sub; `"channel"` discards assignments in a standalone queue). |
| `GCP_PROJECT_ID` | `"my-project-id"` | Google Cloud project ID containing the Pub/Sub assignment topic. |
| `ASSIGNMENT_TOPIC_ID` | `"replace-me"` | Existing Google Cloud Pub/Sub topic ID published to by [`gsdirector`](../gsdirector/README.md); each `mmqueue` instance creates its own subscription on startup and deletes it on shutdown. |
| `LOGGING_FORMAT` | `"json"` | Log output format (`"json"` for structured Cloud Logging, `"text"` for local console output). |
| `LOGGING_LEVEL` | `"info"` | Log verbosity level (`"trace"`, `"debug"`, `"info"`, `"warn"`, `"error"`, `"fatal"`). |
| `LOG_CALLER` | `"false"` | When `"true"`, includes caller file and line number in log output. |
| `OTEL_SIDECAR` | `"true"` | When `"true"`, exports metrics over OTLP gRPC to a local OpenTelemetry Collector sidecar; when `"false"`, starts a local Prometheus HTTP exporter. |
| `OTEL_PROM_PORT` | `2224` | Local Prometheus scrape port used when `OTEL_SIDECAR=false`. |

## Deploy to Cloud Run

First, build the container image from `v2/` following [the matchmaker README's Build section](../../matchmaker/README.md#build) with `_GOOGLE_BUILDABLE=./examples/standalone/mmqueue`:

```bash
cd v2
gcloud builds submit --async \
  --substitutions=COMMIT_SHA=latest,_ARTIFACT_REGISTRY_URI=${REGION}-docker.pkg.dev/${PROJECT_ID}/open-match,_JOB_NAME=om-standalone-mmqueue,_GOOGLE_BUILDABLE=./examples/standalone/mmqueue
```

Then deploy the service from `v2/examples/standalone/mmqueue/deploy/` using [`deploy/cloudbuild.yaml`](deploy/cloudbuild.yaml), supplying substitutions for the `om-core` URL, project ID, and Pub/Sub topic:

```bash
cd v2/examples/standalone/mmqueue/deploy
gcloud builds submit --async --no-source \
  --substitutions=_REGION=${REGION},_ARTIFACT_REGISTRY_REPO=open-match,_MMQUEUE_IMAGE_NAME=om-standalone-mmqueue:latest,_SERVICE_ACCOUNT=${MMQUEUE_SA_EMAIL},_OM_CORE_ADDR=${OM_URL},_GCP_PROJECT_ID=${PROJECT_ID},_ASSIGNMENT_TOPIC_ID=${TOPIC_ID}
```

Key Cloud Run deployment requirements:

* **Exact `https://` `OM_CORE_ADDR` and IAM**: `_OM_CORE_ADDR` must be the exact `https://` service URL of `om-core`. When `K_REVISION` is set, `omclient` authenticates every `om-core` call with a Google-signed ID token for that exact URL audience, and `mmqueue`'s service account needs `roles/run.invoker` on `om-core`.
* **Pub/Sub subscription permissions**: Each `mmqueue` instance creates and deletes its own subscription on `ASSIGNMENT_TOPIC_ID`, so the service account needs permission to create and delete subscriptions on the topic (for example, `roles/pubsub.editor`).
* **Pub/Sub fan-out across instances**: Because every `mmqueue` instance has its own subscription, every instance receives every published assignment and ignores those for tickets it is not tracking (counted in `mmqueue.ticket.assignments.untracked`). See [Fan-out: every queue instance receives every assignment](../../../internal/assignmentdistributor/README.md#fan-out-every-queue-instance-receives-every-assignment) for details and sharding guidance at scale.
* **Gen2 background processing**: [`deploy/cloudbuild.yaml`](deploy/cloudbuild.yaml) uses internal authenticated ingress (`--ingress=internal --no-allow-unauthenticated`), disables CPU throttling (`--no-cpu-throttling`, needed for the background activation, assignment-receiving, expiry-sweep, and test-ticket loops), and deploys an `opentelemetry-collector` sidecar container.
* **Bounded shutdown**: On `SIGTERM`, shutdown is bounded to fit Cloud Run's roughly 10-second grace period: stop accepting HTTP requests, stop background work, flush OpenTelemetry metrics, and then delete this instance's Pub/Sub subscription.
