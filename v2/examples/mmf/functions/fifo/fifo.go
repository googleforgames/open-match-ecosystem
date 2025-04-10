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

// Package fifo provides a sample match function that makes matches using a
// first-in-first-out (FIFO) strategy. It can read a few basic parameters from
// the pb.Profile.Extensions field of the incoming request to determine how
// many tickets to match together, and how many Rosters to put those tickets
// on. NOTE: it has no affordances for pre-made groups (players who should be
// matched together as a unit), as this is outside the scope of this example.
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
//
// NOTE: This function is meant to be easy to read and understand. It is not
// optimized for performance.

package fifo

import (
	"fmt"
	"math"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	knownpb "google.golang.org/protobuf/types/known/wrapperspb"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/examples/mmf/server"
	"open-match.dev/open-match-ecosystem/v2/internal/extensions"
)

const (
	matchName = "simple-fifo-matchfunction"
)

var (
	// Default logger.
	defaultLoggingFields = logrus.Fields{
		"app":       "matchmaker",
		"component": "matchmaking_function",
		"strategy":  "fifo",
	}
	log = logrus.New()

	// Default values for the params for this function.
	desiredNumRosters = 1
	desiredRosterLen  = 2
	minRosterLen      = 2
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
		// use default logging fields with provided logger.
		logger: l.WithFields(defaultLoggingFields),
	}
}

