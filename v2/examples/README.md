この文書を日本語で [Open Match 2 サンプルマッチメーカーのドキュメント（日本語版）](README-JP.md)

# Open Match 2 Examples

The example matchmaker in the `v2/` directory demonstrates how to build a complete matchmaking system using Open Match 2. It is designed to be flexible, supporting both a monolithic deployment for local development and a decoupled, microservices-based architecture recommended for production.

#### **Core Components & Workflow**

The system is primarily composed of applications found in the `examples/` directory, which leverage shared logic from the `internal/` directory.

**1. Applications (`examples/` directory):**

* **[matchmaker](matchmaker/README.md)**: A development and testing harness that runs all components in a single process. It initializes an in-memory `mmqueue`, a `gsdirector`, and local Matchmaking Function (MMF) servers (`fifo` and `debug`), making it ideal for local development without needing to deploy multiple services.
* **[standalone/mmqueue](standalone/mmqueue) (Matchmaking Queue):** This is the game client-facing frontend service.
  * **Function:** It receives matchmaking requests from clients, creates Open Match `Ticket` protobufs, and queues them for creation and activation in `om-core` via `omclient`. It also receives final game server assignments from the director to pass back to clients.
* **[standalone/gsdirector](standalone/gsdirector) (Game Server Director):** This is the backend service that orchestrates the matchmaking process with the game server manager.
  * **Function:** It determines what kinds of matches are needed by querying a game server manager (a mock Agones integration in this example). It then constructs `Profile` objects and calls `om-core`'s `InvokeMatchmakingFunctions` endpoint. After receiving `Match` proposals, it evaluates them, allocates game servers for accepted matches, and sends the connection details to the `mmqueue`.
* **[mmf](mmf/README.md) (Matchmaking Function):** A standalone gRPC server that contains the custom, game-specific matching logic.
  * **Function:** `om-core` calls this service's `Run` method, streaming it a `Profile` containing pools of filtered tickets. The MMF then executes its logic (e.g., the provided `fifo` function creates simple first-in-first-out matches, or the `debug` function logs the incoming profile and pools) and streams back `Match` proposals.

**2. Shared Logic (`internal/` directory):**

* **[omclient](../internal/omclient)**: A RESTful HTTP client for communicating with the `om-core` service's gRPC-gateway endpoints. It handles ticket creation, batched activation with per-ticket result reporting, and invoking matchmaking functions.
* **[mmqueue](../internal/mmqueue)**: The module containing the core logic for the matchmaking queue, including managing client requests and processing assignments. It can be run as part of the monolithic `matchmaker` or as the standalone `mmqueue` service.
* **[gsdirector](../internal/gsdirector)**: Contains the logic for the game server director. This module includes a `MockAgonesIntegration` which simulates a game server manager by providing matchmaking requirements (e.g., game modes available in specific regions) and handling mock server allocations.
* **[assignmentdistributor](../internal/assignmentdistributor/README.md)**: An interface the samples use to return assignments from the game server director to the matchmaking queue, with two implementations (one using Google Cloud Pub/Sub for production-like decoupled deployments, and one using Go channels for in-process local development).
* **[metrics](../internal/metrics) & [logging](../internal/logging)**: Centralized modules for initializing OpenTelemetry metrics and structured Logrus logging, respectively, used by all applications.
* **[extensions](../internal/extensions)**: Provides utility functions and canonical keys for working with the `google.protobuf.Any` type used extensively in Open Match protobuf `extensions` maps.
* **[mocks/gameclient](../internal/mocks/gameclient)**: A utility to generate mock `Ticket` objects, simulating requests from game clients for testing purposes.

### **Matchmaker Design Explanation**

The design of the Open Match v2 example matchmaker revolves around reconciling three key pieces of information: the rules for **how to match players** for a given game mode, the inventory of **available game servers** and the types of matches they can host, and the pool of **available players** waiting for a match.

#### **The Game Server as the Single Source of Truth (Server Availability & Match Logic)**

In a production environment, the information about server availability and the rules for how to create matches are tied together, with the game server itself acting as the single source of truth.

