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
	"io"
	"math"
	"net/http"
	_ "net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel/metric"

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156

	_ "google.golang.org/protobuf/types/known/wrapperspb"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/internal/assignmentdistributor"
	"open-match.dev/open-match-ecosystem/v2/internal/gsdirector"
	"open-match.dev/open-match-ecosystem/v2/internal/logging"
	"open-match.dev/open-match-ecosystem/v2/internal/metrics"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

var (
	statusUpdateMutex sync.RWMutex
	cycleStatus       string
	cfg               = viper.New()
	tickets           sync.Map
	meterptr          *metric.Meter
	otelShutdownFunc  func(context.Context) error
)

func main() {

	// Quit this process on the signals sent by kubernetes/knative/Cloud Run/ctrl+c.
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM, syscall.SIGINT)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Connection config.
	cfg.SetDefault("OM_CORE_ADDR", "http://localhost:8080")
	cfg.SetDefault("PORT", "8090")

	// OM core config that the matchmaker needs to respect
	cfg.SetDefault("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 500)

	// InvokeMatchmaking Function config
	// In production, you will want this to be functionally infinite, thus the
	// use of the largest Int32 number possible.  However, when doing local
	// development of matching logic, it is often useful to run a deterministic
	// number of cycles.
	cfg.SetDefault("NUM_MM_CYCLES", math.MaxInt32)
	// Exit if 3 matchmaking cycles come back empty. Again, you probably don't
	// want to do this in production, but it can be very useful in local
	// testing.
	cfg.SetDefault("NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT", math.MaxInt32)

	// Override these with env vars when doing local development.
	// Suggested values in that case are "text", "debug", and "false",
	// respectively
	cfg.SetDefault("LOGGING_FORMAT", "json")
	cfg.SetDefault("LOGGING_LEVEL", "info")
	cfg.SetDefault("LOG_CALLER", "false")

	// Metrics config
	cfg.SetDefault("OTEL_SIDECAR", "true")
	cfg.SetDefault("OTEL_PROM_PORT", 2225)

	// Assignment distribution config
	// Returning assignments via channel only works if the mmqueue and the
	// gsdirector are running in the same process, so you'll need to provide a
	// different assignment return data flow.  We sugest using a distributed
	// message bus, pub/sub system, or your platform service's notification
	// service when returning assignments.
	cfg.SetDefault("ASSIGNMENT_DISTRIBUTION_PATH", "")
	cfg.SetDefault("GCP_PROJECT_ID", "replace_me")
	cfg.SetDefault("ASSIGNMENT_TOPIC_ID", "replace_me")

	// Read overrides from env vars
	cfg.AutomaticEnv()

	// Set up structured logging
	// Default logging configuration is json that plays nicely with Google Cloud Run.
	log := logging.NewSharedLogger(cfg)
	logger := log.WithFields(logrus.Fields{"component": "game_server_director"})
	logger.Debugf("%v cycles", cfg.GetInt("NUM_MM_CYCLES"))

	// Initialize Metrics
	if cfg.GetBool("OTEL_SIDECAR") {
		meterptr, otelShutdownFunc = metrics.InitializeOtel()
	} else {
		meterptr, otelShutdownFunc = metrics.InitializeOtelWithLocalProm(cfg.GetInt("OTEL_PROM_PORT"))
	}
	defer otelShutdownFunc(ctx) //nolint:errcheck

	// Initialize assignment distribution
	var publisher assignmentdistributor.Sender
	switch cfg.GetString("ASSIGNMENT_DISTRIBUTION_PATH") {
	case "channel":
		log.Info("Using Go channels for assignment distribution")
		log.Error("Using Go channels for assignment distribution isn't possible unless the matchmaking queue and game server director are running in the same process, which should only be done when doing active local development. Since this is a standalone game server director process, the go channel will never be read, so assignments are effectively discarded with this configuration.")
		assignmentsChan := make(chan *pb.Roster)
		publisher = assignmentdistributor.NewChannelSender(assignmentsChan)
	case "pubsub":
		fallthrough // default is 'pubsub' in the standalone mmqueue
	default:
		// note: if using pubsub to send assignments from your director to your
		// matchmaking queue, make sure you have sufficient quota for
		// subscriptions in your GCP project. Each instance of the mmqueue will
		// make a unique topic subscription.
		log.Println("Using Google Cloud Pub/Sub for assignment distribution")

		// Instantiate the Pub/Sub receiver
		publisher = assignmentdistributor.NewPubSubPublisher(
			cfg.GetString("GCP_PROJECT_ID"),
			cfg.GetString("ASSIGNMENT_TOPIC_ID"),
			log,
		)
	}

	// Initialize the director
	d := &gsdirector.MockDirector{
		OmClient: &omclient.RestfulOMGrpcClient{
			Client: &http.Client{},
			Log:    log,
			Cfg:    cfg,
		},
		GSManager: &gsdirector.MockAgonesIntegration{
			Log: log,
		},
		Cfg:                 cfg,
		Log:                 log,
		AssignmentPublisher: publisher,
		OtelMeterPtr:        meterptr,
	}
	// In a real stand-alone director, you would read in your actual game server configuration and construct
	// the FleetConfig, ZonePools, and GameModesInZone. For automated testing, we use the sample ones
	// defined in the internal/gsdirector module.
	err := d.GSManager.Init(gsdirector.FleetConfig, gsdirector.ZonePools, gsdirector.GameModesInZone)
	if err != nil {
		log.Errorf("Failure initializing game server manager matchmaking parameters: %v", err)
	}

	//Check connection before spinning everything up.
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

	// Basic default handler that just returns the current matchmaking progress status as a string.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		statusUpdateMutex.RLock()
		defer statusUpdateMutex.RUnlock()
		io.WriteString(w, cycleStatus)
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
	publisher.Stop()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Fatal(err)
	}
	logger.Info("Exiting...")
}
