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
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	otelmetrics "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
	"open-match.dev/open-match-ecosystem/v2/internal/assignmentdistributor"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

func newTestLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

func newTestMeter(t *testing.T) (*otelmetrics.Meter, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})
	meter := provider.Meter("mmqueue.test")
	return &meter, reader
}

func newTestQueue(t *testing.T, serverURL string, maxConcurrent int) (*MatchmakerQueue, *assignmentdistributor.ChannelSender, *sdkmetric.ManualReader) {
	t.Helper()
	cfg := viper.New()
	cfg.Set("OM_CORE_ADDR", serverURL)
	cfg.Set("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 500)
	cfg.Set("MAX_CONCURRENT_TICKET_CREATIONS", maxConcurrent)
	cfg.Set("TICKET_CREATION_CYCLE_DURATION_MS", 50)
	cfg.Set("TICKET_CREATION_CYCLES", 1)
	cfg.Set("MMQUEUE_TICKET_TTL_MS", 600000)

	log := newTestLogger()
	meterPtr, reader := newTestMeter(t)
	assignChan := make(chan *pb.Roster, 100)
	sender := assignmentdistributor.NewChannelSender(assignChan)
	receiver := assignmentdistributor.NewChannelReceiver(assignChan)

	q := &MatchmakerQueue{
		OmClient:           omclient.New(cfg, log, &http.Client{Timeout: 5 * time.Second}),
		Cfg:                cfg,
		Log:                log,
		ClientRequestChan:  make(chan *ClientRequest, 100),
		AssignmentReceiver: receiver,
		OtelMeterPtr:       meterPtr,
	}
	return q, sender, reader
}

func readDeactivateRequest(t *testing.T, r *http.Request) []string {
	t.Helper()
	if r.Method != http.MethodDelete {
		t.Errorf("deactivate HTTP method = %q, want %q", r.Method, http.MethodDelete)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("reading deactivate body: %v", err)
		return nil
	}
	var req pb.DeactivateTicketsRequest
	if err := protojson.Unmarshal(body, &req); err != nil {
		t.Errorf("unmarshalling DeactivateTicketsRequest: %v", err)
		return nil
	}
	return req.GetTicketIds()
}

func expectSingleResultAndClosed(t *testing.T, ch <-chan int, wantCode int) {
	t.Helper()
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatalf("ResultChan closed without a value, want %d", wantCode)
		}
		if got != wantCode {
			t.Fatalf("ResultChan status = %d, want %d", got, wantCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for status %d on ResultChan", wantCode)
	}

	select {
	case extra, ok := <-ch:
		if ok {
			t.Fatalf("ResultChan received unexpected second value %d after %d", extra, wantCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ResultChan to be closed")
	}
}

func pickArrivalSeed(t *testing.T, cfg *viper.Viper, n int64, window, margin time.Duration) uint64 {
	t.Helper()
	limit := window - margin
	for seed := uint64(1); seed < 1000; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed))
		sched := poissonArrivalSchedule(rng, n, window)
		if len(sched) == 0 || sched[len(sched)-1] <= limit {
			cfg.Set("TICKET_ARRIVAL_SEED", seed)
			return seed
		}
	}
	t.Fatalf("no seed in 1..999 placed %d arrivals within %v - %v", n, window, margin)
	return 0
}

func readInt64Counter(t *testing.T, reader *sdkmetric.ManualReader, metricName string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect failed: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == metricName {
				if sum, ok := m.Data.(metricdata.Sum[int64]); ok && len(sum.DataPoints) > 0 {
					return sum.DataPoints[0].Value
				}
			}
		}
	}
	return 0
}

type fakeOMServerOpts struct {
	ticketID        string
	activateEntered chan<- struct{}
	releaseActivate <-chan struct{}
	deactivatedCh   chan<- []string
}

