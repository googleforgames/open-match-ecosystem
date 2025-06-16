この文書を日本語で [Open Match 2 サンプルマッチメーカーのドキュメント（日本語版）](README-JP.md)
# Open Match 2 Examples

The example matchmaker in the v2/ directory demonstrates how to build a complete matchmaking system using Open Match 2\. It is designed to be flexible, supporting both a monolithic deployment for local development and a decoupled, microservices-based architecture recommended for production.

#### **Core Components & Workflow**

The system is primarily composed of three applications found in the examples/ directory, which leverage shared logic from the internal/ directory.

**1\. Applications (examples/ directory):**

* **matchmaker**: A development and testing harness that runs all components in a single process. It initializes an in-memory mmqueue, a gsdirector, and a local Matchmaking Function (MMF) server, making it ideal for local development without needing to deploy multiple services.  
* **standalone/mmqueue (Matchmaking Queue):** This is the game client-facing frontend service.  
  * **Function:** It receives matchmaking requests from clients, creates Open Match Ticket protobufs, and queues them for creation and activation in om-core via the omclient. It also receives final game server assignments from the director to pass back to clients.  
* **standalone/gsdirector (Game Server Director):** This is the backend service that orchestrates the matchmaking process with the game server manager.  
  * **Function:** It determines what kinds of matches are needed by querying a game server manager (a mock Agones integration in this example). It then constructs Profile objects and calls om-core's InvokeMatchmakingFunctions endpoint. After receiving Match proposals, it evaluates them, allocates game servers for accepted matches, and sends the connection details to the mmqueue.  
* **mmf (Matchmaking Function):** A standalone gRPC server that contains the custom, game-specific matching logic.  
  * **Function:** om-core calls this service's Run method, streaming it a Profile containing pools of filtered tickets. The MMF then executes its logic (e.g., the provided fifo function creates simple first-in-first-out matches) and streams back Match proposals.

**2\. Shared Logic (internal/ directory):**

* **omclient**: A RESTful HTTP client for communicating with the om-core service's gRPC-gateway endpoints. It handles ticket creation, activation, and invoking matchmaking functions.  
* **mmqueue**: The module containing the core logic for the matchmaking queue, including managing client requests and processing assignments via Go channels. It can be run as part of the monolithic matchmaker or as the standalone mmqueue service.  
* **gsdirector**: Contains the logic for the game server director. This module includes a MockAgonesIntegration which simulates a game server manager by providing matchmaking requirements (e.g., game modes available in specific regions) and handling mock server allocations.  
* **metrics & logging**: Centralized modules for initializing OpenTelemetry metrics and structured Logrus logging, respectively, used by all applications.  
* **extensions**: Provides utility functions for working with the google.protobuf.Any type used extensively in Open Match protobuf messages.  
* **mocks/gameclient**: A utility to generate mock Ticket objects, simulating requests from game clients for testing purposes.

### **Matchmaker Design Explanation**

The design of the Open Match v2 example matchmaker revolves around reconciling three key pieces of information: the rules for **how to match players** for a given game mode, the inventory of **available game servers** and the types of matches they can host, and the pool of **available players** waiting for a match.

#### **The Game Server as the Single Source of Truth (Server Availability & Match Logic)**

In a production environment, the information about server availability and the rules for how to create matches are tied together, with the game server itself acting as the single source of truth.

* **Proposed Production Design**: The definition of a GameMode (which includes matchmaking rules, player pools, and MMF parameters) would be managed through Infrastructure-as-Code tools like Terraform or Helm, not hardcoded in the matchmaker. During deployment, these GameMode specifications would be applied as metadata (e.g., Kubernetes annotations) to the game server templates, such as an Agones Fleet. Consequently, every GameServer instance created from that fleet automatically inherits the precise definition of the match it is designed to host.  
* **How the Example Simulates This**: The example matchmaker simulates this production design using the **MockAgonesIntegration** in v2/internal/gsdirector/gsdirector.go.  
  * The static GameMode, FleetConfig, and zonePools variables are stand-ins for configurations that would live in an IaC tool.  
  * The MockAgonesIntegration.Init() function simulates the deployment process, consuming these static configurations to populate an in-memory map of gameServers. Each mock server in this map is associated with the specific MmfRequest it needs.  
  * The director then calls **GetMMFParams()**. In a real system, this function would query the Kubernetes API to discover ready GameServers and read their matchmaking metadata directly. In the example, it simply reads from the in-memory map created during initialization.  
* This approach ensures that the director is always working from the current state of the game server infrastructure, automatically discovering what kinds of matches are needed without requiring separate configuration.

#### **Storing Information on Available Players**

This responsibility is a collaboration between the matchmaking queue and Open Match Core.

