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
package mmqueue

import (
	"context"
	"fmt"
	"io"
	"net/http"
	_ "net/http"
	_ "net/http/pprof"
	"sync"
	"sync/atomic"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	"github.com/sirupsen/logrus"

	"github.com/spf13/viper"

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156
	_ "google.golang.org/protobuf/types/known/wrapperspb"
	//soloduelServer "open-match.dev/functions/golang/soloduel"
	//mmf "open-match.dev/mmf/server"
	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

// Mock of the game platform services frontend that handles player matchmaking
// requests, queueing them to be sent to Open Match, and processing them
// asynchronously.
type MatchmakerQueue struct {
	OmClient          *omclient.RestfulOMGrpcClient
	Cfg               *viper.Viper
	Log               *logrus.Logger
	Tickets           sync.Map
	ClientRequestChan chan *ClientRequest
}

type ClientRequest struct {
	Ticket     *pb.Ticket
	ResultChan chan int
}

// Run the matchmaker queue, asynchronously queueing tickets and
// activating them.
func (q *MatchmakerQueue) Run(ctx context.Context) {
	logger := q.Log.WithFields(logrus.Fields{
		"component": "matchmaking_queue",
		"operation": "ticket_creation_loop",
	})

	// Var init
	ticketIdsToActivate := make(chan string)

	// Metric counters
	var numTixCreated atomic.Uint64

	// Control number of concurrent requests by creating ticket creation 'slots'
	slots := make(chan struct{}, q.Cfg.GetInt("MAX_CONCURRENT_TICKET_CREATIONS"))

	// Generate i tickets every second for j seconds
	logger.Debug("Processing queued tickets")
	go func() {

		// Loop forever
		for {
			// Block until one of the MAX_CONCURRENT_TICKET_CREATIONS slots is available
			slots <- struct{}{}

			logger.Trace("waiting for a new ticket to enter the queue")
			// Block until a client request is in the channel
			request := <-q.ClientRequestChan

			// Asynchronous concurrent ticket creation
			go func(request *ClientRequest) {

				// proxyCreateTicket implements retries exp bo + jitter
				id, err := q.proxyCreateTicket(ctx, request.Ticket)
				if err != nil {
					// Either a permanent error, or retries timed out
					logger.Errorf("CreateTicket failed: %v", err)
					request.ResultChan <- http.StatusRequestTimeout
					return
				}

				// Successful ticket creation
				ticketIdsToActivate <- id
				q.Tickets.Store(id, struct{}{})
				numTixCreated.Add(1)
				// TODO: in reality, we should only send StatusOK back to the client once
				// the ticket is successfully activated, but haven't written the code to get
				// activation errors back from Open Match yet
				request.ResultChan <- http.StatusOK

			}(request)

			// Release the MAX_CONCURRENT_TICKET_CREATIONS slot
			<-slots
		}

	}()

	// activate pending tickets.
	go func() {
		for {
			ctx := context.WithValue(ctx, "type", "activate")
			q.OmClient.ActivateTickets(ctx, ticketIdsToActivate)
			time.Sleep(1 * time.Second)
		}
	}()
}

// proxyCreateTicket Example
// Your platform services layer should take the matchmaking request (in this
// example, we assume it is from a game client, but it could come from another
// of your game platform services as well), add attributes your platform
// services have authority over (ex: MMR, ELO, inventory, etc), then call Open
// Match core on the client's behalf to make a matchmaking ticket. In this sense,
// your platform service acts as a 'proxy' for the player's game client from
// the viewpoint of Open Match.
func (q *MatchmakerQueue) proxyCreateTicket(ctx context.Context, ticket *pb.Ticket) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Local var declarations
	var id string
	var err error
	mmr := 0.0

	logger := q.Log.WithFields(logrus.Fields{
		"component": "matchmaking_queue",
		"operation": "proxy_CreateTicket",
	})
	logger.Trace("creating ticket")

	// Here is where your matchmaker would make additional calls to your
	// game platform services to add additional matchmaking attributes.
	// Ex: mmr, err := mmrService.Get(ticket.Id)
	if ticket.GetAttributes() == nil {
		ticket.Attributes = &pb.Ticket_FilterableData{}
	}
	if ticket.GetAttributes().GetDoubleArgs() == nil {
		ticket.Attributes.DoubleArgs = make(map[string]float64)
	}
	ticket.Attributes.DoubleArgs["mmr"] = mmr

	// With all matchmaking attributes collected from the client and the
	// game backend services, we're now ready to put the ticket in open match.
	err = backoff.RetryNotify(
		func() error {
			// The API call we want to retry with exponential backoff and jitter
			id, err = q.OmClient.CreateTicket(ctx, ticket)
			return err
		},
		// TODO: expose max elapsed time as a config parameter
		backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(5*time.Second)),
		func(err error, bo time.Duration) {
			logger.Errorf("CreateTicket temporary failure (backoff for %v): %v", err, bo)
		},
	)

	if err == nil {
		// Log successful ticket creation
		logger.Debugf("CreateTicket %v successful", id)
	}
	return id, err
}

// readAllBody is a simple helper function to make sure an HTTP body is completely read and closed.
func readAllBody(resp http.Response, logger *logrus.Entry) ([]byte, error) {
	// Get results
	body, err := io.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		logger.Errorf("cannot read bytes from http response body")
		return nil, err
	}

	return body, err
}

func syncMapDump(sm *sync.Map) map[string]interface{} {
	out := map[string]interface{}{}
	sm.Range(func(key, value interface{}) bool {
		out[fmt.Sprint(key)] = value
		return true
	})
	return out
}