func startFakeOMServer(t *testing.T, opts fakeOMServerOpts) *httptest.Server {
	t.Helper()
	var enterOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tickets":
			respBytes, _ := protojson.Marshal(&pb.CreateTicketResponse{TicketId: opts.ticketID})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBytes)
		case "/tickets:activate":
			if opts.activateEntered != nil {
				enterOnce.Do(func() {
					close(opts.activateEntered)
				})
			}
			if opts.releaseActivate != nil {
				<-opts.releaseActivate
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "/tickets:deactivate":
			ids := readDeactivateRequest(t, r)
			if opts.deactivatedCh != nil {
				opts.deactivatedCh <- ids
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 1. Concurrency Slot Limit
func TestRun_ConcurrencySlotLimit(t *testing.T) {
	const maxConcurrent = 3
	const totalRequests = 10

	var inFlight atomic.Int32
	var maxObserved atomic.Int32
	var totalCreated atomic.Int32

	threeInFlight := make(chan struct{})
	var threeOnce sync.Once
	releaseCreate := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tickets":
			cur := inFlight.Add(1)
			for {
				prev := maxObserved.Load()
				if cur <= prev || maxObserved.CompareAndSwap(prev, cur) {
					break
				}
			}
			if cur == int32(maxConcurrent) {
				threeOnce.Do(func() {
					close(threeInFlight)
				})
			}
			<-releaseCreate
			inFlight.Add(-1)

			idx := totalCreated.Add(1)
			respBytes, _ := protojson.Marshal(&pb.CreateTicketResponse{
				TicketId: fmt.Sprintf("ticket-%d", idx),
			})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBytes)
		case "/tickets:activate":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	q, sender, _ := newTestQueue(t, srv.URL, maxConcurrent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		q.Run(ctx)
		close(runDone)
	}()

	resChans := make([]chan int, totalRequests)
	for i := range totalRequests {
		resChans[i] = make(chan int, 1)
		q.ClientRequestChan <- &ClientRequest{
			Ticket:     &pb.Ticket{},
			ResultChan: resChans[i],
		}
	}

	select {
	case <-threeInFlight:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for 3 concurrent /tickets requests")
	}

	// Allow time for any extra goroutines that might violate the concurrency cap.
	time.Sleep(40 * time.Millisecond)
	if got := maxObserved.Load(); got > int32(maxConcurrent) {
		t.Fatalf("concurrency before release = %d, want <= %d", got, maxConcurrent)
	}

	close(releaseCreate)

	for i, ch := range resChans {
		select {
		case status := <-ch:
			if status != http.StatusOK {
				t.Fatalf("request %d status = %d, want %d", i, status, http.StatusOK)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for request %d completion", i)
		}
	}

	if got := maxObserved.Load(); got != int32(maxConcurrent) {
		t.Fatalf("max concurrent /tickets calls = %d, want %d", got, maxConcurrent)
	}

	cancel()
	sender.Stop()
	<-runDone
}

// 2. Activation Confirmation Barrier Before HTTP 200
func TestRun_ActivationConfirmationBarrierBeforeHTTP200(t *testing.T) {
	const ticketID = "ticket-barrier-1"
	activateEntered := make(chan struct{})
	releaseActivate := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tickets":
			respBytes, _ := protojson.Marshal(&pb.CreateTicketResponse{TicketId: ticketID})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBytes)
		case "/tickets:activate":
			close(activateEntered)
			<-releaseActivate
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	q, sender, _ := newTestQueue(t, srv.URL, 3)
	q.Cfg.Set("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		q.Run(ctx)
		close(runDone)
	}()

	resChan := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{
		Ticket:     &pb.Ticket{},
		ResultChan: resChan,
	}

	select {
	case <-activateEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for /tickets:activate to be called")
	}

	// Ticket must already be tracked in q.Tickets before activation finishes.
	if _, exists := q.Tickets.Load(ticketID); !exists {
		t.Fatalf("expected ticket %q to be present in q.Tickets while activation is in-flight", ticketID)
	}

	// ResultChan must NOT receive 200 OK while /tickets:activate is still blocked.
	select {
	case code := <-resChan:
		t.Fatalf("ResultChan received status %d before /tickets:activate completed", code)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseActivate)

	select {
	case code := <-resChan:
		if code != http.StatusOK {
			t.Fatalf("ResultChan status = %d, want %d", code, http.StatusOK)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for 200 OK after /tickets:activate returned")
	}

	// Deliver assignment and verify ticket is removed from q.Tickets.
	err := sender.Send(ctx, &pb.Roster{
		Name:       "roster-1",
		Assignment: &pb.Assignment{Connection: "10.0.0.1:7777"},
		Tickets: []*pb.Ticket{
			{
				Id: ticketID,
				Attributes: &pb.Ticket_FilterableData{
					CreationTime: timestamppb.New(time.Now().Add(-50 * time.Millisecond)),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("sender.Send failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, exists := q.Tickets.Load(ticketID); !exists {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, exists := q.Tickets.Load(ticketID); exists {
		t.Fatalf("expected ticket %q to be deleted from q.Tickets after assignment", ticketID)
	}

	cancel()
	sender.Stop()
	<-runDone
}

// 3. Permanent Activation Failure Returns HTTP 502, Evicts Ticket, and Deactivates
func TestRun_PermanentActivationFailureReturnsHTTP502EvictsAndDeactivates(t *testing.T) {
	const ticketID = "ticket-perm-fail-1"
	deactivatedCh := make(chan []string, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tickets":
			respBytes, _ := protojson.Marshal(&pb.CreateTicketResponse{TicketId: ticketID})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBytes)
		case "/tickets:activate":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":3,"message":"invalid ticket id"}`))
		case "/tickets:deactivate":
			ids := readDeactivateRequest(t, r)
			deactivatedCh <- ids
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	q, sender, _ := newTestQueue(t, srv.URL, 3)
	q.Cfg.Set("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		q.Run(ctx)
		close(runDone)
	}()

	resChan := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{
		Ticket:     &pb.Ticket{},
		ResultChan: resChan,
	}

	expectSingleResultAndClosed(t, resChan, http.StatusBadGateway)

	if _, exists := q.Tickets.Load(ticketID); exists {
		t.Fatalf("expected permanently failed ticket %q to be evicted from q.Tickets", ticketID)
	}

	select {
	case ids := <-deactivatedCh:
		if !slices.Equal(ids, []string{ticketID}) {
			t.Fatalf("deactivated ticket IDs = %v, want [%s]", ids, ticketID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for DELETE /tickets:deactivate after activation failure")
	}

	cancel()
	sender.Stop()
	<-runDone
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// 4. Shutdown Answers Every Pending Request Once
func TestRun_ShutdownAnswersEveryPendingRequestOnce(t *testing.T) {
	var createCalls atomic.Int32
	activateEntered := make(chan struct{})
	releaseActivate := make(chan struct{})
	create1Entered := make(chan struct{})
	releaseCreate1 := make(chan struct{})
	create2Entered := make(chan struct{})
	releaseCreate2 := make(chan struct{})

	t.Cleanup(func() {
		select {
		case <-releaseActivate:
		default:
			close(releaseActivate)
		}
		select {
		case <-releaseCreate1:
		default:
			close(releaseCreate1)
		}
		select {
		case <-releaseCreate2:
		default:
			close(releaseCreate2)
		}
	})

	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/tickets":
			callNum := createCalls.Add(1)
			switch callNum {
			case 1:
				respBytes, _ := protojson.Marshal(&pb.CreateTicketResponse{TicketId: "req-0-ticket"})
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewReader(respBytes)),
				}, nil
			case 2:
				close(create1Entered)
				<-releaseCreate1
				respBytes, _ := protojson.Marshal(&pb.CreateTicketResponse{TicketId: "req-1-ticket"})
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewReader(respBytes)),
				}, nil
			default:
				close(create2Entered)
				<-releaseCreate2
				respBytes, _ := protojson.Marshal(&pb.CreateTicketResponse{TicketId: "req-2-ticket"})
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewReader(respBytes)),
				}, nil
			}
		case "/tickets:activate":
			close(activateEntered)
			<-releaseActivate
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader([]byte(`{}`))),
			}, nil
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(nil)),
			}, nil
		}
	})

	// MAX_CONCURRENT_TICKET_CREATIONS = 2 and unbuffered ClientRequestChan for deterministic staging.
	q, sender, _ := newTestQueue(t, "http://om-core.test", 2)
	q.OmClient.Client = &http.Client{Transport: transport}
	q.Cfg.Set("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 1)
	q.ClientRequestChan = make(chan *ClientRequest)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		q.Run(ctx)
		close(runDone)
	}()

	// Request 0: created (releasing its creation slot) and blocks inside /tickets:activate.
	res0 := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{Ticket: &pb.Ticket{}, ResultChan: res0}
	select {
	case <-activateEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Request 0 to enter /tickets:activate")
	}

	// Request 1: occupies creation slot 1 inside /tickets.
	res1 := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{Ticket: &pb.Ticket{}, ResultChan: res1}
	select {
	case <-create1Entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Request 1 to enter /tickets")
	}

	// Request 2: occupies creation slot 2 inside /tickets.
	res2 := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{Ticket: &pb.Ticket{}, ResultChan: res2}
	select {
	case <-create2Entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Request 2 to enter /tickets")
	}

	// Request 3: read from unbuffered ClientRequestChan and blocked waiting for a creation slot.
	res3 := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{Ticket: &pb.Ticket{}, ResultChan: res3}

	// Cancel queue context while om-core calls are still blocked.
	cancel()

	// Request 3 (waiting for a slot) and Request 0 (waiting for activation)
	// must each receive 503 and be closed before om-core responds.
	expectSingleResultAndClosed(t, res3, http.StatusServiceUnavailable)
	expectSingleResultAndClosed(t, res0, http.StatusServiceUnavailable)

	// Releasing create 1, then create 2, gives each 503 and closes it.
	close(releaseCreate1)
	expectSingleResultAndClosed(t, res1, http.StatusServiceUnavailable)

	close(releaseCreate2)
	expectSingleResultAndClosed(t, res2, http.StatusServiceUnavailable)

	// Release activation and stop assignment sender; Run must exit cleanly with empty Tickets.
	close(releaseActivate)
	sender.Stop()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for q.Run to exit")
	}

	var leftover []any
	q.Tickets.Range(func(key, _ any) bool {
		leftover = append(leftover, key)
		return true
	})
	if len(leftover) != 0 {
		t.Fatalf("expected q.Tickets to be empty after shutdown, got %v", leftover)
	}
}

