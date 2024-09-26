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
	"strings"
	"sync"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
	_ "google.golang.org/protobuf/types/known/wrapperspb"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/internal/gsdirector"
	"open-match.dev/open-match-ecosystem/v2/internal/logging"
	"open-match.dev/open-match-ecosystem/v2/internal/mmqueue"
	"open-match.dev/open-match-ecosystem/v2/internal/mocks/gameclient"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

var (
	statusUpdateMutex sync.RWMutex
	cycleStatus       string
	cfg               = viper.New()
	tickets           sync.Map
)

func main() {

	// Quit this process on the signals sent by kubernetes/knative/Cloud Run/ctrl+c.
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM, syscall.SIGINT)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Read config
	cfg := viper.New()
	cfg.SetDefault("OM_CORE_ADDR", "http://localhost:8080")
	cfg.SetDefault("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 500)
	cfg.SetDefault("MAX_CONCURRENT_TICKET_CREATIONS", 20)
	cfg.SetDefault("PORT", 8081)

	// Connection config.
	cfg.SetDefault("OM_CORE_ADDR", "http://localhost:8080")

	// OM core config that the matchmaker needs to respect
	cfg.SetDefault("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 500)

	// Ticket creation config
	cfg.SetDefault("MAX_CONCURRENT_TICKET_CREATIONS", 3)

	// InvokeMatchmaking Function config
	cfg.SetDefault("NUM_MM_CYCLES", math.MaxInt32)                               // Default is essentially forever
	cfg.SetDefault("NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT", math.MaxInt32) // Exit if consequtive matchmaking cycles come back empty

	// Override these with env vars when doing local development.
	// Suggested values in that case are "text", "debug", and "false",
	// respectively
	cfg.SetDefault("LOGGING_FORMAT", "json")
	cfg.SetDefault("LOGGING_LEVEL", "info")
	cfg.SetDefault("LOG_CALLER", "false")

	// Read overrides from env vars
	cfg.AutomaticEnv()

	// Set up structured logging
	// Default logging configuration is json that plays nicely with Google Cloud Run.
	log := logging.NewSharedLogger(cfg)
	logger := log.WithFields(logrus.Fields{"application": "matchmaker"})
	logger.Debugf("%v cycles", cfg.GetInt("NUM_MM_CYCLES"))

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
		Cfg: cfg,
		Log: log,
	}
	// Initialize the game server manager used by the director
	err := d.GSManager.Init(gsdirector.FleetConfig)
	if err != nil {
		log.Errorf("Failure initializing game server manager matchmaking parameters: %v", err)
	}

	// TODO: move this into the omclient
	//Check connection before spinning everything up.
	buf, err := protojson.Marshal(&pb.CreateTicketRequest{
		Ticket: &pb.Ticket{ExpirationTime: timestamppb.Now()}, // Dummy ticket
	})
	_, err = d.OmClient.Post(ctx, logger, cfg.GetString("OM_CORE_ADDR"), "/", buf)
	if err != nil {
		logger.Errorf("OM Connection test failure: %v", err)
		if strings.Contains(err.Error(), "connect: connection refused") {
			logger.Fatal("Unrecoverable error. Is the OM_CORE_ADDR config set correctly?")
			os.Exit(1)
		}
	}

	// Start the ticket matchmaking queue. This is where tickets get processd and stored in OM2.
	go q.Run(ctx)

	// Start the director. This is where profiles get sent to the InvokeMMFs()
	// call, and matches returned.
	go d.Run(ctx)

	// Basic default handler that just returns the current matchmaking progress status as a string.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {

		// Make a channel on which the queue will return its http.Status result
		resultChan := make(chan int)
		q.ClientRequestChan <- &mmqueue.ClientRequest{
			ResultChan: resultChan,
			Ticket: func(r *http.Request) *pb.Ticket {
				// In a real game client, you'd probably use whatever communication
				// protocol/format (JSON string, binary encoding, a custom format,
				// etc) your game engine or dev kit encourages, and parse that data
				// from the http.Request into the Open Match protobuf `ticket`
				// protobuf here.
				//
				// This example just makes an empty ticket.
				//
				// This ticket can only appear in Pools with a single
				// CreationTimeFilter, as they have no other attributes to
				// match against (CreationTime is created by Open Match upon
				// ticket creation).
				ticket := &pb.Ticket{}
				// TODO: remove this line once the test framework is trued up
				ticket = gameclient.Simple(ctx)
				return ticket
			}(r),
		}

		// Write the http.Status code returned by the queue
		w.WriteHeader(<-resultChan)
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
