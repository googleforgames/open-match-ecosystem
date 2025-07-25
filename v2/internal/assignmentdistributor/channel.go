package assignmentdistributor

import (
	"context"
	"github.com/googleforgames/open-match2/v2/pkg/pb"
)

// ChannelPublisher sends assignments to a Go channel.
type ChannelSender struct {
	assignmentsChan chan<- *pb.Roster
}

func NewChannelSender (ch chan<- *pb.Roster) *ChannelPublisher {
	return &ChannelSender {assignmentsChan: ch}
}

func (p *ChannelSender ) Send(ctx context.Context, roster *pb.Roster) error {
	p.assignmentsChan <- roster
	return nil
}

func (p *ChannelPublisher) Stop() {
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