// 5. Ticket Creation Retry Aborts on Context Cancellation
func TestProxyCreateTicket_RetryAbortsOnContextCancellation(t *testing.T) {
	firstAttemptFailed := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		once.Do(func() {
			close(firstAttemptFailed)
		})
	}))
	defer srv.Close()

	q, _, _ := newTestQueue(t, srv.URL, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errChan := make(chan error, 1)
	go func() {
		_, err := q.proxyCreateTicket(ctx, &pb.Ticket{})
		errChan <- err
	}()

	select {
	case <-firstAttemptFailed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first 503 attempt")
	}

	// Wait briefly so proxyCreateTicket enters backoff sleep, then cancel ctx.
	time.Sleep(20 * time.Millisecond)
	cancelStart := time.Now()
	cancel()

	select {
	case err := <-errChan:
		if err == nil {
			t.Fatal("expected non-nil error when context is cancelled during retry backoff")
		}
		if elapsed := time.Since(cancelStart); elapsed > 500*time.Millisecond {
			t.Fatalf("proxyCreateTicket took %v to abort after cancel, want < 500ms", elapsed)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("proxyCreateTicket did not abort immediately on context cancellation")
	}
}

// 6. GenerateTestTickets Immediate Exit on Context Cancellation
func TestGenerateTestTickets_ImmediateExitOnContextCancellation(t *testing.T) {
	q, _, _ := newTestQueue(t, "http://127.0.0.1:0", 3)
	q.Cfg.Set("TICKET_CREATION_CYCLE_DURATION_MS", 30000) // 30-second cycle duration
	q.Cfg.Set("TICKET_CREATION_CYCLES", 10)
	q.ClientRequestChan = make(chan *ClientRequest) // unbuffered and undrained

	ticketGenerated := make(chan struct{})
	var genOnce sync.Once
	q.SetTestTicketConfig(5000, func(_ context.Context) *pb.Ticket {
		genOnce.Do(func() {
			close(ticketGenerated)
		})
		return &pb.Ticket{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		q.GenerateTestTickets(ctx)
		close(done)
	}()

	select {
	case <-ticketGenerated:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for GenerateTestTickets to start generating tickets")
	}

	cancelStart := time.Now()
	cancel()

	select {
	case <-done:
		if elapsed := time.Since(cancelStart); elapsed > 500*time.Millisecond {
			t.Fatalf("GenerateTestTickets took %v to exit after cancel, want < 500ms", elapsed)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("GenerateTestTickets did not exit immediately on context cancellation")
	}
}

// 7. Ticket Activation Monotonic Counters and Deprecated Metric Absence
func TestMetrics_TicketActivationCountersAndDeprecatedMetricAbsence(t *testing.T) {
	var ticketSeq atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tickets":
			n := ticketSeq.Add(1)
			id := fmt.Sprintf("ok-%d", n)
			if n == 3 {
				id = "fail-1"
			}
			respBytes, _ := protojson.Marshal(&pb.CreateTicketResponse{TicketId: id})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBytes)
		case "/tickets:activate":
			body, _ := io.ReadAll(r.Body)
			var req pb.ActivateTicketsRequest
			_ = protojson.Unmarshal(body, &req)
			if len(req.GetTicketIds()) == 1 && req.GetTicketIds()[0] == "fail-1" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":3,"message":"invalid ticket"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "/tickets:deactivate":
			_ = readDeactivateRequest(t, r)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	q, sender, reader := newTestQueue(t, srv.URL, 3)
	q.Cfg.Set("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		q.Run(ctx)
		close(runDone)
	}()

	expectedStatuses := []int{http.StatusOK, http.StatusOK, http.StatusBadGateway}
	for _, wantStatus := range expectedStatuses {
		resChan := make(chan int, 1)
		q.ClientRequestChan <- &ClientRequest{
			Ticket:     &pb.Ticket{},
			ResultChan: resChan,
		}
		expectSingleResultAndClosed(t, resChan, wantStatus)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("reader.Collect failed: %v", err)
	}

	var foundActivations, foundFailures bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "mmqueue.ticket.activation.requests", "mmqueue.ticket.activations.per_call", "mmqueue.ticket.deletion.failures":
				t.Fatalf("deprecated metric %q must not be registered", m.Name)
			case "mmqueue.ticket.activations":
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("mmqueue.ticket.activations data type = %T, want metricdata.Sum[int64]", m.Data)
				}
				if !sum.IsMonotonic {
					t.Fatal("expected mmqueue.ticket.activations to be monotonic")
				}
				if len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 2 {
					t.Fatalf("mmqueue.ticket.activations value = %+v, want 2", sum.DataPoints)
				}
				foundActivations = true
			case "mmqueue.ticket.activations.failures":
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("mmqueue.ticket.activations.failures data type = %T, want metricdata.Sum[int64]", m.Data)
				}
				if !sum.IsMonotonic {
					t.Fatal("expected mmqueue.ticket.activations.failures to be monotonic")
				}
				if len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 1 {
					t.Fatalf("mmqueue.ticket.activations.failures value = %+v, want 1", sum.DataPoints)
				}
				foundFailures = true
			}
		}
	}

	if !foundActivations {
		t.Fatal("expected mmqueue.ticket.activations metric to be present")
	}
	if !foundFailures {
		t.Fatal("expected mmqueue.ticket.activations.failures metric to be present")
	}

	cancel()
	sender.Stop()
	<-runDone
}

