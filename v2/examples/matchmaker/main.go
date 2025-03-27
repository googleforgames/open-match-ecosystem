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
// NOTE: WIP, this is a testbed right now.
package main

import (
	"context"
	"math"
	"net/http"
	_ "net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel/metric"

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156

	"google.golang.org/protobuf/types/known/anypb"
	_ "google.golang.org/protobuf/types/known/wrapperspb"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/examples/mmf/functions/fifo"
	mmfserver "open-match.dev/open-match-ecosystem/v2/examples/mmf/server"
	"open-match.dev/open-match-ecosystem/v2/internal/extensions"
	"open-match.dev/open-match-ecosystem/v2/internal/gsdirector"
	"open-match.dev/open-match-ecosystem/v2/internal/logging"
	"open-match.dev/open-match-ecosystem/v2/internal/metrics"
	"open-match.dev/open-match-ecosystem/v2/internal/mmqueue"
	"open-match.dev/open-match-ecosystem/v2/internal/mocks/gameclient"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

var (
	// global vars
	statusUpdateMutex sync.RWMutex
	cycleStatus       string
	cfg               = viper.New()
	tickets           sync.Map
	otelShutdownFunc  func(context.Context) error
	meterptr          *metric.Meter

	// configuration that requires recompilation
	mockClientTicket = gameclient.Simple
	mmfFifo          = &pb.MatchmakingFunctionSpec{
		Name: "FIFO",
		Type: pb.MatchmakingFunctionSpec_GRPC,
	}
	backfillMMFs, _ = anypb.New(&pb.MmfRequest{Mmfs: []*pb.MatchmakingFunctionSpec{mmfFifo}})
	soloduel        = &gsdirector.GameMode{
		Name:  "SoloDuel",
		MMFs:  []*pb.MatchmakingFunctionSpec{mmfFifo},
		Pools: map[string]*pb.Pool{"all": gsdirector.EveryTicket},
		ExtensionParams: extensions.Combine(extensions.AnypbIntMap(map[string]int32{
			"desiredNumRosters": 1,
			"desiredRosterLen":  4,
			"minRosterLen":      2,
		}), map[string]*anypb.Any{
			extensions.MMFRequestKey: backfillMMFs,
		}),
	}
	gameModesInZone = map[string][]*gsdirector.GameMode{"asia-northeast1-a": []*gsdirector.GameMode{soloduel}}
	zonePools       = map[string]*pb.Pool{
		"asia-northeast1-a": &pb.Pool{
			DoubleRangeFilters: []*pb.Pool_DoubleRangeFilter{
				&pb.Pool_DoubleRangeFilter{
					DoubleArg: "ping.asia-northeast1-a",
					Minimum:   1,
					Maximum:   120,
				},
			},
		},
	}
	fleetConfig = map[string]map[string]string{
		"APAC": map[string]string{
			"JP_Tokyo": "asia-northeast1-a",
		},
	}
)

func main() {

	// Quit this process on the signals sent by kubernetes/knative/Cloud Run/ctrl+c.
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM, syscall.SIGINT)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Read config
	cfg := viper.New()
	cfg.SetDefault("PORT", 8081)

	// Connection config.
	cfg.SetDefault("OM_CORE_ADDR", "http://localhost:8080")

	// OM core config that the matchmaker needs to respect
	cfg.SetDefault("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 500)

	// Ticket creation config
	cfg.SetDefault("MAX_CONCURRENT_TICKET_CREATIONS", 3)
	cfg.SetDefault("INITIAL_TPC", 10)
	cfg.SetDefault("TICKET_CREATION_CYCLE_DURATION_MS", 1000)
	cfg.SetDefault("TICKET_CREATION_CYCLES", math.MaxInt32)

	// InvokeMatchmaking Function config
	cfg.SetDefault("NUM_MM_CYCLES", math.MaxInt32)                               // math.MaxInt32 seconds is essentially forever
	cfg.SetDefault("MM_CYCLE_MIN_DURATION_MS", 5000)                             // Director will sleep if the invokeMMFs didn't take at least this long.
	cfg.SetDefault("NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT", math.MaxInt32) // Exit if consequtive matchmaking cycles come back empty

	// MMF config
	cfg.SetDefault("SOLODUEL_ADDR", "http://localhost")
	cfg.SetDefault("SOLODUEL_PORT", 50080)

	// Assignment config
	// Set this duration to slightly longer than your configured
	// OM_CACHE_TICKET_TTL_MS and OM_CACHE_ASSIGNMENT_ADDITIONAL_TTL_MS config
	// vars in om-core, added together. If you haven't manually set those
	// config vars, you can see the default values in the om-core repository's
	// 'internal/config/config.go' file.
	cfg.SetDefault("ASSIGNMENT_TTL_MS", 1200000) // default 1200 secs = 10 mins

	// Override these with env vars when doing local development.
	// Suggested values in that case are "text", "debug", and "false",
	// respectively
	cfg.SetDefault("LOGGING_FORMAT", "json")
	cfg.SetDefault("LOGGING_LEVEL", "info")
	cfg.SetDefault("LOG_CALLER", "false")

	// OpenTelemetry metrics config
	cfg.SetDefault("OTEL_SIDECAR", "true")
	cfg.SetDefault("OTEL_PROM_PORT", "2227")

	// Read overrides from env vars
	cfg.AutomaticEnv()

	// Set up structured logging
	// Default logging configuration is json that plays nicely with Google Cloud Run.
	log := logging.NewSharedLogger(cfg)
	logger := log.WithFields(logrus.Fields{"application": "matchmaker"})
	logger.Debugf("%v cycles", cfg.GetInt("NUM_MM_CYCLES"))

	// Initialize Metrics
	if cfg.GetBool("OTEL_SIDECAR") {
		meterptr, otelShutdownFunc = metrics.InitializeOtel()
	} else {
		meterptr, otelShutdownFunc = metrics.InitializeOtelWithLocalProm(cfg.GetInt("OTEL_PROM_PORT"))
	}
	defer otelShutdownFunc(ctx) //nolint:errcheck

	// Create assignments channel used to funnel assignments from the director to the game clients.
	assignmentsChan := make(chan *pb.Roster)

	// Initialize the queue
	q := &mmqueue.MatchmakerQueue{
		OmClient: &omclient.RestfulOMGrpcClient{
			Client: &http.Client{},
			Log:    log,
			Cfg:    cfg,
		},
		Cfg:               cfg,
		Log:               log,
		ClientRequestChan: make(chan *mmqueue.ClientRequest),
		AssignmentsChan:   assignmentsChan,
		OtelMeterPtr:      meterptr,
	}

	// Initialize the director
	d := &gsdirector.MockDirector{
		OmClient: &omclient.RestfulOMGrpcClient{
			Client: &http.Client{},
			Log:    log,
			Cfg:    cfg,
		},
		GSManager: &gsdirector.MockAgonesIntegration{
			Log:          log,
			OtelMeterPtr: meterptr,
		},
		Cfg:             cfg,
		Log:             log,
		AssignmentsChan: assignmentsChan,
		OtelMeterPtr:    meterptr,
	}

	// Configure the mmf server
	mmfFifo.Host = cfg.GetString("SOLODUEL_ADDR")
	mmfFifo.Port = cfg.GetInt32("SOLODUEL_PORT")

	// Initialize the game server manager used by the director
	err := d.GSManager.Init(fleetConfig, zonePools, gameModesInZone)
	if err != nil {
		log.Errorf("Failure initializing game server manager matchmaking parameters: %v", err)
	}

	// FOR TESTING ONLY
	// Run an in-process local mmf server. This approach will NOT scale to production workloads
	// and should only be used for local development and testing your matching logic.
	if cfg.GetString("SOLODUEL_ADDR") == "http://localhost" {
		localTestMMF := fifo.NewWithLogger(log)
		go mmfserver.StartServer(cfg.GetInt32("SOLODUEL_PORT"), localTestMMF, log)
	}

	//Check connection before spinning up the matchmaking queue
	err = q.OmClient.ValidateConnection(ctx, cfg.GetString("OM_CORE_ADDR"))
	if err != nil {
		logger.Errorf("OM Connection test failure: %v", err)
		if strings.Contains(err.Error(), "connect: connection refused") {
			logger.Fatal("Unrecoverable error. Is the OM_CORE_ADDR config set correctly?")
			os.Exit(1)
		}
	}
	// Start the ticket matchmaking queue. This is where tickets get processd and stored in OM2.
	go q.Run(ctx)

	//Check connection before spinning up the game server director
	err = d.OmClient.ValidateConnection(ctx, cfg.GetString("OM_CORE_ADDR"))
	if err != nil {
		logger.Errorf("OM Connection test failure: %v", err)
		if strings.Contains(err.Error(), "connect: connection refused") {
			logger.Fatal("Unrecoverable error. Is the OM_CORE_ADDR config set correctly?")
			os.Exit(1)
		}
	}
	// Start the director. This is where profiles get sent to the InvokeMMFs()
	// call, and matches returned.
	go d.Run(ctx)

	// Handler to manually insert 1 test ticket into the queue.
	http.HandleFunc("/tickets", func(w http.ResponseWriter, r *http.Request) {

		// Make a channel on which the queue will return its http.Status
		// result.  In a real matchmaker, you need to keep this channel and
		// process the status result in a way that makes sense to send back to
		// your game client.
		resultChan := make(chan int)
		q.ClientRequestChan <- &mmqueue.ClientRequest{
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
				return mockClientTicket(ctx)
			}(r),
		}

		// Write the http.Status code returned by the queue
		w.WriteHeader(<-resultChan)
	})

	// FOR TESTING ONLY
	// Asynchronous goroutine that loops TICKET_CREATION_CYCLES number of
	// times, attempting to queue the given number of tickets each cycle.
	// Update the tpc by sending an http GET to /tpc/<NUM> as per the handler
	// below.
	q.SetTestTicketConfig(cfg.GetInt64("INITIAL_TPC"), mockClientTicket)
	go q.GenerateTestTickets(ctx)

	// Handler to update the (best effort) number of test tickets we want to
	// generate and put into the queue per second. If the requested TPC is more
	// than the matchmaker can manage in a second, it will do as many as it
	// can. Set it to 0 to stop automatically creating tickets.
	http.HandleFunc("/tpc/{tixPerCycle}", func(w http.ResponseWriter, r *http.Request) {

		// Attempt to convert requested TPC string into an int64
		rTPC, err := strconv.ParseInt(r.PathValue("tixPerCycle"), 10, 64)
		if err != nil {
			// Couldn't convert to int; maybe a typo in the http request?
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Hard cap of 10k per second to protect against mistyped http requests
		if rTPC > 10000 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Update tixPerSec number, and generate tickets using the gameclient.Simple function.
		q.SetTestTicketConfig(rTPC, mockClientTicket)

		logger.Infof("TPC set to %v", rTPC)
		w.WriteHeader(http.StatusOK)
	})

	// Start http server so this application can be run on serverless platforms.
	logger.Infof("PORT '%v' detected", cfg.GetString("PORT"))
	srv := &http.Server{Addr: ":" + cfg.GetString("PORT")}
	go func() {
		// ErrServerClosed is the error returned by http.Server on graceful exit.
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			logger.Fatal(err)
		}
	}()

	// Wait for quit signal
	<-signalChan
	if err := srv.Shutdown(ctx); err != nil {
		logger.Fatal(err)
	}
	logger.Info("Exiting...")
}
