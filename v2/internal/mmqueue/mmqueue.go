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
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/metric"

	"github.com/spf13/viper"

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156
	_ "google.golang.org/protobuf/types/known/wrapperspb"
	//soloduelServer "open-match.dev/functions/golang/soloduel"
	//mmf "open-match.dev/mmf/server"
	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

var (
	tpsMutex  sync.Mutex
	newTicket func(context.Context) *pb.Ticket
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
	AssignmentsChan   chan *pb.Roster
	OtelMeterPtr      *metric.Meter
	TPS               int64
}

type ClientRequest struct {
	Ticket     *pb.Ticket
	ResultChan chan int
}

// Run the matchmaker queue, asynchronously queueing tickets and
// activating them.
func (q *MatchmakerQueue) Run(ctx context.Context) {
	logger := q.Log.WithFields(logrus.Fields{
		"app":       "matchmaker",
		"component": "queue",
	})

	// Var init
	var wg sync.WaitGroup
	ticketIdsToActivate := make(chan string, 10000)

	// Initialize metrics
	if q.OtelMeterPtr != nil {
		logger.Tracef("Initializing otel metris")
		registerMetrics(q.OtelMeterPtr)
	}
	// This inline function is a simple callback that open telemetry uses to read
	// the current TPS
	meter := *q.OtelMeterPtr
	_, err := meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			// TPS can be updated asynchronously, so we protect reading it with a lock.
			tpsMutex.Lock()
			o.ObserveInt64(otelTicketsGeneratedPerSecond, q.TPS)
			tpsMutex.Unlock()
			return nil
		},
		otelTicketsGeneratedPerSecond,
	)
	if err != nil {
		logger.Fatalf("Failed to set up tps gauge: %v", err)
	}

	// Control number of concurrent requests by creating ticket creation 'slots'
	slots := make(chan struct{}, q.Cfg.GetInt("MAX_CONCURRENT_TICKET_CREATIONS"))

	// Goroutine to read from the incoming ticket request channel, and create
	// one ticket for each incoming request.  Limit the number of concurrent
	// creations by blocking until one of the MAX_CONCURRENT_TICKET_CREATIONS
	// 'slots' is available.
	go func() {
		cLogger := q.Log.WithFields(logrus.Fields{
			"operation": "ticket_creation",
		})
		cLogger.Debug("Processing queued ticket creation requests")

		// Don't exit the Run() function as long as this goroutine is running.
		wg.Add(1)
		defer wg.Done()

		// Loop forever
		for {
			// Block until one of the MAX_CONCURRENT_TICKET_CREATIONS 'slots' is available
			slots <- struct{}{}

			logger.Trace("waiting for a new ticket to enter the queue")
			// Block until a client request is in the channel
			request := <-q.ClientRequestChan

			// Asynchronous concurrent ticket creation
			go func(request *ClientRequest) {
				defer close(request.ResultChan)

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
				otelTicketCreations.Add(ctx, 1)

				// TODO: in reality, we should only send StatusOK back to the client once
				// the ticket is successfully activated, but haven't written the code to get
				// activation errors back from Open Match yet
				request.ResultChan <- http.StatusOK

			}(request)

			// Release the MAX_CONCURRENT_TICKET_CREATIONS slot
			<-slots
		}

	}()

	// goroutine to activate pending tickets.
	go func() {
		aLogger := q.Log.WithFields(logrus.Fields{
			"operation": "ticket_activation",
		})
		aLogger.Debug("activating created tickets")

		// Don't exit the Run() function as long as this goroutine is running.
		wg.Add(1)
		defer wg.Done()

		// Make a derived context with the activationType 'activate', which is used
		// by the omclient to distinguish between logs for initial ticket activation
		// and re-activation after a failed matching attempt.
		ctx := context.WithValue(ctx, "activationType", "activate")
		//aLogger := q.Log.WithFields(logrus.Fields{
		//	"component": "matchmaking_queue",
		//	"operation": "ticket_activation",
		//})

		q.OmClient.ActivateTickets(ctx, ticketIdsToActivate)
	}()

	// goroutine to process incoming assignments.
	go func() {
		bLogger := q.Log.WithFields(logrus.Fields{
			"operation": "ticket_assignment",
		})
		logger.Debug("processing incoming ticket assignments")

		// Don't exit the Run() function as long as this goroutine is running.
		wg.Add(1)
		defer wg.Done()

		var assignment string
		for roster := range q.AssignmentsChan {
			// Get the assignment string
			assignment = roster.GetAssignment().GetConnection()

			// Loop through all tickets in the assignment roster
			index := 0
			var ticket *pb.Ticket
			for index, ticket = range roster.GetTickets() {
				// TODO: in reality, this is where your matchmaking queue would
				// return the assignment to the game client. This sample
				// instead just logs the assignment.
				bLogger.Debugf("Received ticket %v assignment: %v", ticket.GetId(), assignment)
				// Stop tracking this ticket; it's matchmaking is complete.
				// Your matchmaker may wish to instead keep this for a time (in
				// case the game client needs to request the same assignment
				// again later).
				q.Tickets.Delete(ticket.GetId())
			}

			// Update metric
			otelTicketAssignments.Add(ctx, int64(index))
		}
	}()

	// Don't exit the Run() function as long as any of the goroutines are
	// running (in normal operation, this is forever).
	wg.Wait()
}

