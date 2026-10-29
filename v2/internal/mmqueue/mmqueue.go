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
	"math/rand/v2"
	"net/http"
	// revive: blank-imports — pprof registers profiling HTTP handlers via side-effect import.
	// revive:disable-next-line:blank-imports
	_ "net/http/pprof"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/types/known/timestamppb"

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156
	// revive:disable-next-line:blank-imports
	_ "google.golang.org/protobuf/types/known/wrapperspb"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/internal/assignmentdistributor"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

// defaultTicketTTL is om-core's default OM_CACHE_TICKET_TTL_MS, used if
// MMQUEUE_TICKET_TTL_MS isn't set.
const defaultTicketTTL = 10 * time.Minute

// arrivalTimerGranularity is the minimum remaining wait for which the arrival
// scheduler sleeps on a timer. Arrivals due sooner than this are released
// immediately instead of sleeping, which bounds timer churn at high TPC, where
// the mean gap can be well under 1 ms.
const arrivalTimerGranularity = time.Millisecond

// Mock of the game platform services frontend that handles player matchmaking
// requests, queueing them to be sent to Open Match, and processing them
// asynchronously.
type MatchmakerQueue struct {
	OmClient *omclient.RestfulOMGrpcClient
	Cfg      *viper.Viper
	Log      *logrus.Logger
	// Tickets maps the ID of each ticket this queue created and is waiting on
	// (for an assignment) to the time.Time at which om-core will expire it.
	// Entries are removed when the ticket is assigned, when its request fails
	// or is abandoned, or by the expiry sweep in Run.
	Tickets            sync.Map
	ClientRequestChan  chan *ClientRequest
	AssignmentReceiver assignmentdistributor.Receiver
	OtelMeterPtr       *metric.Meter
	TPC                int64

	tpcMu         sync.Mutex
	ticketGenFunc func(context.Context) *pb.Ticket
	metricsOnce   sync.Once

	ticketAssignmentsCounter          metric.Int64Counter
	untrackedAssignmentsCounter       metric.Int64Counter
	ticketExpirationsCounter          metric.Int64Counter
	ticketAbandonmentsCounter         metric.Int64Counter
	ticketCreationRetriesCounter      metric.Int64Counter
	ticketCreationsCounter            metric.Int64Counter
	tpcRequestGauge                   metric.Int64ObservableGauge
	tpcActualHistogram                metric.Int64Histogram
	tpcConfirmedHistogram             metric.Int64Histogram
	ticketGenerationDurationHistogram metric.Int64Histogram
	ticketActivationsCounter          metric.Int64Counter
	ticketActivationFailuresCounter   metric.Int64Counter
	ticketQueuedDurationsHistogram    metric.Float64Histogram
	ticketArrivalLagHistogram         metric.Float64Histogram
}

// ClientRequest is one game client's matchmaking request, handed to the queue
// on ClientRequestChan.
type ClientRequest struct {
	// Ctx is the client's request context (for an HTTP frontend, the request's
	// own context). If it is done before the queue has reported a result, the
	// client has gone away and its player shouldn't be matched. The queue then
	// skips ticket creation if it hasn't started yet, and otherwise makes sure
	// the ticket ends up inactive in om-core. Ctx is only watched until the
	// result is reported. After that, taking the player out of matchmaking
	// (they cancel, or go offline) is up to the platform, which must call
	// DeactivateTickets with the ticket ID. A nil Ctx means the request is
	// never abandoned.
	Ctx context.Context

	// Ticket is the ticket to create on the client's behalf. The queue's
	// proxyCreateTicket helper overwrites the attributes the platform has
	// authority over before sending it to om-core.
	Ticket *pb.Ticket

	// ResultChan, if non-nil, receives the outcome as an HTTP status code:
	//   - 200 (http.StatusOK): om-core created and activated the ticket, and
	//     the player is now in matchmaking.
	//   - 502 (http.StatusBadGateway): om-core's CreateTicket or
	//     ActivateTickets failed.
	//   - 503 (http.StatusServiceUnavailable): the queue is shutting down.
	//
	// The queue writes to ResultChan exactly once and then closes it. The one
	// exception is an abandoned request (Ctx done): its ResultChan is closed
	// without a value, because nobody is listening. ResultChan must be buffered
	// (capacity at least 1) and used for a single request, so the queue never
	// blocks writing to it.
	ResultChan chan int
}

