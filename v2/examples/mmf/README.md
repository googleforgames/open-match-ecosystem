# OM2 Matchmaking Function (MMF) Sample

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

## MMF Best Practices

[!NOTE]
Actual matching logic is very bespoke, and there are a number of recommended approaches. If you're trying to work through a specific matching algorythm, we suggest reaching out to the Open Match community using a github discussion or the slack channel. 

This section describes best practices for an Open Match MMF.

* The Open Match Profile protobuf message containing Pools filled with Tickets may exceed the default protobuf message size limit of 4mb. OM respects this limit (as some middlewares do not support higher limits), and will 'chunk' these Profiles into multiple messages, streamed to your MMF. Your MMF SHOULD read all streamed chunks sent to it by OM and concatinate all the Pools it receives. However, each 'chunk' contains all of the data in the Profile *except* for the Profile's Pools, of which each 'chunk' gets a guaranteed-unique portion. This means your MMF MAY still perform matching even if it does not receive all 'chunks'.
* ALWAYS make sure your matching logic (i.e. the Run() function) will eventually exit. Open Match will never cancel your MMF invocation, exiting is your responsibility.
* DO copy all extensions in the profile over to the extensions of each outgoing match.
* DO make an extension field holding the name of the Profile used to make the match. By convention, the key for this field should be `profileName`. 
* DO make an extension field holding the name of the MMF used to make the match. By convention, the key for this field should be `mmfName`. 
* DO stream back your matches as you make them when possible.
* If you want observability beyond the system-level metrics you get from the platform where your MMF runs, you SHOULD instrument your MMF yourself, and have it output metrics to your observability suite. Open Match does not collect metrics from your MMFs on your behalf.
