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
//   - Stores the ClientID to TicketID mapping. This mapping can be kept local
//     to this process (not scalable, not persistent, not highly available), but
//     it is recommended that it is stored in a player status service instead.
//   - POST batches of TicketIDs to the /tickeds:activate endpoint to mark those
//     tickets as available for matching.
//

package main

import (
	"context"
	"net/http"
	_ "net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel/metric"

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156

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
	// different assignment return data flow.  We sugest using a distributed
	// message bus, pub/sub system, or your platform service's notification
	// service when returning assignments.
	cfg.SetDefault("ASSIGNMENT_DISTRIBUTION_PATH", "pubsub")
	cfg.SetDefault("GCP_PROJECT_ID", "replace_me")
	cfg.SetDefault("ASSIGNMENT_TOPIC_ID", "replace_me")

	// Read overrides from env vars
	cfg.AutomaticEnv()

	// initialize shared structured logging
	log := logging.NewSharedLogger(cfg)
	logger := log.WithFields(logrus.Fields{"component": "matchmaking_queue"})

	// Initialize Metrics
	if cfg.GetBool("OTEL_SIDECAR") {
		meterptr, otelShutdownFunc = metrics.InitializeOtel()
	} else {
		meterptr, otelShutdownFunc = metrics.InitializeOtelWithLocalProm(cfg.GetInt("OTEL_PROM_PORT"))
	}
	defer otelShutdownFunc(ctx) //nolint:errcheck

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

		// Instantiate the Pub/Sub receiver
		receiver = assignmentdistributor.NewPubSubSubscriber(
			cfg.GetString("GCP_PROJECT_ID"),
			cfg.GetString("ASSIGNMENT_TOPIC_ID"),
			log,
		)
	}

	// Initialize the queue
	q := &mmqueue.MatchmakerQueue{
		OmClient: &omclient.RestfulOMGrpcClient{
			Client: &http.Client{},
			Log:    log,
			Cfg:    cfg,
		},
		Cfg:                cfg,
		Log:                log,
		ClientRequestChan:  make(chan *mmqueue.ClientRequest),
		AssignmentReceiver: receiver,
		OtelMeterPtr:       meterptr,
	}

	//Check connection before spinning everything up.
	err := q.OmClient.ValidateConnection(ctx, cfg.GetString("OM_CORE_ADDR"))
	if err != nil {
		logger.Errorf("OM Connection test failure: %v", err)
		if strings.Contains(err.Error(), "connect: connection refused") {
			logger.Fatal("Unrecoverable error. Is the OM_CORE_ADDR config set correctly?")
			os.Exit(1)
		}
	}
	// Start the queue. This is where requests are processed and added to Open Match.
	go q.Run(ctx)

	// Handler for client matchmaking requests
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

	// Start http server.
	logger.Infof("PORT %v detected", cfg.GetString("PORT"))
	srv := &http.Server{Addr: ":" + cfg.GetString("PORT")}
	go func() {
		// ErrServerClosed is the error returned by http.Server on graceful exit.
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			logger.Fatal(err)
		}
	}()

	// Wait for quit signal
	<-signalChan
	receiver.Stop()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Fatal(err)
	}
	logger.Info("Exiting...")
}
