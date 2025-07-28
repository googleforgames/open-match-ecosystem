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
	"github.com/googleforgames/open-match2/v2/pkg/pb"
)

// ChannelSender sends assignments to a Go channel.
type ChannelSender struct {
	assignmentsChan chan<- *pb.Roster
}

func NewChannelSender(ch chan<- *pb.Roster) *ChannelSender {
	return &ChannelSender{assignmentsChan: ch}
}

func (p *ChannelSender) Send(ctx context.Context, roster *pb.Roster) error {
	p.assignmentsChan <- roster
	return nil
}

func (p *ChannelSender) Stop() {
	close(p.assignmentsChan)
}

// ChannelReceiver receives assignments from a Go channel.
type ChannelReceiver struct {
	assignmentsChan <-chan *pb.Roster
}

func NewChannelReceiver(ch <-chan *pb.Roster) *ChannelReceiver {
	return &ChannelReceiver{assignmentsChan: ch}
}

func (r *ChannelReceiver) Receive(ctx context.Context, handler func(ctx context.Context, roster *pb.Roster)) error {
	go func() {
		for roster := range r.assignmentsChan {
			handler(ctx, roster)
		}
	}()
	return nil
}

// For a channel, this is a no-op.
func (r *ChannelReceiver) Stop() { }
