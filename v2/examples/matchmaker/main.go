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
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	_ "google.golang.org/protobuf/types/known/wrapperspb"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/internal/extensions"
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
	mmfFifo           = &pb.MatchmakingFunctionSpec{
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
	fleetConfig = map[string]map[string]map[string][]*gsdirector.GameMode{
		"APAC": map[string]map[string][]*gsdirector.GameMode{
			"JP_Tokyo": map[string][]*gsdirector.GameMode{
				"asia-northeast1-a": []*gsdirector.GameMode{soloduel},
			},
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
	cfg.SetDefault("OM_CORE_ADDR", "http://localhost:8080")
	cfg.SetDefault("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 500)
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

	// MMF config
	cfg.SetDefault("SOLODUEL_ADDR", "http://localhost")
	cfg.SetDefault("SOLODUEL_PORT", "")

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
		Cfg:             cfg,
		Log:             log,
		AssignmentsChan: assignmentsChan,
	}

	// Initialize the game server manager used by the director
	mmfFifo.Host = cfg.GetString("SOLODUEL_ADDR")
	mmfFifo.Port = cfg.GetInt32("SOLODUEL_PORT")
	err := d.GSManager.Init(fleetConfig)
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
				return gameclient.Simple(ctx)
			}(r),
		}

		// Write the http.Status code returned by the queue
		w.WriteHeader(<-resultChan)
	})

	// Asynchronous goroutine that loops forever, attempting to queue
	// 'tixPerSec' number of tickets each second. Update the tps by sending an
	// http GET to /tps/<NUM> as per the handler below.
	tpsLogger := logger.WithFields(logrus.Fields{
		"component": "generate_tickets",
	})
	var tpsMutex sync.Mutex
	tixPerSec := 0
	go func() {
		for {

			// Every second we'll update our target tps.  This can be updated
			// concurrently by http requests, so use a mutex.
			tpsMutex.Lock()
			tps := tixPerSec
			tpsMutex.Unlock()

			// Channel where we will put one struct for each ticket we want to create this second.
			tq := make(chan struct{})

			// Use a wait group to wait for the deadline to be reached.
			var wg sync.WaitGroup
			wg.Add(1)

			// Loop for 1 second, making as many tickets as we can, up to the requested TPS.
			tpsLogger.Trace("generating tickets until deadline")
			go func() {
				defer wg.Done()

				// Start a new cycle after 1 second, whether we queued the requested TPS or not.
				deadline := time.NewTimer(1 * time.Second)

				numTixQueued := 0
				for {
					select {
					case _, ok := <-tq:
						if ok {
							// There are still structs in the channel representing tickets we want
							// created.

							rChan := make(chan int)
							q.ClientRequestChan <- &mmqueue.ClientRequest{
								// Make a channel on which the queue will return
								// its http.Status result (not used in this example)
								ResultChan: rChan,
								// Use the provided internal library to generate a simple
								// ticket with a few example attributes for testing.
								Ticket: gameclient.Simple(ctx),
							}

							// TODO: in a real matchmaker, you'd want to do something with the result; here
							// we simply receive it and discard it.
							go func() {
								_ = <-rChan
							}()

							numTixQueued++
						}
					case <-deadline.C:
						// deadline reached; return from this function and call the deferred
						// waitgroup Done(), which signals that we've processed
						// as many tickets as we can this second
						tpsLogger.Tracef("DEADLINE EXCEEDED: %v/%v tps (achieved/requested)", numTixQueued, tps)
						return
					}
				}
			}()

			// Simple goroutine to put one struct in the channel for every ticket
			// we want to generate this second.
			go func() {
				for i := tps; i > 0; i-- {
					tq <- struct{}{}
				}
				close(tq)
			}()

			// Wait for the 1 second deadline.
			wg.Wait()
		}

	}()

	// Handler to update the (best effort) number of test tickets we want to
	// generate and put into the queue per second. If the requested TPS is more
	// than the matchmaker can manage in a second, it will do as many as it
	// can. Set it to 0 to stop automatically creating tickets.
	http.HandleFunc("/tps/{tixPerSec}", func(w http.ResponseWriter, r *http.Request) {

		// Attempt to convert requested TPS string into an integer
		rTPS, err := strconv.Atoi(r.PathValue("tixPerSec"))
		if err != nil {
			// Couldn't convert to int; maybe a typo in the http request?
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Hard cap of 10k per second to protect against mistyped http requests
		if rTPS > 10000 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Update tixPerSec. This is read in the asynch goroutine above, so
		// protect it with a mutex.
		tpsMutex.Lock()
		tixPerSec = rTPS
		tpsMutex.Unlock()

		tpsLogger.Infof("TPS set to %v", rTPS)
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