// 8. Assignment Delivery and Queued Duration Metrics
func TestRun_AssignmentDeliveryAndQueuedDurationMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tickets":
			respBytes, _ := protojson.Marshal(&pb.CreateTicketResponse{TicketId: "tracked-1"})
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(respBytes)
		case "/tickets:activate":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	q, sender, reader := newTestQueue(t, srv.URL, 3)
	q.Cfg.Set("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		q.Run(ctx)
		close(runDone)
	}()

	resChan := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{
		Ticket:     &pb.Ticket{},
		ResultChan: resChan,
	}
	expectSingleResultAndClosed(t, resChan, http.StatusOK)

	createdAt := timestamppb.New(time.Now().Add(-150 * time.Millisecond))
	err := sender.Send(ctx, &pb.Roster{
		Name:       "roster-assign",
		Assignment: &pb.Assignment{Connection: "10.0.0.2:7777"},
		Tickets: []*pb.Ticket{
			{Id: "tracked-1", Attributes: &pb.Ticket_FilterableData{CreationTime: createdAt}},
			{Id: "tracked-1", Attributes: &pb.Ticket_FilterableData{CreationTime: createdAt}}, // duplicate
			{Id: "untracked-99", Attributes: &pb.Ticket_FilterableData{CreationTime: createdAt}},
		},
	})
	if err != nil {
		t.Fatalf("sender.Send failed: %v", err)
	}

	// Wait until tracked-1 is removed and metrics are updated.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, exists := q.Tickets.Load("tracked-1"); !exists {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, exists := q.Tickets.Load("tracked-1"); exists {
		t.Fatal("expected tracked-1 to be removed from q.Tickets")
	}

	// Give the receiver loop a moment to finish the remaining two tickets in the same roster slice.
	time.Sleep(30 * time.Millisecond)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("reader.Collect failed: %v", err)
	}

	var assignmentsVal, untrackedVal int64
	var queuedCount uint64
	var queuedSum float64

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "mmqueue.ticket.assignments":
				if sum, ok := m.Data.(metricdata.Sum[int64]); ok && len(sum.DataPoints) > 0 {
					assignmentsVal = sum.DataPoints[0].Value
				}
			case "mmqueue.ticket.assignments.untracked":
				if sum, ok := m.Data.(metricdata.Sum[int64]); ok && len(sum.DataPoints) > 0 {
					untrackedVal = sum.DataPoints[0].Value
				}
			case "mmqueue.ticket.queued_durations":
				if hist, ok := m.Data.(metricdata.Histogram[float64]); ok && len(hist.DataPoints) > 0 {
					queuedCount = hist.DataPoints[0].Count
					queuedSum = hist.DataPoints[0].Sum
				}
			}
		}
	}

	if assignmentsVal != 1 {
		t.Fatalf("mmqueue.ticket.assignments = %d, want 1", assignmentsVal)
	}
	if untrackedVal != 2 {
		t.Fatalf("mmqueue.ticket.assignments.untracked = %d, want 2", untrackedVal)
	}
	if queuedCount != 1 || queuedSum < 100 {
		t.Fatalf("mmqueue.ticket.queued_durations count=%d sum=%v, want count=1 sum>=100ms", queuedCount, queuedSum)
	}

	cancel()
	sender.Stop()
	<-runDone
}

// 9. Zero Initial TPC Metric Initialization Without Panic
func TestGenerateTestTickets_ZeroInitialTPCMetricInitializationWithoutPanic(t *testing.T) {
	q, _, reader := newTestQueue(t, "http://127.0.0.1:0", 3)
	q.Cfg.Set("TICKET_CREATION_CYCLES", 1)
	q.Cfg.Set("TICKET_CREATION_CYCLE_DURATION_MS", 25)
	q.SetTestTicketConfig(0, func(_ context.Context) *pb.Ticket {
		return &pb.Ticket{}
	})

	// Call GenerateTestTickets before Run with TPC == 0; must not panic.
	q.GenerateTestTickets(context.Background())

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect failed: %v", err)
	}

	var foundCycleDur, foundTPCActual, foundTPCRequest bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "mmqueue.ticket.generation.cycle_duration":
				if hist, ok := m.Data.(metricdata.Histogram[int64]); ok && len(hist.DataPoints) == 1 && hist.DataPoints[0].Count == 1 {
					foundCycleDur = true
				}
			case "mmqueue.tpc.actual":
				if hist, ok := m.Data.(metricdata.Histogram[int64]); ok && len(hist.DataPoints) == 1 && hist.DataPoints[0].Count == 1 && hist.DataPoints[0].Sum == 0 {
					foundTPCActual = true
				}
			case "mmqueue.tpc.request":
				if gauge, ok := m.Data.(metricdata.Gauge[int64]); ok && len(gauge.DataPoints) == 1 && gauge.DataPoints[0].Value == 0 {
					foundTPCRequest = true
				}
			}
		}
	}

	if !foundCycleDur || !foundTPCActual || !foundTPCRequest {
		t.Fatalf("missing expected metrics: cycle_duration=%v tpc.actual=%v tpc.request=%v", foundCycleDur, foundTPCActual, foundTPCRequest)
	}
}