* **Proposed Production Design**: The definition of a `GameMode` (which includes matchmaking rules, player pools, and MMF parameters) would be managed through Infrastructure-as-Code tools like Terraform or Helm, not hardcoded in the matchmaker. During deployment, these `GameMode` specifications would be applied as metadata (e.g., Kubernetes annotations) to the game server templates, such as an Agones Fleet. Consequently, every `GameServer` instance created from that fleet automatically inherits the precise definition of the match it is designed to host.
* **How the Example Simulates This**: The example matchmaker simulates this production design using the **`MockAgonesIntegration`** in [`v2/internal/gsdirector/gsdirector.go`](../internal/gsdirector/gsdirector.go).
  * The static `GameMode`, `FleetConfig`, and `ZonePools` variables are stand-ins for configurations that would live in an IaC tool.
  * The `MockAgonesIntegration.Init()` function simulates the deployment process, consuming these static configurations to populate an in-memory map of `gameServers`. Each mock server set in this map is associated with the specific `MmfRequest` it needs.
  * The director then calls **`GetMMFParams()`**. In a real system, this function would query the Kubernetes API to discover ready `GameServer`s and read their matchmaking metadata directly. In the example, it simply reads from the in-memory map created during initialization.
* This approach ensures that the director is always working from the current state of the game server infrastructure, automatically discovering what kinds of matches are needed without requiring separate configuration.

#### **Storing Information on Available Players**

This responsibility is a collaboration between the matchmaking queue and Open Match Core.

* **Matchmaking Queue (`mmqueue`)**: This service ([`v2/internal/mmqueue/mmqueue.go`](../internal/mmqueue/mmqueue.go)) is the entrypoint for players. It accepts their requests to find a match and creates Open Match `Ticket` objects.
* **Open Match Core (`om-core`)**: `om-core` is the authoritative state store. The `mmqueue` creates and activates tickets within `om-core`, which maintains the definitive, queryable pool of all players currently waiting to be matched.

### **Reconciliation Process**

The system's primary function is to first reconcile server availability with the rules of the game modes they can host to understand what kinds of players are needed. It then reconciles that need against the pool of available players to create optimal matches and assign them to the appropriate servers.

#### **Reconciling Server Needs**

* **Responsible Component**: Game Server Director **(`gsdirector`)**.
* **Process**: The `gsdirector`'s main loop begins by querying its game server manager (simulated by `MockAgonesIntegration.GetMMFParams()`). This discovers the `MmfRequest` objects directly from the metadata of the available game servers. This first step reconciles server availability with match requirements, yielding a concrete list of matchmaking tasks to be performed.

#### **Reconciling Players with Server Needs**

* **Responsible Components**: A collaboration between the **`gsdirector`**, **`om-core`**, and the **`mmf`**.
* **Process**:
  * The `gsdirector` takes the discovered `MmfRequest`s and sends them to `om-core`.
  * `om-core` filters its active ticket cache of available players based on the criteria in each request's `Pools`.
  * `om-core` then invokes the appropriate **Matchmaking Function (`mmf`)**, streaming it the filtered pools of players.
  * The `mmf` executes its game-specific logic and streams back `Match` proposals.
  * Finally, the `gsdirector` receives these proposals, performs a final validation, and allocates a specific, ready game server for each accepted match, completing the assignment of players to a server. ([`v2/internal/gsdirector/gsdirector.go`](../internal/gsdirector/gsdirector.go))

### **Advanced Matchmaker Operations: Backfills and Collision Handling**

Beyond the primary matchmaking flow, a robust system must handle common scenarios like filling partially empty game servers (backfill, join-in-progress, high-density game servers, etc.) and resolving cases where the same player is proposed for multiple matches simultaneously (ticket collisions).

#### **Handling Backfills**

The example matchmaker treats a backfill as a specific, high-priority matchmaking request generated by a Matchmaking Function (MMF) when it creates a match that isn't full.

