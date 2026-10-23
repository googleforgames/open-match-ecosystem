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

// ChannelSender sends assignments to a Go channel.
type ChannelSender struct {
	assignmentsChan chan<- *pb.Roster
	mu              sync.RWMutex
	stopped         bool
	initOnce        sync.Once
	stopOnce        sync.Once
	stopChan        chan struct{}
}

func NewChannelSender(ch chan<- *pb.Roster) *ChannelSender {
	return &ChannelSender{
		assignmentsChan: ch,
		stopChan:        make(chan struct{}),
	}
}

func (p *ChannelSender) ensureInit() {
	p.initOnce.Do(func() {
		if p.stopChan == nil {
			p.stopChan = make(chan struct{})
		}
	})
}

func (p *ChannelSender) Send(ctx context.Context, roster *pb.Roster) error {
	p.ensureInit()
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.stopped {
		return ErrSenderStopped
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.stopChan:
		return ErrSenderStopped
	case p.assignmentsChan <- roster:
		return nil
	}
}

func (p *ChannelSender) Stop() {
	p.ensureInit()
	p.stopOnce.Do(func() {
		close(p.stopChan)
		p.mu.Lock()
		p.stopped = true
		if p.assignmentsChan != nil {
			close(p.assignmentsChan)
		}
		p.mu.Unlock()
	})
}

// ChannelReceiver receives assignments from a Go channel.
type ChannelReceiver struct {
	assignmentsChan <-chan *pb.Roster
	initOnce        sync.Once
	stopOnce        sync.Once
	stopChan        chan struct{}
}

func NewChannelReceiver(ch <-chan *pb.Roster) *ChannelReceiver {
	return &ChannelReceiver{
		assignmentsChan: ch,
		stopChan:        make(chan struct{}),
	}
}

func (r *ChannelReceiver) ensureInit() {
	r.initOnce.Do(func() {
		if r.stopChan == nil {
			r.stopChan = make(chan struct{})
		}
	})
}

func (r *ChannelReceiver) Receive(ctx context.Context, handler func(ctx context.Context, roster *pb.Roster)) error {
	r.ensureInit()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-r.stopChan:
			return nil
		case roster, ok := <-r.assignmentsChan:
			if !ok {
				return nil
			}
			handler(ctx, roster)
		}
	}
}

func (r *ChannelReceiver) Stop() {
	r.ensureInit()
	r.stopOnce.Do(func() {
		close(r.stopChan)
	})
}
