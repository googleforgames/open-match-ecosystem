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
	"io"
	"math/rand"
	"time"

	"google.golang.org/grpc"

	"open-match.dev/open-match-ecosystem/demoui"
	"open-match.dev/open-match/pkg/pb"
)

func runDirector(update demoui.SetFunc) {
	for {
		run(update)
	}
}

type statusDirector struct {
	Status        string
	LatestMatches []*pb.Match `json:",omitempty"`
}

func run(update demoui.SetFunc) {
	defer func() {
		r := recover()
		if r != nil {
			err, ok := r.(error)
			if !ok {
				err = fmt.Errorf("pkg: %v", r)
			}

			update(statusDirector{Status: fmt.Sprintf("Encountered error: %s", err.Error())})
			time.Sleep(time.Second * 10)
		}
	}()

	s := statusDirector{}

	//////////////////////////////////////////////////////////////////////////////
	s.Status = "Connecting to backend"
	update(s)

	// See https://open-match.dev/site/docs/guides/api/
	conn, err := grpc.Dial("om-backend.open-match.svc.cluster.local:50505", grpc.WithInsecure())
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	be := pb.NewBackendClient(conn)

	//////////////////////////////////////////////////////////////////////////////
	s.Status = "Match Match: Sending Request"
	update(s)

	var matches []*pb.Match
	{
		req := &pb.FetchMatchesRequest{
			Config: &pb.FunctionConfig{
				Host: "om-demo.open-match-firstmatch.svc.cluster.local",
				Port: 50502,
				Type: pb.FunctionConfig_GRPC,
			},
			Profiles: []*pb.MatchProfile{
				{
					Name: "1v1",
					Pools: []*pb.Pool{
						{
							Name: "Everyone",
						},
					},
				},
			},
		}

		stream, err := be.FetchMatches(context.Background(), req)
		if err != nil {
			panic(err)
		}

		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				panic(err)
			}
			matches = append(matches, resp.GetMatch())
		}
	}

	//////////////////////////////////////////////////////////////////////////////
	s.Status = "Matches Found"
	s.LatestMatches = matches
	update(s)

	//////////////////////////////////////////////////////////////////////////////
	s.Status = "Assigning Players"
	update(s)

	for _, match := range matches {
		ids := []string{}

		for _, t := range match.Tickets {
			ids = append(ids, t.Id)
		}

		req := &pb.AssignTicketsRequest{
			TicketIds: ids,
			Assignment: &pb.Assignment{
				Connection: fmt.Sprintf("%d.%d.%d.%d:2222", rand.Intn(256), rand.Intn(256), rand.Intn(256), rand.Intn(256)),
			},
		}

		resp, err := be.AssignTickets(context.Background(), req)
		if err != nil {
			panic(err)
		}

		_ = resp
	}

	//////////////////////////////////////////////////////////////////////////////
	s.Status = "Sleeping"
	update(s)

	time.Sleep(time.Second * 5)
}
