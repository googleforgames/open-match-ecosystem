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
	"sync"

	"github.com/googleforgames/open-match2/v2/pkg/pb"
)

// ErrSenderStopped is returned when attempting to send an assignment on a stopped ChannelSender.
var ErrSenderStopped = errors.New("assignment channel sender is stopped")

// ChannelSender sends assignments to a Go channel. It is only useful when the
// director and the matchmaking queue run in the same process. The caller owns
// the channel, and Stop does not close it, because the channel is shared with a
// ChannelReceiver (closing a channel that something else might still send on,
// or close, panics); receivers stop on their own Stop or context instead.
type ChannelSender struct {
	rosterCh chan<- *pb.Roster
	stopOnce sync.Once
	stopped  chan struct{}
}

// NewChannelSender returns a ChannelSender that sends on ch.
func NewChannelSender(ch chan<- *pb.Roster) *ChannelSender {
	return &ChannelSender{
		rosterCh: ch,
		stopped:  make(chan struct{}),
	}
}

// Send blocks until the roster is sent, the context is done, or the sender is stopped.
func (s *ChannelSender) Send(ctx context.Context, roster *pb.Roster) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Check the stop signal before the blocking select so a stopped sender
	// never sends even if the channel has buffer space (a select picks
	// randomly among ready cases).
	select {
	case <-s.stopped:
		return ErrSenderStopped
	default:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopped:
		return ErrSenderStopped
	case s.rosterCh <- roster:
		return nil
	}
}

// Stop makes current and future Sends return ErrSenderStopped and is safe to call more than once.
func (s *ChannelSender) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopped)
	})
}

// ChannelReceiver receives assignments from a Go channel.
type ChannelReceiver struct {
	rosterCh <-chan *pb.Roster
	stopOnce sync.Once
	stopped  chan struct{}
}

// NewChannelReceiver returns a ChannelReceiver that receives from ch.
func NewChannelReceiver(ch <-chan *pb.Roster) *ChannelReceiver {
	return &ChannelReceiver{
		rosterCh: ch,
		stopped:  make(chan struct{}),
	}
}

// Receive calls handler for each roster until ctx is done, the receiver is stopped, or the channel is closed.
func (r *ChannelReceiver) Receive(ctx context.Context, handler func(ctx context.Context, roster *pb.Roster)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.stopped:
			return nil
		case roster, ok := <-r.rosterCh:
			if !ok {
				return nil
			}
			handler(ctx, roster)
		}
	}
}

// Stop makes Receive return; safe to call more than once.
func (r *ChannelReceiver) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopped)
	})
}