// SetTPS instructs the test function GenerateTestTickets() to make `newTPS`
// tickets per second using `ticketCreationFunc()`. Example mock client ticket
// creations functions can be found in internal/mocks/gameclient
func (q *MatchmakerQueue) SetTestTicketConfig(newTPS int64,
	ticketCreationFunc func(context.Context) *pb.Ticket) {

	// This can be updated at any time by the calling code, so protect it with a mutex
	tpsMutex.Lock()
	q.TPS = newTPS
	newTicket = ticketCreationFunc
	tpsMutex.Unlock()
}

// Test the queue by generating tickets every second.
// This is only suitable for automated testing; in a real matchmaker, the
// server that instantiated the mmqueue will be inserting all the ClientRequest
// objects as client matchmaking requests come in.  You can find example mock client
// ticket creation functions in internal/mocks/gameclient.
// To use this function:
//   - instante your mmqueue object (by convention, in a variable named 'q')
//   - asynchronously run the queue: `go q.Run(ctx)`
//   - to set up ticket generation, send a non-zero TPS & a ticket creation function to the SetTPS function:
//     `q.SetTestTicketConfig(10, <ticket creation function>)`
//   - asynchronously run the test ticket generator: `go q.GenerateTestTickets(ctx)`
//   - to stop generating tickets, send 0 to the SetTPS function:
//     `q.SetTestTicketConfig(0, <ticket creation function>)`
func (q *MatchmakerQueue) GenerateTestTickets(ctx context.Context) {
	tpsLogger := q.Log.WithFields(logrus.Fields{"component": "generate_tickets"})

	for {
		// Channel where we will put one struct for each ticket we want to create this second.
		tq := make(chan struct{})

		// Use a wait group to wait for the 1-second deadline to be reached.
		var wg sync.WaitGroup
		wg.Add(1)

		// Loop for 1 second, making as many tickets as we can, up to the requested TPS.
		tpsLogger.Trace("generating tickets until deadline")
		go func() {
			defer wg.Done()

			// Start a new cycle after 1 second, whether we queued the requested TPS or not.
			deadline := time.NewTimer(1 * time.Second)

			var numTixQueued int64
			for {
				select {
				// Read all the structs put into the channel by the tps goroutine below.
				case _, ok := <-tq:
					if ok {
						// There are still structs in the channel representing tickets we want
						// created.

						// Make a channel on which the queue will return
						// its http.Status result (not used in tests)
						rChan := make(chan int)
						q.ClientRequestChan <- &ClientRequest{
							ResultChan: rChan,
							// Use the provided internal library to generate a simple
							// ticket with a few example attributes for testing.
							Ticket: newTicket(ctx),
						}

						// In tests, we simply discard the ticket creation results.
						go func() {
							_ = <-rChan
						}()

						numTixQueued++
					}
				case <-deadline.C:
					// deadline reached; return from this function and call the deferred
					// waitgroup Done(), which signals that we've processed
					// as many tickets as we can this second
					tpsLogger.Tracef("DEADLINE EXCEEDED: %v/%v tps (achieved/requested)", numTixQueued, q.TPS)
					otelTicketGenerationsAchievedPerSecond.Record(ctx, numTixQueued)
					return
				}
			}
		}()

		// Simple tps goroutine that puts one struct in the channel for every ticket
		// we want to generate this second.
		go func() {
			// TPS can be updated at any time, so protect it with a mutex.
			tpsMutex.Lock()
			thisTPS := q.TPS
			tpsMutex.Unlock()
			for i := thisTPS; i > 0; i-- {
				tq <- struct{}{}
			}
			close(tq)
		}()

		// Wait for ticket creation goroutine to hit the 1 second deadline.
		wg.Wait()
	} // end for loop

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
	ticket.Attributes.DoubleArgs["example_mmr"] = mmr

	// With all matchmaking attributes collected from the client and the
	// game backend services, we're now ready to put the ticket in open match.
	err = backoff.RetryNotify(
		func() error {
			// The API call we want to retry with exponential backoff and jitter
			id, err = q.OmClient.CreateTicket(ctx, ticket)
			return err
		},
		// TODO: expose max elapsed retry time as a configurable deadline parameter
		backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(5*time.Second)),
		func(err error, bo time.Duration) {
			otelTicketCreationRetries.Add(ctx, 1)
			logger.Warnf("CreateTicket temporary failure (backoff for %v): %v", err, bo)
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