* **Matchmaking Queue (mmqueue)**: This service (v2/internal/mmqueue/mmqueue.go) is the entrypoint for players. It accepts their requests to find a match and creates Open Match Ticket objects.  
* **Open Match Core (om-core)**: om-core is the authoritative state store. The mmqueue creates and activates tickets within om-core, which maintains the definitive, queryable pool of all players currently waiting to be matched. 

### **Reconciliation Process**

The system's primary function is to first reconcile server availability with the rules of the game modes they can host to understand what kinds of players are needed. It then reconciles that need against the pool of available players to create optimal matches and assign them to the appropriate servers.

#### **Reconciling Server Needs**

* **Responsible Component**: Game Server Director **(gsdirector)**.  
* **Process**: The gsdirector's main loop begins by querying its game server manager (simulated by MockAgonesIntegration.GetMMFParams()). This discovers the MmfRequest objects directly from the metadata of the available game servers. This first step reconciles server availability with match requirements, yielding a concrete list of matchmaking tasks to be performed.

#### **Reconciling Players with Server Needs**

* **Responsible Components**: A collaboration between the **gsdirector**, **om-core**, and the **mmf**.  
* **Process**:  
  * The gsdirector takes the discovered MmfRequests and sends them to om-core.  
  * om-core filters its database of available players based on the criteria in each request's Pools.  
  * om-core then invokes the appropriate **Matchmaking Function (mmf)**, streaming it the filtered pools of players.  
  * The mmf executes its game-specific logic and streams back Match proposals.  
  * Finally, the gsdirector receives these proposals, performs a final validation, and allocates a specific, ready game server for each accepted match, completing the assignment of players to a server. \[v2/internal/gsdirector/[gsdirector.go](http://gsdirector.go)\]

### **Advanced Matchmaker Operations: Backfills and Collision Handling**

Beyond the primary matchmaking flow, a robust system must handle common scenarios like filling partially empty game servers (backfill, join-in-progress, high-density game servers, etc) and resolving cases where the same player is proposed for multiple matches simultaneously (ticket collisions).

#### **Handling Backfills**

The example matchmaker treats a backfill as a specific, high-priority matchmaking request generated by a Matchmaking Function (MMF) when it creates a match that isn't full.

* **Backfill Request Generation (in the MMF)**:  
  * The process begins in the MMF, such as the one in v2/examples/mmf/functions/fifo/fifo.go. When the MMF forms a match but cannot fill all the desired player slots from the available ticket pools, it identifies the match as incomplete.  
  * Before sending the match proposal back to om-core, the MMF constructs a *new* MmfRequest. This new request contains a new Profile tailored to find the specific types and number of players needed to fill the empty slots.  
  * This new backfill MmfRequest is then attached to the extensions field of the original Match proposal. \[v2/examples/mmf/functions/fifo/fifo.go\]  
* **Backfill Request Processing (in the Director)**:  
  * The Game Server Director (gsdirector) receives the match proposal from the MMF and inspects its extensions. If it finds the attached backfill request, it knows the game server that will host this match will need more players later.  
  * The director then calls UpdateMMFRequest on its game server manager (MockAgonesIntegration). This function updates the matchmaking metadata on the allocated game server to reflect the new backfill requirement. This ensures that in the next matchmaking cycle, this specific server will be the source of a new, unique matchmaking request. \[v2/internal/gsdirector/gsdirector.go\]  
  * **Prioritization**: The director inherently prioritizes these specific backfill requests. When its GetMMFParams function gathers all matchmaking requests from the game servers, it sorts them based on how many servers share the same request. Since a backfill request is unique to a single server, it has the lowest reference count and is placed at the front of the list. This ensures that backfill requests are sent to om-core first, increasing the likelihood they are fulfilled quickly. \[v2/internal/gsdirector/gsdirector.go\]

#### **Handling Ticket Collisions**

Because Open Match 2 can run multiple MMFs in parallel, it's possible for the same Ticket to be included in more than one Match proposal within the same cycle. The responsibility for detecting and resolving these "ticket collisions" rests entirely with the director.

* **Collision Detection**:  
  * The gsdirector maintains an assignments map, which serves as a record of every ticket ID that has been successfully assigned to a game server in the current session. \[v2/internal/gsdirector/gsdirector.go\]  
  * After receiving a stream of match proposals from the MMFs and before allocating any servers, the director iterates through each proposed match. For every ticket in that match, it checks if the ticket's ID already exists in its assignments map.  
* **Collision Resolution**:  
  * If the director finds a ticket in a proposed match that has already been assigned, it considers this a collision.  
  * The entire match proposal containing the colliding ticket is immediately **rejected**.  
  * All tickets from the rejected match are then sent back to om-core for re-activation via the ActivateTickets API. This makes those players (excluding the one who was already assigned) available again for the next matchmaking cycle. This simple "first-come, first-served" approach ensures that once a player is assigned, they cannot be placed in another match. \[v2/internal/gsdirector/gsdirector.go\]

