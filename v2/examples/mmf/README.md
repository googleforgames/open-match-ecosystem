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

A typical approach is to consider your matching logic in your MMF as a matching 'strategy'. You are encouraged to have multiple strategies available to your matchmaker concurrently in the form of multiple MMFs deployed in your infrastructure. Note that OM2 makes no effort to prevent individual concurrent MMF invocations from returning matches containing the same ticket (ticket 'collision'). Your matchmaker should be coded to avoid or remediate collisions.

This section describes best practices for an Open Match MMF.

* The Open Match Profile protobuf message containing Pools filled with Tickets may exceed the default protobuf message size limit of 4mb. OM respects this limit (as some middlewares do not support higher limits), and will 'chunk' these Profiles into multiple messages, streamed to your MMF. MMFs SHOULD read all streamed chunks sent to them by OM and concatinate all the Pools received in those 'chunks'. Every 'chunk' contains all of the data in the Profile *except* for the Profile's Pools, of which each 'chunk' gets a guaranteed-unique portion. This means your MMF MAY still perform matching even if it does not receive all 'chunks'.
* MMF matching logic (i.e. the Run() function) MUST eventually exit of its own accord. Open Match will never cancel your MMF invocation, exiting is your MMF's responsibility.
* Regarding extension fields, MMFs SHOULD:
  * copy all extensions in the received profile over to the extensions of each outgoing match.
  * make an extension field holding the name of the Profile used to make the match. By convention, the key for this field should be `profileName`. 
  * make an extension field holding the name of the MMF used to make the match. By convention, the key for this field should be `mmfName`. 
  * make an extension field containing the MMF's subjective evaluation of the match quality. By convention, the key for this field should be `score`. (See the below section "MMF considerations & common misconceptions" for more details.)
* MMFs SHOULD stream back matches as they are made, when possible.
* When generating a match ID string, MMFs SHOULD use [reverse-DNS notation](https://en.wikipedia.org/wiki/Reverse_domain_name_notation) by convention. Put any frequently changing portions of the match ID (timestamps, match counter, etc) between the last dot and the end of the ID string. This is to allow for recording match ID as a metric attribute - everything after the last dot can be truncated and metrics can then still be meaningfully aggregated per MMF across multiple invocations by multiple profiles. For more information, Google 'high cardinality metrics'.

## MMF considerations & common misconceptions

This section tries to list some common misconceptions and considerations that frequently come up when discussing Open Match.

* A good approach to understanding the relationship between your matchmaker and your MMF is to think of the MMF as a remote procedure that your matchmaker calls (OM core acts as a proxy for this call). As such thinking of the Profile message type defined by OM as your MMF 'function arguments' and the Match message type as your 'function return values' is valuable. It is therefore RECOMMENDED that you make your parameterize your matching logic where it makes sense, and choose the values for those parameters in your matchmaker when sending the Profile to the InvokeMatchmakingFunction() OM endpoint.  Decide some canonical extension key names and code your matchmaker to write values to them and your MMF to read values from them.  For example, your MMF for a team game should be able to handle creating an arbitrary number of teams, and arbitrary team sizes - choose key names like "numTeams" and "teamSize", and let the matchmaker write those to the Profile extensions field to control the number of teams and team size returned by the MMF.
* An MMF SHOULD NOT it is the only MMF trying to make a match with the tickets it receives: other MMFs could be running concurrently, trying to match some of the same tickets using completely different criteria. The consequence of this is that MMFs SHOULD send back to the matchmaker some kind of subjective opinion about how good a given match is. The sample code from the project maintainers has a convention of writing this as an integer score to an extension field with the key `score`.
  * Typically, it is RECOMMENDED that an MMF is able to target a score threshold, and only return matches above that threshold. A common misconception is that an MMF must make as many matches as possible with the tickets it receives. In reality, it is often better to not make a match than to make a poor match.
* If you want observability beyond the system-level metrics you get from the platform where your MMF runs, you MAY instrument your MMF, and have it output metrics to your observability suite. Open Match does not collect metrics from your MMFs on your behalf.