// activationWaiters lets each request goroutine wait for its own ticket's
// activation result. ActivateTicketsWithResults reports every result on one
// channel, so a single goroutine in Run reads them and hands each one to the
// waiting request goroutine through that ticket's channel.
type activationWaiters struct {
	mu      sync.Mutex
	waiters map[string]chan int
}

func newActivationWaiters() *activationWaiters {
	return &activationWaiters{
		waiters: make(map[string]chan int),
	}
}

func (w *activationWaiters) register(ticketID string) <-chan int {
	ch := make(chan int, 1)
	w.mu.Lock()
	w.waiters[ticketID] = ch
	w.mu.Unlock()
	return ch
}

func (w *activationWaiters) resolve(ticketID string, status int) {
	w.mu.Lock()
	ch, ok := w.waiters[ticketID]
	if ok {
		delete(w.waiters, ticketID)
	}
	w.mu.Unlock()
	if ok {
		ch <- status
	}
}

func (w *activationWaiters) remove(ticketID string) {
	w.mu.Lock()
	delete(w.waiters, ticketID)
	w.mu.Unlock()
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
	waiters := newActivationWaiters()

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
		// On shutdown, answer requests already waiting in ClientRequestChan
		// rather than leaving their clients hanging. This is best effort,
		// because a request sent after this point is never read, so clients
		// should also stop waiting when their own context is done.
		defer func() {
			for {
				select {
				case req, ok := <-q.ClientRequestChan:
					if !ok {
						return
					}
					if req != nil {
						respondToClient(req, http.StatusServiceUnavailable)
					}
				default:
					return
				}
			}
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
				respondToClient(request, http.StatusServiceUnavailable)
				return
			}

			creationWg.Add(1)
			go func(req *ClientRequest) {
				defer creationWg.Done()
				q.handleClientRequest(ctx, req, slots, waiters, ticketIdsToActivate)
			}(request)
		}
	}()

	// Goroutine to activate pending tickets.
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
		atCtx := omclient.WithActivationType(ctx, "activate")

		activationResults := make(chan omclient.ActivationResult, 1000)
		var resultsWg sync.WaitGroup
		resultsWg.Add(1)
		go func() {
			defer resultsWg.Done()
			for res := range activationResults {
				status := http.StatusOK
				if res.Err != nil {
					aLogger.Errorf("ActivateTickets failed for ticket %s: %v", res.TicketId, res.Err)
					if q.ticketActivationFailuresCounter != nil {
						q.ticketActivationFailuresCounter.Add(ctx, 1)
					}
					status = http.StatusBadGateway
				} else {
					if q.ticketActivationsCounter != nil {
						q.ticketActivationsCounter.Add(ctx, 1)
					}
				}
				waiters.resolve(res.TicketId, status)
			}
		}()

		q.OmClient.ActivateTicketsWithResults(atCtx, ticketIdsToActivate, activationResults)
		close(activationResults)
		resultsWg.Wait()
	}()

	// Goroutine to process incoming assignments.
	//
	// NOTE: the assignment API endpoints in om-core are deprecated, and
	// shouldn't be used in production. This implementation assumes you have
	// some other way of getting assignments from your game server director to
	// your matchmaking queue OTHER than going through om-core. It processes
	// every assignment delivered by AssignmentReceiver (see
	// internal/assignmentdistributor for the Go-channel and Pub/Sub
	// implementations). In a real implementation, you'd write a Receiver that
	// gets assignments from whatever handles them.
	//
	// It is recommended that you incorporate your assignment storage and
	// retrieval into the part of your online services suite responsible for
	// returning player online status, like a player online event bus or
	// presence service.
	wg.Add(1)
	go func() {
		defer wg.Done()

		bLogger := q.Log.WithFields(logrus.Fields{
			"operation": "ticket_assignment",
		})
		bLogger.Debug("processing incoming ticket assignments")

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
					if q.ticketAssignmentsCounter != nil {
						q.ticketAssignmentsCounter.Add(ctx, 1)
					}
					// Duration is measured from the creation time that proxyCreateTicket set.
					queuedDur := time.Since(ticket.GetAttributes().GetCreationTime().AsTime())
					if q.ticketQueuedDurationsHistogram != nil {
						q.ticketQueuedDurationsHistogram.Record(ctx, float64(queuedDur.Microseconds())/1000.0)
					}
				} else {
					// With Pub/Sub assignment distribution, every instance has its own
					// subscription and receives every assignment, so most of these are
					// other instances' tickets and the count grows with the number of
					// instances. The rest are redelivered or duplicate assignments and
					// tickets this instance stopped tracking (expired or abandoned).
					// Telling those apart would need a record of every ticket ever seen,
					// which grows without bound.
					if q.untrackedAssignmentsCounter != nil {
						q.untrackedAssignmentsCounter.Add(ctx, 1)
					}
					bLogger.Tracef("Ignoring assignment %v for untracked ticket %v", assignment, ticket.GetId())
				}
			}
		}
		bLogger.Info("Starting assignment receiver")
		err := q.AssignmentReceiver.Receive(ctx, assignmentHandler)
		if err != nil {
			bLogger.Errorf("Assignment receiver error: %v", err)
		}
	}()

	// Goroutine to sweep tickets that have expired in om-core.
	//
	// om-core deletes every ticket OM_CACHE_TICKET_TTL_MS after its creation
	// time, whether or not it was matched, and has no DeleteTicket RPC. A
	// ticket still tracked in Tickets after that time will never be assigned. A
	// real platform would also tell the player's client that its request timed
	// out so it can re-queue (which creates a new ticket).
	wg.Add(1)
	go func() {
		defer wg.Done()

		ticker := time.NewTicker(expirySweepInterval(q.ticketTTL()))
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case tickTime := <-ticker.C:
				q.sweepExpiredTickets(ctx, tickTime)
			}
		}
	}()

	// Don't exit the Run() function as long as any of the goroutines are
	// running (in normal operation, this is forever).
	wg.Wait()
}

