# OM2 Matchmaking Function (MMF) Sample

### Language-Agnostic MMFs

While this sample repository provides Matchmaking Functions (MMFs) written in Go, it is important to understand that Open Match 2 is designed to be language-agnostic. You are not required to write your MMFs in Go.  We've repeatedly seen developers using Open Match adopt Go for their MMFs because they think it is required.  **It is not.**

The only language requirement for any MMF is to implement the `MatchmakingFunction` gRPC service defined in the `mmf.proto` protocol buffer definition file. Protocol Buffers (protobufs) provide a language-neutral, platform-neutral mechanism for defining services and structuring data. You can use the protobuf compiler, `protoc`, to generate client and server code in any of its many supported languages. This allows you to develop your custom matching logic in the language that best fits your team and technical requirements.  

For more details on which languages are supported by `protoc` and how to get started, you can visit the official Protocol Buffers documentation on [Supported Languages](https://protobuf.dev/reference/other/).

This Open Match 2 sample MMF is intended as a starting point for your development of custom matching logic. This repository tries to make getting started with an MMF in Go as easy as possible by putting all the boilerplate code required to set up and start the gRPC service into reusable `main.go` and `server/server.go` source files. As a result, most developers will only have to adapt the matching logic in the `functions/soloduel/soloduel.go:Run()` function to suit their game's needs.

The sample is a golang application designed to be built using [CNCF Buildpacks](https://www.cncf.io/projects/buildpacks/), and our primary build process specifically uses [Google Cloud's buildpacks](https://cloud.google.com/docs/buildpacks/overview).

## Build

We test builds [remotely using Cloud Build on Google Cloud](https://cloud.google.com/docs/buildpacks/build-application#remote_builds). A typical build goes something like this, with the repo cloned, `gcloud` initialized, and an existing [Docker Artifact Registry](https://cloud.google.com/artifact-registry/docs/docker/store-docker-container-images) called `open-match`:

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
This section describes best practices for an Open Match MMF.

> [!NOTE]
> Actual matching logic is very bespoke, and there are a number of recommended approaches. If you're trying to work through a specific matching algorithm, we suggest reaching out to the Open Match community using a github discussion or the slack channel.

A typical approach is to consider your matching logic in your MMF as a matching 'strategy'. You are encouraged to have multiple strategies available to your matchmaker concurrently in the form of multiple MMFs deployed in your infrastructure. 

> [!WARNING]
> Be aware that OM2 makes no effort to prevent individual concurrent MMF invocations from returning matches containing the same ticket (ticket 'collision'). Your matchmaker should be coded to avoid or remediate collisions.

* The Open Match Profile protobuf message containing Pools filled with Tickets may exceed the default protobuf message size limit of 4mb. OM respects this limit (as some middlewares do not support higher limits), and will 'chunk' these Profiles into multiple messages, streamed to your MMF. MMFs SHOULD read all streamed chunks sent to them by OM and concatinate all the Pools received in those 'chunks'. Every 'chunk' contains all of the data in the Profile *except* for the Profile's Pools, of which each 'chunk' gets a guaranteed-unique portion. This means your MMF MAY still perform matching even if it does not receive all 'chunks'.
* MMF matching logic (i.e. the Run() function) MUST eventually exit of its own accord. Open Match will never cancel your MMF invocation, exiting is your MMF's responsibility.
* MMFs SHOULD stream back matches as they are made, when possible.

By convention:

* Regarding extension fields, MMFs SHOULD:
  * copy all extensions in the received profile over to the extensions of each outgoing match.
  * create and populate an extension field holding the name of the Profile used to make the match. The key for this field should be `profileName`. 
  * create and populate an extension field holding the name of the MMF used to make the match. The key for this field should be `mmfName`. 
  * create and populate an extension field containing the MMF's subjective evaluation of the match quality. The key for this field should be `score`. (See the below section "MMF considerations & common misconceptions" for more details.)
* When generating a match ID string, MMFs SHOULD use [reverse-DNS notation](https://en.wikipedia.org/wiki/Reverse_domain_name_notation) by convention. Put any frequently changing portions of the match ID (timestamps, match counter, etc) between the last dot and the end of the ID string. This is to allow for recording a match ID as a metric attribute - everything after the last dot can be truncated and metrics can then still be meaningfully aggregated per MMF across multiple invocations by multiple profiles. For more information, Google 'high cardinality metrics'.  See how the example does it [here](https://github.com/googleforgames/open-match-ecosystem/blob/a215e8590b4eb8d64b81e50290e9a3daff8868ac/v2/examples/mmf/functions/fifo/fifo.go#L163).

## MMF considerations & common misconceptions

This section tries to list some common misconceptions and considerations that frequently come up when discussing Open Match.

* A good approach to understanding the relationship between your matchmaker and your MMF is to think of the MMF as a remote procedure that your matchmaker calls (OM core acts as a proxy for this call). As such, it is valuable to think of the Profile message type defined by OM as your MMF 'function arguments' and the Match message type as your 'function return values'. It is therefore RECOMMENDED that you parameterize your matching logic where it makes sense, and choose the values for those parameters in your matchmaker when sending the Profile to the InvokeMatchmakingFunction() OM endpoint.  Decide some canonical extension key names and code your matchmaker to write those values and your MMF to read those values, just like arguments to a function.  For example, your MMF for a team game should be able to handle creating an arbitrary number of teams, and arbitrary team sizes - choose key names like "numTeams" and "teamSize", and let the matchmaker write those to the Profile extensions field to control the number of teams and team size returned by the MMF according to the needs of a particular game server.
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
  The idea here is that the Matchmaker look at all these scores and the current state of your fleet(s) of game servers and intelligently decide which matches should be put on servers, and which should be discarded.
* Typically, it is RECOMMENDED that an MMF is able to target a score threshold, and only return matches above that threshold. A common misconception is that an MMF must make as many matches as possible with the tickets it receives. In reality, it is often better to not make a match than to make a poor match - in a well-architected matchmaking approach, the tickets left behind will be considered by other concurrent matchmaking strategies or reconsidered in a future `InvokeMatchmakingFunctions` call, when more players have had had the opportunity to join the matchmaking pool.
* If you want observability beyond the system-level metrics you get from the platform where your MMF runs, you MAY instrument your MMF, and have it output metrics to your observability suite. Open Match does not collect metrics from your MMFs on your behalf.
