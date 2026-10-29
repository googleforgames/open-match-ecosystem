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
// Standalone game server director: asks a (mock) game server manager what
// matches it needs, invokes matchmaking functions through om-core, allocates
// servers for the accepted matches, and publishes the assignments (to Pub/Sub
// by default) for the mmqueue to hand back to players. Run a single instance of
// it (or one per partition of your fleet).
//
// To run it locally (listens on PORT 8083 by default; see README.md):
//
//	LOGGING_FORMAT=text LOGGING_LEVEL=debug OTEL_SIDECAR=false \
//	  GCP_PROJECT_ID=<your-project-id> ASSIGNMENT_TOPIC_ID=<your-topic-id> go run .
package main

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
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

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/internal/assignmentdistributor"
	"open-match.dev/open-match-ecosystem/v2/internal/gsdirector"
	"open-match.dev/open-match-ecosystem/v2/internal/logging"
	"open-match.dev/open-match-ecosystem/v2/internal/metrics"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

var (
	meterptr         *metric.Meter
	otelShutdownFunc func(context.Context) error
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

	cfg := viper.New()

	// Connection config.
	cfg.SetDefault("OM_CORE_ADDR", "http://localhost:8080")
	// Per-attempt timeout for unary om-core calls (ActivateTickets for
	// re-activation, ValidateConnection).
	cfg.SetDefault("OM_CORE_REQUEST_TIMEOUT_MS", 5000)
	// Cloud Run injects PORT; the local default avoids om-core's default 8080.
	cfg.SetDefault("PORT", 8083)
	// SOLODUEL_PORT 50090 matches the standalone mmf's default local PORT.
	cfg.SetDefault("SOLODUEL_ADDR", "http://localhost")
	cfg.SetDefault("SOLODUEL_PORT", 50090)
	// The DEBUG MMF is opt-in; "none" (or empty) disables it.
	cfg.SetDefault("DEBUGMMF_ADDR", "none")
	cfg.SetDefault("DEBUGMMF_PORT", 50081)

	// OM core config that the matchmaker needs to respect.
	// Must be less than or equal to om-core's OM_MAX_STATE_UPDATES_PER_CALL, or
	// om-core rejects every activation call.
	cfg.SetDefault("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 500)
	cfg.SetDefault("OM_CORE_MAX_CONCURRENT_ACTIVATION_CALLS", 10)

	// InvokeMatchmaking Function config
	// In production, you will want this to be functionally infinite, thus the
	// use of the largest Int32 number possible.  However, when doing local
	// development of matching logic, it is often useful to run a deterministic
	// number of cycles. When the director stops, this process shuts down.
	cfg.SetDefault("NUM_MM_CYCLES", math.MaxInt32) // math.MaxInt32 cycles is essentially forever
	cfg.SetDefault("MM_CYCLE_MIN_DURATION_MS", 5000)
	// Director-side deadline for each cycle's InvokeMatchmakingFunctions calls;
	// the default matches om-core's OM_MMF_TIMEOUT_SECS default; 0 means no
	// director-side deadline.
	cfg.SetDefault("MMF_INVOCATION_TIMEOUT_SECS", 600)
	// Exit if this many consecutive matchmaking cycles come back empty. Again,
	// you probably don't want to do this in production, but it can be very useful
	// in local testing. The default (math.MaxInt32) never exits.
	cfg.SetDefault("NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT", math.MaxInt32)
	cfg.SetDefault("REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT", false)
	cfg.SetDefault("WAIT_FOR_BACKFILL_MMFS", false)
	// Percentage of proposed matches the director randomly rejects, to exercise
	// the ticket re-activation path; 0 disables it.
	cfg.SetDefault("RANDOM_MATCH_REJECT_PERCENT", 1)

	// Override these with env vars when doing local development.
	// Suggested values in that case are "text", "debug", and "false",
	// respectively
	cfg.SetDefault("LOGGING_FORMAT", "json")
	cfg.SetDefault("LOGGING_LEVEL", "info")
	cfg.SetDefault("LOG_CALLER", "false")

	// Metrics config
	cfg.SetDefault("OTEL_SIDECAR", "true")
	cfg.SetDefault("OTEL_PROM_PORT", 2225)

	// How long the director remembers a ticket it has already assigned; the
	// director uses this map to reject proposals that contain an already-assigned
	// ticket while om-core is still replicating its deactivation. Keep it at
	// least as long as om-core's OM_CACHE_TICKET_TTL_MS (default 10 mins).
	cfg.SetDefault("ASSIGNMENT_TTL_MS", 1200000) // default 1200 secs = 20 mins

	// Assignment distribution config
	// Returning assignments via channel only works if the mmqueue and the
	// gsdirector are running in the same process, so you'll need to provide a
	// different assignment return data flow. We suggest using a distributed
	// message bus, pub/sub system, or your platform service's notification
	// service when returning assignments. Any value other than "channel"
	// (including the empty default) uses Pub/Sub.
	cfg.SetDefault("ASSIGNMENT_DISTRIBUTION_PATH", "")
	cfg.SetDefault("GCP_PROJECT_ID", "my-project-id")
	// The Pub/Sub TOPIC to publish assignments to; it must already exist.
	cfg.SetDefault("ASSIGNMENT_TOPIC_ID", "replace-me")

	// Read overrides from env vars
	cfg.AutomaticEnv()

	// Set up structured logging
	// Default logging configuration is json that plays nicely with Google Cloud Run.
	log := logging.NewSharedLogger(cfg)
	logger := log.WithFields(logrus.Fields{"component": "game_server_director"})
	logger.Debugf("%v cycles", cfg.GetInt("NUM_MM_CYCLES"))

	// Initialize Metrics
	if cfg.GetBool("OTEL_SIDECAR") {
		meterptr, otelShutdownFunc = metrics.InitializeOtel("gsdirector")
	} else {
		meterptr, otelShutdownFunc = metrics.InitializeOtelWithLocalProm(cfg.GetInt("OTEL_PROM_PORT"), "gsdirector")
	}
	// Flushing is called explicitly during shutdown (before the Pub/Sub
	// cleanup), and deferred in case run returns early.
	flushMetrics := sync.OnceFunc(func() {
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer flushCancel()
		if err := otelShutdownFunc(flushCtx); err != nil {
			logger.Errorf("Failed to flush metrics: %v", err)
		}
	})
	defer flushMetrics()

	// The director's client has no overall request timeout because
	// InvokeMatchmakingFunctions is a long stream, so its calls are bounded by
	// contexts instead.
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

	// Check once before creating anything that needs cleaning up. Any failure is
	// fatal because a director that cannot reach om-core cannot do anything
	// useful.
	omCoreAddr := cfg.GetString("OM_CORE_ADDR")
	connCtx, connCancel := context.WithTimeout(ctx, 10*time.Second)
	err := directorOmClient.ValidateConnection(connCtx, omCoreAddr)
	connCancel()
	if err != nil {
		logger.Errorf("Unable to reach om-core at OM_CORE_ADDR=%q: %v. Check that om-core is running and OM_CORE_ADDR is its exact URL (on Cloud Run: the service's https:// URL, and this service's identity needs the Cloud Run Invoker role on om-core).", omCoreAddr, err)
		return 1
	}

	// Initialize assignment distribution
	var publisher assignmentdistributor.Sender
	switch cfg.GetString("ASSIGNMENT_DISTRIBUTION_PATH") {
	case "channel":
		log.Info("Using Go channels for assignment distribution")
		log.Error("Using Go channels for assignment distribution isn't possible unless the matchmaking queue and game server director are running in the same process, which should only be done when doing active local development. Since this is a standalone game server director process, the go channel will never be read, so assignments are effectively discarded with this configuration.")
		assignmentsChan := make(chan *pb.Roster)
		publisher = assignmentdistributor.NewChannelSender(assignmentsChan)
	case "pubsub":
		fallthrough // default is 'pubsub' in the standalone gsdirector
	default:
		// note: if using pubsub to send assignments from your director to your
		// matchmaking queue, make sure you have sufficient quota for
		// subscriptions in your GCP project. Each instance of the mmqueue will
		// make a unique topic subscription.
		log.Println("Using Google Cloud Pub/Sub for assignment distribution")

		// On failure the constructor logs the error; returning 1 exits through
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

	gsdirector.Fifo.Host = cfg.GetString("SOLODUEL_ADDR")
	gsdirector.Fifo.Port = cfg.GetInt32("SOLODUEL_PORT")
	gsdirector.SoloDuel.MMFs = []*pb.MatchmakingFunctionSpec{gsdirector.Fifo}
	if debugMMFAddr := cfg.GetString("DEBUGMMF_ADDR"); debugMMFAddr != "" && debugMMFAddr != "none" {
		gsdirector.SoloDuel.MMFs = append(gsdirector.SoloDuel.MMFs, &pb.MatchmakingFunctionSpec{
			Name: "DEBUG",
			Host: debugMMFAddr,
			Port: cfg.GetInt32("DEBUGMMF_PORT"),
			Type: pb.MatchmakingFunctionSpec_GRPC,
		})
	}
	if err := gsdirector.SoloDuel.SetBackfillMMFs(gsdirector.Fifo); err != nil {
		log.Errorf("Failure setting backfill MMFs on soloduel game mode: %v", err)
	}

	// In a real stand-alone director, you would read in your actual game server configuration and construct
	// the FleetConfig, ZonePools, and GameModesInZone. For automated testing, we use the sample ones
	// defined in the internal/gsdirector module.
	err = d.GSManager.Init(gsdirector.FleetConfig, gsdirector.ZonePools, gsdirector.GameModesInZone)
	if err != nil {
		log.Errorf("Failure initializing game server manager matchmaking parameters: %v", err)
	}

	// Start the director. This is where profiles get sent to the InvokeMMFs()
	// call, and matches returned. directorDone is closed when the director stops
	// on its own (NUM_MM_CYCLES or NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT),
	// which shuts down the whole process.
	directorDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(directorDone)
		d.Run(ctx)
	}()

	// Each server gets its own ServeMux, so the app endpoints are only served on
	// PORT (and /metrics only on the Prometheus port).
	mux := http.NewServeMux()

	// Basic default handler that just returns the current matchmaking progress status as a string.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, d.Status())
	})

	// Profiling endpoints are mounted explicitly on this server's mux. The
	// example deployment only allows internal, authenticated Cloud Run traffic;
	// do not expose these on an internet-facing port.
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
	//  1. Cancel the context, stopping the director.
	//  2. Stop accepting HTTP requests (1s).
	//  3. Wait for the director to stop (3s).
	//  4. Flush metrics (2s).
	//  5. Stop the assignment publisher, which flushes pending messages (3s).
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 1*time.Second)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Errorf("HTTP server shutdown: %v", err)
	}
	shutdownCancel()

	if !waitBounded(3*time.Second, wg.Wait) {
		logger.Error("Timed out waiting for the director to stop")
	}

	flushMetrics()

	if !waitBounded(3*time.Second, publisher.Stop) {
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