func respondToClient(req *ClientRequest, status int) {
	if req.ResultChan == nil {
		return
	}
	req.ResultChan <- status
	close(req.ResultChan)
}

func (q *MatchmakerQueue) abandonRequest(ctx context.Context, req *ClientRequest, ticketID string) {
	q.Log.WithFields(logrus.Fields{
		"component": "matchmaking_queue",
		"operation": "handle_request",
	}).Debugf("Client abandoned matchmaking request for ticket %q", ticketID)
	if q.ticketAbandonmentsCounter != nil {
		q.ticketAbandonmentsCounter.Add(ctx, 1)
	}
	if req.ResultChan != nil {
		close(req.ResultChan)
	}
}

// deactivateTicket calls DeactivateTickets for a single ticket using the queue
// context. Failure is logged and otherwise ignored because om-core expires the
// ticket eventually. For simplicity this helper makes one call per ticket; a
// matchmaker that deactivates often should batch, the way activations are
// batched.
func (q *MatchmakerQueue) deactivateTicket(ctx context.Context, ticketID, reason string) {
	if err := q.OmClient.DeactivateTickets(ctx, []string{ticketID}); err != nil {
		q.Log.WithFields(logrus.Fields{
			"component": "matchmaking_queue",
			"operation": "deactivate_ticket",
		}).Warnf("Ticket %s could not be deactivated (%s); om-core will expire it: %v", ticketID, reason, err)
	}
}

