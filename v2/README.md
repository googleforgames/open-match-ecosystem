この文書を日本語で読む: [Open Match v2.x エコシステム（日本語版）](README-JP.md)

# Open Match v2.x Ecosystem

This is the subdirectory holding the examples and development framework for building a matchmaker based on Open Match v2.x. The following are included:

* The [`examples/`](examples/) directory contains examples of how to write a matchmaker application that interfaces with Open Match. See [`examples/README.md`](examples/README.md) (or [`examples/README-JP.md`](examples/README-JP.md)) for more details.
* The [`internal/`](internal/) directory contains shared Go packages (`omclient`, `mmqueue`, `gsdirector`, `assignmentdistributor`, `extensions`, `logging`, `metrics`, and `mocks/gameclient`) used by the applications in the `examples/` directory.
* The [`cloudbuild.yaml`](cloudbuild.yaml) file can be used to build container images of the applications in the `examples/` directory using Google Cloud Buildpacks. The instructions for using it are in the comments at the top of the file, and instructions for deploying the application container images can be found in the application directories themselves.

## Development

### For Maintainers

This module (`open-match.dev/open-match-ecosystem/v2`) targets **Go 1.26+**. When maintaining dependencies from the `v2/` directory:

1. **Upgrade Open Match Core (`om-core`) protobufs and client libraries**:
   ```bash
   cd v2
   go get github.com/googleforgames/open-match2/v2@latest
   ```
2. **Update direct and transitive module dependencies**:
   ```bash
   cd v2
   go get -u ./... && go mod tidy
   ```
3. **Verify module integrity, static analysis, and unit tests** before submitting changes:
   ```bash
   cd v2
   go mod verify
   go vet ./...
   go test -race -count=1 ./...
   ```

### Build

All commands below should be run from the `v2/` directory (where `go.mod` and `internal/` reside):

* **Compile all packages and example binaries**:
  ```bash
  go build ./...
  ```
* **Run static analysis**:
  ```bash
  go vet ./...
  golangci-lint run ./...
  ```
* **Run race-enabled unit tests**:
  ```bash
  go test -race -count=1 ./...
  ```
* **Build container images remotely with Google Cloud Build**:
  Use [`cloudbuild.yaml`](cloudbuild.yaml) from `v2/` by passing the target example package in `_GOOGLE_BUILDABLE`:
  ```bash
  # Build the all-in-one matchmaker example image
  gcloud builds submit --async \
    --substitutions=COMMIT_SHA=latest,_JOB_NAME=matchmaker-example,_GOOGLE_BUILDABLE=./examples/matchmaker

  # Build the sample Matchmaking Function (MMF) image
  gcloud builds submit --async \
    --substitutions=COMMIT_SHA=latest,_JOB_NAME=soloduel,_GOOGLE_BUILDABLE=./examples/mmf/
  ```

## Deploy & Run

* **[Architecture & Design Guide (`examples/README.md`)](examples/README.md)** ([日本語版 (`examples/README-JP.md`)](examples/README-JP.md)): Explains how the Open Match 2 example components collaborate, how server availability and matchmaking profiles are reconciled, how backfills are prioritized, and how to diagnose and resolve ticket collisions vs. replication lag.
* **[All-in-One Matchmaker Harness (`examples/matchmaker`)](examples/matchmaker/README.md)**: Runs `mmqueue`, `gsdirector`, and in-process `fifo` / `debug` MMF servers inside a single binary for rapid local iteration or singleton Cloud Run testing. Includes instructions for local execution, HTTP control endpoints (`POST /tickets`, `GET/POST /tpc/{tixPerCycle}`, `GET /`), environment variables, and Cloud Run deployment via [`examples/matchmaker/deploy/cloudbuild.yaml`](examples/matchmaker/deploy/cloudbuild.yaml).
* **[Matchmaking Function Sample (`examples/mmf`)](examples/mmf/README.md)**: Standalone gRPC server wrapping the `fifo` and `debug` matchmaking functions selectable via `MMF_FUNCTION`. Includes MMF implementation best practices and Cloud Run deployment via [`examples/mmf/deploy/cloudbuild.yaml`](examples/mmf/deploy/cloudbuild.yaml).
* **[Standalone Microservices (`examples/standalone`)](examples/standalone)**: Production-oriented split of the matchmaking frontend queue ([`examples/standalone/mmqueue`](examples/standalone/mmqueue)) and backend game server director ([`examples/standalone/gsdirector`](examples/standalone/gsdirector)), communicating match assignments asynchronously over Google Cloud Pub/Sub (`ASSIGNMENT_DISTRIBUTION_PATH=pubsub`).