// 10. GenerateTestTickets Deadline Enforcement When Queue Full
func TestGenerateTestTickets_DeadlineEnforcementWhenQueueFull(t *testing.T) {
	q, _, reader := newTestQueue(t, "http://127.0.0.1:0", 3)
	q.Cfg.Set("TICKET_CREATION_CYCLES", 2)
	q.Cfg.Set("TICKET_CREATION_CYCLE_DURATION_MS", 40)
	q.ClientRequestChan = make(chan *ClientRequest) // unbuffered and undrained

	q.SetTestTicketConfig(10, func(_ context.Context) *pb.Ticket {
		return &pb.Ticket{}
	})

	start := time.Now()
	done := make(chan struct{})
	go func() {
		q.GenerateTestTickets(context.Background())
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
			t.Fatalf("GenerateTestTickets took %v for two 40ms cycles, want < 600ms", elapsed)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("GenerateTestTickets deadlocked when ClientRequestChan was full")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect failed: %v", err)
	}

	var cycleDurCount, tpcActualCount uint64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "mmqueue.ticket.generation.cycle_duration":
				if hist, ok := m.Data.(metricdata.Histogram[int64]); ok && len(hist.DataPoints) > 0 {
					cycleDurCount = hist.DataPoints[0].Count
				}
			case "mmqueue.tpc.actual":
				if hist, ok := m.Data.(metricdata.Histogram[int64]); ok && len(hist.DataPoints) > 0 {
					tpcActualCount = hist.DataPoints[0].Count
				}
			}
		}
	}

	if cycleDurCount != 2 {
		t.Fatalf("mmqueue.ticket.generation.cycle_duration count = %d, want 2", cycleDurCount)
	}
	if tpcActualCount != 2 {
		t.Fatalf("mmqueue.tpc.actual count = %d, want 2", tpcActualCount)
	}
}

// 11. Per-Instance Test Ticket Configuration Isolation
func TestSetTestTicketConfig_PerInstanceIsolation(t *testing.T) {
	q1, _, reader1 := newTestQueue(t, "http://127.0.0.1:0", 3)
	q2, _, reader2 := newTestQueue(t, "http://127.0.0.1:0", 3)

	q1.Cfg.Set("TICKET_CREATION_CYCLES", 1)
	q1.Cfg.Set("TICKET_CREATION_CYCLE_DURATION_MS", 60)
	q2.Cfg.Set("TICKET_CREATION_CYCLES", 1)
	q2.Cfg.Set("TICKET_CREATION_CYCLE_DURATION_MS", 60)

	pickArrivalSeed(t, q1.Cfg, 2, 60*time.Millisecond, 20*time.Millisecond)
	pickArrivalSeed(t, q2.Cfg, 5, 60*time.Millisecond, 20*time.Millisecond)

	q1.SetTestTicketConfig(2, func(_ context.Context) *pb.Ticket {
		return &pb.Ticket{Id: "q1-ticket"}
	})
	q2.SetTestTicketConfig(5, func(_ context.Context) *pb.Ticket {
		return &pb.Ticket{Id: "q2-ticket"}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var q1Tickets, q2Tickets []string
	var mu1, mu2 sync.Mutex

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-q1.ClientRequestChan:
				mu1.Lock()
				q1Tickets = append(q1Tickets, req.Ticket.GetId())
				mu1.Unlock()
				req.ResultChan <- http.StatusOK
				close(req.ResultChan)
			}
		}
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-q2.ClientRequestChan:
				mu2.Lock()
				q2Tickets = append(q2Tickets, req.Ticket.GetId())
				mu2.Unlock()
				req.ResultChan <- http.StatusOK
				close(req.ResultChan)
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		q1.GenerateTestTickets(ctx)
	}()
	go func() {
		defer wg.Done()
		q2.GenerateTestTickets(ctx)
	}()
	wg.Wait()

	mu1.Lock()
	got1 := slices.Clone(q1Tickets)
	mu1.Unlock()

	mu2.Lock()
	got2 := slices.Clone(q2Tickets)
	mu2.Unlock()

	if len(got1) != 2 {
		t.Fatalf("q1 generated %d tickets, want 2", len(got1))
	}
	for _, id := range got1 {
		if id != "q1-ticket" {
			t.Fatalf("q1 generated ticket id %q, want %q", id, "q1-ticket")
		}
	}

	if len(got2) != 5 {
		t.Fatalf("q2 generated %d tickets, want 5", len(got2))
	}
	for _, id := range got2 {
		if id != "q2-ticket" {
			t.Fatalf("q2 generated ticket id %q, want %q", id, "q2-ticket")
		}
	}

	var rm1, rm2 metricdata.ResourceMetrics
	if err := reader1.Collect(ctx, &rm1); err != nil {
		t.Fatalf("reader1.Collect failed: %v", err)
	}
	if err := reader2.Collect(ctx, &rm2); err != nil {
		t.Fatalf("reader2.Collect failed: %v", err)
	}

	assertGauge := func(rm metricdata.ResourceMetrics, want int64) {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name == "mmqueue.tpc.request" {
					if g, ok := m.Data.(metricdata.Gauge[int64]); ok && len(g.DataPoints) == 1 && g.DataPoints[0].Value == want {
						return
					}
				}
			}
		}
		t.Fatalf("expected mmqueue.tpc.request gauge = %d", want)
	}
	assertGauge(rm1, 2)
	assertGauge(rm2, 5)
}

// 12. Confirmations After the Deadline Are Counted
func TestGenerateTestTickets_LateConfirmationsCounted(t *testing.T) {
	q, _, reader := newTestQueue(t, "http://127.0.0.1:0", 3)
	q.Cfg.Set("TICKET_CREATION_CYCLES", 1)
	q.Cfg.Set("TICKET_CREATION_CYCLE_DURATION_MS", 50)
	pickArrivalSeed(t, q.Cfg, 5, 50*time.Millisecond, 20*time.Millisecond)
	q.ClientRequestChan = make(chan *ClientRequest)

	q.SetTestTicketConfig(5, func(_ context.Context) *pb.Ticket {
		return &pb.Ticket{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var held []*ClientRequest
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-q.ClientRequestChan:
				mu.Lock()
				held = append(held, req)
				mu.Unlock()
			}
		}
	}()

	genDone := make(chan struct{})
	go func() {
		q.GenerateTestTickets(ctx)
		close(genDone)
	}()

	time.Sleep(150 * time.Millisecond)

	select {
	case <-genDone:
		t.Fatal("GenerateTestTickets returned before late confirmations were delivered")
	default:
	}

	mu.Lock()
	accepted := slices.Clone(held)
	mu.Unlock()
	if len(accepted) != 5 {
		t.Fatalf("accepted requests after 150ms = %d, want 5", len(accepted))
	}

	accepted[0].ResultChan <- http.StatusInternalServerError
	close(accepted[0].ResultChan)
	for _, req := range accepted[1:] {
		req.ResultChan <- http.StatusOK
		close(req.ResultChan)
	}

	select {
	case <-genDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for GenerateTestTickets to return after confirmations")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("reader.Collect failed: %v", err)
	}

	var foundActual, foundConfirmed bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "mmqueue.tpc.actual":
				hist, ok := m.Data.(metricdata.Histogram[int64])
				if !ok || len(hist.DataPoints) != 1 || hist.DataPoints[0].Count != 1 || hist.DataPoints[0].Sum != 5 {
					t.Fatalf("mmqueue.tpc.actual = %+v, want 1 data point with sum 5", m.Data)
				}
				foundActual = true
			case "mmqueue.tpc.confirmed":
				hist, ok := m.Data.(metricdata.Histogram[int64])
				if !ok || len(hist.DataPoints) != 1 || hist.DataPoints[0].Count != 1 || hist.DataPoints[0].Sum != 4 {
					t.Fatalf("mmqueue.tpc.confirmed = %+v, want 1 data point with sum 4", m.Data)
				}
				foundConfirmed = true
			}
		}
	}
	if !foundActual || !foundConfirmed {
		t.Fatalf("missing expected metrics: actual=%v confirmed=%v", foundActual, foundConfirmed)
	}
}