// handleClientRequest is called holding one creation slot and releases it as
// soon as CreateTicket returns, because activation is batched separately.
func (q *MatchmakerQueue) handleClientRequest(
	ctx context.Context,
	req *ClientRequest,
	slots <-chan struct{},
	waiters *activationWaiters,
	ticketIdsToActivate chan<- string,
) {
	clientCtx := req.Ctx
	if clientCtx == nil {
		clientCtx = context.Background()
	}

	// Normalise a missing ticket once on the request itself, so that the ticket
	// sent to om-core and the one its expiry is computed from are the same.
	if req.Ticket == nil {
		req.Ticket = &pb.Ticket{}
	}

	if clientCtx.Err() != nil {
		<-slots
		q.abandonRequest(ctx, req, "")
		return
	}

	id, err := q.proxyCreateTicket(ctx, req.Ticket)
	<-slots
	if err != nil {
		if ctx.Err() != nil {
			respondToClient(req, http.StatusServiceUnavailable)
			return
		}
		q.Log.WithFields(logrus.Fields{
			"component": "matchmaking_queue",
			"operation": "handle_request",
		}).Errorf("CreateTicket failed: %v", err)
		respondToClient(req, http.StatusBadGateway)
		return
	}

	if q.ticketCreationsCounter != nil {
		q.ticketCreationsCounter.Add(ctx, 1)
	}

	// om-core creates tickets in the inactive state, so a ticket that's never
	// activated never appears in a pool and om-core simply expires it.
	if ctx.Err() != nil {
		respondToClient(req, http.StatusServiceUnavailable)
		return
	}
	if clientCtx.Err() != nil {
		q.abandonRequest(ctx, req, id)
		return
	}

	q.Tickets.Store(id, ticketExpiry(req.Ticket, q.ticketTTL()))
	actResChan := waiters.register(id)
	select {
	case ticketIdsToActivate <- id:
	case <-ctx.Done():
		waiters.remove(id)
		q.Tickets.Delete(id)
		respondToClient(req, http.StatusServiceUnavailable)
		return
	}

	var status int
	select {
	case status = <-actResChan:
	case <-ctx.Done():
		// When shutting down, results stop arriving. The activation may still
		// take effect in om-core, and the ticket then stays in the pools until
		// om-core expires it.
		waiters.remove(id)
		q.Tickets.Delete(id)
		respondToClient(req, http.StatusServiceUnavailable)
		return
	case <-clientCtx.Done():
		// Wait for the activation call to finish before deactivating. om-core
		// applies ticket state changes in the order it receives them, so a
		// deactivation sent while the activation is in flight could be undone
		// by it, leaving a ghost player in the pools.
		select {
		case <-actResChan:
		case <-ctx.Done():
			waiters.remove(id)
		}
		q.Tickets.Delete(id)
		q.deactivateTicket(ctx, id, "client abandoned the request")
		q.abandonRequest(ctx, req, id)
		return
	}

	if status != http.StatusOK {
		// ActivateTickets reported a failure, but the update may still have
		// reached om-core (for example, a timeout after om-core applied it), so
		// the queue deactivates the ticket to make sure a player who was told
		// matchmaking failed can't be matched anyway.
		q.Tickets.Delete(id)
		respondToClient(req, status)
		q.deactivateTicket(ctx, id, "activation failed")
		return
	}
	respondToClient(req, http.StatusOK)
}

// ticketTTL returns the configured ticket TTL from MMQUEUE_TICKET_TTL_MS, or
// defaultTicketTTL if unset or non-positive. The value should match om-core's
// OM_CACHE_TICKET_TTL_MS.
func (q *MatchmakerQueue) ticketTTL() time.Duration {
	if ms := q.Cfg.GetInt("MMQUEUE_TICKET_TTL_MS"); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return defaultTicketTTL
}

// ticketExpiry computes when om-core will expire ticket. om-core accepts a
// shorter expiration but not a longer one.
func ticketExpiry(ticket *pb.Ticket, ttl time.Duration) time.Time {
	expiry := ticket.GetAttributes().GetCreationTime().AsTime().Add(ttl)
	if exp := ticket.GetExpirationTime(); exp.IsValid() {
		if expTime := exp.AsTime(); expTime.Before(expiry) {
			return expTime
		}
	}
	return expiry
}

func expirySweepInterval(ttl time.Duration) time.Duration {
	interval := ttl / 10
	if interval < 100*time.Millisecond {
		return 100 * time.Millisecond
	}
	if interval > time.Minute {
		return time.Minute
	}
	return interval
}