* **Backfill Request Generation (in the MMF)**:
  * The process begins in the MMF, such as the one in [`v2/examples/mmf/functions/fifo/fifo.go`](mmf/functions/fifo/fifo.go). When the MMF forms a match but cannot fill all the desired player slots from the available ticket pools, it identifies the match as incomplete.
  * Before sending the match proposal back to `om-core`, the MMF constructs a *new* `MmfRequest` whose `Profile.Name` is prefixed with `"backfill."` (`"backfill." + match.GetId()`). This new request contains an updated `Profile` tailored to find the remaining number of players needed to fill the empty slots.
  * This new backfill `MmfRequest` is then attached to the `Extensions` field of the original `Match` proposal under `open-match.dev/mmf-request`. ([`v2/examples/mmf/functions/fifo/fifo.go`](mmf/functions/fifo/fifo.go))
* **Backfill Request Processing (in the Director)**:
  * The Game Server Director (`gsdirector`) receives the match proposal from the MMF and inspects its `Extensions`. If it finds a valid attached backfill request (with a non-empty `Profile.Name`), it knows the game server that will host this match will need more players in a future cycle.
  * The director then calls `UpdateMMFRequest` on its game server manager (`MockAgonesIntegration`), which updates the matchmaking metadata on the allocated game server to reflect the new backfill requirement. When `Allocate` is subsequently called for that match, `MockAgonesIntegration` preserves the server's newly attached backfill `MmfRequest` rather than clearing its matchmaking parameters. ([`v2/internal/gsdirector/gsdirector.go`](../internal/gsdirector/gsdirector.go))
  * **Prioritization**: The director prioritizes backfill requests at two stages:
    1. **Request Dispatch Ordering**: When `GetMMFParams()` gathers matchmaking requests from game servers, it sorts them in ascending order by how many servers share the same allocation spec. Because a backfill request is unique to a single server (reference count of `1`), it is placed at the front of the request list and dispatched to `om-core` first. When `WAIT_FOR_BACKFILL_MMFS=true` is set, the director also waits for `"backfill."` `InvokeMatchmakingFunctions` calls to finish before launching regular concurrent MMF requests.
    2. **Proposal Evaluation Ordering**: Before running `ProposalsLoop`, the director stable-sorts all returned `proposedMatches` so proposals whose `profileName` extension starts with `"backfill."` are evaluated before standard match proposals. ([`v2/internal/gsdirector/gsdirector.go`](../internal/gsdirector/gsdirector.go))

#### **Handling Ticket Collisions**

Because Open Match 2 can run multiple MMFs and profiles in parallel—and because ticket deactivation state updates replicate asynchronously across `om-core` instances via Redis—the same `Ticket` can appear in more than one `Match` proposal either within the same matchmaking cycle or across consecutive cycles. Detecting and resolving these ticket collisions is the responsibility of the director ([`v2/internal/gsdirector/gsdirector.go`](../internal/gsdirector/gsdirector.go)).

* **Cross-Cycle vs. Same-Cycle Collision Detection**:
  * **Cross-Cycle Assigned Ticket Tracking (`assignments` map)**: The `gsdirector` maintains a cross-cycle `assignments` map (with entries expired after `ASSIGNMENT_TTL_MS`) recording every ticket ID that has already been allocated a game server and sent an assignment in an earlier cycle. If a proposed match contains any ticket in `assignments` (`numAssigned > 0`), it means a previously assigned ticket was still visible in an `om-core` instance's cache when the cycle started. The match is rejected and recorded under `gsdirector.matchmaking_cycle.match.rejections.assigned` and `gsdirector.ticket.rejections.assigned`.
  * **Same-Cycle Accepted Ticket Tracking (`acceptedTicketsThisCycle` map)**: Within each matchmaking cycle, the `gsdirector` also initializes a per-cycle `acceptedTicketsThisCycle` map. Whenever a proposed match passes validation in `ProposalsLoop`, all of its ticket IDs are added to `acceptedTicketsThisCycle` (and removed if server allocation later fails). If a subsequent proposed match in the same cycle contains any ticket already in `acceptedTicketsThisCycle` (`numSameCycleOverlap > 0`), the director detects a same-cycle MMF/profile overlap and rejects the match (incrementing `gsdirector.matchmaking_cycle.match.rejections` without incrementing `gsdirector.matchmaking_cycle.match.rejections.assigned`).
