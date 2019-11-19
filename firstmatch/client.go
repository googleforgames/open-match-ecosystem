// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"google.golang.org/grpc"
	"open-match.dev/open-match-ecosystem/demoui"
	"open-match.dev/open-match/pkg/pb"
)

func runClients(update demoui.SetFunc) {
	u := demoui.NewNested(context.Background(), update)

	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("fakeplayer_%d", i)
		go func() {
			for {
				runScenario(name, u.ForField(name))
			}
		}()
	}
}

type statusClient struct {
	Status     string
	Error      string         `json:",omitempty"`
	Assignment *pb.Assignment `json:",omitempty"`
}

func runScenario(name string, update demoui.SetFunc) {
	s := statusClient{}

	defer func() {
		r := recover()
		if r != nil {
			err, ok := r.(error)
			if !ok {
				err = fmt.Errorf("pkg: %v", r)
			}
			s.Error = fmt.Sprintf("Encountered error: %s", err.Error())

			update(s)
			time.Sleep(time.Second * 10)
		}
	}()

	//////////////////////////////////////////////////////////////////////////////
	s.Status = "Main Menu"
	update(s)

	time.Sleep(time.Duration(rand.Int63()) % (time.Second * 15))

	//////////////////////////////////////////////////////////////////////////////
	s.Status = "Connecting to Open Match frontend"
	update(s)

	// See https://open-match.dev/site/docs/guides/api/
	conn, err := grpc.Dial("om-frontend.open-match.svc.cluster.local:50504", grpc.WithInsecure())
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	fe := pb.NewFrontendClient(conn)

	//////////////////////////////////////////////////////////////////////////////
	s.Status = "Creating Open Match Ticket"
	update(s)

	var ticketId string
	{
		req := &pb.CreateTicketRequest{
			Ticket: &pb.Ticket{},
		}

		resp, err := fe.CreateTicket(context.Background(), req)
		if err != nil {
			panic(err)
		}
		ticketId = resp.Ticket.Id
	}

	//////////////////////////////////////////////////////////////////////////////
	s.Status = fmt.Sprintf("Waiting match with ticket Id %s", ticketId)
	update(s)

	var assignment *pb.Assignment
	{
		req := &pb.GetAssignmentsRequest{
			TicketId: ticketId,
		}

		stream, err := fe.GetAssignments(context.Background(), req)
		for assignment.GetConnection() == "" {
			resp, err := stream.Recv()
			if err != nil {
				// For now we don't expect to get EOF, so that's still an error worthy of panic.
				panic(err)
			}

			assignment = resp.Assignment
		}

		err = stream.CloseSend()
		if err != nil {
			panic(err)
		}
	}

	//////////////////////////////////////////////////////////////////////////////
	s.Status = "Sleeping (pretend this is playing a match...)"
	s.Assignment = assignment
	update(s)

	time.Sleep(time.Second * 10)
}