func (q *MatchmakerQueue) sweepExpiredTickets(ctx context.Context, now time.Time) {
	eLogger := q.Log.WithFields(logrus.Fields{
		"component": "matchmaking_queue",
		"operation": "expire_tickets",
	})
	var expiredCount int64
	q.Tickets.Range(func(key, value any) bool {
		expiry, ok := value.(time.Time)
		if !ok {
			return true
		}
		if expiry.Before(now) {
			if q.Tickets.CompareAndDelete(key, expiry) {
				expiredCount++
				eLogger.Debugf("Ticket %v expired in om-core before it was assigned", key)
			}
		}
		return true
	})
	if expiredCount > 0 && q.ticketExpirationsCounter != nil {
		q.ticketExpirationsCounter.Add(ctx, expiredCount)
	}
}

// SetTestTicketConfig instructs the test function GenerateTestTickets() to
// schedule `newTPC` simulated player arrivals per cycle using
// `ticketCreationFunc()`. A changed TPC takes effect at the next cycle
// boundary. Example mock client ticket creation functions can be found in
// internal/mocks/gameclient.
func (q *MatchmakerQueue) SetTestTicketConfig(newTPC int64,
	ticketCreationFunc func(context.Context) *pb.Ticket) {

	// This can be updated at any time by the calling code, so protect it with a mutex
	q.tpcMu.Lock()
	q.TPC = newTPC
	q.ticketGenFunc = ticketCreationFunc
	q.tpcMu.Unlock()
}

// poissonArrivalSchedule returns n offsets in [0, window), sorted in ascending
// order. n points placed independently and uniformly in a window and then
// sorted are the arrival times of a Poisson process conditioned on n arrivals:
// the gaps are approximately exponential (natural clusters and lulls), and the
// count per cycle is still exact.
func poissonArrivalSchedule(rng *rand.Rand, n int64, window time.Duration) []time.Duration {
	if n <= 0 {
		return nil
	}
	offsets := make([]time.Duration, n)
	if window <= 0 {
		// Everyone arrives at once.
		return offsets
	}
	winNs := window.Nanoseconds()
	for i := range offsets {
		offsets[i] = time.Duration(rng.Int64N(winNs))
	}
	slices.Sort(offsets)
	return offsets
}

