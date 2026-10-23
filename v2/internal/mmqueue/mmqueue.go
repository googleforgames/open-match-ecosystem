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
	"net/http"
	// revive: blank-imports — pprof registers profiling HTTP handlers via side-effect import.
	// revive:disable-next-line:blank-imports
	_ "net/http/pprof"
	"sync"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel/metric"

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156
	// revive:disable-next-line:blank-imports
	_ "google.golang.org/protobuf/types/known/wrapperspb"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/internal/assignmentdistributor"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

// Mock of the game platform services frontend that handles player matchmaking
// requests, queueing them to be sent to Open Match, and processing them
// asynchronously.
type MatchmakerQueue struct {
	OmClient           *omclient.RestfulOMGrpcClient
	Cfg                *viper.Viper
	Log                *logrus.Logger
	Tickets            sync.Map
	ClientRequestChan  chan *ClientRequest
	AssignmentReceiver assignmentdistributor.Receiver
	AssignmentsChan    chan *pb.Roster
	OtelMeterPtr       *metric.Meter
	TPC                int64

	tpcMu         sync.Mutex
	ticketGenFunc func(context.Context) *pb.Ticket
	metricsOnce   sync.Once

	ticketAssignmentsCounter          metric.Int64Counter
	ticketCreationRetriesCounter      metric.Int64Counter
	ticketCreationsCounter            metric.Int64Counter
	ticketDeletionFailuresCounter     metric.Int64Counter
	tpcRequestGauge                   metric.Int64ObservableGauge
	tpcActualHistogram                metric.Int64Histogram
	ticketGenerationDurationHistogram metric.Int64Histogram
	ticketActivationsCounter          metric.Int64Counter
	ticketActivationFailuresCounter   metric.Int64Counter
	ticketQueuedDurationsHistogram    metric.Float64Histogram
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
		"component": "matchmaking_queue",
	})

	// Var init
	var wg sync.WaitGroup
	ticketIdsToActivate := make(chan string, 10000)

	var pendingMu sync.Mutex
	activationClosed := false
	pendingActivations := make(map[string][]chan int)

	// Initialize metrics
	if q.OtelMeterPtr != nil {
		logger.Tracef("Initializing otel metrics")
		q.ensureMetricsRegistered()
	} else {
		logger.Warnf("Unable to add matchmaking queue metrics to the OTEL configuration")
	}

	// Control number of concurrent requests by creating ticket creation 'slots'
	slots := make(chan struct{}, q.Cfg.GetInt("MAX_CONCURRENT_TICKET_CREATIONS"))

	// Goroutine to read from the incoming ticket request channel, and create
	// one ticket for each incoming request. Limit the number of concurrent
	// creations by blocking until one of the MAX_CONCURRENT_TICKET_CREATIONS
	// 'slots' is available.
	wg.Add(1)
	go func() {
		defer wg.Done()

		cLogger := q.Log.WithFields(logrus.Fields{
			"operation": "ticket_creation",
		})
		cLogger.Debug("Processing queued ticket creation requests")

		var creationWg sync.WaitGroup
		defer func() {
			creationWg.Wait()
			close(ticketIdsToActivate)
		}()

		for {
			logger.Trace("waiting for a new ticket to enter the queue")
			var request *ClientRequest
			var ok bool
			select {
			case <-ctx.Done():
				return
			case request, ok = <-q.ClientRequestChan:
				if !ok {
					return
				}
			}
			if request == nil {
				continue
			}

			// Block until one of the MAX_CONCURRENT_TICKET_CREATIONS 'slots' is available
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				if request.ResultChan != nil {
					request.ResultChan <- http.StatusRequestTimeout
					close(request.ResultChan)
				}
				return
			}

			creationWg.Add(1)
			go func(req *ClientRequest) {
				defer creationWg.Done()
				defer func() { <-slots }()

				// proxyCreateTicket implements retries exp bo + jitter
				id, err := q.proxyCreateTicket(ctx, req.Ticket)
				if err != nil {
					// Either a permanent error, or retries timed out
					logger.Errorf("CreateTicket failed: %v", err)
					if req.ResultChan != nil {
						req.ResultChan <- http.StatusRequestTimeout
						close(req.ResultChan)
					}
					return
				}

				pendingMu.Lock()
				if activationClosed {
					pendingMu.Unlock()
					if req.ResultChan != nil {
						req.ResultChan <- http.StatusRequestTimeout
						close(req.ResultChan)
					}
					return
				}
				q.Tickets.Store(id, struct{}{})
				if req.ResultChan != nil {
					pendingActivations[id] = append(pendingActivations[id], req.ResultChan)
				}
				pendingMu.Unlock()

				if q.ticketCreationsCounter != nil {
					q.ticketCreationsCounter.Add(ctx, 1)
				}

				select {
				case ticketIdsToActivate <- id:
				case <-ctx.Done():
				}
			}(request)
		}
	}()

	// goroutine to activate pending tickets.
	wg.Add(1)
	go func() {
		defer wg.Done()

		aLogger := q.Log.WithFields(logrus.Fields{
			"operation": "ticket_activation",
		})
		aLogger.Debug("activating created tickets")

		// Make a derived context with the activationType 'activate', which is used
		// by the omclient to distinguish between logs for initial ticket activation
		// and re-activation after a failed matching attempt.
		atCtx := context.WithValue(ctx, "activationType", "activate") //nolint:staticcheck // omclient reads string key "activationType"

		activationResults := make(chan omclient.ActivationResult, 1000)
		var resultsWg sync.WaitGroup
		resultsWg.Add(1)
		go func() {
			defer resultsWg.Done()
			for res := range activationResults {
				pendingMu.Lock()
				waiters := pendingActivations[res.TicketId]
				delete(pendingActivations, res.TicketId)
				pendingMu.Unlock()

				if res.Err != nil {
					aLogger.Errorf("ActivateTickets failed for ticket %s: %v", res.TicketId, res.Err)
					if q.ticketActivationFailuresCounter != nil {
						q.ticketActivationFailuresCounter.Add(ctx, 1)
					}
					q.Tickets.Delete(res.TicketId)
					for _, ch := range waiters {
						ch <- http.StatusInternalServerError
						close(ch)
					}
				} else {
					if q.ticketActivationsCounter != nil {
						q.ticketActivationsCounter.Add(ctx, 1)
					}
					for _, ch := range waiters {
						ch <- http.StatusOK
						close(ch)
					}
				}
			}

			pendingMu.Lock()
			activationClosed = true
			for ticketID, waiters := range pendingActivations {
				delete(pendingActivations, ticketID)
				q.Tickets.Delete(ticketID)
				for _, ch := range waiters {
					ch <- http.StatusRequestTimeout
					close(ch)
				}
			}
			pendingMu.Unlock()
		}()

		q.OmClient.ActivateTicketsWithResults(atCtx, ticketIdsToActivate, activationResults)
		close(activationResults)
		resultsWg.Wait()
	}()

	// goroutine to process incoming assignments.
	//
	// NOTE: the assignment api endpoints in om-core are deprecated, and
	// shouldn't be used in production.  This implementation assumes you have
	// some other way of getting assignments from your game server director to
	// your matchmaking queue OTHER than going through om-core, but isn't
	// specific about what that 'other way' is. This just reads all incoming
	// assignments from the AssignmentsChan and processes them. In a real
	// implementation, you'd write some code to get assignments from whatever
	// is handling them and put them in the channel.
	//
	// It is recommended that you incorporate your assignment storage and
	// retreival into the part of your online services suite responsible for
	// returning player online status, like a player online event bus or
	// presence service.
	wg.Add(1)
	go func() {
		defer wg.Done()

		bLogger := q.Log.WithFields(logrus.Fields{
			"operation": "ticket_assignment",
		})
		bLogger.Debug("processing incoming ticket assignments")

		var assigned sync.Map

		assignmentHandler := func(ctx context.Context, roster *pb.Roster) {
			// Get the assignment string
			assignment := roster.GetAssignment().GetConnection()

			// Loop through all tickets in the assignment roster
			for _, ticket := range roster.GetTickets() {
				// Stop tracking this ticket; its matchmaking is complete.
				// Your matchmaker may wish to instead keep this for a time (in
				// case the game client should be allowed to ask for the same
				// assignment again later).
				if _, existed := q.Tickets.LoadAndDelete(ticket.GetId()); existed {
					bLogger.Debugf("Received ticket %v assignment: %v", ticket.GetId(), assignment)
					assigned.Store(ticket.GetId(), true)
					// Update metric
					if q.ticketAssignmentsCounter != nil {
						q.ticketAssignmentsCounter.Add(ctx, 1)
					}
					// Get diff between now and ticket creation time, in milliseconds
					queuedDur := time.Since(ticket.GetAttributes().GetCreationTime().AsTime())
					// Record queue duration in fractional milliseconds
					if q.ticketQueuedDurationsHistogram != nil {
						q.ticketQueuedDurationsHistogram.Record(ctx, float64(queuedDur.Microseconds()/1000))
					}
				} else {
					if q.ticketDeletionFailuresCounter != nil {
						q.ticketDeletionFailuresCounter.Add(ctx, 1)
					}
					if _, previouslyassigned := assigned.Load(ticket.GetId()); previouslyassigned {
						bLogger.Debugf("PREVIOUSLY ASSIGNED ticket %v assignment: %v", ticket.GetId(), assignment)
					} else {
						bLogger.Debugf("FAILED UNTRACKED ticket %v assignment: %v", ticket.GetId(), assignment)
					}
				}
			}
		}
		bLogger.Info("Starting assignment receiver")
		err := q.AssignmentReceiver.Receive(ctx, assignmentHandler)
		if err != nil {
			bLogger.Errorf("Assignment receiver error: %v", err)
		}
	}()

	// Don't exit the Run() function as long as any of the goroutines are
	// running (in normal operation, this is forever).
	wg.Wait()
}

