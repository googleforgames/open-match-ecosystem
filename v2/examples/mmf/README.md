# Development

This Open Match 2 sample MMF is intended as a starting point for your development of custom matching logic for Open Match 2. You are expected to adapt the logic in the `functions/soloduel/soloduel.go:Run()` function to suit your game's needs. 

The sample is a golang application designed to be built using [CNCF Buildpacks](https://www.cncf.io/projects/buildpacks/), and our primary build process specifically uses [Google Cloud's buildpacks](https://cloud.google.com/docs/buildpacks/overview). 

## Build

We test builds [remotely using Cloud Build on Google Cloud](https://cloud.google.com/docs/buildpacks/build-application#remote_builds).  A typical build goes something like this, with the repo cloned,  `gcloud` initialized, and an existing [Docker Artifact Registry](https://cloud.google.com/artifact-registry/docs/docker/store-docker-container-images) called `open-match`:
```
# The build must be kicked off from the ../../v2 directory.
cd <repo>/v2

# Update the dependencies to the latest versions and kick off a Cloud Build
go get -u && go mod tidy && gcloud builds submit --async --pack \
image=$(gcloud config get artifacts/location)-docker.pkg.dev/$(gcloud config get project)/open-match/soloduel,env=GOOGLE_BUILDABLE=./examples/mmf
```
There is no `cloudbuild.yaml` or `Dockerfile` provided. [Google Cloud's buildpacks](https://cloud.google.com/docs/buildpacks/overview) takes care of everything without those files.

This application can also be built with the [`pack`](https://buildpacks.io/docs/for-platform-operators/how-to/integrate-ci/pack/) or [`docker`](https://www.docker.com/) CLIs with a little effort.