* **`backfill.` Proposal Prioritization During Collision Resolution**:
  * Because `proposedMatches` is stable-sorted to evaluate `"backfill."` proposals before non-backfill proposals, backfill matches claim their tickets in `acceptedTicketsThisCycle` first. If a backfill MMF and a standard MMF propose the same player in the same cycle, the backfill match wins and the competing standard match is rejected.
* **Skipping Already-Assigned and Already-Accepted Tickets During Re-Activation**:
  * When a proposed match is rejected for any reason, its unclaimed tickets must be returned to the active matchmaking pool via `om-core`'s `ActivateTickets` endpoint (`rejectMatch`).
  * However, a rejected match may contain a mix of unclaimed tickets and tickets that were already assigned in a prior cycle (`assignments[tid]`) or already accepted in an earlier match during the same cycle (`acceptedTicketsThisCycle[tid]`).
  * During `rejectMatch`, the director explicitly checks `if assignments[tid] || acceptedTicketsThisCycle[tid]` and **skips** those ticket IDs, queuing **only** unclaimed ticket IDs onto `ticketIdsToActivate`. This ensures that players who already have a valid match are never accidentally re-activated in `om-core`.

#### **How om-core Helps, and How to Tell Replication Lag from MMF Overlap**

`om-core` and `gsdirector` cooperate to minimize cross-cycle ticket collisions and expose metrics that let you distinguish **Redis cache replication lag** from **same-cycle MMF overlap**:

* **Deactivation Confirmation in `om-core`**:
  * Whenever an MMF streams a `Match` proposal back to `om-core`, `om-core` automatically queues ticket deactivation updates for every ticket in the match's rosters so those tickets will not be returned to subsequent `InvokeMatchmakingFunctions` calls.
  * When **`OM_MATCH_TICKET_DEACTIVATION_WAIT`** is `true` (the default in `om-core`), `om-core` waits for those deactivation updates to round-trip through the Redis replication stream and be applied to the local in-memory ticket cache before streaming the `Match` back to the director, up to **`OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS`** (default `60000` ms).
  * If local deactivation confirmation does not finish within **`OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS`**, `om-core` increments `om_core.mmf.deactivation.timeouts`, attaches a boolean extension **`deactivation_timeout`** (`true`) to `Match.Extensions["deactivation_timeout"]`, and still streams the match to the director so the matchmaker can decide how to handle it.
* **Director Policy (`REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT`) and Adaptive Backoff**:
  * `gsdirector` inspects every incoming match for the `deactivation_timeout` extension (`hasDeactivationTimeout`) and records `gsdirector.matchmaking_cycle.match.deactivation_timeouts` and `gsdirector.matchmaking_cycle.ticket.deactivation_timeouts`.
  * When **`REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT=false`** (default), `gsdirector` logs a warning, accepts the match if it passes all other checks, and increases the inter-cycle sleep multiplier (`computeCycleBackoffFactor`, based on `max(numMatchesWithAssignedTickets, numMatchesWithDeactivationTimeout) / numProposed + 1.0`) to give `om-core` replication time to catch up before the next cycle.
  * When **`REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT=true`**, `gsdirector` immediately rejects any match with `deactivation_timeout=true` and re-activates its unclaimed tickets in addition to applying the adaptive cycle backoff.

**Diagnostic Table: Pairing `gsdirector` / `mmqueue` Metrics with `om_core` Metrics**

