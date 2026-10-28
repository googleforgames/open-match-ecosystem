// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// This example is part of one approach to writing a matchmaker using Open
// Match v2.x. Many approaches are viable, and we encourage you to read the
// documentation at open-match.dev for more ideas and patterns.
//
// Minimally, a matchmaker using open-match is expected to:
// 1) queue player matchmaking requests,
// 2) request matches and distribute them to servers,
// 3) notify players of their match assignments,
// 4) and provide custom matchmaking logic.
// These components can be one in the same, or have their duties federated
// across a number of platform services. Regardless of the pattern used, the
// collection of the above functionality is referred to by Open Match v2.x as
// your 'matchmaker'.
//
// NOTE: This example does not use om-core for assignments as this
// functionality is deprecated. It is recommended you use a player status
// service for storing and retrieving player assignments in production
// environments.
//
// This file mocks a component of a matchmaker that receives player matchmaking
// requests and creates tickets in Open Match (#1 above). A typical approach is
// a horizontally-scalable server process with:
// - an endpoint the client hits when it wants to begin matchmaking that does
//   some or all of these things:
//   - Generate a Open Match Ticket protobuf message containing the game
//     client-provided matching attributes (e.g. ping values, game mode and
//     character selections)
//   - Add additional data useful for matchmaking from your own authoritative
//     systems as necessary (e.g. MMR values, character progression, purchased
//     unlocks)
//   - Queues this ticket for creation in Open Match. This is a
//     production best practice and allows you to manage ticket pressure in Open
//     Match during spikes in demand (launch, immediately after a maintenance
//     window, after unexpected downtime, etc.)
// - An asynchronous loop to process queued tickets, handling these tasks:
//   - Creates queued tickets in Open Match (POST /tickets -> receive TicketID in
//     response). These TicketIDs need to be activated before they are available
//     for matching.
//   - POST batches of TicketIDs to the /tickets:activate endpoint to mark those
//     tickets as available for matching, and reports the result back to the
//     waiting client request.
//   - If a client goes away before its ticket is active, skips creating the
//     ticket or deactivates it (DELETE /tickets:deactivate) so that player is
//     not matched.
//   - Receives match assignments from the director over Pub/Sub. This example
//     only records them in metrics and logs; a real platform would deliver each
//     assignment to the right player, typically keeping the player to TicketID
//     mapping in a player status service rather than in this process.
//
// To run it locally (listens on PORT 8082 by default; see README.md):
//
//	LOGGING_FORMAT=text LOGGING_LEVEL=debug OTEL_SIDECAR=false \
//	  GCP_PROJECT_ID=<your-project-id> ASSIGNMENT_TOPIC_ID=<your-topic-id> go run .

package main

