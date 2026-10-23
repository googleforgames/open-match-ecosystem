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
	"net/http"
	"net/http/httptest"
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
		AssignmentsChan:    assignChan,
		OtelMeterPtr:       meterPtr,
	}
	return q, sender, reader
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

// 3. Permanent Activation Failure Returns HTTP 500 and Evicts Ticket
func TestRun_PermanentActivationFailureReturnsHTTP500AndEvictsTicket(t *testing.T) {
	const ticketID = "ticket-perm-fail-1"

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
	case code := <-resChan:
		if code != http.StatusInternalServerError {
			t.Fatalf("ResultChan status = %d, want %d", code, http.StatusInternalServerError)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for 500 status on permanent activation failure")
	}

	if _, exists := q.Tickets.Load(ticketID); exists {
		t.Fatalf("expected permanently failed ticket %q to be evicted from q.Tickets", ticketID)
	}

	cancel()
	sender.Stop()
	<-runDone
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// 4. Context Cancellation Unblocks and Drains Pending Waiters
func TestRun_ContextCancellationUnblocksAndDrainsPendingWaiters(t *testing.T) {
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
				respBytes, _ := protojson.Marshal(&pb.CreateTicketResponse{TicketId: "seed-activate-ticket"})
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewReader(respBytes)),
				}, nil
			case 2:
				close(create1Entered)
				<-releaseCreate1
				respBytes, _ := protojson.Marshal(&pb.CreateTicketResponse{TicketId: "pending-act-ticket"})
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewReader(respBytes)),
				}, nil
			default:
				close(create2Entered)
				<-releaseCreate2
				respBytes, _ := protojson.Marshal(&pb.CreateTicketResponse{TicketId: "post-shutdown-ticket"})
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

	// Ticket 0: enters /tickets:activate and holds ActivateTicketsWithResults open until releaseActivate.
	q.ClientRequestChan <- &ClientRequest{Ticket: &pb.Ticket{}, ResultChan: nil}
	select {
	case <-activateEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for seed ticket to enter /tickets:activate")
	}

	// Ticket 1: occupies concurrency slot 1 inside /tickets.
	res1 := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{Ticket: &pb.Ticket{}, ResultChan: res1}
	select {
	case <-create1Entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Ticket 1 to enter /tickets")
	}

	// Ticket 2: occupies concurrency slot 2 inside /tickets.
	res2 := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{Ticket: &pb.Ticket{}, ResultChan: res2}
	select {
	case <-create2Entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Ticket 2 to enter /tickets")
	}

	// Ticket 3: read from unbuffered ClientRequestChan and blocked waiting for a concurrency slot.
	res3 := make(chan int, 1)
	q.ClientRequestChan <- &ClientRequest{Ticket: &pb.Ticket{}, ResultChan: res3}

	// Cancel ctx: Ticket 3 is waiting for a concurrency slot.
	cancel()

	// 1) Request waiting for a concurrency slot must unblock with 408 RequestTimeout.
	select {
	case code := <-res3:
		if code != http.StatusRequestTimeout {
			t.Fatalf("slot-waiter status = %d, want %d", code, http.StatusRequestTimeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for slot-blocked request to unblock with 408")
	}

	// 2) Release Ticket 1 while ActivateTicketsWithResults is still held open on Ticket 0,
	// so Ticket 1 is stored in q.Tickets and pendingActivations before activationResults closes.
	close(releaseCreate1)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, exists := q.Tickets.Load("pending-act-ticket"); exists {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, exists := q.Tickets.Load("pending-act-ticket"); !exists {
		t.Fatal("expected pending-act-ticket to be stored in q.Tickets before activation shutdown completes")
	}

	// Now release Ticket 0's activation call so ActivateTicketsWithResults exits and closes activationResults,
	// draining pendingActivations with 408 and removing pending-act-ticket from q.Tickets.
	close(releaseActivate)

	select {
	case code := <-res1:
		if code != http.StatusRequestTimeout {
			t.Fatalf("pending-activation waiter status = %d, want %d", code, http.StatusRequestTimeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pending-activation waiter to drain with 408")
	}

	if _, exists := q.Tickets.Load("pending-act-ticket"); exists {
		t.Fatal("expected pending-act-ticket to be removed from q.Tickets on shutdown")
	}

	// 3) Release Ticket 2 after activation shutdown has set activationClosed = true;
	// it must immediately drain with 408 and not remain in q.Tickets.
	close(releaseCreate2)

	select {
	case code := <-res2:
		if code != http.StatusRequestTimeout {
			t.Fatalf("in-flight request status = %d, want %d", code, http.StatusRequestTimeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight request to drain with 408")
	}

	if _, exists := q.Tickets.Load("post-shutdown-ticket"); exists {
		t.Fatal("expected post-shutdown-ticket not to remain in q.Tickets")
	}

	sender.Stop()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for q.Run to exit")
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
	q.SetTestTicketConfig(50, func(_ context.Context) *pb.Ticket {
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

	expectedStatuses := []int{http.StatusOK, http.StatusOK, http.StatusInternalServerError}
	for i, wantStatus := range expectedStatuses {
		resChan := make(chan int, 1)
		q.ClientRequestChan <- &ClientRequest{
			Ticket:     &pb.Ticket{},
			ResultChan: resChan,
		}
		select {
		case got := <-resChan:
			if got != wantStatus {
				t.Fatalf("request %d status = %d, want %d", i+1, got, wantStatus)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for request %d result", i+1)
		}
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("reader.Collect failed: %v", err)
	}

	var foundActivations, foundFailures bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "mmqueue.ticket.activation.requests", "mmqueue.ticket.activations.per_call":
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
	if code := <-resChan; code != http.StatusOK {
		t.Fatalf("ResultChan status = %d, want %d", code, http.StatusOK)
	}

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

	var assignmentsVal, deletionFailuresVal int64
	var queuedCount uint64
	var queuedSum float64

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "mmqueue.ticket.assignments":
				if sum, ok := m.Data.(metricdata.Sum[int64]); ok && len(sum.DataPoints) > 0 {
					assignmentsVal = sum.DataPoints[0].Value
				}
			case "mmqueue.ticket.deletion.failures":
				if sum, ok := m.Data.(metricdata.Sum[int64]); ok && len(sum.DataPoints) > 0 {
					deletionFailuresVal = sum.DataPoints[0].Value
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
	if deletionFailuresVal != 2 {
		t.Fatalf("mmqueue.ticket.deletion.failures = %d, want 2", deletionFailuresVal)
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
	got1 := append([]string(nil), q1Tickets...)
	mu1.Unlock()

	mu2.Lock()
	got2 := append([]string(nil), q2Tickets...)
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
