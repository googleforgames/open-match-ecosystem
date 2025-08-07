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

// Package debug provides a sample match function that doesn't actually attempt
// to make matches - it just dumps out the contents of the profile it received
// from om-core.  This can be helpful when debugging your matchmaker, if you
// just want to validate that the profile you are sending to
// InvokeMatchmakingFunctions is sorting and filtering the tickets in the way
// you expected.
//
// This sample is a reference to demonstrate the usage of an mmf as a general
// tool for accessing tickets in Open Match and doing things _other_ than
// matching those tickets.  For more details about this pattern, have a look at
// the OM2 repository's docs/ADVANCED.md file.  This kind of functionality is
// described in the section called "Advanced MMF Patterns: Beyond Simple
// Matchmaking".
//
// If you're trying to build this MMF into a container image to deploy on
// a serverless platform, your starting point should be
// github.com/googleforgames/open-match-ecosystem/v2/examples/mmf/main.go

package debug

import (
	"fmt"
	"math"
	"time"

	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/types/known/anypb"
	knownpb "google.golang.org/protobuf/types/known/wrapperspb"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"open-match.dev/open-match-ecosystem/v2/examples/mmf/server"
	"open-match.dev/open-match-ecosystem/v2/internal/extensions"
)

const (
	matchName = "profile-debug-matchfunction"
)

var (
	// Default logger.
	defaultLoggingFields = logrus.Fields{
		"app":       "matchmaker",
		"component": "matchmaking_function",
		"strategy":  "debug",
	}
	log = logrus.New()
)

// Use the golang programming pattern of a private struct, forcing someone using
// this module to use New() or NewWithLogger() to instantiate the object. Doing
// it this way lets us have an overridable, default logger.

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
// the OM2 repo proto/v2/mmf.proto file.
func (s *mmfServer) Run(stream pb.MatchMakingFunctionService_RunServer) error {

	startTime := time.Now()
	t := time.Now().Format("2006-01-02T15:04:05.00")

	// Use the helper function to reconstruct the incoming request from partial
	// 'chunked' requests.
	req, err := server.GetChunkedRequest(stream)
	if err != nil {
		s.logger.Errorf("error getting chunked request: %v", err)
	}

	// Print the name of the profile.
	s.logger.Infof("Profile      | %v ", req.GetName())

	// Print custom parameters.
	printExtensions := func(exMap map[string]*anypb.Any, logger *logrus.Entry) {
		for exKey := range exMap {
			// Try it as an int32
			if exValue, err := extensions.Int32(exMap, exKey); err == nil {
				logger.Infof(" +Extensions | %v = %v", exKey, exValue)
			}
			// Try it as a bool
			if exValue, err := extensions.Bool(exMap, exKey); err == nil {
				logger.Infof(" +Extensions | %v = %v", exKey, exValue)
			}
			// Try it as a string
			if exValue, err := extensions.String(exMap, exKey); err == nil {
				logger.Infof(" +Extensions | %v = %v", exKey, exValue)
			}
			// Including and then printing other data types as extensions are
			// possible, but you will need to write the code to cast the
			// anypb.Any and retreive the value for it yourself (the extentions
			// module only provides functions to handle that for the above
			// three data types).
		}
	}
	// Print the extensions in the profile.
	printExtensions(req.GetExtensions(), s.logger)

	// Print debug info for the pools specified in the Match Profile.
	for pname, pool := range req.GetPools() {
		// Name of pool & number of matching tickets
		s.logger.Infof(" Pools       | %v contains %v tickets ", pname, len(pool.GetParticipants().GetTickets()))

		// Pool filters
		if crTimeFilter := pool.GetCreationTimeRangeFilter(); crTimeFilter != nil {
			s.logger.Infof(" +Filter     | CreationTimeRange %v - %v", crTimeFilter.GetStart(), crTimeFilter.GetEnd())
		}
		for _, filter := range pool.GetDoubleRangeFilters() {
			s.logger.Infof(" +Filter     | Double at key %v within range %v - %v", filter.GetDoubleArg(), filter.GetMinimum(), filter.GetMaximum())
		}
		for _, filter := range pool.GetStringEqualsFilters() {
			s.logger.Infof(" +Filter     | String at key %v = %v", filter.GetStringArg(), filter.GetValue())
		}
		for _, filter := range pool.GetTagPresentFilters() {
			s.logger.Infof(" +Filter     | Tag %v exists", filter.GetTag())
		}

		// Print the extensions in the pool.
		printExtensions(pool.GetExtensions(), s.logger)
	}

	// Example of how to pass ticket analysis back to the game server director:
	// just make an empty match with some extension fields you pre-defined, and
	// return it.  Then, add code to your director to read those extension
	// fields and use the data in them as desired.
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
		Id: fmt.Sprintf("profile-%s.time-%s", matchName, t),
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

	// Set the quality 'score' of this match to a flag value, so the game
	// server director knows it should never be used to allocate a server.
	var score int32
	score = math.MinInt32
	match.Extensions["score"], err = anypb.New(&knownpb.Int32Value{Value: score})
	if err != nil {
		s.logger.Errorf("Unable to create 'score' extension for match %v", match.Id)
	}

	// Stream the generated match back to Open Match.
	err = stream.Send(&pb.StreamedMmfResponse{Match: match})
	if err != nil {
		s.logger.Debugf("Failed to stream proposal to Open Match, got %s", err.Error())
		return err
	}

	dur := time.Since(startTime)
	s.logger.Infof("Completed debug of incoming profile in  %.03f seconds", float64(dur.Milliseconds())/1000.0)

	return nil
}
