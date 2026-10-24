# Example Matchmaker (All-in-One Development Harness)

This example matchmaker is a Go application designed to be built using [CNCF Buildpacks](https://www.cncf.io/projects/buildpacks/), and our primary build process specifically uses [Google Cloud's buildpacks](https://cloud.google.com/docs/buildpacks/overview). It requires Open Match 2 (`om-core`), and will exit on startup if it cannot contact the configured `OM_CORE_ADDR` instance.

Note that this example matchmaker demonstrates **all of the pieces of code you need to write and maintain on your own** to build a matchmaker using Open Match 2 (`mmqueue`, `gsdirector`, a mock Agones game server manager integration, and in-process `fifo` and `debug` MMFs). It is **explicitly not** an example of how that code should be packaged for production. In production, you will want horizontally scalable services for your matchmaking queue and matchmaking functions, whereas your game server director should run as a singleton (or partitioned by fleet/region). The [`v2/examples/standalone`](../standalone) directory contains decoupled copies of `mmqueue` and `gsdirector` as a model for production packaging. However, spinning up 5+ microservices locally is high friction when iterating on matching logic and ticket attributes—this all-in-one harness runs the exact same shared packages inside a single process (analogous to `minimatch` in Open Match 1.x).

## Build

Because `go.mod` and the shared `internal/` packages reside in the `v2/` directory, container image builds must be invoked from `v2/`.

You can build the image remotely on Google Cloud Build using [`v2/cloudbuild.yaml`](../../cloudbuild.yaml), which invokes `pack build` with Google Cloud Buildpacks and pushes the resulting image to Artifact Registry:

```bash
cd v2
go get && go mod tidy && gcloud builds submit --async \
  --substitutions=COMMIT_SHA=latest,_ARTIFACT_REGISTRY_URI=${REGION}-docker.pkg.dev/${PROJECT_ID}/open-match,_JOB_NAME=matchmaker-example,_GOOGLE_BUILDABLE=./examples/matchmaker
```

### `v2/cloudbuild.yaml` Substitutions

| Substitution | Default | Description |
| :--- | :--- | :--- |
| `_ARTIFACT_REGISTRY_URI` | `us-docker.pkg.dev/open-match-public-images/open-match2` | Target Artifact Registry repository URI prefix. |
| `_JOB_NAME` | `matchmaker-example` | Image name within the Artifact Registry repository. |
| `_GOOGLE_BUILDABLE` | `./examples/matchmaker/` | Relative path from `v2/` to the Go `main` package to build with Cloud Buildpacks. |
| `COMMIT_SHA` | *(provided by Cloud Build or `--substitutions`)* | Image tag pushed to `${_ARTIFACT_REGISTRY_URI}/${_JOB_NAME}:${COMMIT_SHA}`. |

Alternatively, you can build directly with `gcloud builds submit --pack` without a config file:

```bash
cd v2
gcloud builds submit --pack \
  image=${REGION}-docker.pkg.dev/${PROJECT_ID}/open-match/matchmaker-example:latest,env=GOOGLE_BUILDABLE=./examples/matchmaker
```

## Deploy

The [`deploy/cloudbuild.yaml`](deploy/cloudbuild.yaml) file deploys the `matchmaker` container alongside an OpenTelemetry Collector sidecar container to Cloud Run. Run it from `v2/examples/matchmaker/deploy/`:

```bash
cd v2/examples/matchmaker/deploy
gcloud builds submit --async --no-source \
  --substitutions=_REGION=${REGION},_ARTIFACT_REGISTRY_REPO=open-match,_MM_IMAGE_NAME=matchmaker-example:latest,_SERVICE_ACCOUNT=matchmaker-example-identity@${PROJECT_ID}.iam.gserviceaccount.com,_OM_CORE_ADDR=${OM_URL},_SOLODUEL_ADDR=${FIFO_URL},_DEBUGMMF_ADDR=${DEBUG_URL}
```

### `deploy/cloudbuild.yaml` Substitutions

| Substitution | Default | Description |
| :--- | :--- | :--- |
| `_JOB_NAME` | `matchmaker` | Cloud Run service name. |
| `_REGION` | `us-central1` | Google Cloud region for Cloud Run and Artifact Registry. |
| `_SERVICE_ACCOUNT` | `matchmaker-identity` | IAM service account identity used by the Cloud Run service. |
| `_ARTIFACT_REGISTRY_REPO` | `open-match-2-test` | Artifact Registry repository name in `${_REGION}-docker.pkg.dev/${PROJECT_ID}/${_ARTIFACT_REGISTRY_REPO}`. |
| `_MM_IMAGE_NAME` | `om-demo-matchmaker:0.0.1` | Matchmaker image name and tag. |
| `_OTEL_SIDECAR_IMAGE` | `otel-collector-sidecar` | OpenTelemetry Collector sidecar image name and tag. |
| `_OM_CORE_ADDR` | `OM_CORE_CLOUD_RUN_URL` | Base URL of the deployed `om-core` Cloud Run service. |
| `_SOLODUEL_ADDR` | `SOLODUEL_CLOUD_RUN_URL` | Base URL of the deployed `fifo` (SoloDuel) MMF Cloud Run service. |
| `_SOLODUEL_PORT` | `"443"` | Port of the `fifo` (SoloDuel) MMF service. |
| `_DEBUGMMF_ADDR` | `DEBUGMMF_CLOUD_RUN_URL` | Base URL of the deployed `debug` MMF Cloud Run service. |
| `_DEBUGMMF_PORT` | `"443"` | Port of the `debug` MMF service. |

### Cloud Run Gen2 Singleton Flags

[`deploy/cloudbuild.yaml`](deploy/cloudbuild.yaml) deploys the service with specific Cloud Run flags required by the stateful, background-loop architecture of the matchmaker harness:

* `--execution-environment=gen2`: Uses the Cloud Run 2nd generation execution environment for full Linux syscall compatibility and predictable network/CPU behavior.
* `--no-cpu-throttling`: Allocates CPU continuously outside of HTTP request handling. Because `mmqueue.Run`, `gsdirector.Run`, and `q.GenerateTestTickets` run as background goroutines, throttling CPU between HTTP requests would stall matchmaking cycles, ticket activations, and metric exports.
* `--min-instances=1` and `--max-instances=1`: Runs the service as a pinned singleton instance (`1` min / `1` max). `min-instances=1` keeps the continuous director loop running without cold starts or scale-to-zero, while `max-instances=1` prevents Cloud Run from autoscaling multiple in-memory `gsdirector` and `MockAgonesIntegration` instances that would compete for the same tickets and maintain separate in-memory assignment maps.
* `--network=default --subnet=default --vpc-egress=all-traffic --ingress=internal --no-allow-unauthenticated`: Routes egress over Direct VPC while restricting ingress to authenticated internal callers.

## Service Account

Best practices dictate that you deploy applications to Cloud Run with minimal IAM permissions. This creates a service account named `matchmaker-example-identity` with Cloud Run Invoker permissions (so it can call `om-core` and MMFs running in Cloud Run), usable by you and the Cloud Build service accounts:

```bash
export PROJECT_ID=$(gcloud config get project)
export PROJECT_NUMBER=$(gcloud projects list --filter=$(gcloud config get project) --format="value(PROJECT_NUMBER)")
export GCLOUD_ACCOUNT=$(gcloud config get account)

gcloud iam service-accounts create matchmaker-example-identity \
  --display-name="Example Matchmaker Service Account"

gcloud iam service-accounts add-iam-policy-binding \
  matchmaker-example-identity@${PROJECT_ID}.iam.gserviceaccount.com \
  --role=roles/iam.serviceAccountUser \
  --member=user:${GCLOUD_ACCOUNT}

gcloud iam service-accounts add-iam-policy-binding \
  matchmaker-example-identity@${PROJECT_ID}.iam.gserviceaccount.com \
  --role=roles/iam.serviceAccountUser \
  --member=serviceAccount:${PROJECT_NUMBER}@cloudbuild.gserviceaccount.com

gcloud projects add-iam-policy-binding ${PROJECT_ID} \
  --member=serviceAccount:matchmaker-example-identity@${PROJECT_ID}.iam.gserviceaccount.com \
  --role=roles/run.invoker
```

In addition, if you deploy using `deploy/cloudbuild.yaml`, your compute/Cloud Build service account (`${PROJECT_NUMBER}-compute@developer.gserviceaccount.com`) needs `roles/run.developer` on the project and `roles/iam.serviceAccountUser` on the target service account (see the header comments in [`deploy/cloudbuild.yaml`](deploy/cloudbuild.yaml)).

## HTTP Endpoints

The matchmaker starts an HTTP server on `PORT` (default `8080`) exposing three endpoints:

1. **`POST /tickets`** (also accepts `GET /tickets`):
   * Generates a single mock client ticket (`gameclient.Simple`) and submits it to `mmqueue.ClientRequestChan` for creation (`CreateTicket`) and batched activation (`ActivateTickets`) in `om-core`.
   * Blocks until `om-core` completes activation for that ticket and responds with:
     * `200 OK`: Ticket was created and activated in `om-core`.
     * `500 Internal Server Error`: `ActivateTickets` failed for the ticket.
     * `408 Request Timeout`: Context deadline expired while waiting for queue slot, ticket creation, or activation.
     * `503 Service Unavailable`: Matchmaker process is shutting down.
2. **`GET /tpc/{tixPerCycle}`** / **`POST /tpc/{tixPerCycle}`**:
   * Dynamically sets the target Tickets Per Cycle (`TPC`) rate used by the background `GenerateTestTickets` loop (which attempts to create and queue `tixPerCycle` tickets every `TICKET_CREATION_CYCLE_DURATION_MS`, default 1 second).
   * `{tixPerCycle}` must be an integer between `0` and `10000` inclusive:
     * Setting `/tpc/0` stops automatic background ticket creation (`200 OK`).
     * Non-integer values, negative numbers, or values greater than `10000` are rejected with `400 Bad Request`.
3. **`GET /`** (`GET /{$}`):
   * Returns a plain-text status summary of the latest completed matchmaking cycle from `MockDirector.Status()`, reporting:
     * Accepted vs. proposed matches (with rejection counts broken down into `rejected`, `random_drop`, `assigned_tickets`, and `deactivation_timeout`).
     * Accepted vs. proposed tickets (with `re-activated` and `already_assigned` ticket counts).
     * Cycle execution duration, base `MM_CYCLE_MIN_DURATION_MS`, adaptive contention backoff multiplier, and percentage of minimum cycle duration used.

## Using the Example Matchmaker & Environment Variables

For local development, run the matchmaker from `v2/examples/matchmaker/`:

```bash
LOGGING_FORMAT=text LOGGING_LEVEL=debug OTEL_SIDECAR=false go run .
```

All configuration is read via Viper from environment variables:

| Environment Variable | Default | Description |
| :--- | :--- | :--- |
| `PORT` | `8080` | HTTP port on which the matchmaker listens for `/`, `/tickets`, and `/tpc/{tixPerCycle}`. |
| `OM_CORE_ADDR` | `"http://localhost:8080"` | Base URL of the `om-core` HTTP/gRPC-gateway service. |
| `OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL` | `500` | Maximum number of ticket IDs batched into a single `ActivateTickets` call to `om-core`. |
| `MAX_CONCURRENT_TICKET_CREATIONS` | `500` | Maximum number of concurrent `CreateTicket` calls in flight from `mmqueue`. |
| `INITIAL_TPC` | `10` | Initial tickets-per-cycle rate for the automatic `GenerateTestTickets` loop (`0` disables automatic generation at startup). |
| `TICKET_CREATION_CYCLE_DURATION_MS` | `1000` | Duration of each ticket generation cycle in milliseconds. |
| `TICKET_CREATION_CYCLES` | `2147483647` (`math.MaxInt32`) | Maximum number of ticket generation cycles to execute before the generator exits. |
| `NUM_MM_CYCLES` | `2147483647` (`math.MaxInt32`) | Maximum number of matchmaking cycles `gsdirector` will run before exiting. |
| `MM_CYCLE_MIN_DURATION_MS` | `5000` | Base minimum duration of a matchmaking cycle in milliseconds before adaptive backoff scaling. |
| `NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT` | `2147483647` (`math.MaxInt32`) | Exits the director loop early if this many consecutive cycles return zero proposed matches. |
| `REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT` | `false` | When `true`, rejects any proposed match with `deactivation_timeout=true` in `Match.Extensions` and re-activates its unclaimed tickets. |
| `WAIT_FOR_BACKFILL_MMFS` | `false` | When `true`, invokes `MmfRequest`s whose profile name starts with `"backfill."` synchronously before launching standard concurrent MMF requests. |
| `SOLODUEL_ADDR` | `"http://localhost"` | Address of the `fifo` (SoloDuel) MMF gRPC server. When set to `"http://localhost"`, `matchmaker` starts an in-process `fifo` MMF server automatically. |
| `SOLODUEL_PORT` | `50080` | Port of the `fifo` (SoloDuel) MMF gRPC server (`443` when deployed on Cloud Run). |
| `DEBUGMMF_ADDR` | `"http://localhost"` | Address of the `debug` MMF gRPC server. When set to `"http://localhost"`, `matchmaker` starts an in-process `debug` MMF server automatically. |
| `DEBUGMMF_PORT` | `50081` | Port of the `debug` MMF gRPC server (`443` when deployed on Cloud Run). |
| `LOGGING_FORMAT` | `"json"` | Log output format (`"json"` for structured Cloud Logging, `"text"` for human-readable local console output). |
| `LOGGING_LEVEL` | `"info"` | Log verbosity level (`"trace"`, `"debug"`, `"info"`, `"warn"`, `"error"`, `"fatal"`). |
| `LOG_CALLER` | `"false"` | When `"true"`, includes caller file and line number in log output. |
| `OTEL_SIDECAR` | `"true"` | When `"true"`, exports metrics over OTLP gRPC to a local OpenTelemetry Collector sidecar; when `"false"`, starts a local Prometheus HTTP exporter. |
| `OTEL_PROM_PORT` | `"2227"` | Local Prometheus scrape port used when `OTEL_SIDECAR=false`. |
| `ASSIGNMENT_TTL_MS` | `1200000` | Time in milliseconds (default 20 minutes) to retain assigned ticket IDs in the director's `assignments` collision-tracking map before expiring them. |
| `ASSIGNMENT_DISTRIBUTION_PATH` | `"channel"` | Transport for sending roster assignments from `gsdirector` to `mmqueue` (`"channel"` for in-process Go channels, `"pubsub"` for Google Cloud Pub/Sub). |
| `GCP_PROJECT_ID` | `"my-project-id"` | Google Cloud project ID used when `ASSIGNMENT_DISTRIBUTION_PATH=pubsub`. |
| `ASSIGNMENT_TOPIC_ID` | `"replace-me"` | Google Cloud Pub/Sub topic ID used when `ASSIGNMENT_DISTRIBUTION_PATH=pubsub`. |

## Future Refactoring Proposal (Director Testability)

Currently, `MockDirector.Run(ctx)` in [`v2/internal/gsdirector/gsdirector.go`](../../internal/gsdirector/gsdirector.go) couples long-running lifecycle concerns (the `NUM_MM_CYCLES` loop, background ticket re-activation worker channels, `assignmentTTL` map compaction, and inter-cycle sleep timers) directly with single-cycle matchmaking logic (fan-out/fan-in `InvokeMatchmakingFunctions` streaming, `ProposalsLoop` validation, random match drops via `rand.Intn(100) <= 1`, server allocation, and assignment publishing).

To improve unit testability and make custom director implementations easier to compose, we propose the following refactoring in a future iteration:

1. **Extract Single-Cycle Execution (`runCycle` / ` evaluateProposals`)**:
   * Separate a single matchmaking cycle's execution and proposal evaluation into deterministic, pure/isolated methods on `MockDirector` so unit tests can feed synthetic `[]*pb.Match` slices and verify accepted matches, rejected ticket IDs, and cycle metrics directly without orchestrating background goroutines or timers.
2. **Introduce an `OMClient` Interface**:
   * Replace the concrete `*omclient.RestfulOMGrpcClient` field on `MockDirector` (and `MatchmakerQueue`) with a narrow interface (`ValidateConnection`, `CreateTicket`, `ActivateTicketsWithResults`, `InvokeMatchmakingFunctions`), allowing tests to inject lightweight mocks without spinning up an `httptest.Server` and marshaling JSON-enveloped protobuf streams.
3. **Inject Match Evaluation Policies**:
   * Move inline policy decisions in `ProposalsLoop`—such as the simulated 2% random match drop (`rand.Intn(100) <= 1`), `deactivation_timeout` rejection handling, and collision/score tie-breaking—behind configurable policy functions or a `MatchEvaluator` interface so tests and production users can configure deterministic evaluation rules without relying on global PRNG state.
