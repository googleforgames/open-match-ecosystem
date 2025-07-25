package assignmentdistributor

import (
	"context"
	"github.com/googleforgames/open-match2/v2/pkg/pb"
)

// Sender defines the interface for sending roster assignments.
type Sender interface {
	Send(ctx context.Context, roster *pb.Roster) error
	Stop()
}

// Receiver defines the interface for receiving roster assignments.
type Receiver interface {
	Receive(ctx context.Context, handler func(ctx context.Context, roster *pb.Roster)) error
}