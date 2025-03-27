# Open Match v2.x Examples

This directory contains the example application `main` functions. For the most part, these only handle basic configuration and setup, the majority of the functionality for the applications themselves is located in the modules in the associated `../internal/` directory. They are tested locally and in Google Cloud Run, and target being run as simple server applications in OCI container format, suitable to be run on most platforms that support running containers with just a little effort.

Included applications have additional instructions for us in their individual README files, but at a high level:
* `mmf/`, a simple implementation of a grpc matchmaking function server. Sample logic functions are included.
* `standalone/`, which includes example applications for the recommended production deployment of an OM2-based matchmaker:
  * `mmqueue/` ("Matchmaking Queue"), the game client-facing matchmaking frontend that queues player requests and interacts with the Open Match ticketing endpoints.
  * `gsdirector/` ("Game Server Director"), the game server manager-facing matchmaking backend that interacts with the game server manager, collects matchmaking function invocation requests, receives match results from matchmaking functions, and allocates servers for those matches.
* `matchmaker/`, which is a matchmaker development and testing harness. It is a single application that can run matchmaking functions, the game client-facing queue, and the game server manager-facing director. It therefore does *not* scale to production workloads and is only intended for local testing.

* ## Intended use & deployment

* A live production OM2-based matchmaker is recommended to look like this:
* 
