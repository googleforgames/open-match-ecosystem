// Copyright 2025 Google LLC
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
package assignmentdistributor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/googleforgames/open-match2/v2/pkg/pb"
)

func TestChannelReceiveBlockingAndOrderedDelivery(t *testing.T) {
	ch := make(chan *pb.Roster)
	sender := NewChannelSender(ch)
	receiver := NewChannelReceiver(ch)

	ctx := context.Background()
	var (
		mu       sync.Mutex
		received []string
	)
	allDelivered := make(chan struct{})
	receiveDone := make(chan error, 1)

	expected := []string{"roster-1", "roster-2", "roster-3"}

	go func() {
		receiveDone <- receiver.Receive(ctx, func(_ context.Context, roster *pb.Roster) {
			mu.Lock()
			received = append(received, roster.GetName())
			if len(received) == len(expected) {
				close(allDelivered)
			}
			mu.Unlock()
		})
	}()

	// Verify Receive blocks initially before any stop signal.
	select {
	case err := <-receiveDone:
		t.Fatalf("Receive returned prematurely before Stop: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	for _, name := range expected {
		if err := sender.Send(ctx, &pb.Roster{Name: name}); err != nil {
			t.Fatalf("Send(%q) failed: %v", name, err)
		}
	}

	select {
	case <-allDelivered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rosters to be delivered")
	}

	mu.Lock()
	if len(received) != len(expected) {
		t.Fatalf("received %d rosters, want %d", len(received), len(expected))
	}
	for i, want := range expected {
		if received[i] != want {
			t.Fatalf("received[%d] = %q, want %q", i, received[i], want)
		}
	}
	mu.Unlock()

	// Verify Receive continues blocking until Stop is invoked.
	select {
	case err := <-receiveDone:
		t.Fatalf("Receive returned before Stop was called: %v", err)
	default:
	}

	// Verify Stop is idempotent on both receiver and sender.
	receiver.Stop()
	receiver.Stop()

	select {
	case err := <-receiveDone:
		if err != nil {
			t.Fatalf("Receive returned non-nil error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Receive to unblock after receiver.Stop()")
	}

	sender.Stop()
	sender.Stop()
}

func TestChannelSendContextCancellation(t *testing.T) {
	// Pre-cancelled context must fail immediately even if the channel has buffer space.
	bufferedCh := make(chan *pb.Roster, 1)
	bufferedSender := NewChannelSender(bufferedCh)
	defer bufferedSender.Stop()

	preCanceledCtx, preCancel := context.WithCancel(context.Background())
	preCancel()

	if err := bufferedSender.Send(preCanceledCtx, &pb.Roster{Name: "pre-canceled"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Send with pre-cancelled context returned %v, want %v", err, context.Canceled)
	}
	select {
	case r := <-bufferedCh:
		t.Fatalf("unexpected roster sent on pre-cancelled context: %v", r)
	default:
	}

	// In-flight context cancellation must unblock Send on an unbuffered channel.
	unbufferedCh := make(chan *pb.Roster)
	unbufferedSender := NewChannelSender(unbufferedCh)
	defer unbufferedSender.Stop()

	inFlightCtx, inFlightCancel := context.WithCancel(context.Background())
	sendErr := make(chan error, 1)
	started := make(chan struct{})

	go func() {
		close(started)
		sendErr <- unbufferedSender.Send(inFlightCtx, &pb.Roster{Name: "in-flight"})
	}()

	<-started
	time.Sleep(20 * time.Millisecond)
	inFlightCancel()

	select {
	case err := <-sendErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("in-flight cancelled Send returned %v, want %v", err, context.Canceled)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight cancelled Send to return")
	}
}

func TestChannelSenderStopUnblocksAndConcurrentRace(t *testing.T) {
	// Verify Stop unblocks a Send blocked on an unbuffered channel without deadlocking.
	unbufferedCh := make(chan *pb.Roster)
	sender := NewChannelSender(unbufferedCh)

	sendErr := make(chan error, 1)
	started := make(chan struct{})

	go func() {
		close(started)
		sendErr <- sender.Send(context.Background(), &pb.Roster{Name: "blocked-roster"})
	}()

	<-started
	time.Sleep(20 * time.Millisecond)

	stopDone := make(chan struct{})
	go func() {
		sender.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("sender.Stop() deadlocked while Send was blocked on unbuffered channel")
	}

	select {
	case err := <-sendErr:
		if !errors.Is(err, ErrSenderStopped) {
			t.Fatalf("blocked Send returned %v, want %v", err, ErrSenderStopped)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for blocked Send to return after Stop")
	}

	// Subsequent sends after Stop must immediately return ErrSenderStopped.
	if err := sender.Send(context.Background(), &pb.Roster{Name: "subsequent"}); !errors.Is(err, ErrSenderStopped) {
		t.Fatalf("subsequent Send after Stop returned %v, want %v", err, ErrSenderStopped)
	}

	// 25-iteration race test with 16 concurrent senders racing against Stop and context cancellation.
	const (
		iterations = 25
		numSenders = 16
	)
	for iter := 0; iter < iterations; iter++ {
		ch := make(chan *pb.Roster, 2)
		iterSender := NewChannelSender(ch)
		iterReceiver := NewChannelReceiver(ch)

		ctx, cancel := context.WithCancel(context.Background())
		recvDone := make(chan error, 1)
		go func() {
			recvDone <- iterReceiver.Receive(ctx, func(_ context.Context, _ *pb.Roster) {})
		}()

		startGate := make(chan struct{})
		var wg sync.WaitGroup
		errChan := make(chan error, numSenders*4)

		for s := 0; s < numSenders; s++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				<-startGate
				for j := 0; j < 4; j++ {
					err := iterSender.Send(ctx, &pb.Roster{Name: fmt.Sprintf("iter-%d-sender-%d-%d", iter, id, j)})
					if err != nil && !errors.Is(err, ErrSenderStopped) && !errors.Is(err, context.Canceled) {
						errChan <- err
						return
					}
				}
			}(s)
		}

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-startGate
			iterSender.Stop()
		}()
		go func() {
			defer wg.Done()
			<-startGate
			cancel()
			iterReceiver.Stop()
		}()

		close(startGate)
		wg.Wait()
		iterSender.Stop()
		iterReceiver.Stop()
		cancel()

		select {
		case err := <-errChan:
			t.Fatalf("iteration %d: unexpected error from concurrent Send: %v", iter, err)
		default:
		}

		select {
		case err := <-recvDone:
			if err != nil {
				t.Fatalf("iteration %d: Receive returned unexpected error: %v", iter, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: timed out waiting for Receive to finish", iter)
		}
	}
}

func TestChannelSenderStopDoesNotCloseCallerChannel(t *testing.T) {
	ch := make(chan *pb.Roster, 1)
	sender := NewChannelSender(ch)

	sender.Stop()

	if err := sender.Send(context.Background(), &pb.Roster{Name: "after-stop"}); !errors.Is(err, ErrSenderStopped) {
		t.Fatalf("Send after Stop returned %v, want %v", err, ErrSenderStopped)
	}

	select {
	case r := <-ch:
		t.Fatalf("stopped sender sent unexpected roster despite free buffer space: %v", r)
	default:
	}

	// As the channel's owner, sending a roster and closing the channel must not panic.
	ch <- &pb.Roster{Name: "owner-roster"}
	close(ch)
}
