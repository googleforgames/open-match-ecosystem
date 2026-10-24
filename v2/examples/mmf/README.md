# OM2 Matchmaking Function (MMF) Sample

### Language-Agnostic MMFs

While this sample repository provides Matchmaking Functions (MMFs) written in Go, it is important to understand that Open Match 2 is designed to be language-agnostic. You are not required to write your MMFs in Go. We've repeatedly seen developers using Open Match adopt Go for their MMFs because they think it is required. **It is not.**

The only language requirement for any MMF is to implement the `MatchMakingFunctionService` gRPC service defined in the `mmf.proto` protocol buffer definition file. Protocol Buffers (protobufs) provide a language-neutral, platform-neutral mechanism for defining services and structuring data. You can use the protobuf compiler, `protoc`, to generate client and server code in any of its many supported languages. This allows you to develop your custom matching logic in the language that best fits your team and technical requirements.

For more details on which languages are supported by `protoc` and how to get started, you can visit the official Protocol Buffers documentation on [Supported Languages](https://protobuf.dev/reference/other/).

This Open Match 2 sample MMF is intended as a starting point for your development of custom matching logic. This repository makes getting started with an MMF in Go as easy as possible by putting all the boilerplate code required to set up and start the gRPC service into reusable [`main.go`](main.go) and [`server/server.go`](server/server.go) source files. As a result, most developers will only have to adapt the matching logic in the [`functions/fifo/fifo.go`](functions/fifo/fifo.go) `Run()` function to suit their game's needs.

The sample is a Go application designed to be built using [CNCF Buildpacks](https://www.cncf.io/projects/buildpacks/), and our primary build process specifically uses [Google Cloud's buildpacks](https://cloud.google.com/docs/buildpacks/overview).

## Selecting the MMF Implementation (`MMF_FUNCTION`)

[`main.go`](main.go) bundles two sample Matchmaking Function strategies into a single binary and selects which one to serve at startup based on the `MMF_FUNCTION` environment variable (case-insensitive, whitespace-trimmed; defaults to `fifo`):

* **`MMF_FUNCTION=fifo`** (default, or empty `""`): Runs the first-in-first-out matchmaking implementation in [`functions/fifo/fifo.go`](functions/fifo/fifo.go). It reconstructs chunked `Profile` messages, deduplicates tickets across pools, builds matches with `desiredNumRosters` rosters of up to `desiredRosterLen` tickets (or partial matches down to `minRosterLen` with a `"backfill."` `MmfRequest` attached to `Match.Extensions`), and streams `StreamedMmfResponse` messages back to `om-core`.
* **`MMF_FUNCTION=debug`**: Runs the diagnostic profile inspection implementation in [`functions/debug/debug.go`](functions/debug/debug.go). Instead of matching tickets, it logs the received `Profile` name, extensions, and pool filter definitions/ticket counts, and returns a single empty `Match` (with zero rosters/tickets and `score = math.MinInt32`) so `om-core` skips ticket deactivation while verifying end-to-end MMF connectivity.
* Passing any other value for `MMF_FUNCTION` causes the process to exit on startup with `unknown MMF_FUNCTION "<value>" (expected 'fifo' or 'debug')`.

### Environment Variables

| Environment Variable | Default | Description |
| :--- | :--- | :--- |
| `PORT` | `8080` | TCP port on which the MMF gRPC server listens. |
| `MMF_FUNCTION` | `"fifo"` | MMF strategy to serve (`"fifo"` or `"debug"`). |
| `LOGGING_FORMAT` | `"json"` | Log output format (`"json"` for Cloud Logging or `"text"` for local development). |
| `LOGGING_LEVEL` | `"info"` | Minimum log level (`"trace"`, `"debug"`, `"info"`, `"warn"`, `"error"`, `"fatal"`). |
| `LOG_CALLER` | `"false"` | When `"true"`, includes calling file and line number in log output. |

## Build

We test builds [remotely using Cloud Build on Google Cloud](https://cloud.google.com/docs/buildpacks/build-application#remote_builds). Because `go.mod` and `internal/` live in `v2/`, the build must be started from the `v2/` directory.

You can build using [`v2/cloudbuild.yaml`](../../cloudbuild.yaml):

```bash
cd <repo>/v2

# Build the MMF container image using v2/cloudbuild.yaml
gcloud builds submit --async \
  --substitutions=COMMIT_SHA=latest,_ARTIFACT_REGISTRY_URI=$(gcloud config get artifacts/location)-docker.pkg.dev/$(gcloud config get project)/open-match,_JOB_NAME=soloduel,_GOOGLE_BUILDABLE=./examples/mmf/
```

Or build directly with `gcloud builds submit --pack`:

```bash
cd <repo>/v2

# Update dependencies to the latest versions and kick off a Cloud Build
go get -u && go mod tidy && gcloud builds submit --async --pack \
  image=$(gcloud config get artifacts/location)-docker.pkg.dev/$(gcloud config get project)/open-match/soloduel,env=GOOGLE_BUILDABLE=./examples/mmf
```

This application can also be built locally with the [`pack`](https://buildpacks.io/docs/for-platform-operators/how-to/integrate-ci/pack/) or [`docker`](https://www.docker.com/) CLIs.

## Deploy

The [`deploy/cloudbuild.yaml`](deploy/cloudbuild.yaml) file (`v2/examples/mmf/deploy/cloudbuild.yaml`) deploys the MMF container image alongside an `opentelemetry-collector` sidecar container to Google Cloud Run (with `--use-http2` enabled for streaming gRPC, `--execution-environment=gen2`, and `--no-cpu-throttling`). Because the same container image supports both `fifo` and `debug`, you can deploy multiple Cloud Run services from a single built image by varying `_JOB_NAME` and `_MMF_FUNCTION`:

```bash
cd <repo>/v2/examples/mmf/deploy

# Deploy the FIFO Matchmaking Function service
gcloud builds submit --async --no-source \
  --substitutions=_JOB_NAME=soloduel,_REGION=${REGION},_SERVICE_ACCOUNT=${SA_EMAIL},_ARTIFACT_REGISTRY_REPO=open-match,_MMF_IMAGE_NAME=soloduel:latest,_MMF_FUNCTION=fifo

# Deploy the Debug Matchmaking Function service using the same image
gcloud builds submit --async --no-source \
  --substitutions=_JOB_NAME=debugmmf,_REGION=${REGION},_SERVICE_ACCOUNT=${SA_EMAIL},_ARTIFACT_REGISTRY_REPO=open-match,_MMF_IMAGE_NAME=soloduel:latest,_MMF_FUNCTION=debug
```

### `v2/examples/mmf/deploy/cloudbuild.yaml` Substitutions

| Substitution | Default | Description |
| :--- | :--- | :--- |
| `_JOB_NAME` | `soloduel` | Cloud Run service name to deploy. |
| `_REGION` | `us-central1` | Google Cloud region for Cloud Run and Artifact Registry. |
| `_SERVICE_ACCOUNT` | `soloduel-identity` | IAM service account used by the deployed MMF Cloud Run service. |
| `_ARTIFACT_REGISTRY_REPO` | `open-match-2-Test` | Artifact Registry repository name in `${_REGION}-docker.pkg.dev/${PROJECT_ID}/${_ARTIFACT_REGISTRY_REPO}`. |
| `_MMF_IMAGE_NAME` | `om-FIFO-mmf:0.0.1` | MMF container image name and tag. |
| `_OTEL_SIDECAR_IMAGE` | `otel-collector-sidecar` | OpenTelemetry Collector sidecar image name and tag. |
| `_MMF_FUNCTION` | `fifo` | Sets the `MMF_FUNCTION` environment variable (`fifo` or `debug`) on the `mmf` container. |
| `_LOGGING_LEVEL` | `info` | Sets the `LOGGING_LEVEL` environment variable on the `mmf` container. |

## MMF Best Practices

This section describes best practices for an Open Match MMF.

> [!NOTE]
> Actual matching logic is very bespoke, and there are a number of recommended approaches. If you're trying to work through a specific matching algorithm, we suggest reaching out to the Open Match community using a GitHub discussion or the Slack channel.

A typical approach is to consider your matching logic in your MMF as a matching 'strategy'. You are encouraged to have multiple strategies available to your matchmaker concurrently in the form of multiple MMFs deployed in your infrastructure.

> [!WARNING]
> Be aware that OM2 makes no effort to prevent individual concurrent MMF invocations from returning matches containing the same ticket (ticket 'collision'). Your matchmaker should be coded to avoid or remediate collisions.

* The Open Match `Profile` protobuf message containing `Pools` filled with `Tickets` may exceed the default protobuf message size limit of 4MB. OM respects this limit (as some middlewares do not support higher limits), and will 'chunk' these `Profiles` into multiple messages, streamed to your MMF. MMFs SHOULD read all streamed chunks sent to them by OM and concatenate all the `Pools` received in those 'chunks' (see `server.GetChunkedRequest(stream)` in [`server/server.go`](server/server.go)). Every 'chunk' contains all of the data in the `Profile` *except* for the `Profile`'s `Pools`, of which each 'chunk' gets a guaranteed-unique portion. This means your MMF MAY still perform matching even if it does not receive all 'chunks'.
* MMF matching logic (i.e. the `Run()` function) MUST eventually exit of its own accord. Open Match will never cancel your MMF invocation; exiting is your MMF's responsibility.
* MMFs SHOULD stream back matches as they are made, when possible.
* **Place ONLY valid ticket IDs into match rosters**:
  * Every `*pb.Ticket` added to `Match.Rosters[...].Tickets` MUST be a genuine ticket received in the streamed `Profile`'s `Pools` with its original, non-empty `om-core` ticket ID intact (and deduplicated if a ticket appears in multiple pools within the same profile, as demonstrated in [`functions/fifo/fifo.go`](functions/fifo/fifo.go)).
  * MMFs MUST NEVER place synthetic, placeholder, or empty ticket IDs into match rosters. `om-core` deactivates every ticket ID present in a returned match's rosters, and if the director subsequently rejects that match, it sends all roster ticket IDs to `om-core`'s `ActivateTickets` endpoint—where invalid IDs will fail validation (`InvalidArgument` with `google.rpc.BadRequest` field violations) and increment `om_core.ticket.activation.failures.invalid_id` and `gsdirector.ticket.reactivations.failures`.

By convention:

* Regarding `Extensions` fields, MMFs SHOULD:
  * **Clone `Profile.Extensions` via `maps.Clone` and initialize `nil` maps**:
    * Copy all extensions from the received `Profile` (`req.GetExtensions()`) to each outgoing `Match.Extensions` (and to any newly constructed backfill `Profile.Extensions`) using `maps.Clone` rather than assigning the map reference directly (`match.Extensions = req.GetExtensions()`). Assigning the map directly causes every match generated in the loop to share and overwrite the same underlying map (corrupting per-match keys such as `mmfName`, `profileName`, `score`, or `open-match.dev/mmf-request`) and will panic if `req.GetExtensions()` is `nil`. Always clone and nil-check:
      ```go
      matchExtensions := maps.Clone(req.GetExtensions())
      if matchExtensions == nil {
          matchExtensions = make(map[string]*anypb.Any)
      }
      ```
  * **Never set the reserved `deactivation_timeout` key in MMFs**:
    * The extension key `deactivation_timeout` (`extensions.MatchDeactivationTimeoutKey`) is reserved for `om-core`. `om-core` sets `Match.Extensions["deactivation_timeout"] = true` when local cache replication of ticket deactivations does not complete within `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS`.
    * MMFs MUST NEVER set or modify `deactivation_timeout` in `Profile.Extensions` or `Match.Extensions`. Setting it in an MMF will cause `gsdirector` to treat healthy matches as having timed out during ticket deactivation, falsely inflating `gsdirector.matchmaking_cycle.match.deactivation_timeouts`, triggering unnecessary inter-cycle contention backoff, or causing matches to be rejected when `REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT=true`.
  * Create and populate an extension field holding the name of the `Profile` used to make the match. The key for this field should be `profileName`.
  * Create and populate an extension field holding the name of the MMF used to make the match. The key for this field should be `mmfName`.
  * Create and populate an extension field containing the MMF's subjective evaluation of the match quality. The key for this field should be `score`. (See the below section "MMF considerations & common misconceptions" for more details.)
* When generating a match ID string, MMFs SHOULD use [reverse-DNS notation](https://en.wikipedia.org/wiki/Reverse_domain_name_notation) by convention. Put any frequently changing portions of the match ID (timestamps, match counter, etc.) between the last dot and the end of the ID string. This is to allow for recording a match ID as a metric attribute - everything after the last dot can be truncated and metrics can then still be meaningfully aggregated per MMF across multiple invocations by multiple profiles. For more information, search for 'high cardinality metrics'. See how the example does it in [`functions/fifo/fifo.go`](functions/fifo/fifo.go).

## MMF considerations & common misconceptions

This section lists common misconceptions and considerations that frequently come up when discussing Open Match.

* A good approach to understanding the relationship between your matchmaker and your MMF is to think of the MMF as a remote procedure that your matchmaker calls (OM core acts as a proxy for this call). As such, it is valuable to think of the `Profile` message type defined by OM as your MMF 'function arguments' and the `Match` message type as your 'function return values'. It is therefore RECOMMENDED that you parameterize your matching logic where it makes sense, and choose the values for those parameters in your matchmaker when sending the `Profile` to the `InvokeMatchmakingFunctions()` OM endpoint. Decide some canonical extension key names and code your matchmaker to write those values and your MMF to read those values, just like arguments to a function. For example, your MMF for a team game should be able to handle creating an arbitrary number of teams, and arbitrary team sizes - choose key names like `"desiredNumRosters"` and `"desiredRosterLen"`, and let the matchmaker write those to the `Profile` extensions field to control the number of teams and team size returned by the MMF according to the needs of a particular game server.
* An MMF SHOULD NOT assume it is the only MMF trying to make a match with the tickets it receives: other MMFs could be running concurrently, trying to match some of the same tickets using completely different criteria. The consequence of this is that MMFs SHOULD send back to the matchmaker some kind of subjective opinion about how good a given match is. The sample code from the project maintainers has a convention of writing this as an integer score to an extension field with the key `score`, although the sample use of it is trivial. In reality, a common approach would be something like:
  ```
  score = 
  (
   ( how close in matchmaking rating the players are on a 100-point scale, 
     higher scores are closer
     +
     how similar each player's latency to the server is on a 100-point scale, 
     higher scores are more similar)
  * multiplier based on how long the oldest ticket in the match has been waiting
  * multiplier based on if the match is an urgent backfill, join-in-progress, etc
  )
  ```
  The idea here is that the Matchmaker looks at all these scores and the current state of your fleet(s) of game servers and intelligently decides which matches should be put on servers, and which should be discarded.
* Typically, it is RECOMMENDED that an MMF is able to target a score threshold, and only return matches above that threshold. A common misconception is that an MMF must make as many matches as possible with the tickets it receives. In reality, it is often better to not make a match than to make a poor match - in a well-architected matchmaking approach, the tickets left behind will be considered by other concurrent matchmaking strategies or reconsidered in a future `InvokeMatchmakingFunctions` call, when more players have had the opportunity to join the matchmaking pool.
* If you want observability beyond the system-level metrics you get from the platform where your MMF runs, you MAY instrument your MMF, and have it output metrics to your observability suite. Open Match does not collect metrics from your MMFs on your behalf.
