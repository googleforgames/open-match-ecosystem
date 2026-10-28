// Copyright 2024 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// NOTE: this is a test harness for local development of your
// matchmaker/matching logic.  It runs every component of your matchmaker in
// the same process - which is very unlikely to be how you want to do this in
// production.  Use this when iterating on your ticket properties and MMF logic
// locally, and look at the examples in the 'standalone' directory when you're
// ready to run the components separately in production (because you'll very
// likely want to scale your mmqueue processes horizontally).
//
// Here's a typical command line for running a copy of this file locally for
// development (it listens on PORT 8081 by default so it does not collide with
// a local om-core on 8080):
//
// LOGGING_FORMAT=text LOGGING_LEVEL=debug OTEL_SIDECAR=false go run .
package main

import (
	"context"
	"io"
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
	"google.golang.org/protobuf/types/known/anypb"
	knownpb "google.golang.org/protobuf/types/known/wrapperspb"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/examples/mmf/functions/debug"
	"open-match.dev/open-match-ecosystem/v2/examples/mmf/functions/fifo"
	mmfserver "open-match.dev/open-match-ecosystem/v2/examples/mmf/server"
	"open-match.dev/open-match-ecosystem/v2/internal/assignmentdistributor"
	"open-match.dev/open-match-ecosystem/v2/internal/extensions"
	"open-match.dev/open-match-ecosystem/v2/internal/gsdirector"
	"open-match.dev/open-match-ecosystem/v2/internal/logging"
	"open-match.dev/open-match-ecosystem/v2/internal/metrics"
	"open-match.dev/open-match-ecosystem/v2/internal/mmqueue"
	"open-match.dev/open-match-ecosystem/v2/internal/mocks/gameclient"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

type skillTier struct {
	name   string
	minMMR float64
	maxMMR float64
}

var (
	// global vars
	otelShutdownFunc func(context.Context) error
	meterptr         *metric.Meter

	// configuration that requires recompilation
	mockClientTicket = gameclient.Simple
	mmfDebug         = &pb.MatchmakingFunctionSpec{
		Name: "DEBUG",
		Type: pb.MatchmakingFunctionSpec_GRPC,
	}
	mmfFifo = &pb.MatchmakingFunctionSpec{
		Name: "FIFO",
		Type: pb.MatchmakingFunctionSpec_GRPC,
	}

	// Simple test extension to put into pools, so we can make sure it is
	// properly propogated to the MMF.
	testEx, _ = anypb.New(&knownpb.StringValue{Value: "testValue"})

	// NOTE: These tier boundaries deliberately overlap by a couple of MMR points
	// to exercise ticket collision handling: om-core's DoubleRangeFilters are
	// inclusive at both ends, so (for example) a player with 1400 MMR is in both
	// BronzeSilver and Gold. Such players are eligible for two tiers' profiles in
	// the same matchmaking cycle, so both MMFs can put them in a match, and the
	// director must resolve the same-cycle collision (only one match can be
	// accepted; the other is rejected and its remaining tickets re-activated),
	// which shows up in gsdirector.matchmaking_cycle.match.rejections.
	skillTiers = []skillTier{
		{name: "SoloDuel_BronzeSilver", minMMR: 500, maxMMR: 1401},
		{name: "SoloDuel_Gold", minMMR: 1399, maxMMR: 1601},
		{name: "SoloDuel_Platinum", minMMR: 1599, maxMMR: 1801},
		{name: "SoloDuel_DiamondPlus", minMMR: 1799, maxMMR: 2750},
	}

	// Populated at startup by buildSegmentedFleetConfig.
	gameModesInZone = map[string][]*gsdirector.GameMode{}
	zonePools       = map[string]*pb.Pool{}
	fleetConfig     = map[string]map[string]string{}
)

func buildSegmentedFleetConfig(activeMMFs []*pb.MatchmakingFunctionSpec) {
	tierModes := make([]*gsdirector.GameMode, len(skillTiers))
	for i, tier := range skillTiers {
		// Leave CreationTimeRangeFilter unset: om-core treats a pool without one
		// as "any creation time", which is wanted, whereas copying
		// gsdirector.EveryTicket's filter would capture the window computed at
		// package init time, which eventually stops matching new tickets on a
		// long-running instance.
		tierPool := &pb.Pool{
			Name: tier.name,
			DoubleRangeFilters: []*pb.Pool_DoubleRangeFilter{
				{
					DoubleArg: "mmr",
					Minimum:   tier.minMMR,
					Maximum:   tier.maxMMR,
				},
			},
			Extensions: map[string]*anypb.Any{"testKey": testEx},
		}

		mode := &gsdirector.GameMode{
			Name:  tier.name,
			MMFs:  activeMMFs,
			Pools: map[string]*pb.Pool{"mmr_tier": tierPool},
			ExtensionParams: extensions.AnypbIntMap(map[string]int32{
				"desiredNumRosters": 1,
				"desiredRosterLen":  4,
				"minRosterLen":      2,
			}),
		}
		_ = mode.SetBackfillMMFs(mmfFifo)
		tierModes[i] = mode
	}

	for category, regions := range gsdirector.FleetConfig {
		categoryFleets := make(map[string]string, len(regions)*len(skillTiers))
		for city, zone := range regions {
			for i, tier := range skillTiers {
				fleetKey := city + "_" + tier.name
				zoneKey := zone + "." + tier.name
				categoryFleets[fleetKey] = zoneKey
				zonePools[zoneKey] = &pb.Pool{
					DoubleRangeFilters: []*pb.Pool_DoubleRangeFilter{
						{
							DoubleArg: "ping." + zone,
							Minimum:   1,
							Maximum:   100,
						},
					},
				}
				gameModesInZone[zoneKey] = []*gsdirector.GameMode{tierModes[i]}
			}
		}
		fleetConfig[category] = categoryFleets
	}
}

func main() {
	// run returns instead of calling os.Exit or logger.Fatal directly so its
	// deferred cleanup (metrics flush, Pub/Sub cleanup) always runs.
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
	// Cloud Run injects PORT; the local default avoids om-core's default 8080.
	cfg.SetDefault("PORT", 8081)

	// Connection config.
	cfg.SetDefault("OM_CORE_ADDR", "http://localhost:8080")
	// Per-attempt timeout for unary om-core calls (CreateTicket, ActivateTickets,
	// DeactivateTickets, ValidateConnection).
	cfg.SetDefault("OM_CORE_REQUEST_TIMEOUT_MS", 5000)

	// OM core config that the matchmaker needs to respect.
	// Must be less than or equal to om-core's OM_MAX_STATE_UPDATES_PER_CALL, or
	// om-core rejects every activation call.
	cfg.SetDefault("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 500)
	cfg.SetDefault("OM_CORE_MAX_CONCURRENT_ACTIVATION_CALLS", 10)

	// Ticket creation config
	cfg.SetDefault("MAX_CONCURRENT_TICKET_CREATIONS", 500)
	cfg.SetDefault("INITIAL_TPC", 10)
	cfg.SetDefault("TICKET_CREATION_CYCLE_DURATION_MS", 1000)
	cfg.SetDefault("TICKET_CREATION_CYCLES", math.MaxInt32)
	cfg.SetDefault("TICKET_ARRIVAL_SEED", 0) // 0 means a random seed each run
	// How long the queue tracks a ticket before treating it as expired in
	// om-core; keep it equal to om-core's OM_CACHE_TICKET_TTL_MS.
	cfg.SetDefault("MMQUEUE_TICKET_TTL_MS", 600000)

	// InvokeMatchmaking Function config
	// In production, you will want this to be functionally infinite, thus the
	// use of the largest Int32 number possible.  However, when doing local
	// development of matching logic, it is often useful to run a deterministic
	// number of cycles. When the director stops, this process shuts down.
	cfg.SetDefault("NUM_MM_CYCLES", math.MaxInt32)   // math.MaxInt32 cycles is essentially forever
	cfg.SetDefault("MM_CYCLE_MIN_DURATION_MS", 5000) // Director will sleep if the invokeMMFs didn't take at least this long.
	// Director-side deadline for each cycle's InvokeMatchmakingFunctions calls;
	// the default matches om-core's OM_MMF_TIMEOUT_SECS default; 0 means no
	// director-side deadline.
	cfg.SetDefault("MMF_INVOCATION_TIMEOUT_SECS", 600)

	// Exit if this many consecutive matchmaking cycles come back empty. Again,
	// you probably don't want to do this in production, but it can be very useful
	// in local testing. The default (math.MaxInt32) never exits.
	cfg.SetDefault("NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT", math.MaxInt32)

	// Director match acceptance and backfill orchestration flags
	cfg.SetDefault("REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT", false)
	cfg.SetDefault("WAIT_FOR_BACKFILL_MMFS", false)
	// Percentage of proposed matches the director randomly rejects, to exercise
	// the ticket re-activation path; 0 disables it.
	cfg.SetDefault("RANDOM_MATCH_REJECT_PERCENT", 1)

	// MMF config
	cfg.SetDefault("SOLODUEL_ADDR", "http://localhost")
	cfg.SetDefault("SOLODUEL_PORT", 50080)
	// The DEBUG MMF is opt-in because it doubles the number of MMF invocations
	// per cycle and never produces matches. Set it to http://localhost to run it
	// in-process, or to its URL; the literal value "none" disables it; an empty
	// value does not work because viper falls back to this default when the env
	// var is empty.
	cfg.SetDefault("DEBUGMMF_ADDR", "none")
	cfg.SetDefault("DEBUGMMF_PORT", 50081)

	// Override these with env vars when doing local development.
	// Suggested values in that case are "text", "debug", and "false",
	// respectively
	cfg.SetDefault("LOGGING_FORMAT", "json")
	cfg.SetDefault("LOGGING_LEVEL", "info")
	cfg.SetDefault("LOG_CALLER", "false")

	// OpenTelemetry metrics config
	cfg.SetDefault("OTEL_SIDECAR", "true")
	cfg.SetDefault("OTEL_PROM_PORT", "2227")

	// Assignment config
	// How long the director remembers a ticket it has already assigned. The
	// director uses this map to reject proposals that contain an already-assigned
	// ticket, which can happen while om-core is still replicating that ticket's
	// deactivation. It must be longer than the time a ticket can stay visible in
	// om-core, so keep it at least as long as om-core's OM_CACHE_TICKET_TTL_MS
	// (default 10 mins).
	cfg.SetDefault("ASSIGNMENT_TTL_MS", 1200000) // default 1200 secs = 20 mins

	// Assignment distribution default config uses golang channels for this
	// 'all-in-one' matchmaker, where the mmqueue and gsdirector are in the
	// same process.  Example trivial 'pubsub' implementation is also in this
	// file, see switch statement later in this file.
	cfg.SetDefault("ASSIGNMENT_DISTRIBUTION_PATH", "channel")

	// Only used if the ASSIGNMENT_DISTRIBUTION_PATH is set to 'pubsub'
	cfg.SetDefault("GCP_PROJECT_ID", "my-project-id")
	cfg.SetDefault("ASSIGNMENT_TOPIC_ID", "replace-me")

	// Read overrides from env vars
	cfg.AutomaticEnv()

	// Set up structured logging
	// Default logging configuration is json that plays nicely with Google Cloud Run.
	log := logging.NewSharedLogger(cfg)
	logger := log.WithFields(logrus.Fields{"application": "matchmaker"})
	logger.Debugf("%v cycles", cfg.GetInt("NUM_MM_CYCLES"))

	initialTPC := cfg.GetInt64("INITIAL_TPC")
	if initialTPC < 0 || initialTPC > 10000 {
		logger.Errorf("INITIAL_TPC must be between 0 and 10000 (the same limits as the /tpc endpoint); got %d", initialTPC)
		return 1
	}

	// Initialize Metrics
	if cfg.GetBool("OTEL_SIDECAR") {
		meterptr, otelShutdownFunc = metrics.InitializeOtel("matchmaker")
	} else {
		meterptr, otelShutdownFunc = metrics.InitializeOtelWithLocalProm(cfg.GetInt("OTEL_PROM_PORT"), "matchmaker")
	}
	// Flushing is called explicitly during shutdown (before the slow cleanup
	// steps), and deferred in case run returns early.
	flushMetrics := sync.OnceFunc(func() {
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer flushCancel()
		if err := otelShutdownFunc(flushCtx); err != nil {
			logger.Errorf("Failed to flush metrics: %v", err)
		}
	})
	defer flushMetrics()

	// The queue's client has an overall request timeout; the director's does not
	// because InvokeMatchmakingFunctions is a long stream, so its calls are
	// bounded by contexts instead.
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
	directorOmClient := omclient.New(
		cfg,
		log,
		&http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,              // Maximum idle connections to keep open
				MaxIdleConnsPerHost: 100,              // Maximum idle connections per host
				IdleConnTimeout:     90 * time.Second, // How long to keep idle connections open
				DisableKeepAlives:   false,            // Make sure keep-alives are enabled
			},
		},
	)

	// Check once before creating anything that needs cleaning up (like a Pub/Sub
	// subscription); both clients use the same address so one check covers both.
	// Any failure is fatal because a matchmaker that cannot reach om-core cannot
	// do anything useful.
	omCoreAddr := cfg.GetString("OM_CORE_ADDR")
	connCtx, connCancel := context.WithTimeout(ctx, 10*time.Second)
	err := directorOmClient.ValidateConnection(connCtx, omCoreAddr)
	connCancel()
	if err != nil {
		logger.Errorf("Unable to reach om-core at OM_CORE_ADDR=%q: %v. Check that om-core is running and OM_CORE_ADDR is its exact URL (on Cloud Run: the service's https:// URL, and this service's identity needs the Cloud Run Invoker role on om-core).", omCoreAddr, err)
		return 1
	}

	// By default, create assignments channel used to funnel assignments from
	// the director to the game clients.
	var publisher assignmentdistributor.Sender
	var receiver assignmentdistributor.Receiver
	switch cfg.GetString("ASSIGNMENT_DISTRIBUTION_PATH") {
	case "pubsub":
		// NOTE: If using pubsub to send assignments from your director to your
		// matchmaking queue, make sure you have sufficient quota for
		// subscriptions in your GCP project. Each instance of the mmqueue will
		// make a unique topic subscription.
		log.Println("Using Google Cloud Pub/Sub for assignment distribution")

		// On failure the constructors log the error; returning 1 exits through
		// run's deferred cleanup.
		var err error
		publisher, err = assignmentdistributor.NewPubSubPublisher(
			cfg.GetString("GCP_PROJECT_ID"),
			cfg.GetString("ASSIGNMENT_TOPIC_ID"),
			log,
		)
		if err != nil {
			return 1
		}
		// Instantiate the Pub/Sub Receiver
		receiver, err = assignmentdistributor.NewPubSubSubscriber(
			cfg.GetString("GCP_PROJECT_ID"),
			cfg.GetString("ASSIGNMENT_TOPIC_ID"),
			log,
		)
		if err != nil {
			publisher.Stop()
			return 1
		}
	case "channel":
		fallthrough // default is 'channel'
	default:
		log.Info("Using Go channels for assignment distribution")
		assignmentsChan := make(chan *pb.Roster)
		publisher = assignmentdistributor.NewChannelSender(assignmentsChan)
		receiver = assignmentdistributor.NewChannelReceiver(assignmentsChan)
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

	// Initialize the director
	d := &gsdirector.MockDirector{
		OmClient: directorOmClient,
		GSManager: &gsdirector.MockAgonesIntegration{
			Log:          log,
			OtelMeterPtr: meterptr,
		},
		Cfg:                 cfg,
		Log:                 log,
		AssignmentPublisher: publisher,
		OtelMeterPtr:        meterptr,
	}

	// FOR TESTING ONLY
	// Run in-process local mmf server(s). This approach will NOT scale to production workloads
	// and should only be used for local development and testing your matching logic.
	mmfFifo.Host = cfg.GetString("SOLODUEL_ADDR")
	mmfFifo.Port = cfg.GetInt32("SOLODUEL_PORT")
	debugMMFAddr := cfg.GetString("DEBUGMMF_ADDR")
	mmfDebug.Host = debugMMFAddr
	mmfDebug.Port = cfg.GetInt32("DEBUGMMF_PORT")

	activeMMFs := []*pb.MatchmakingFunctionSpec{mmfFifo}
	if debugMMFAddr != "" && debugMMFAddr != "none" {
		activeMMFs = append(activeMMFs, mmfDebug)
	}
	buildSegmentedFleetConfig(activeMMFs)

	var wg sync.WaitGroup

	if mmfFifo.Host == "http://localhost" {
		fifoPort := mmfFifo.Port
		localTestMMF := fifo.NewWithLogger(log)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := mmfserver.StartServerWithContext(ctx, fifoPort, localTestMMF, log); err != nil {
				logger.Errorf("In-process FIFO MMF server on port %d failed: %v", fifoPort, err)
			}
		}()
	}
	if debugMMFAddr == "http://localhost" {
		debugPort := mmfDebug.Port
		localDebugMMF := debug.NewWithLogger(log)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := mmfserver.StartServerWithContext(ctx, debugPort, localDebugMMF, log); err != nil {
				logger.Errorf("In-process DEBUG MMF server on port %d failed: %v", debugPort, err)
			}
		}()
	}

	// Initialize the game server manager used by the director
	err = d.GSManager.Init(fleetConfig, zonePools, gameModesInZone)
	if err != nil {
		log.Errorf("Failure initializing game server manager matchmaking parameters: %v", err)
	}

	// Start the ticket matchmaking queue. This is where tickets get processed and stored in OM2.
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.Run(ctx)
	}()

	// Start the director. This is where profiles get sent to the InvokeMMFs()
	// call, and matches returned. directorDone is closed when the director stops
	// on its own (NUM_MM_CYCLES or NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT),
	// which shuts down the whole process.
	directorDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(directorDone)
		d.Run(ctx)
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

	// Basic default handler that just returns the current matchmaking progress status as a string.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, d.Status())
	})

	// Handler to manually insert 1 test ticket into the queue. Creating a ticket
	// changes state, so only POST is accepted.
	mux.HandleFunc("POST /tickets", func(w http.ResponseWriter, r *http.Request) {
		// Make a channel on which the queue will return its http.Status
		// result.  In a real matchmaker, you need to keep this channel and
		// process the status result in a way that makes sense to send back to
		// your game client.
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
				// from the http.Request 'r' into the Open Match protobuf `ticket`
				// protobuf here.
				//
				// This example uses the provided internal library to generate a simple
				// ticket with a few example attributes for testing.

				// TODO: replace the gameclient.Simple() call with your own code to
				// process the request 'r' into an Open Match *pb.Ticket.
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

	// Start http server so this application can be run on serverless platforms.
	logger.Infof("PORT '%v' detected", cfg.GetString("PORT"))
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
	case <-directorDone:
		logger.Info("Director finished; shutting down")
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
	//  1. Cancel the application context (stops ticket generation, the queue,
	//     and the director, and makes waiting /tickets handlers answer 503).
	//  2. Stop accepting HTTP requests and let in-flight ones finish (1s).
	//  3. Wait for background work to stop (3s).
	//  4. Flush OpenTelemetry metrics (2s).
	//  5. Stop assignment distribution in parallel (3s).
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

	if !waitBounded(3*time.Second, func() {
		var stopWG sync.WaitGroup
		stopWG.Add(2)
		go func() {
			defer stopWG.Done()
			receiver.Stop()
		}()
		go func() {
			defer stopWG.Done()
			publisher.Stop()
		}()
		stopWG.Wait()
	}) {
		logger.Error("Timed out stopping assignment distribution")
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