import (
	"context"
	"math"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel/metric"

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156
	// revive:disable-next-line:blank-imports
	_ "google.golang.org/protobuf/types/known/wrapperspb"

	//soloduelServer "open-match.dev/functions/golang/soloduel"
	//mmf "open-match.dev/mmf/server"
	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/internal/assignmentdistributor"
	"open-match.dev/open-match-ecosystem/v2/internal/logging"
	"open-match.dev/open-match-ecosystem/v2/internal/metrics"
	"open-match.dev/open-match-ecosystem/v2/internal/mmqueue"
	"open-match.dev/open-match-ecosystem/v2/internal/mocks/gameclient"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

var (
	otelShutdownFunc func(context.Context) error
	meterptr         *metric.Meter
	mockClientTicket = gameclient.Simple
)

func main() {
	// run returns instead of calling os.Exit or logger.Fatal directly so its
	// deferred cleanup (metrics flush) always runs.
	os.Exit(run())
}

func run() int {
	// Set up signal handling first so a SIGTERM during startup (for example
	// while waiting on the om-core connection check) also stops the process.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	// Cancelled either by a signal or explicitly during shutdown, stopping all
	// background work.
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	// Read config
	cfg := viper.New()
	cfg.SetDefault("OM_CORE_ADDR", "http://localhost:8080")
	// Per-attempt timeout for unary om-core calls (CreateTicket, ActivateTickets,
	// DeactivateTickets, ValidateConnection).
	cfg.SetDefault("OM_CORE_REQUEST_TIMEOUT_MS", 5000)
	// Must be less than or equal to om-core's OM_MAX_STATE_UPDATES_PER_CALL, or
	// om-core rejects every activation call.
	cfg.SetDefault("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 500)
	cfg.SetDefault("OM_CORE_MAX_CONCURRENT_ACTIVATION_CALLS", 10)
	cfg.SetDefault("MAX_CONCURRENT_TICKET_CREATIONS", 500)
	cfg.SetDefault("INITIAL_TPC", 0)
	cfg.SetDefault("TICKET_CREATION_CYCLE_DURATION_MS", 1000)
	cfg.SetDefault("TICKET_CREATION_CYCLES", math.MaxInt32)
	cfg.SetDefault("TICKET_ARRIVAL_SEED", 0) // 0 means a random seed each run
	// How long the queue tracks a ticket before treating it as expired in
	// om-core; keep it equal to om-core's OM_CACHE_TICKET_TTL_MS.
	cfg.SetDefault("MMQUEUE_TICKET_TTL_MS", 600000)
	// Cloud Run injects PORT; the local default avoids om-core's default 8080.
	cfg.SetDefault("PORT", 8082)

	// Override these with env vars when doing local development.
	// Suggested values in that case are "text", "debug", and "false",
	// respectively
	cfg.SetDefault("LOGGING_FORMAT", "json")
	cfg.SetDefault("LOGGING_LEVEL", "info")
	cfg.SetDefault("LOG_CALLER", "false")

	// Open Telemetry metrics config
	cfg.SetDefault("OTEL_SIDECAR", "true")
	cfg.SetDefault("OTEL_PROM_PORT", 2224)

	// Assignment distribution config
	// Returning assignments via channel only works if the mmqueue and the
	// gsdirector are running in the same process, so you'll need to provide a
	// different assignment return data flow. We suggest using a distributed
	// message bus, pub/sub system, or your platform service's notification
	// service when returning assignments.
	cfg.SetDefault("ASSIGNMENT_DISTRIBUTION_PATH", "pubsub")
	cfg.SetDefault("GCP_PROJECT_ID", "my-project-id")
	// The Pub/Sub TOPIC the director publishes assignments to; each mmqueue
	// instance creates (and deletes on shutdown) its own subscription to it.
	cfg.SetDefault("ASSIGNMENT_TOPIC_ID", "replace-me")

	// Read overrides from env vars
	cfg.AutomaticEnv()

	// initialize shared structured logging
	log := logging.NewSharedLogger(cfg)
	logger := log.WithFields(logrus.Fields{"component": "matchmaking_queue"})

	initialTPC := cfg.GetInt64("INITIAL_TPC")
	if initialTPC < 0 || initialTPC > 10000 {
		logger.Errorf("INITIAL_TPC must be between 0 and 10000 (the same limits as the /tpc endpoint); got %d", initialTPC)
		return 1
	}

	// Initialize Metrics
	if cfg.GetBool("OTEL_SIDECAR") {
		meterptr, otelShutdownFunc = metrics.InitializeOtel("mmqueue")
	} else {
		meterptr, otelShutdownFunc = metrics.InitializeOtelWithLocalProm(cfg.GetInt("OTEL_PROM_PORT"), "mmqueue")
	}
	// Flushing is called explicitly during shutdown (before the slow Pub/Sub
	// cleanup), and deferred in case run returns early.
	flushMetrics := sync.OnceFunc(func() {
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer flushCancel()
		if err := otelShutdownFunc(flushCtx); err != nil {
			logger.Errorf("Failed to flush metrics: %v", err)
		}
	})
	defer flushMetrics()

	queueOmClient := omclient.New(
		cfg,
		log,
		&http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        500,              // Maximum idle connections to keep open
				MaxIdleConnsPerHost: 500,              // Maximum idle connections per host
				IdleConnTimeout:     90 * time.Second, // How long to keep idle connections open
				DisableKeepAlives:   false,            // Make sure keep-alives are enabled
			},
			Timeout: 30 * time.Second,
		},
	)

	// Check once before creating anything that needs cleaning up (the Pub/Sub
	// subscription). Any failure is fatal because a matchmaking queue that cannot
	// reach om-core cannot do anything useful.
	omCoreAddr := cfg.GetString("OM_CORE_ADDR")
	connCtx, connCancel := context.WithTimeout(ctx, 10*time.Second)
	err := queueOmClient.ValidateConnection(connCtx, omCoreAddr)
	connCancel()
	if err != nil {
		logger.Errorf("Unable to reach om-core at OM_CORE_ADDR=%q: %v. Check that om-core is running and OM_CORE_ADDR is its exact URL (on Cloud Run: the service's https:// URL, and this service's identity needs the Cloud Run Invoker role on om-core).", omCoreAddr, err)
		return 1
	}

	// Initialize assignment distribution
	var receiver assignmentdistributor.Receiver
	switch cfg.GetString("ASSIGNMENT_DISTRIBUTION_PATH") {
	case "channel":
		log.Info("Using Go channels for assignment distribution")
		log.Error("Using Go channels for assignment distribution isn't possible unless the matchmaking queue and game server director are running in the same process, which should only be done when doing active local development. Since this is a standalone matchmaking queue process, there is no game server director sending data on the go channel - assignments are effectively discarded with this configuration.")
		assignmentsChan := make(chan *pb.Roster)
		receiver = assignmentdistributor.NewChannelReceiver(assignmentsChan)
	case "pubsub":
		fallthrough // default is 'pubsub' in the standalone mmqueue
	default:
		// note: if using pubsub to send assignments from your director to your
		// matchmaking queue, make sure you have sufficient quota for
		// subscriptions in your GCP project. Each instance of the mmqueue will
		// make a unique topic subscription.
		log.Println("Using Google Cloud Pub/Sub for assignment distribution")

		// On failure the constructor logs the error; returning 1 exits through
		// run's deferred cleanup.
		var err error
		receiver, err = assignmentdistributor.NewPubSubSubscriber(
			cfg.GetString("GCP_PROJECT_ID"),
			cfg.GetString("ASSIGNMENT_TOPIC_ID"),
			log,
		)
		if err != nil {
			return 1
		}
	}

	// Initialize the queue
	q := &mmqueue.MatchmakerQueue{
		OmClient:           queueOmClient,
		Cfg:                cfg,
		Log:                log,
		ClientRequestChan:  make(chan *mmqueue.ClientRequest),
		AssignmentReceiver: receiver,
		OtelMeterPtr:       meterptr,
	}

	// Start the queue. This is where requests are processed and added to Open Match.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.Run(ctx)
	}()

	// FOR TESTING ONLY
	// Asynchronous goroutine that loops TICKET_CREATION_CYCLES number of
	// times, attempting to queue the given number of tickets each cycle.
	// Update the tpc by sending an http GET to /tpc/<NUM> as per the handler
	// below.
	q.SetTestTicketConfig(initialTPC, mockClientTicket)
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.GenerateTestTickets(ctx)
	}()

	// Each server gets its own ServeMux, so the app endpoints are only served on
	// PORT (and /metrics only on the Prometheus port).
	mux := http.NewServeMux()

	// Handler for client matchmaking requests. Creating a ticket changes state,
	// so only POST is accepted.
	mux.HandleFunc("POST /tickets", func(w http.ResponseWriter, r *http.Request) {
		// Make a channel on which the queue will return its http.Status result
		resultChan := make(chan int, 1)
		req := &mmqueue.ClientRequest{
			// If the client goes away before the ticket is active, the queue skips
			// or deactivates it so the player isn't matched.
			Ctx:        r.Context(),
			ResultChan: resultChan,
			Ticket: func(r *http.Request) *pb.Ticket {
				// In a real game client, you'd probably use whatever communication
				// protocol/format (JSON string, binary encoding, a custom format,
				// etc) your game engine or dev kit encourages, and parse that data
				// from the http.Request into the Open Match protobuf `ticket`
				// protobuf here.
				//
				// This example uses the provided internal library to generate a simple
				// ticket with a few example attributes for testing.
				return mockClientTicket(r.Context())
			}(r),
		}

		select {
		case q.ClientRequestChan <- req:
		case <-ctx.Done():
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		case <-r.Context().Done():
			// The client went away; nobody is listening for a response.
			return
		}

		// The queue sends 200 (created and activated), 502 (om-core call failed),
		// or 503 (shutting down), and closes resultChan without a value if it
		// abandoned the request because the client went away.
		select {
		case status, ok := <-resultChan:
			if ok {
				w.WriteHeader(status)
			}
		case <-ctx.Done():
			// Shutting down; don't wait for the queue.
			w.WriteHeader(http.StatusServiceUnavailable)
		case <-r.Context().Done():
			// The client went away; nobody is listening for a response.
		}
	})

	// Handler to update the (best effort) number of test tickets we want to
	// generate and put into the queue per ticket creation cycle
	// (TICKET_CREATION_CYCLE_DURATION_MS). If the requested TPC is more than the
	// matchmaker can manage in a cycle, it will do as many as it can. Set it to 0
	// to stop automatically creating tickets.
	mux.HandleFunc("/tpc/{tixPerCycle}", func(w http.ResponseWriter, r *http.Request) {
		// Attempt to convert requested TPC string into an int64
		rTPC, err := strconv.ParseInt(r.PathValue("tixPerCycle"), 10, 64)
		if err != nil {
			// Couldn't convert to int; maybe a typo in the http request?
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Hard cap of 10k per cycle and reject negative values to protect against mistyped http requests
		if rTPC < 0 || rTPC > 10000 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Update tixPerCycle number, and generate tickets using the gameclient.Simple function.
		q.SetTestTicketConfig(rTPC, mockClientTicket)

		logger.Infof("TPC set to %v", rTPC)
		w.WriteHeader(http.StatusOK)
	})

	// Profiling endpoints are mounted explicitly on this server's mux. The
	// example deployments only allow internal, authenticated traffic (Cloud Run
	// with internal ingress and no unauthenticated access; ClusterIP on GKE); do
	// not expose these on an internet-facing port.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	// Start http server.
	logger.Infof("PORT %v detected", cfg.GetString("PORT"))
	srv := &http.Server{
		Addr:              ":" + cfg.GetString("PORT"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	srvErr := make(chan error, 1)
	go func() {
		// ErrServerClosed is the error returned by http.Server on graceful exit.
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			srvErr <- err
		}
	}()

	exitCode := 0
	select {
	case <-sigCtx.Done():
		logger.Info("Shutdown signal received")
	case err := <-srvErr:
		logger.Errorf("HTTP server failed: %v", err)
		exitCode = 1
	}

	// Restore default signal handling so a second SIGINT or SIGTERM (for example
	// an impatient ctrl+c) kills the process instead of waiting for shutdown.
	stopSignals()

	// Shutdown budget: Cloud Run sends SIGTERM and kills the container about 10
	// seconds later, so each step is bounded and the most important work goes
	// first (1s + 3s + 2s + 3s = 9s worst case):
	//  1. Cancel the application context (stops ticket generation and the queue,
	//     and makes waiting /tickets handlers answer 503).
	//  2. Stop accepting HTTP requests and let in-flight ones finish (1s).
	//  3. Wait for background work to stop (3s).
	//  4. Flush OpenTelemetry metrics (2s).
	//  5. Delete this instance's Pub/Sub subscription (3s).
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 1*time.Second)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Errorf("HTTP server shutdown: %v", err)
	}
	shutdownCancel()

	if !waitBounded(3*time.Second, wg.Wait) {
		logger.Error("Timed out waiting for background work to stop")
	}

	flushMetrics()

	if !waitBounded(3*time.Second, receiver.Stop) {
		logger.Error("Timed out stopping assignment distribution; the Pub/Sub subscription may need to be deleted manually")
	}

	logger.Info("Exiting...")
	return exitCode
}

// waitBounded runs fn in a goroutine and reports whether it finished within
// timeout. If fn did not finish in time, it keeps running in the background
// until the process exits.
func waitBounded(timeout time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