// GenerateTestTickets simulates players joining matchmaking organically at an
// average rate of TPC per cycle (TICKET_CREATION_CYCLE_DURATION_MS, default
// 1 second). This is only suitable for automated testing; in a real matchmaker,
// the server that instantiated the mmqueue inserts ClientRequest values as
// client matchmaking requests arrive. Example mock client ticket creation
// functions can be found in internal/mocks/gameclient.
//
// Usage:
//   - instantiate your mmqueue object (by convention, in a variable named 'q')
//   - asynchronously run the queue: `go q.Run(ctx)`
//   - configure ticket generation with a non-zero TPC and a ticket creation
//     function: `q.SetTestTicketConfig(10, <ticket creation function>)`
//   - asynchronously run the test ticket generator: `go q.GenerateTestTickets(ctx)`
//   - to stop generating tickets, pass 0 to SetTestTicketConfig:
//     `q.SetTestTicketConfig(0, <ticket creation function>)`
//
// Arrivals follow a Poisson schedule per cycle; set TICKET_ARRIVAL_SEED to a
// non-zero value for reproducibility.
//
// Backpressure: arrivals stay on schedule however fast the queue accepts them.
// Overdue players are handed over as soon as the queue unblocks, and the delay
// is recorded in mmqueue.ticket.arrival_lag. Players not accepted by the end of
// the cycle are the shortfall (requested minus mmqueue.tpc.actual). The one
// player waiting at the deadline is carried to the front of the next cycle.
//
// Each generated ticket behaves like a real client request (see the /tickets
// handler in examples/standalone/mmqueue): it carries a ResultChan and waits
// for confirmation. Two per-cycle metrics are recorded: mmqueue.tpc.actual at
// the cycle deadline (requests accepted into the queue) and
// mmqueue.tpc.confirmed once all results for that cycle's queued requests have
// arrived. Confirmations are awaited independently of the cycle timer because
// creation retries and activation batching routinely finish after the cycle
// deadline.
//
// With a finite TICKET_CREATION_CYCLES, GenerateTestTickets returns after the
// final cycle's results are in, or when ctx is cancelled.
func (q *MatchmakerQueue) GenerateTestTickets(ctx context.Context) {
	q.ensureMetricsRegistered()

	tpcLogger := q.Log.WithFields(logrus.Fields{"component": "tester", "operation": "generate_tickets"})

	totalCycles := q.Cfg.GetInt("TICKET_CREATION_CYCLES")
	seed := q.Cfg.GetUint64("TICKET_ARRIVAL_SEED")
	for seed == 0 {
		seed = rand.Uint64()
	}
	rng := rand.New(rand.NewPCG(seed, seed))
	tpcLogger.Debugf("test ticket arrival seed: %d", seed)

	var carryReq *ClientRequest
	var carryArrival time.Time

	var reporterWg sync.WaitGroup
	defer reporterWg.Wait()

	for cycle := 1; cycle <= totalCycles; cycle++ {
		if ctx.Err() != nil {
			return
		}

		cycleCtx, cycleCancel := context.WithCancel(ctx)

		q.tpcMu.Lock()
		thisTPC := q.TPC
		ticketFunc := q.ticketGenFunc
		q.tpcMu.Unlock()

		deadlineDur := time.Millisecond * time.Duration(q.Cfg.GetInt("TICKET_CREATION_CYCLE_DURATION_MS"))
		schedule := poissonArrivalSchedule(rng, thisTPC, deadlineDur)

		tpcLogger.Trace("generating tickets until deadline")
		deadline := time.NewTimer(deadlineDur)
		startTime := time.Now()

		arrivalCh := make(chan time.Time)
		go func(offsets []time.Duration) {
			defer close(arrivalCh)
			timer := time.NewTimer(time.Hour)
			timer.Stop()
			defer timer.Stop()

			for _, offset := range offsets {
				arrivalTime := startTime.Add(offset)
				if wait := time.Until(arrivalTime); wait > arrivalTimerGranularity {
					timer.Reset(wait)
					select {
					case <-cycleCtx.Done():
						return
					case <-timer.C:
					}
				}
				select {
				case <-cycleCtx.Done():
					return
				case arrivalCh <- arrivalTime:
				}
			}
		}(schedule)

		var numTixQueued int64
		durationRecorded := false
		var dur time.Duration
		var confirmWg sync.WaitGroup
		var confirmedCount atomic.Int64
		var failedCount atomic.Int64

		recordDuration := func() {
			if !durationRecorded {
				dur = time.Since(startTime)
				if q.ticketGenerationDurationHistogram != nil {
					q.ticketGenerationDurationHistogram.Record(ctx, dur.Milliseconds())
				}
				durationRecorded = true
			}
		}

		finishCycle := func() {
			recordDuration()
			if q.tpcActualHistogram != nil {
				q.tpcActualHistogram.Record(ctx, numTixQueued)
			}
			tpcLogger.Infof("matchmaker queue ticket creation cycle %010d/%010d: tickets queued %05d/%05d (achieved/requested), time elapsed %.3f/%.3f seconds",
				cycle,
				totalCycles,
				numTixQueued,
				thisTPC,
				dur.Seconds(),
				deadlineDur.Seconds(),
			)
		}

	cycleLoop:
		for {
			select {
			case <-ctx.Done():
				deadline.Stop()
				cycleCancel()
				return

			case arrivalTime, ok := <-arrivalCh:
				if !ok {
					arrivalCh = nil
					recordDuration()
					continue
				}

				var req *ClientRequest
				if carryReq != nil {
					req = carryReq
					arrivalTime = carryArrival
					carryReq = nil
					carryArrival = time.Time{}
				} else {
					var ticket *pb.Ticket
					if ticketFunc != nil {
						ticket = ticketFunc(ctx)
					} else {
						ticket = &pb.Ticket{}
					}

					// Buffer capacity 1 so the queue never blocks reporting a result,
					// even if nobody is listening any more.
					rChan := make(chan int, 1)
					req = &ClientRequest{
						Ticket:     ticket,
						ResultChan: rChan,
					}
				}

				select {
				case <-ctx.Done():
					deadline.Stop()
					cycleCancel()
					return
				case <-deadline.C:
					carryReq = req
					carryArrival = arrivalTime
					finishCycle()
					break cycleLoop
				case q.ClientRequestChan <- req:
					numTixQueued++
					if q.ticketArrivalLagHistogram != nil {
						lag := time.Since(arrivalTime)
						if lag < 0 {
							lag = 0
						}
						q.ticketArrivalLagHistogram.Record(ctx, float64(lag.Microseconds())/1000.0)
					}
					confirmWg.Add(1)
					go func(resChan <-chan int) {
						defer confirmWg.Done()
						select {
						case <-ctx.Done():
							return
						case status, open := <-resChan:
							if open && status == http.StatusOK {
								confirmedCount.Add(1)
							} else {
								failedCount.Add(1)
							}
						}
					}(req.ResultChan)
				}

			case <-deadline.C:
				finishCycle()
				break cycleLoop
			}
		}

		deadline.Stop()
		cycleCancel()

		reporterWg.Add(1)
		go func(cycleNum int, queued int64, cycleStart time.Time) {
			defer reporterWg.Done()
			confirmWg.Wait()
			if ctx.Err() != nil {
				return
			}
			confirmed := confirmedCount.Load()
			failed := failedCount.Load()
			if q.tpcConfirmedHistogram != nil {
				q.tpcConfirmedHistogram.Record(ctx, confirmed)
			}
			tpcLogger.Infof("matchmaker queue ticket creation cycle %010d/%010d: tickets confirmed %05d/%05d (confirmed/queued), %05d failed, all results in after %.3f seconds",
				cycleNum,
				totalCycles,
				confirmed,
				queued,
				failed,
				time.Since(cycleStart).Seconds(),
			)
		}(cycle, numTixQueued, startTime)
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
//
// ticket must be non-nil (handleClientRequest normalises a missing one) and is
// modified in place, so the caller sees the attributes that were sent.
func (q *MatchmakerQueue) proxyCreateTicket(ctx context.Context, ticket *pb.Ticket) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	q.ensureMetricsRegistered()

	var id string
	var err error

	logger := q.Log.WithFields(logrus.Fields{
		"component": "matchmaking_queue",
		"operation": "proxy_CreateTicket",
	})
	logger.Trace("creating ticket")

	if ticket.GetAttributes() == nil {
		ticket.Attributes = &pb.Ticket_FilterableData{}
	}
	// The platform, not the client, decides when the player entered
	// matchmaking. This is the time that the queue's expiry tracking and the
	// queued-durations metric are measured from, and om-core expires the ticket
	// OM_CACHE_TICKET_TTL_MS after it.
	ticket.Attributes.CreationTime = timestamppb.Now()

	// This is where a matchmaker would call its platform services to add
	// attributes it has authority over, overwriting anything the client sent.
	// For example, it would fetch the player's MMR from an MMR service and
	// write it under mmr. For simplicity, this example trusts the mmr that the
	// mock game client (internal/mocks/gameclient) puts in the ticket, and the
	// matchmaker's skill-tier pools filter on it. Never trust a real client
	// with its own MMR.

	// Each CreateTicket attempt is bounded by OM_CORE_REQUEST_TIMEOUT_MS
	// (default 5 s), so the retry budget has to be well beyond that for an
	// attempt that times out to actually be retried. CreateTicket marks errors
	// that retrying can't fix as permanent, so those aren't retried.
	expBackoff := backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(15 * time.Second))
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
			logger.Warnf("CreateTicket temporary failure (backoff for %v): %v", bo, err)
		},
	)

	if err == nil {
		// Log successful ticket creation
		logger.Debugf("CreateTicket %v successful", id)
	}
	return id, err
}