// 13. Carry-Over of an Unqueued Ticket
func TestGenerateTestTickets_CarryOverUnqueuedTicket(t *testing.T) {
	q, _, _ := newTestQueue(t, "http://127.0.0.1:0", 3)
	q.Cfg.Set("TICKET_CREATION_CYCLES", 3)
	q.Cfg.Set("TICKET_CREATION_CYCLE_DURATION_MS", 40)
	pickArrivalSeed(t, q.Cfg, 10, 40*time.Millisecond, 10*time.Millisecond)
	q.ClientRequestChan = make(chan *ClientRequest) // unbuffered and undrained

	var calls atomic.Int32
	q.SetTestTicketConfig(10, func(_ context.Context) *pb.Ticket {
		calls.Add(1)
		return &pb.Ticket{}
	})

	q.GenerateTestTickets(context.Background())

	if got := calls.Load(); got != 1 {
		t.Fatalf("ticket function calls across 3 blocked cycles = %d, want 1", got)
	}
}

// 14. Poisson Schedule Shape
func TestPoissonArrivalSchedule_Shape(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 42))
	const n = 5000
	const window = time.Second

	offsets := poissonArrivalSchedule(rng, n, window)
	if len(offsets) != n {
		t.Fatalf("len(offsets) = %d, want %d", len(offsets), n)
	}
	if !slices.IsSorted(offsets) {
		t.Fatal("offsets are not sorted in ascending order")
	}
	if offsets[0] < 0 || offsets[len(offsets)-1] >= window {
		t.Fatalf("offsets out of [0, %v): first=%v last=%v", window, offsets[0], offsets[len(offsets)-1])
	}

	gaps := make([]float64, n)
	prev := time.Duration(0)
	var sum float64
	for i, off := range offsets {
		g := float64(off - prev)
		gaps[i] = g
		sum += g
		prev = off
	}
	mean := sum / float64(len(gaps))
	wantMean := float64(window) / float64(n)
	if math.Abs(mean-wantMean)/wantMean > 0.10 {
		t.Fatalf("mean gap = %v ns, want within 10%% of %v ns", mean, wantMean)
	}

	var sqDiffSum float64
	for _, g := range gaps {
		d := g - mean
		sqDiffSum += d * d
	}
	stdDev := math.Sqrt(sqDiffSum / float64(len(gaps)))
	cv := stdDev / mean
	if cv < 0.9 || cv > 1.1 {
		t.Fatalf("gap coefficient of variation = %.4f, want in [0.9, 1.1]", cv)
	}

	if got := poissonArrivalSchedule(rng, 0, time.Second); got != nil {
		t.Fatalf("poissonArrivalSchedule(n=0) = %v, want nil", got)
	}

	zeroWin := poissonArrivalSchedule(rng, 3, 0)
	if !slices.Equal(zeroWin, []time.Duration{0, 0, 0}) {
		t.Fatalf("poissonArrivalSchedule(n=3, window=0) = %v, want [0 0 0]", zeroWin)
	}
}

// 15. Arrivals Spread Across the Cycle
func TestGenerateTestTickets_ArrivalsSpreadAcrossCycle(t *testing.T) {
	const tpc = 20
	const cycleDur = 200 * time.Millisecond

	q, _, _ := newTestQueue(t, "http://127.0.0.1:0", 3)
	q.Cfg.Set("TICKET_CREATION_CYCLES", 1)
	q.Cfg.Set("TICKET_CREATION_CYCLE_DURATION_MS", int(cycleDur.Milliseconds()))
	seed := pickArrivalSeed(t, q.Cfg, tpc, cycleDur, 30*time.Millisecond)
	q.ClientRequestChan = make(chan *ClientRequest)

	expected := poissonArrivalSchedule(rand.New(rand.NewPCG(seed, seed)), tpc, cycleDur)
	if span := expected[len(expected)-1] - expected[0]; span < cycleDur/2 {
		t.Fatalf("precondition failed: seeded schedule span = %v, want >= %v", span, cycleDur/2)
	}

	q.SetTestTicketConfig(tpc, func(_ context.Context) *pb.Ticket {
		return &pb.Ticket{}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var genStart time.Time
	startReady := make(chan struct{})
	var mu sync.Mutex
	var acceptedOffsets []time.Duration

	go func() {
		<-startReady
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-q.ClientRequestChan:
				elapsed := time.Since(genStart)
				mu.Lock()
				acceptedOffsets = append(acceptedOffsets, elapsed)
				mu.Unlock()
				req.ResultChan <- http.StatusOK
				close(req.ResultChan)
			}
		}
	}()

	genStart = time.Now()
	close(startReady)
	q.GenerateTestTickets(ctx)

	mu.Lock()
	got := slices.Clone(acceptedOffsets)
	mu.Unlock()

	if len(got) != tpc {
		t.Fatalf("accepted %d requests, want %d", len(got), tpc)
	}
	for i, actualOff := range got {
		if actualOff < expected[i]-20*time.Millisecond {
			t.Fatalf("arrival %d accepted at %v, more than 20ms before scheduled %v", i, actualOff, expected[i])
		}
	}
	if span := got[len(got)-1] - got[0]; span < cycleDur/2 {
		t.Fatalf("accepted arrivals span = %v, want >= %v", span, cycleDur/2)
	}
}

