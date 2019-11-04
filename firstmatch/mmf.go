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
	"errors"
	"fmt"
	"net"
	"time"

	"open-match.dev/open-match-ecosystem/demoui"
	"open-match.dev/open-match/pkg/matchfunction"

	"google.golang.org/grpc"
	"open-match.dev/open-match/pkg/pb"
)

const (
	matchName      = "a-simple-1v1-matchfunction"
	mmlogicAddress = "om-mmlogic.open-match.svc.cluster.local:50503"
)

func runMmf(update demoui.SetFunc) {
	update("Initializing")

	mmlogicClient, err := grpc.Dial(mmlogicAddress, grpc.WithInsecure())
	if err != nil {
		update(err.Error())
		return
	}
	defer mmlogicClient.Close()

	mmf := &mmf{
		update:  update,
		mmlogic: pb.NewMmLogicClient(mmlogicClient),
	}

	server := grpc.NewServer()
	pb.RegisterMatchFunctionServer(server, mmf)

	ln, err := net.Listen("tcp", ":50502")
	if err != nil {
		update(err.Error())
		return
	}

	update("Serving started")
	err = server.Serve(ln)
	update(fmt.Sprintf("Error servering grpc: %s", err.Error()))
}

type mmf struct {
	update  demoui.SetFunc
	mmlogic pb.MmLogicClient
}

func (mmf *mmf) Run(req *pb.RunRequest, stream pb.MatchFunction_RunServer) error {
	poolTickets, err := matchfunction.QueryPools(stream.Context(), mmf.mmlogic, req.GetProfile().GetPools())
	if err != nil {
		return err
	}

	tickets, ok := poolTickets["Everyone"]
	if !ok {
		return errors.New("Expected pool named Everyone.")
	}

	t := time.Now().Format("2006-01-02T15:04:05.00")

	matchesFound := 0
	for i := 0; i+1 < len(tickets); i += 2 {
		proposal := &pb.Match{
			MatchId:       fmt.Sprintf("profile-%s-time-%s-num-%d", req.Profile.Name, t, i/2),
			MatchProfile:  req.Profile.Name,
			MatchFunction: "first-match-mmf",
			Tickets: []*pb.Ticket{
				tickets[i], tickets[i+1],
			},
		}
		matchesFound++

		err := stream.Send(&pb.RunResponse{Proposal: proposal})
		if err != nil {
			return err
		}
	}

	mmf.update(fmt.Sprintf("Last run created %d matches", matchesFound))

	return nil
}