// SetTestTicketConfig instructs the test function GenerateTestTickets() to make `newTPC`
// tickets per second using `ticketCreationFunc()`. Example mock client ticket
// creations functions can be found in internal/mocks/gameclient
func (q *MatchmakerQueue) SetTestTicketConfig(newTPC int64,
	ticketCreationFunc func(context.Context) *pb.Ticket) {

	// This can be updated at any time by the calling code, so protect it with a mutex
	q.tpcMu.Lock()
	q.TPC = newTPC
	q.ticketGenFunc = ticketCreationFunc
	q.tpcMu.Unlock()
}

// Test the queue by generating tickets every second.
// This is only suitable for automated testing; in a real matchmaker, the
// server that instantiated the mmqueue will be inserting all the ClientRequest
// objects as client matchmaking requests come in.  You can find example mock client
// ticket creation functions in internal/mocks/gameclient.
// To use this function:
//   - instante your mmqueue object (by convention, in a variable named 'q')
//   - asynchronously run the queue: `go q.Run(ctx)`
//   - to set up ticket generation, send a non-zero TPC & a ticket creation function to the SetTPC function:
//     `q.SetTestTicketConfig(10, <ticket creation function>)`
//   - asynchronously run the test ticket generator: `go q.GenerateTestTickets(ctx)`
//   - to stop generating tickets, send 0 to the SetTPC function:
//     `q.SetTestTicketConfig(0, <ticket creation function>)`
func (q *MatchmakerQueue) GenerateTestTickets(ctx context.Context) {
	q.ensureMetricsRegistered()

	tpcLogger := q.Log.WithFields(logrus.Fields{"component": "tester", "operation": "generate_tickets"})

	for i := q.Cfg.GetInt("TICKET_CREATION_CYCLES"); i > 0; i-- {
		if ctx.Err() != nil {
			return
		}

		cycleCtx, cycleCancel := context.WithCancel(ctx)

		// Channel where we will put one struct for each ticket we want to create this second.
		tq := make(chan struct{})

		q.tpcMu.Lock()
		thisTPC := q.TPC
		ticketFunc := q.ticketGenFunc
		q.tpcMu.Unlock()

		// Simple tpc goroutine that puts one struct in the channel for every ticket
		// we want to generate this second.
		go func(tpc int64) {
			defer close(tq)
			for j := tpc; j > 0; j-- {
				select {
				case <-cycleCtx.Done():
					return
				case tq <- struct{}{}:
				}
			}
		}(thisTPC)

		// Loop for 1 second, making as many tickets as we can, up to the requested TPC.
		tpcLogger.Trace("generating tickets until deadline")
		deadlineDur := time.Millisecond * time.Duration(q.Cfg.GetInt("TICKET_CREATION_CYCLE_DURATION_MS"))
		deadline := time.NewTimer(deadlineDur)
		startTime := time.Now()

		var numTixQueued int64
		var ticketCounterMutex sync.Mutex
		durationRecorded := false
		var dur time.Duration

		finishCycle := func() {
			ticketCounterMutex.Lock()
			numTixCreationsAchieved := numTixQueued
			ticketCounterMutex.Unlock()

			if !durationRecorded {
				dur = time.Since(startTime)
				if q.ticketGenerationDurationHistogram != nil {
					q.ticketGenerationDurationHistogram.Record(ctx, dur.Milliseconds())
				}
				durationRecorded = true
			}
			if q.tpcActualHistogram != nil {
				q.tpcActualHistogram.Record(ctx, numTixCreationsAchieved)
			}

			tpcLogger.Infof("matchmaker queue ticket creation cycle %010d/%010d: ticket creations %05d/%05d (achieved/requested), time elapsed %.3f/%.3f seconds",
				q.Cfg.GetInt("TICKET_CREATION_CYCLES")+1-i,
				q.Cfg.GetInt("TICKET_CREATION_CYCLES")+1,
				numTixCreationsAchieved,
				thisTPC,
				float64(dur.Milliseconds())/1000.0,
				float64(deadlineDur.Milliseconds())/1000.0,
			)
		}

	cycleLoop:
		for {
			select {
			case <-ctx.Done():
				deadline.Stop()
				cycleCancel()
				return

			case _, ok := <-tq:
				if !ok {
					tq = nil
					if !durationRecorded {
						dur = time.Since(startTime)
						if q.ticketGenerationDurationHistogram != nil {
							q.ticketGenerationDurationHistogram.Record(ctx, dur.Milliseconds())
						}
						durationRecorded = true
					}
					continue
				}

				var ticket *pb.Ticket
				if ticketFunc != nil {
					ticket = ticketFunc(ctx)
				} else {
					ticket = &pb.Ticket{}
				}

				rChan := make(chan int, 1)
				req := &ClientRequest{
					ResultChan: rChan,
					Ticket:     ticket,
				}

				select {
				case <-ctx.Done():
					deadline.Stop()
					cycleCancel()
					return
				case <-deadline.C:
					finishCycle()
					cycleCancel()
					break cycleLoop
				case q.ClientRequestChan <- req:
					go func(resChan <-chan int) {
						select {
						case <-cycleCtx.Done():
							return
						case status, ok := <-resChan:
							if ok && status == http.StatusOK {
								ticketCounterMutex.Lock()
								numTixQueued++
								ticketCounterMutex.Unlock()
							}
						}
					}(rChan)
				}

			case <-deadline.C:
				finishCycle()
				cycleCancel()
				break cycleLoop
			}
		}

		deadline.Stop()
		cycleCancel()
	}
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	q.ensureMetricsRegistered()

	// Local var declarations
	var id string
	var err error
	mmr := 0.0

	logger := q.Log.WithFields(logrus.Fields{
		"component": "matchmaking_queue",
		"operation": "proxy_CreateTicket",
	})
	logger.Trace("creating ticket")

	if ticket == nil {
		ticket = &pb.Ticket{}
	}
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
	expBackoff := backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(5 * time.Second))
	err = backoff.RetryNotify(
		func() error {
			// The API call we want to retry with exponential backoff and jitter
			id, err = q.OmClient.CreateTicket(ctx, ticket)
			return err
		},
		// TODO: expose max elapsed retry time as a configurable deadline parameter
		backoff.WithContext(expBackoff, ctx),
		func(err error, bo time.Duration) {
			if q.ticketCreationRetriesCounter != nil {
				q.ticketCreationRetriesCounter.Add(ctx, 1)
			}
			logger.Warnf("CreateTicket temporary failure (backoff for %v): %v", err, bo)
		},
	)

	if err == nil {
		// Log successful ticket creation
		logger.Debugf("CreateTicket %v successful", id)
	}
	return id, err
}