// 16. Overdue Arrivals Released When the Queue Unblocks
func TestGenerateTestTickets_OverdueArrivalsReleasedOnUnblock(t *testing.T) {
	const tpc = 10
	const cycleDur = 300 * time.Millisecond
	const blockDur = 150 * time.Millisecond

	q, _, reader := newTestQueue(t, "http://127.0.0.1:0", 3)
	q.Cfg.Set("TICKET_CREATION_CYCLES", 1)
	q.Cfg.Set("TICKET_CREATION_CYCLE_DURATION_MS", int(cycleDur.Milliseconds()))
	q.ClientRequestChan = make(chan *ClientRequest)

	var chosenSeed uint64
	var sched []time.Duration
	for s := uint64(1); s < 1000; s++ {
		cand := poissonArrivalSchedule(rand.New(rand.NewPCG(s, s)), tpc, cycleDur)
		if cand[0] <= 75*time.Millisecond && cand[len(cand)-1] <= 270*time.Millisecond {
			chosenSeed = s
			sched = cand
			break
		}
	}
	if chosenSeed == 0 {
		t.Fatal("failed to find seed with first <= 75ms and last <= 270ms")
	}
	q.Cfg.Set("TICKET_ARRIVAL_SEED", chosenSeed)

	q.SetTestTicketConfig(tpc, func(_ context.Context) *pb.Ticket {
		return &pb.Ticket{}
	})
	q.ensureMetricsRegistered()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var accepted atomic.Int32
	startBlock := make(chan struct{})
	go func() {
		<-startBlock
		timer := time.NewTimer(blockDur)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-q.ClientRequestChan:
				accepted.Add(1)
				req.ResultChan <- http.StatusOK
				close(req.ResultChan)
			}
		}
	}()

	close(startBlock)
	q.GenerateTestTickets(ctx)

	if got := accepted.Load(); got != tpc {
		t.Fatalf("accepted = %d, want %d", got, tpc)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("reader.Collect failed: %v", err)
	}

	var foundLag bool
	wantMinMaxMs := float64((blockDur - sched[0] - 10*time.Millisecond).Microseconds()) / 1000.0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "mmqueue.ticket.arrival_lag" {
				hist, ok := m.Data.(metricdata.Histogram[float64])
				if !ok || len(hist.DataPoints) != 1 {
					t.Fatalf("mmqueue.ticket.arrival_lag = %+v, want 1 histogram data point", m.Data)
				}
				dp := hist.DataPoints[0]
				if dp.Count != tpc {
					t.Fatalf("mmqueue.ticket.arrival_lag count = %d, want %d", dp.Count, tpc)
				}
				maxVal, defined := dp.Max.Value()
				if !defined || maxVal < wantMinMaxMs {
					t.Fatalf("mmqueue.ticket.arrival_lag max = %v (defined=%v), want >= %v ms", maxVal, defined, wantMinMaxMs)
				}
				foundLag = true
			}
		}
	}
	if !foundLag {
		t.Fatal("expected mmqueue.ticket.arrival_lag metric to be recorded")
	}
}

