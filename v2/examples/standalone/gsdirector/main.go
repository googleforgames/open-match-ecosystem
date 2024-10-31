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

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156

	_ "google.golang.org/protobuf/types/known/wrapperspb"

	"open-match.dev/open-match-ecosystem/v2/internal/gsdirector"
	"open-match.dev/open-match-ecosystem/v2/internal/logging"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

var (
	statusUpdateMutex sync.RWMutex
	cycleStatus       string
	cfg               = viper.New()
	tickets           sync.Map
)

func main() {

	// Connection config.
	cfg.SetDefault("OM_CORE_ADDR", "http://localhost:8080")
	cfg.SetDefault("SOLODUEL_ADDR", "http://localhost")
	cfg.SetDefault("PORT", "8090")

	// OM core config that the matchmaker needs to respect
	cfg.SetDefault("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 500)

	// InvokeMatchmaking Function config
	cfg.SetDefault("NUM_MM_CYCLES", math.MaxInt32)                   // Default is essentially forever
	cfg.SetDefault("NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT", 3) // Exit if 3 matchmaking cycles come back empty

	// Override these with env vars when doing local development.
	// Suggested values in that case are "text", "debug", and "false",
	// respectively
	cfg.SetDefault("LOGGING_FORMAT", "json")
	cfg.SetDefault("LOGGING_LEVEL", "info")
	cfg.SetDefault("LOG_CALLER", "false")

	// Metrics config
	cfg.SetDefault("OTEL_SIDECAR", "false")

	// Read overrides from env vars
	cfg.AutomaticEnv()

	// Set up structured logging
	// Default logging configuration is json that plays nicely with Google Cloud Run.
	log := logging.NewSharedLogger(cfg)
	logger := log.WithFields(logrus.Fields{"component": "game_server_director"})
	logger.Debugf("%v cycles", cfg.GetInt("NUM_MM_CYCLES"))

	// Connect to om-core server
	ctx := context.Background()

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
		Cfg: cfg,
		Log: log,
	}
	err := d.GSManager.Init(gsdirector.FleetConfig)
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

	// Quit this process on the signals sent by kubernetes/knative/Cloud Run.
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM, syscall.SIGINT)

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
	if err := srv.Shutdown(ctx); err != nil {
		logger.Fatal(err)
	}
	logger.Info("Exiting...")
}
