// Copyright 2024 Google LLC
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

// Package soloduel provides a sample match function that makes set up 1v1
// matches using a first-in-first-out (FIFO) strategy.
//
// This sample is a reference to demonstrate the usage of
// the mmf gRPC server and should be used as a starting point for your match
// function. You will need to modify the matchmaking logic in this function
// based on your game's requirements.
//
// This implements approximately the same matchmaking logic as the Open Match
// 1.8 example match functions provided in these Open Match 1.8 files:
// - examples/scale/scenarios/firstmatch and
// - examples/functions/golang/soloduel
//
// If you're trying to build this MMF into a container image to deploy on
// a serverless platform, your starting point should be
// github.com/googleforgames/open-match-ecosystem/v2/examples/mmf/main.go
package fifo

import (
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	knownpb "google.golang.org/protobuf/types/known/wrapperspb"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/examples/mmf/server"
)

const (
	matchName   = "a-simple-1v1-matchfunction"
	tixPerMatch = 2
)

var (
	// Default logger.
	defaultLoggingFields = logrus.Fields{
		"app":       "open_match",
		"component": "matchmaking_function",
		"function":  "soloduel",
	}
	log = logrus.New()
)

// Use the golang programming pattern of a private struct, forcing someone using
// this module to use New() or NewWithLogger() to instantiate the object. Doing
// it this way lets us set have an overridable, default logger.

// Private struct
type mmfServer struct {
	pb.UnimplementedMatchMakingFunctionServiceServer
	logger *logrus.Entry
}

// New instantiates the MMF Server using default logging
func New() *mmfServer {
	return &mmfServer{
		// use default logger.
		logger: log.WithFields(defaultLoggingFields),
	}
}

// New instantiates the MMF Server using the provided logger and adds default
// structured logging fields
func NewWithLogger(l *logrus.Logger) *mmfServer {
	return &mmfServer{
		// use default logger.
		logger: l.WithFields(defaultLoggingFields),
	}
}

// Run is this match function's implementation of the gRPC call defined in
// proto/v2/mmf.proto.  This is where your custom matching logic goes.
func (s *mmfServer) Run(stream pb.MatchMakingFunctionService_RunServer) error {

	// Use the helper function to reconstruct the incoming request from partial
	// 'chunked' requests.
	req, err := server.GetChunkedRequest(stream)
	if err != nil {
		s.logger.Errorf("error getting chunked request: %v", err)
	}
	s.logger.Infof("Generating matches for profile %v", req.GetName())

	// Process tickets for the pools specified in the Match Profile.
	// In this example `soloduel` game mode, any player can be matched with any
	// other player, so just concatinate all the pools together.
	tickets := []*pb.Ticket{}
	for pname, pool := range req.GetPools() {
		for _, ticket := range pool.GetParticipants().GetTickets() {
			tickets = append(tickets, ticket)
		}
		s.logger.Debugf("Found %v tickets in pool %v", len(tickets), pname)
	}
	s.logger.Debugf("Matching among %v tickets from %v provided pools", len(tickets), len(req.GetPools()))

	t := time.Now().Format("2006-01-02T15:04:05.00")

	// We'll make 1v1 sessions, so each match will contain a roster of 2 players.
	rosterPlayers := make([]*pb.Ticket, 0, tixPerMatch)
	matchNum := 0

	// NOTE: This function is meant to be easy to read and understand. It is not optimized for performance.
	for _, ticket := range tickets {
		s.logger.Debugf("FIFO sample, adding next ticket id %v to match %v", ticket.Id, matchNum)
		rosterPlayers = append(rosterPlayers, ticket)

		if len(rosterPlayers) >= tixPerMatch {

			// Initialize an empty roster map and roster name
			rName := fmt.Sprintf("%v_roster%04d", matchName, matchNum)
			rosters := make(map[string]*pb.Roster)

			// Example of adding data to the roster extension field.
			rosterExtensions := make(map[string]*anypb.Any)
			rosterExtensions["CreationTime"], err = anypb.New(timestamppb.Now())
			if err != nil {
				s.logger.Errorf("Unable to create 'CreationTime' extension for outgoing roster %v", rName)
			}

			// This example game type only returns one roster per match.
			// Add the matched player roster to the roster map.
			rosters[rName] = &pb.Roster{
				Name:       rName,
				Tickets:    rosterPlayers,
				Extensions: rosterExtensions,
			}

			// It is best practice to copy all extensions in the profile over to each outgoing match.
			matchExtensions := req.GetExtensions()

			// Examples of adding data to the match extensions field
			id := fmt.Sprintf("profile-%s-time-%s-num-%d", matchName, t, matchNum)
			matchExtensions["score"], err = anypb.New(&knownpb.Int32Value{Value: 100})
			if err != nil {
				s.logger.Errorf("Unable to create 'score' extension for outgoing match %v", id)
			}

			matchExtensions["mmfName"], err = anypb.New(&knownpb.StringValue{Value: matchName})
			if err != nil {
				s.logger.Errorf("Unable to create 'mmfName' extension for outgoing match %v", id)
			}

			matchExtensions["profileName"], err = anypb.New(&knownpb.StringValue{Value: req.Name})
			if err != nil {
				s.logger.Errorf("Unable to create 'profileName' extension for outgoing match %v", id)
			}
			s.logger.Debugf("Streaming match '%v' back to om-core with roster of %v tickets", id, len(rosterPlayers))

			// Stream the generated match back to Open Match.
			err = stream.Send(&pb.StreamedMmfResponse{Match: &pb.Match{
				Id:         id,
				Rosters:    rosters,
				Extensions: matchExtensions,
			}})
			if err != nil {
				s.logger.Debugf("Failed to stream proposal to Open Match, got %s", err.Error())
				return err
			}

			// Re-initialize the roster variable for the next match.
			rosterPlayers = make([]*pb.Ticket, 0, 2)
			matchNum++
		}
	}
	s.logger.Infof("Total of %v matches returned", matchNum)

	return nil
}