| Scenario | `gsdirector` / `mmqueue` Metrics | `om_core` Metrics | What It Means |
| :--- | :--- | :--- | :--- |
| **1. Cross-cycle collisions (Replication Lag)** | `gsdirector.matchmaking_cycle.match.rejections.assigned` > 0<br>`gsdirector.ticket.rejections.assigned` > 0 | `om_core.cache.replication.lag` elevated (high p95/p99)<br>`om_core.cache.incoming.backlog` / `om_core.cache.outgoing.backlog` > 0 | Tickets assigned in a previous cycle had not yet been marked inactive in all `om-core` instances' local caches when the next cycle started. |
| **2. Same-cycle collisions (Concurrent MMF Overlap)** | `gsdirector.matchmaking_cycle.match.rejections` > 0<br>`gsdirector.matchmaking_cycle.match.rejections.assigned` == 0<br>`gsdirector.matchmaking_cycle.match.deactivation_timeouts` == 0 | `om_core.cache.replication.lag` low/healthy<br>`om_core.mmf.deactivation.timeouts` == 0 | Multiple concurrent MMFs or profiles invoked in the same cycle selected overlapping tickets; `acceptedTicketsThisCycle` resolved the collision locally in the director. |
| **3. Deactivation confirmation timeouts** | `gsdirector.matchmaking_cycle.match.deactivation_timeouts` > 0<br>`gsdirector.matchmaking_cycle.ticket.deactivation_timeouts` > 0<br>`gsdirector.matchmaking_cycle.duration` increasing | `om_core.mmf.deactivation.timeouts` > 0<br>`om_core.mmf.deactivation.wait` near `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS` | `om-core` could not confirm local cache application of match ticket deactivations within `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS` and set `deactivation_timeout=true`. |
| **4. Activation / Re-activation failures** | `mmqueue.ticket.activations.failures` > 0<br>`gsdirector.ticket.reactivations.failures` > 0 | `om_core.ticket.activation.failures.invalid_id` > 0<br>`om_core.ticket.activation.failures.unspecified` > 0<br>`om_core.rpc.errors` > 0 | If `invalid_id` > 0, an MMF placed synthetic or corrupted ticket IDs into a match roster; if `unspecified` > 0, `om-core` encountered backing Redis state store errors during `ActivateTickets`. |
| **5. MMF invocation or stream failures** | `gsdirector.mmf.response.failures` > 0<br>`gsdirector.matchmaking_cycle.match.proposals` drops to 0 while `mmqueue.ticket.creations` > 0 | `om_core.mmf.failures` > 0<br>`om_core.cache.tickets.active` growing while `om_core.mmf.deactivations` stalls | An MMF timed out (`OM_MMF_TIMEOUT_SECS`), returned a gRPC error, or was unreachable from `om-core`. |

**Remediation Steps**:

1. **Reduce Redis Replication Overhead with Packed Updates (`OM_CACHE_PACK_TICKET_STATE_UPDATES`)**:
   * Enable `OM_CACHE_PACK_TICKET_STATE_UPDATES=true` on `om-core` so multiple ticket activation/deactivation updates are packed into fewer Redis stream entries (`om_core.cache.outgoing.updates`), significantly lowering Redis write/read overhead and replication lag at high ticket rates.
   * **Safe Rollout Order**: Older `om-core` instances cannot parse packed multi-ticket stream entries. Always roll out in two steps:
     1. First, deploy the updated `om-core` binary across all instances with `OM_CACHE_PACK_TICKET_STATE_UPDATES=false` (or flush Redis during a maintenance window so no legacy instance reads the stream).
     2. Once 100% of running `om-core` instances support unpacking, update `om-core` to set `OM_CACHE_PACK_TICKET_STATE_UPDATES=true`.
2. **Tune Deactivation Wait & Cycle Cadence**:
   * Keep `OM_MATCH_TICKET_DEACTIVATION_WAIT=true` on `om-core` and ensure `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS` is larger than peak `om_core.cache.replication.lag`.
   * Set `MM_CYCLE_MIN_DURATION_MS` on `gsdirector` higher than your p95/p99 `om_core.cache.replication.lag` (and `OM_CACHE_IN_WAIT_TIMEOUT_MS`), and enable `REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT=true` if you prefer to reject unconfirmed matches rather than risk cross-instance staleness during lag spikes.
   * If `om_core.cache.replication.lag` remains high, scale out `om-core` instances (ensuring `--no-cpu-throttling` is enabled on Cloud Run so background replication goroutines are never CPU-throttled between requests) or add Redis read replicas.
3. **Reduce Same-Cycle MMF Overlap**:
   * Partition `Pools` across concurrent profiles so MMFs do not compete for the exact same player set at the same time, and set `WAIT_FOR_BACKFILL_MMFS=true` on `gsdirector` if you want `"backfill."` profiles to finish deactivating matched tickets in `om-core` before standard profiles run.