// 17. Expiry Sweep Drops Expired Tickets
func TestRun_ExpirySweepDropsExpiredTickets(t *testing.T) {
	const ticketID = "expiring-ticket-1"
	srv := startFakeOMServer(t, fakeOMServerOpts{ticketID: ticketID})

	q, sender, reader := newTestQueue(t, srv.URL, 3)
	q.Cfg.Set("MMQUEUE_TICKET_TTL_MS", 300) // sweep interval clamped to 100ms
	q.Cfg.Set("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		q.Run(ctx)
		close(runDone)
	}()

	resChan := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{
		Ticket:     &pb.Ticket{},
		ResultChan: resChan,
	}
	expectSingleResultAndClosed(t, resChan, http.StatusOK)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, exists := q.Tickets.Load(ticketID); !exists {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, exists := q.Tickets.Load(ticketID); exists {
		t.Fatalf("expected %q to be swept from q.Tickets within 3s", ticketID)
	}

	if got := readInt64Counter(t, reader, "mmqueue.ticket.expirations"); got != 1 {
		t.Fatalf("mmqueue.ticket.expirations = %d, want 1", got)
	}

	cancel()
	sender.Stop()
	<-runDone
}

// 18. Ticket Expiry Calculation
func TestTicketExpiry_Calculation(t *testing.T) {
	created := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	// 1. Creation time with no ExpirationTime -> creation + 10m.
	t1 := &pb.Ticket{
		Attributes: &pb.Ticket_FilterableData{
			CreationTime: timestamppb.New(created),
		},
	}
	if got, want := ticketExpiry(t1, defaultTicketTTL), created.Add(10*time.Minute); !got.Equal(want) {
		t.Fatalf("expiry without ExpirationTime = %v, want %v", got, want)
	}

	// 2. Earlier ExpirationTime (+1m) wins.
	earlier := created.Add(1 * time.Minute)
	t2 := &pb.Ticket{
		ExpirationTime: timestamppb.New(earlier),
		Attributes: &pb.Ticket_FilterableData{
			CreationTime: timestamppb.New(created),
		},
	}
	if got := ticketExpiry(t2, defaultTicketTTL); !got.Equal(earlier) {
		t.Fatalf("expiry with earlier ExpirationTime = %v, want %v", got, earlier)
	}

	// 3. Later ExpirationTime (+1h) is capped at creation + 10m.
	t3 := &pb.Ticket{
		ExpirationTime: timestamppb.New(created.Add(1 * time.Hour)),
		Attributes: &pb.Ticket_FilterableData{
			CreationTime: timestamppb.New(created),
		},
	}
	if got, want := ticketExpiry(t3, defaultTicketTTL), created.Add(10*time.Minute); !got.Equal(want) {
		t.Fatalf("expiry with later ExpirationTime = %v, want %v", got, want)
	}
}

// 19. Abandoned Before Create Skips CreateTicket
func TestRun_AbandonedBeforeCreateSkipsCreateTicket(t *testing.T) {
	var omCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		omCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	q, sender, reader := newTestQueue(t, srv.URL, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		q.Run(ctx)
		close(runDone)
	}()

	clientCtx, clientCancel := context.WithCancel(context.Background())
	clientCancel()

	resChan := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{
		Ctx:        clientCtx,
		Ticket:     &pb.Ticket{},
		ResultChan: resChan,
	}

	select {
	case val, ok := <-resChan:
		if ok {
			t.Fatalf("ResultChan received %d on abandoned request, want closed without value", val)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for abandoned request ResultChan to close")
	}

	if got := omCalls.Load(); got != 0 {
		t.Fatalf("om-core calls = %d, want 0", got)
	}
	if got := readInt64Counter(t, reader, "mmqueue.ticket.abandonments"); got != 1 {
		t.Fatalf("mmqueue.ticket.abandonments = %d, want 1", got)
	}

	cancel()
	sender.Stop()
	<-runDone
}

// 20. Abandoned During Activation Deactivates After Activation
func TestRun_AbandonedDuringActivationDeactivatesAfterActivation(t *testing.T) {
	const ticketID = "abandoned-act-1"
	activateEntered := make(chan struct{})
	releaseActivate := make(chan struct{})
	deactivatedCh := make(chan []string, 1)

	srv := startFakeOMServer(t, fakeOMServerOpts{
		ticketID:        ticketID,
		activateEntered: activateEntered,
		releaseActivate: releaseActivate,
		deactivatedCh:   deactivatedCh,
	})

	q, sender, reader := newTestQueue(t, srv.URL, 3)
	q.Cfg.Set("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		q.Run(ctx)
		close(runDone)
	}()

	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()

	resChan := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{
		Ctx:        clientCtx,
		Ticket:     &pb.Ticket{},
		ResultChan: resChan,
	}

	select {
	case <-activateEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for /tickets:activate to be entered")
	}

	clientCancel()

	// Must NOT deactivate while activation is still in flight.
	select {
	case ids := <-deactivatedCh:
		t.Fatalf("premature deactivation %v while activation still in flight", ids)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseActivate)

	select {
	case ids := <-deactivatedCh:
		if !slices.Equal(ids, []string{ticketID}) {
			t.Fatalf("deactivated ticket IDs = %v, want [%s]", ids, ticketID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for deactivation after in-flight activation completed")
	}

	select {
	case val, ok := <-resChan:
		if ok {
			t.Fatalf("ResultChan received %d on abandoned request, want closed without value", val)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ResultChan to close after abandonment")
	}

	if _, exists := q.Tickets.Load(ticketID); exists {
		t.Fatalf("expected abandoned ticket %q not to remain in q.Tickets", ticketID)
	}
	if got := readInt64Counter(t, reader, "mmqueue.ticket.abandonments"); got != 1 {
		t.Fatalf("mmqueue.ticket.abandonments = %d, want 1", got)
	}

	cancel()
	sender.Stop()
	<-runDone
}

// 21. proxyCreateTicket Sets CreationTime and Preserves DoubleArgs Without example_mmr
func TestProxyCreateTicket_SetsCreationTime(t *testing.T) {
	receivedCh := make(chan *pb.CreateTicketRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		var req pb.CreateTicketRequest
		if err := protojson.Unmarshal(body, &req); err != nil {
			t.Errorf("unmarshalling CreateTicketRequest: %v", err)
		}
		receivedCh <- &req
		respBytes, _ := protojson.Marshal(&pb.CreateTicketResponse{TicketId: "proxy-created-1"})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBytes)
	}))
	defer srv.Close()

	q, _, _ := newTestQueue(t, srv.URL, 3)

	ticket := &pb.Ticket{
		Attributes: &pb.Ticket_FilterableData{
			CreationTime: timestamppb.New(time.Now().Add(-1 * time.Hour)),
			DoubleArgs:   map[string]float64{"mmr": 1500},
		},
	}

	before := time.Now()
	id, err := q.proxyCreateTicket(context.Background(), ticket)
	after := time.Now()
	if err != nil {
		t.Fatalf("proxyCreateTicket failed: %v", err)
	}
	if id != "proxy-created-1" {
		t.Fatalf("ticket id = %q, want %q", id, "proxy-created-1")
	}

	var gotReq *pb.CreateTicketRequest
	select {
	case gotReq = <-receivedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CreateTicketRequest")
	}

	gotTime := gotReq.GetTicket().GetAttributes().GetCreationTime().AsTime()
	if gotTime.Before(before) || gotTime.After(after) {
		t.Fatalf("received CreationTime = %v, want in [%v, %v]", gotTime, before, after)
	}

	wantDoubles := map[string]float64{"mmr": 1500}
	if gotDoubles := gotReq.GetTicket().GetAttributes().GetDoubleArgs(); !maps.Equal(gotDoubles, wantDoubles) {
		t.Fatalf("received DoubleArgs = %v, want %v", gotDoubles, wantDoubles)
	}
}

// 22. Nil Ticket Gets a Real Expiry
func TestRun_NilTicketGetsRealExpiry(t *testing.T) {
	const ticketID = "nil-ticket-1"
	srv := startFakeOMServer(t, fakeOMServerOpts{ticketID: ticketID})

	q, sender, reader := newTestQueue(t, srv.URL, 3)
	q.Cfg.Set("MMQUEUE_TICKET_TTL_MS", 60000)
	q.Cfg.Set("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		q.Run(ctx)
		close(runDone)
	}()

	resChan := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{
		Ticket:     nil,
		ResultChan: resChan,
	}
	expectSingleResultAndClosed(t, resChan, http.StatusOK)

	val, exists := q.Tickets.Load(ticketID)
	if !exists {
		t.Fatalf("expected %q to be tracked in q.Tickets", ticketID)
	}
	expiry, ok := val.(time.Time)
	if !ok {
		t.Fatalf("q.Tickets[%q] value type = %T, want time.Time", ticketID, val)
	}
	if rem := time.Until(expiry); rem < 50*time.Second {
		t.Fatalf("remaining TTL = %v, want >= 50s", rem)
	}

	q.sweepExpiredTickets(ctx, time.Now())

	if _, stillTracked := q.Tickets.Load(ticketID); !stillTracked {
		t.Fatalf("expected %q to remain tracked after manual sweep at current time", ticketID)
	}
	if got := readInt64Counter(t, reader, "mmqueue.ticket.expirations"); got != 0 {
		t.Fatalf("mmqueue.ticket.expirations = %d, want 0", got)
	}

	cancel()
	sender.Stop()
	<-runDone
}