// Run is this match function's implementation of the gRPC call defined in
// proto/v2/mmf.proto. This is the function you will want to customize.
func (s *mmfServer) Run(stream pb.MatchMakingFunctionService_RunServer) error {

	startTime := time.Now()

	// Use the helper function to reconstruct the incoming request from partial
	// 'chunked' requests.
	req, err := server.GetChunkedRequest(stream)
	if err != nil {
		s.logger.Errorf("error getting chunked request: %v", err)
	}
	s.logger.Infof("Generating matches for profile %v", req.GetName())

	// Process tickets for the pools specified in the Match Profile.  In this
	// example fifo matching strategy, any player can be matched with any other
	// player, so just concatinate all the pools together.
	tickets := []*pb.Ticket{}
	for pname, pool := range req.GetPools() {
		for _, ticket := range pool.GetParticipants().GetTickets() {
			tickets = append(tickets, ticket)
		}
		s.logger.Debugf("Found %v tickets in pool %v", len(tickets), pname)
	}
	s.logger.Debugf("Matching among %v tickets from %v provided pools", len(tickets), len(req.GetPools()))

	t := time.Now().Format("2006-01-02T15:04:05.00")

	// Retrieve custom parameters we put into the request profile. If they
	// don't exist, we just fall back to the defaults declared in the var
	// section near the beginning of this file.
	exDesiredNumRosters, err := extensions.Int32(req.GetExtensions(), "desiredNumRosters")
	if err == nil {
		desiredNumRosters = exDesiredNumRosters
	}
	exDesiredRosterLen, err := extensions.Int32(req.GetExtensions(), "desiredRosterLen")
	if err == nil {
		desiredRosterLen = exDesiredRosterLen
	}
	exMinRosterLen, err := extensions.Int32(req.GetExtensions(), "minRosterLen")
	if err == nil {
		minRosterLen = exMinRosterLen
	}

	matchNum := 0

	// Continue as long as there are enough tickets left to make the desired
	// number of rosters of the requested minmum length
	for len(tickets) > (minRosterLen * desiredNumRosters) {

		match := &pb.Match{
			// Make a unique Match ID.
			//
			// By convention, match names should use reverse-DNS notation
			// https://en.wikipedia.org/wiki/Reverse_domain_name_notation This
			// helps with metric attribute cardinality, as we can record
			// profile names alongside metric readings after stripping off the
			// most-unique portion. Keep any timestamps, 'generation' counters,
			// unique hashes, etc in the part of the profile name _after_ the last
			// dot.
			// https://grafana.com/blog/2022/02/15/what-are-cardinality-spikes-and-why-do-they-matter/
			Id: fmt.Sprintf("profile-%s.time-%s-num-%d", matchName, t, matchNum),
			// Initialize empty roster map.
			Rosters: make(map[string]*pb.Roster),
			// It is best practice to copy all extensions in the profile over
			// to each outgoing match.
			Extensions: req.GetExtensions(),
		}
		// Examples of adding data to the outgoing match's extensions field
		match.Extensions["mmfName"], err = anypb.New(&knownpb.StringValue{Value: matchName})
		if err != nil {
			s.logger.Errorf("Unable to create 'mmfName' extension for match %v", match.Id)
		}

		match.Extensions["profileName"], err = anypb.New(&knownpb.StringValue{Value: req.Name})
		if err != nil {
			s.logger.Errorf("Unable to create 'profileName' extension for match %v", match.Id)
		}

		// Initialize the number of requested rosters
		indexedRosterNames := make([]string, 0, desiredNumRosters)
		for rosterNum := 1; rosterNum <= desiredNumRosters; rosterNum++ {

			// Generate a unique roster name.
			rosterName := fmt.Sprintf("%v.%v_roster%04d", matchName, matchNum, rosterNum)

			// Add this roster name to a list of rosters. This allows us to
			// easily iterate over them when adding tickets, filling the
			// rosters as evenly as possible.
			indexedRosterNames = append(indexedRosterNames, rosterName)

			// Example of adding data to the outgoing match rosters' extension field.
			rosterExtensions := make(map[string]*anypb.Any)
			rosterExtensions["CreationTime"], err = anypb.New(timestamppb.Now())
			if err != nil {
				s.logger.Errorf("Unable to create 'CreationTime' extension for match roster: %v", err)
			}

			// Add an empty player roster to the roster list.
			match.Rosters[rosterName] = &pb.Roster{
				Name:       rosterName,
				Tickets:    make([]*pb.Ticket, 0, desiredRosterLen),
				Extensions: rosterExtensions,
			}
		}

		var score int32 // Used to hold the quality 'score' of this match
		tixCount := 0
		for len(tickets) > 0 {
			// ----------------------------------------------------------------
			// This is where, in an actual MMF, you would write your logic to
			// choose a ticket from the Pool that fits well with the other
			// tickets in the Roster.
			s.logger.Debugf("FIFO sample, adding next ticket id %v to match %v", tickets[0].Id, matchNum)

			// This example uses the indexedRosterNames and a modulo on the
			// current ticket counter to rotate through all rosters, filling
			// them as evenly as possible.
			curRoster := indexedRosterNames[tixCount%desiredNumRosters]
			match.Rosters[curRoster].Tickets = append(match.Rosters[curRoster].Tickets, tickets[0])
			// ----------------------------------------------------------------

			// Remove this ticket from the pool
			tickets = tickets[1:]

			// Quit adding tickets if all rosters are full.
			if !rosterLengthsLessThan(match, desiredRosterLen) {
				// In a real MMF, you'd probably evaluate how well matched the
				// tickets you selected are, and assign a quality 'score' for
				// this match. This can then be sent back to your matchmaker
				// using the pb.Match.Extensions field, so it an decide if this
				// match is better or worse than others.
				score = 100
				break
			}

			// rotate to the next roster for the next ticket
			tixCount++
		}

		// Ran out of tickets before we met the desired roster length for all
		// rosters. Go ahead and return this match, but mark it as having
		// a lower quality 'score' and wanting more players.
		if rosterLengthsLessThan(match, desiredRosterLen) {
			score = 25 // see above comments about the score.

			// ----------------------------------------------------------------
			// If your matchmaker design includes having the matching logic
			// determine what kind of tickets should be added to an existing
			// session on subsequent matchmaking cycles (for example, a
			// backfill, join-in-progress, or high-density game server case),
			// here is where you would construct a profile with all the
			// necessary matching parameters for subsequent requests coming
			// from the game server that hosts this match. Alternatively, if
			// your matchmaker design prefers to determine what kinds of
			// tickets to search for elsewhere - for example, in your game
			// server management software, then you can remove or ignore this
			// section.
			// ----------------------------------------------------------------

			// Generate an updated profile to get more tickets for this match.
			updatedPools := map[string]*pb.Pool{}
			// Copy over the pool filters and extensions used to run this MMF.
			for pname, pool := range req.GetPools() {
				updatedPools[pname] = &pb.Pool{
					Name:                    pool.GetName(),
					TagPresentFilters:       pool.GetTagPresentFilters(),
					StringEqualsFilters:     pool.GetStringEqualsFilters(),
					DoubleRangeFilters:      pool.GetDoubleRangeFilters(),
					CreationTimeRangeFilter: pool.GetCreationTimeRangeFilter(),
					Extensions:              pool.GetExtensions(),
				}
			}
			updatedProfile := &pb.Profile{
				// Name this as a backfill profile so we can see it clearly in logs/metrics.
				Name:       "backfill." + match.GetId(),
				Pools:      updatedPools,
				Extensions: req.GetExtensions(),
			}
			// Next time this profile is used for matching, we will only
			// request as many tickets as we have empty slots in the rosters.
			remainingRostersCapacity := desiredRosterLen - lowestRosterLen(match)
			updatedProfile.Extensions["desiredRosterLen"], err = anypb.New(&knownpb.Int32Value{Value: int32(remainingRostersCapacity)})
			if err != nil {
				s.logger.Errorf("Unable to update the desiredRosterLen to reflect ticket capacity of match %v",
					match.Id)
			}

			// A pb.MmfRequest with a list of MMFs to use for backfills and an empty profile were
			// attached as a request extension by our matchmaker, retrieve it.
			s.logger.Tracef("Retrieving MMF list to invoke when requesting backfill from extension %v", extensions.MMFRequestKey)
			bfRequest, err := extensions.MMFRequest(req.GetExtensions(), extensions.MMFRequestKey)
			if err != nil {
				s.logger.Errorf("Unable to retrieve backfill details from extension %v, returning match without backfill request: err", extensions.MMFRequestKey)
				continue
			}

			// Attach the updated profile with new matching criteria to our new backfill matchmaking request.
			bfRequest.Profile = updatedProfile

			// Add the new backfill matchmaking request to the match extensions
			// field, so it is returned to our matchmaker.
			match.Extensions[extensions.MMFRequestKey], err = anypb.New(bfRequest)
			if err != nil {
				s.logger.Errorf("Unable to create backfill matching profile as an extension for match %v",
					match.Id)
			}
			s.logger.Tracef("Returning match with %v/%v players in a roster; attaching a backfill MMF request: %v", lowestRosterLen(match), desiredRosterLen, bfRequest)
		}

		// Add match quality 'score' extension data to the match. See above
		// comments about the score for more details.
		match.Extensions["score"], err = anypb.New(&knownpb.Int32Value{Value: score})
		if err != nil {
			s.logger.Errorf("Unable to create 'score' extension for match %v", match.Id)
		}
		s.logger.Debugf("Streaming match '%v' back to om-core with roster of %v tickets",
			match.Id, tixCount)

		// Stream the generated match back to Open Match.
		err = stream.Send(&pb.StreamedMmfResponse{Match: match})
		if err != nil {
			s.logger.Debugf("Failed to stream proposal to Open Match, got %s", err.Error())
			return err
		}

		// Keep track of the number of matches sent
		matchNum++
	}
	dur := time.Since(startTime)
	s.logger.Infof("Returning %v matches in %.03f seconds", matchNum, float64(dur.Milliseconds())/1000.0)

	return nil
}

// lowestRosterLength returns the length of the roster with the lowest number of tickets in it.
func lowestRosterLen(match *pb.Match) (length int) {
	length = math.MaxInt32
	for _, roster := range match.Rosters {
		if len(roster.Tickets) < length {
			length = len(roster.Tickets)
		}
	}
	return
}

// rosterLengthsLessThan returns true if all pb.Roster.Tickets slices in the provided
// match have fewer than 'length' tickets in them.
func rosterLengthsLessThan(match *pb.Match, length int) bool {
	if lowestRosterLen(match) < length {
		return true
	}
	return false
}
