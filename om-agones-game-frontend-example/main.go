package gamefrontend

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"open-match.dev/open-match/pkg/pb"
)

func createOMFrontendClient() (pb.FrontendClient, func() error) {
	conn, err := grpc.Dial("om-frontend.open-match.svc.cluster.local:50504", grpc.WithInsecure())
	if err != nil {
		panic(err)
	}
	return pb.NewFrontendClient(conn), conn.Close
}

func createOMTicket() *pb.Ticket {
	return &pb.Ticket{
		SearchFields: &pb.SearchFields{
			DoubleArgs: map[string]float64{"defense": 10, "attack": 20, "level": 50},
			StringArgs: map[string]string{"guildName": "open-match", "location": "alterac"},
			Tags:       []string{"beta-gameplay", "demo-map"},
		},
	}
}

// doGameQueue accepts a channel that represents status of a player in the queue.
// An openned channel means the player is actively seeking for a game match, while
// a closed channel means the player exited the game queue.
func doGameQueue(c chan bool) {
	fc, closer := createOMFrontendClient()
	defer closer()

	resp, err := fc.CreateTicket(context.Background(), &pb.CreateTicketRequest{Ticket: createOMTicket()})
	if err != nil {
		panic(err.Error())
	}

	// Each channel represents a player waiting for a game, the channel will be closed if the player exit the queue
	tid := resp.GetTicket().GetId()
	for {
		// TODO: once we find a game, have the game send something to this channel once the player starts a match making
		_, ok := <-c

		ticket, err := fc.GetTicket(context.Background(), &pb.GetTicketRequest{TicketId: tid})
		if err != nil {
			panic(err.Error())
		}

		tconn := ticket.GetAssignment().GetConnection()
		// channel is ok. player is still in the queue and waiting for an assignment.
		if ok && tconn == "" {
			// wait for the next check
			time.Sleep(1 * time.Second)
			continue
		}

		// otherwise player is not interested in joining a match anymore, delete the ticket
		defer fc.DeleteTicket(context.Background(), &pb.DeleteTicketRequest{TicketId: tid})
		if ok { // player got an assignment,
			fmt.Printf("a player got assigned to %v\n", tconn)
		} else if tconn != "" {
			// player left the queue after getting an assignment, need to recycle the staled game server.
			// we assume this won't happen in a workshop scenario.
			fmt.Printf("oops, we got a staled game server\n")
		} else { // player left the queue without being assign.
			fmt.Printf("oops, player rage quit after waiting so long\n")
		}
		return
	}
}
