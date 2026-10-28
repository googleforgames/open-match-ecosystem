// Copyright 2024 Google LLC
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

// Package gsdirector is a sample Open Match 2 game server director. It is both
// a reference for how a director should drive om-core and the director used by
// the repository's local and cloud end-to-end/load tests. Its game server
// backend (MockAgonesIntegration) is a mock that models the metadata flow of an
// Agones integration without talking to a real cluster.
package gsdirector

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	backoff "github.com/cenkalti/backoff/v4"
	"github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/zeebo/xxh3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	knownpb "google.golang.org/protobuf/types/known/wrapperspb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156
	"google.golang.org/protobuf/types/known/anypb"
	_ "google.golang.org/protobuf/types/known/wrapperspb"

	"open-match.dev/open-match-ecosystem/v2/internal/assignmentdistributor"
	"open-match.dev/open-match-ecosystem/v2/internal/extensions"
	ex "open-match.dev/open-match-ecosystem/v2/internal/extensions"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

var (
	// pool with a set of filters to find all players. Really only
	// useful in development and testing; Always use some meaningful
	// filters in production matchmaking Pools!
	//
	// om-core treats a missing End as open-ended, so every ticket created after
	// Start matches. Setting an absolute End would silently stop matching new
	// tickets once a long-running director passed it.
	EveryTicket = &pb.Pool{
		Name: "everyone",
		CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
			Start: timestamppb.New(time.Now().Add(time.Hour * -1)),
		},
	}

	// Pool with a set of filters to get tickets where the player selected a
	// tank class
	Tanks = &pb.Pool{Name: "tank",
		StringEqualsFilters: []*pb.Pool_StringEqualsFilter{{StringArg: "class", Value: "tank"}}}

	// Pool with a set of filters to get where the player selected a dps class
	DPS = &pb.Pool{Name: "dps",
		StringEqualsFilters: []*pb.Pool_StringEqualsFilter{{StringArg: "class", Value: "dps"}}}

	// Pool with a set of filters to get where the player selected a healer class
	Healers = &pb.Pool{Name: "healer",
		StringEqualsFilters: []*pb.Pool_StringEqualsFilter{{StringArg: "class", Value: "healer"}}}

	// Matchmaking function specifications
	Fifo = &pb.MatchmakingFunctionSpec{
		Name: "FIFO",
		Host: "http://localhost",
		Port: 50090,
		Type: pb.MatchmakingFunctionSpec_GRPC,
	}
	// BalancedTeam is illustrative only: no MMF with this name ships in the
	// repository, and it is only used by the also-illustrative HeroShooter game mode.
	BalancedTeam = &pb.MatchmakingFunctionSpec{
		Name: "Balanced_Team",
		Host: "http://localhost",
		Port: 50091,
		Type: pb.MatchmakingFunctionSpec_GRPC,
	}

	// If an MMF invocation needs to make a backfill request for an incomplete
	// match to find additional tickets in future matchmaking cycles, it must
	// define what MMFs should be invoked to service that backfill request.
	// (This example generates the rest of the pb.MmfRequest such as the
	// Profile backfill request in the mmf according to what kinds of tickets
	// are required).
	//
	// Naming contract with the MMF: backfill profiles are named
	// "backfill.<base-profile-name>.<unique-suffix>" where <unique-suffix>
	// contains no dots. The director relies on this to (1) recognise backfill
	// requests and matches by the "backfill." prefix (to prioritise them, and to
	// invoke them first when WAIT_FOR_BACKFILL_MMFS is true), and (2) record
	// bounded metric attributes by stripping everything after the last dot (see
	// boundedProfileName).
	BackfillMMFs, _ = anypb.New(&pb.MmfRequest{Mmfs: []*pb.MatchmakingFunctionSpec{Fifo}})

	// Game mode specifications
	SoloDuel = &GameMode{
		Name:  "SoloDuel",
		MMFs:  []*pb.MatchmakingFunctionSpec{Fifo},
		Pools: map[string]*pb.Pool{"all": EveryTicket},
		ExtensionParams: extensions.Combine(extensions.AnypbIntMap(map[string]int32{
			"desiredNumRosters": 1,
			"desiredRosterLen":  4,
			"minRosterLen":      2,
		}), map[string]*anypb.Any{
			ex.MMFRequestKey: BackfillMMFs, // Use FIFO MMF for SoloDuel backfill requests.
		}),
	}

	// HeroShooter illustrates a multi-pool, team-based game mode. It is not used
	// (it is not in GameModesInZone), and the BalancedTeam MMF it names does not
	// exist in the repository.
	HeroShooter = &GameMode{
		Name: "HeroShooter",
		MMFs: []*pb.MatchmakingFunctionSpec{BalancedTeam},
		Pools: map[string]*pb.Pool{
			"tank":   Tanks,
			"dps":    DPS,
			"healer": Healers,
		},
		ExtensionParams: extensions.Combine(extensions.AnypbIntMap(map[string]int32{
			"desiredNumRosters":   2,
			"tankMinPerRoster":    1,
			"tankMaxPerRoster":    1,
			"healerMinPerRoster":  1,
			"healerMaxPerRoster":  2,
			"dpsMinPerRoster":     1,
			"dpsMaxPerRoster":     2,
			"maxTicketsPerRoster": 5,
		}), map[string]*anypb.Any{
			ex.MMFRequestKey: BackfillMMFs, // Use FIFO MMF for HeroShooter backfill requests.
		}),
	}

	// Map of game modes on offer in which GCP zones. If a zone doesn't appear
	// in this map, then no matchmaking profile will be initialized for it by
	// the game server managment integration (it will not participate in
	// matchmaking)
	//
	// This is an example used by our automated testing. In an actual
	// matchmaker, your game server director 'main' package would define which
	// game modes have game servers available in each zone of your
	// infrastructure, and instantiate the GSDirector struct using that custom
	// GameModesInZone map.
	GameModesInZone = map[string][]*GameMode{
		"asia-northeast1-a": {SoloDuel},
	}

	// Map of filter sets (held in pb.Pool structs, for easy combination) to
	// regions. In most cases, you probably only need to make a filter for each
	// region defining the min and max pings for tickets to play in those
	// zones.
	//
	// This is an example used by our automated testing. In an actual matchmaker,
	// your game server director 'main' package would define filters for each zone
	// that works for your infrastructure, and instantiate the GSDirector struct
	// using that custom ZonePools map.
	ZonePools = map[string]*pb.Pool{
		"asia-northeast1-a": {
			DoubleRangeFilters: []*pb.Pool_DoubleRangeFilter{
				{
					DoubleArg: "ping.asia-northeast1-a",
					Minimum:   1,
					Maximum:   120,
				},
			},
		},
		// All other zones aren't used currently and are included for
		// instructional purposes.
		"asia-northeast3-b":      {},
		"asia-south2-c":          {},
		"australia-southeast1-c": {},
		"europe-central2-b":      {},
		"europe-west3-a":         {},
		"me-central2-a":          {},
		"us-east4-b":             {},
		"us-west2-c":             {},
		"southamerica-east1-c":   {},
		"africa-south1-b":        {},
	}

	// Nested map structure defining which game modes are available in which
	// data centers. This mocks out one Agones fleet in each region which can
	// host a match session from any of the provided game modes.
	//
	// This is an example used by our automated testing. In an actual matchmaker,
	// your game server director 'main' package would define a FleetConfig
	// that works for your infrastructure, and instantiate the GSDirector struct
	// using that custom FleetConfig.
	FleetConfig = map[string]map[string]string{
		"APAC": {
			"JP_Tokyo":  "asia-northeast1-a",
			"KR_Seoul":  "asia-northeast3-b",
			"IN_Delhi":  "asia-south2-c",
			"AU_Sydney": "australia-southeast1-c",
		},
		"EMEA": {
			"PL_Warsaw":    "europe-central2-b",
			"DE_Frankfurt": "europe-west3-a",
			"SA_Dammam":    "me-central2-a",
		},
		"NA": {
			"US_Ashburn":    "us-east4-b",
			"US_LosAngeles": "us-west2-c",
		},
		"SA": {
			"BR_SaoPaulo": "southamerica-east1-c",
		},
		"Africa": {
			"ZA_Johannesburg": "africa-south1-b",
		},
	}

	NoSuchAllocationSpecError = errors.New("No such AllocationSpec exists")
	MMFTimeoutError           = errors.New("MMF timeout for the game server director (defined in config var) exceeded")
	MMFComplete               = errors.New("Game server director detected that all MMFs have stopped streaming back results")
)

// GameMode holds data about how to create an Open Match pb.MmfRequest that
// will make the kind of matches desired by this game mode.
type GameMode struct {
	// Name of this game mode, human-readable string.
	Name string
	// List of MMFs to use when matching for this game mode.
	MMFs []*pb.MatchmakingFunctionSpec
	// Pools filled with filters used to find tickets that can play this game mode.
	Pools map[string]*pb.Pool
	// Additional user-defined parameters for the matchmaker or MMF.
	ExtensionParams map[string]*anypb.Any
}

// SetBackfillMMFs serializes the provided MatchmakingFunctionSpecs as a pb.MmfRequest
// into the GameMode's ExtensionParams under ex.MMFRequestKey.
func (gm *GameMode) SetBackfillMMFs(mmfs ...*pb.MatchmakingFunctionSpec) error {
	backfillReq, err := anypb.New(&pb.MmfRequest{Mmfs: mmfs})
	if err != nil {
		return err
	}
	if gm.ExtensionParams == nil {
		gm.ExtensionParams = make(map[string]*anypb.Any)
	}
	gm.ExtensionParams[ex.MMFRequestKey] = backfillReq
	return nil
}

// MockAgonesIntegration is an abstraction of the game server integration
// module you would write for a real matchmaker. It mocks out quite a bit of
// functionality that would, in a real system, require you to read data from
// kubernetes or Agones. This is only provided for testing.
//
// Generally you would right your game server integration module as a standalone
// package you would import in your game server director. However for the time
// being we've only written this one sample. Expect this to be broken into it's own
// package if/when we ever have another game server integration implementation.
type MockAgonesIntegration struct {
	// Shared logger.
	Log *logrus.Logger

	// Open Telemetric metrics meter
	OtelMeterPtr *metric.Meter // TODO: add metrics to the MockAgonesIntegration

	// read-write mutex to control concurrent map access.
	mutex sync.RWMutex

	// A game server has a copy of the pb.MmfRequest protocol buffer message
	// describing the criteria for finding tickets for that server's unique
	// circumstances as an ObjectMeta.Annotation in the GameServer at
	// `ex.MMFRequestKey`.  This mock integration marshals the pb.MmfRequest to
	// JSON when storing it as metadata, as that is human-readable, although it
	// would be more efficient to store the binary protobuf message data.
	//
	// In addition, a hash of the pb.MmfRequest protocol buffer message is
	// stored as an ObjectMeta.Label at `ex.MMFRequestHashKey` on the
	// GameServer. This allows us to use a kubernetes LabelSelector to find
	// game servers by their pb.MmfRequest at allocation time. The included
	// agonesv1.AllocationSpec is an allocation spec including that
	// LabelSelector. The map is keyed by a that AllocationSpec marshaled to a
	// JSON string, as that is what is sent out to the director and what the
	// director send back to this agones integration to specify the game server
	// set it wishes to allocate from for a given game session.
	//
	// This is a greatly simplified mock.  In reality, this would be
	// implemented using a lister for GameServer objects, and all the metadata
	// above would live on the Game Server k8s objects.
	gameServers map[string]*mockGameServerSet

	// Channel on which IndividualGameServerMatchmakingRequests are sent; this
	// allows us to mock out an Agones Informer on GameServer objects and write
	// event-driven code that responds to game server allocations the way a
	// real Agones integration would.
	gameServerUpdateChan chan *mockGameServerUpdate
}

// mmfParametersAnnotation mocks a Kubernetes Annotation to each Agones Game
// Server that describes how to make matches for that game server.  For
// example, when a game server is created by a fleet scaling event or an
// individual server needs more players, a real Agones integration would add a
// Kubernetes Annotation to describe the kind of players that server needs.
type mmfParametersAnnotation struct {
	mmfParams []string
}

// mockGameServer mocks out the parts of an Agones GameServer object that our
// mock Agones Integration interacts with.
type mockGameServer struct {
	mmfParams []*pb.MmfRequest
	allocSpec *allocationv1.GameServerAllocationSpec
}

// mockGameServerSet is a set of mock game servers with identical matchmaking
// needs, and a count to represent how many ready game servers like this are
// currently in the Agones backend.
type mockGameServerSet struct {
	count  int
	server mockGameServer
}

// mockGameServerUpdate is the update event message struct used to mock out
// situations where an Agones Game Server Informer would CREATE/DELETE/UPDATE
// the k8s object.
//
// To mock a 'DELETE':
//
//	gs.mmfParams = nil
//	gs.allocatorSpec = allocationv1.GameServerAllocationSpec that describes how to select a server to DELETE
//
// To mock a 'CREATE':
//
//	gs.mmfParams = array of pb.MmfRequests describing matches this server wants to host
//	gs.allocatorSpec = nil
//
// To mock an 'UPDATE':
//
//	gs.mmfParams = array of pb.MmfRequests describing matches this server wants to host
//	gs.allocatorSpec = allocationv1.GameServerAllocationSpec that describes how to select the server as it exists before the UPDATE
//
// The 'done' channel allows the function sending the update to wait for a
// signal that the update has been successfuly processed before continuing.
type mockGameServerUpdate struct {
	gs   mockGameServer
	done chan struct{}
}

// updateGameServerMetadata is a mock function that adds metadata to
// an Agones GameServer object in Kubernetes.  This mock only simulates adding
// matching parameters to the game server metadata. In a real implementation,
// you'd probably also have input parameters to this function to allow it to
// add lists, counters, labels, and any other metadata your matchmaker needs to
// be able to add to a given server.
//
// In a real environment, updating k8s metadata generates an update event for
// all GameServer Informers,
// (https://agones.dev/site/docs/guides/access-api/#best-practice-using-informers-and-listers)
// which can be used to trigger code to update your GameServerManager internal
// state. In this mock, this is all simulated using a golang channel and a
// simple goroutine that updates the GameServerManager internal state directly
// when it sees a GameServer update arrive on the channel.
func (m *MockAgonesIntegration) updateGameServerMetadata(defunctAllocationSpecJSON string, mmfParams []*pb.MmfRequest) {
	doneChan := make(chan struct{})
	update := &mockGameServerUpdate{
		gs: mockGameServer{
			// If there was no next matchmaking request returned from the MMF, then
			// the input mmfReq variable will be nil.
			mmfParams: mmfParams,
		},
		done: doneChan,
	}

	// Decorative closure to make it easy to see what happens inside the mutex lock.
	{
		m.mutex.Lock()
		if gsSet, exists := m.gameServers[defunctAllocationSpecJSON]; exists {
			update.gs.allocSpec = gsSet.server.allocSpec
		}
		m.mutex.Unlock()
	}

	m.Log.WithFields(logrus.Fields{
		"new_mmfParams": mmfParams,
		"gs.allocSpec":  update.gs.allocSpec,
	}).Trace("sending gs metadata update")

	m.gameServerUpdateChan <- update
	<-doneChan
}

// UpdateMMFRequest() is called by the director associate a new mmfRequest with
// the server sescribed by the provided allocation spec. This is commonly used
// to add a matchmaking request with new parameters to a server for which a
// session already exists (e.g. scenarios like backfill, join-in-progress, and
// high-density gameservers). The new allocation spec that will match servers
// that can handle matches resulting from this mmfRequest is returned to the
// director.
func (m *MockAgonesIntegration) UpdateMMFRequest(defunctAllocationSpecJSON string, mmfParam *pb.MmfRequest) string {
	// Make a new allocation spec by copying the old allocation spec, and adding
	// mmfParams to write to the server when it is allocated.
	newAllocSpec, err := allocationSpecFromMMFRequests([]*pb.MmfRequest{mmfParam}, m.Log)
	if err != nil {
		m.Log.Errorf("Failure to generate new Agones Allocation Spec from provided MmfRequest: %v", err)
	}

	// Convert the allocation spec to json, as the calling function expects it
	// as a JSON string not a struct.
	newAllocSpecJSON, err := json.Marshal(newAllocSpec)
	if err != nil {
		m.Log.Errorf("Failure to marshal new Agones Allocation Spec to JSON: %v", err)
	}

	// Send the mock game server update. For more details, see the comment
	// describing the mockGameServerUpdate struct.
	m.updateGameServerMetadata(defunctAllocationSpecJSON, []*pb.MmfRequest{mmfParam})

	// Return the new allocation spec.
	return string(newAllocSpecJSON[:])
}

// allocationSpecFromMMFRequests() generates a new allocation spec that will
// allocate a server which can host a game session created by the input
// mmfRequests.
// TODO: move to lists if they become able to delete at allocation time
// https://agones.dev/site/docs/guides/counters-and-lists/
// https://github.com/googleforgames/agones/issues/4003
func allocationSpecFromMMFRequests(mmfParams []*pb.MmfRequest, logger *logrus.Logger) (*allocationv1.GameServerAllocationSpec, error) {

	// Var init
	var err error

	// Add JSON string of this Mmf Request to the list of requests to annotate this game server with.
	mpa := mmfParametersAnnotation{mmfParams: make([]string, 0)}

	for _, mmfParam := range mmfParams {
		// Marshal this mmf request to a JSON string
		mmfParamJSON, err := protojson.Marshal(mmfParam)
		if err != nil {
			return nil, err
		}

		// Add this set of matchmaking parameters
		mpa.mmfParams = append(mpa.mmfParams, string(mmfParamJSON[:]))
	}

	// Convert the mmfParams to JSON, and get a hash of that JSON
	mmfParamsAnnotationJSON, err := json.Marshal(mpa.mmfParams)
	if err != nil {
		return nil, err
	}
	xxHash := xxh3.Hash128Seed(mmfParamsAnnotationJSON, 0).Bytes()

	// Make an allocation spec that will match a game server with a
	// label under the key `ex.MMFRequestHashKey` with a value equal to the
	// hash of the new MMF Requests, and that will add the MMF Requests
	// as an annotation to the game server upon allocation.
	return &allocationv1.GameServerAllocationSpec{
		Selectors: []allocationv1.GameServerSelector{
			{
				LabelSelector: metav1.LabelSelector{
					// TODO: move to lists, and have one hash for every mmfRequest
					// https://github.com/googleforgames/agones/issues/4003
					MatchLabels: map[string]string{
						ex.MMFRequestHashKey: hex.EncodeToString(xxHash[:]),
					},
				},
			},
		},
		MetaPatch: allocationv1.MetaPatch{
			// Attach the matchmaking request that can be used to
			// make matches this server wants to host as a k8s
			// annotation.
			Annotations: map[string]string{
				ex.MMFRequestKey: string(mmfParamsAnnotationJSON[:]),
			},
		},
	}, nil

}

// ProcessGameServerUpdates is a mock for Agones Integration code you'd write to
// handle UPDATE/CREATE/DELETE events from an Agones GameServer object listener
// https://agones.dev/site/docs/guides/access-api/#best-practice-using-informers-and-listers
func (m *MockAgonesIntegration) processGameServerUpdates() {
	pLogger := m.Log.WithFields(logrus.Fields{
		"component": "agones_integration",
		"operation": "get_mmf_params",
	})

	for update := range m.gameServerUpdateChan {

		pLogger.WithFields(logrus.Fields{"update": update}).Trace("Received game server update")
		// This mocks processing the data that a k8s informer event
		// handler would get as the 'old' game server in a DELETE or
		// UPDATE operation. Since k8s is effectively removing this
		// server, we have to stop tracking it in our agones
		// integration code.
		if update.gs.allocSpec != nil {
			asLogger := pLogger.WithFields(logrus.Fields{
				"alloc_spec": update.gs.allocSpec.Selectors[0].LabelSelector.MatchLabels,
			})
			asLogger.Trace("game server update contains an alloc spec to remove from matching")
			// convert allocationv1.GameServerAllocationSpec to JSON.
			allocSpecJSONBytes, err := json.Marshal(update.gs.allocSpec)
			if err != nil {
				asLogger.Errorf("Unable to marshal Agones Game Server Allocation object to JSON: %v", err)
				continue
			}
			allocSpecJSON := string(allocSpecJSONBytes[:])

			m.mutex.Lock()
			// Make sure there's an existing reference count for this allocation spec
			if gsSet, exists := m.gameServers[allocSpecJSON]; exists {
				if gsSet.count-1 > 0 {
					// If removing one reference count would result in at least
					// one remaining reference, then decrement
					m.gameServers[allocSpecJSON].count--
					asLogger.WithFields(logrus.Fields{
						"ready_servers_remaining": m.gameServers[allocSpecJSON].count,
					}).Trace("decremented the number of game servers managed by Agones matching this alloc spec")
				} else {
					asLogger.Trace("removed last server matching this allocation spec from matching, so also removing this alloc spec from matching")
					// If removing one reference count would make the reference
					// count < 1, remove this allocation spec from memory.
					delete(m.gameServers, allocSpecJSON)
				}
				// done with our updates; let other goroutines access the allocator specs.
				m.mutex.Unlock()
			} else {
				// no updates to make; release the lock while we log an error.
				m.mutex.Unlock()
				asLogger.Errorf("Error: %v", NoSuchAllocationSpecError)
			}
		}

		// This mocks processing the data that a k8s informer event
		// handler would receive as 'new' game server with an attached
		// MMF request in a CREATE or UPDATE operation. We now want to
		// track this new object spec in our agones integration code.
		if update.gs.mmfParams != nil && len(update.gs.mmfParams) > 0 {
			newAllocatorSpec, err := allocationSpecFromMMFRequests(update.gs.mmfParams, m.Log)
			if err != nil {
				pLogger.Errorf("Failure to generate new Agones Allocation Spec from provided MmfRequests: %v", err)
			}

			// Now hash the allocation spec we just created to use as a key.
			newAllocatorSpecJSONBytes, err := json.Marshal(newAllocatorSpec)
			if err != nil {
				pLogger.Errorf("Unable to marshal Agones Game Server Allocation object to JSON: %v", err)
				continue
			}
			//xxHash = xxh3.Hash128Seed(newAllocatorSpecJSONBytes, 0).Bytes()
			//newAllocatorSpecHash := hex.EncodeToString(xxHash[:])
			newAllocatorSpecJSON := string(newAllocatorSpecJSONBytes[:])

			// Decorative closure to make it easy to see what happens inside the mutex lock.
			{
				// Finally, track this game server in memory
				m.mutex.Lock()
				// Create if it doesn't exist
				if _, exists := m.gameServers[newAllocatorSpecJSON]; !exists {
					m.gameServers[newAllocatorSpecJSON] = &mockGameServerSet{
						count:  0,
						server: mockGameServer{},
					}
				}
				// Track server set by alloc spec (as a JSON string)
				m.gameServers[newAllocatorSpecJSON].server.allocSpec = newAllocatorSpec
				m.gameServers[newAllocatorSpecJSON].server.mmfParams = update.gs.mmfParams
				// Increment reference count
				m.gameServers[newAllocatorSpecJSON].count++
				m.mutex.Unlock()
			}
		}
		update.done <- struct{}{}
	}
}

// Init initializes the fleets the Agones backend mocks according to the
// provided fleetConfig, and starts a mock Kubernetes Informer that
// asynchronously watches for changes to Agones Game Server objects.
// https://agones.dev/site/docs/guides/access-api/#best-practice-using-informers-and-listers
//
// Init should be called once and only once, after defining the externally-visible
// parts of the MockAgonesIntegration structure that are desired, and before calling
// the gsdirector.Run() function.
func (m *MockAgonesIntegration) Init(fleetConfig map[string]map[string]string, zonePools map[string]*pb.Pool, gameModesInZone map[string][]*GameMode) (err error) {
	if m.Log == nil {
		m.Log = logrus.New()
	}
	initLogger := m.Log.WithFields(logrus.Fields{
		"component": "agones_integration",
		"operation": "init",
	})
	initLogger.Debug("Initializing matching parameters")

	now := time.Now()
	// Start-only, open-ended range (see EveryTicket declaration).
	EveryTicket.CreationTimeRangeFilter = &pb.Pool_CreationTimeRangeFilter{
		Start: timestamppb.New(now.Add(time.Hour * -1)),
	}

	// appendPoolParams copies the filters and extensions from src pool to dest
	// pool, allowing us to combine different pools to create pools
	// dynamically.
	//
	// NOTE: If the destination contains a CreationTimeFilter and the source does
	// not, the destination CreationTimeFilter will be preserved.
	// NOTE: If the destination and source both have an extension with the same key,
	// the value from the src extension will take precedence.
	appendPoolParams := func(dest *pb.Pool, src *pb.Pool) (result *pb.Pool) {
		result = &pb.Pool{}
		result.TagPresentFilters = make([]*pb.Pool_TagPresentFilter, 0, len(dest.GetTagPresentFilters())+len(src.GetTagPresentFilters()))
		result.TagPresentFilters = append(result.TagPresentFilters, dest.GetTagPresentFilters()...)
		result.TagPresentFilters = append(result.TagPresentFilters, src.GetTagPresentFilters()...)

		result.DoubleRangeFilters = make([]*pb.Pool_DoubleRangeFilter, 0, len(dest.GetDoubleRangeFilters())+len(src.GetDoubleRangeFilters()))
		result.DoubleRangeFilters = append(result.DoubleRangeFilters, dest.GetDoubleRangeFilters()...)
		result.DoubleRangeFilters = append(result.DoubleRangeFilters, src.GetDoubleRangeFilters()...)

		result.StringEqualsFilters = make([]*pb.Pool_StringEqualsFilter, 0, len(dest.GetStringEqualsFilters())+len(src.GetStringEqualsFilters()))
		result.StringEqualsFilters = append(result.StringEqualsFilters, dest.GetStringEqualsFilters()...)
		result.StringEqualsFilters = append(result.StringEqualsFilters, src.GetStringEqualsFilters()...)

		// Check that the source has a creation time filter before copying to avoid
		// an empty src filter overwriting an existing dest filter. This can only
		// happen with this type of filter, as all others are contained in arrays.
		result.CreationTimeRangeFilter = dest.GetCreationTimeRangeFilter()
		if src.GetCreationTimeRangeFilter() != nil {
			result.CreationTimeRangeFilter = src.GetCreationTimeRangeFilter()
		}

		// Combine extensions
		result.Extensions = maps.Clone(dest.GetExtensions())
		if result.Extensions == nil {
			result.Extensions = map[string]*anypb.Any{}
		}
		for key, value := range src.GetExtensions() {
			result.Extensions[key] = value
		}

		return result
	}

	// In-memory game server state tracking maps init
	m.gameServers = make(map[string]*mockGameServerSet)

	// channel + goroutine used to simulate a Kubernetes informer event-driven pattern.
	m.gameServerUpdateChan = make(chan *mockGameServerUpdate)
	go m.processGameServerUpdates()

	// Initialize the matchmaking parameters of the mock Agones fleets
	// specified in the fleetConfig
	var xxHash [16]byte
	for category, regions := range fleetConfig {
		for region, zone := range regions {
			if modes, exists := gameModesInZone[zone]; exists {

				// Create a new mock GameServer set for this fleet.
				mGSSet := &mockGameServerSet{
					server: mockGameServer{},
					// Since the mock doesn't actually do allocations from
					// fleets, the fleets will never scale up to include
					// new game servers.  In a real environment, the
					// scaling would generate game server update events
					// sent to the k8s informer, which the
					// ProcessGameServerUpdates goroutine is responsible
					// for monitoring and incrementing the ready server
					// account accordingly. Instead, we mock out the
					// creation of new servers by setting the count of
					// ready servers returned by this Agones Allocator Spec
					// to the maximum number held by an int32 (functionally
					// infinite).
					count: math.MaxInt32,
				}

				// Construct a fleet name based on the FleetConfig
				fleetName := fmt.Sprintf("%s/%s@%s", category, region, zone)
				fLogger := initLogger.WithFields(logrus.Fields{
					"fleet": fleetName,
				})
				fLogger.Debug("Adding matching parameters to fleet")

				// Make an array of all the matchmaking profiles that apply to
				// this fleet, one profile per game mode.
				mpa := &mmfParametersAnnotation{mmfParams: make([]string, 0)} // String version to attach to k8s metadata
				mGSSet.server.mmfParams = make([]*pb.MmfRequest, 0)           // protobuf version to send to Open Match.

				// retrieve filters & extensions for this specific datacenter.
				zonePool, exists := zonePools[zone]
				if !exists {
					zonePool = &pb.Pool{}
				}

				for _, mode := range modes {

					// By convention, profile names should use reverse-DNS notation
					// https://en.wikipedia.org/wiki/Reverse_domain_name_notation This
					// helps with metric attribute cardinality, as we can record
					// profile names alongside metric readings after stripping off the
					// most-unique portion. Keep any timestamps, 'generation' counters,
					// unique hashes, etc in the part of the profile name _after_ the last
					// dot.
					// https://grafana.com/blog/2022/02/15/what-are-cardinality-spikes-and-why-do-they-matter/
					profileName := fmt.Sprintf("dev.open-match.%v.%v.%d",
						fleetName,
						mode.Name,
						time.Now().UnixNano(),
					)

					// Dynamically combine sets of filters and extensions that
					// define the kinds of tickets this game mode in this zone
					// wants to use for matching.
					composedPools := map[string]*pb.Pool{}
					for pname, thisModePool := range mode.Pools {
						// Combine the pool parameters defined for this game
						// mode, and for this server zone.
						composedPool := appendPoolParams(thisModePool, zonePool)
						composedName := fmt.Sprintf("%v.%v", fleetName, pname)
						composedPool.Name = composedName
						composedPools[composedName] = composedPool
					}

					profileExtensions := maps.Clone(mode.ExtensionParams)
					if profileExtensions == nil {
						profileExtensions = map[string]*anypb.Any{}
					}

					// Add this mmf request to our list of parameters
					mGSSet.server.mmfParams = append(mGSSet.server.mmfParams, &pb.MmfRequest{
						Mmfs: mode.MMFs,
						Profile: &pb.Profile{
							Name:       profileName,
							Pools:      composedPools,
							Extensions: profileExtensions, // Include all the user-defined data in the game mode extensions field.
						},
					})

					// Marshal this mmf request to a JSON string
					paramsJSON, err := protojson.Marshal(mGSSet.server.mmfParams[len(mGSSet.server.mmfParams)-1])
					if err != nil {
						fLogger.WithFields(logrus.Fields{
							"game_mode": mode,
						}).Errorf("Failed to marshal MMF parameters into JSON: %v", err)
						return err
					}
					mpa.mmfParams = append(mpa.mmfParams, string(paramsJSON[:]))
				}

				// Marshal the array of matchmaking parameters used by game modes in this fleet into JSON
				mmfParamsJSON, err := json.Marshal(mpa.mmfParams)
				if err != nil {
					fLogger.Errorf("Failed to marshal MMF parameters annotation into JSON: %v", err)
					return err
				}

				// Hash the matchmaking paramaters and store it in the agones allocation spec
				xxHash = xxh3.Hash128Seed(mmfParamsJSON, 0).Bytes()
				mmfParamsHash := hex.EncodeToString(xxHash[:])

				// Generate an Agones Game Server Allocation that can allocate a ready server from this fleet.
				mGSSet.server.allocSpec = &allocationv1.GameServerAllocationSpec{
					Selectors: []allocationv1.GameServerSelector{
						{
							LabelSelector: metav1.LabelSelector{
								// TODO: move to lists, and have one hash for every mmfRequest
								// https://github.com/googleforgames/agones/issues/4003
								MatchLabels: map[string]string{ex.MMFRequestHashKey: mmfParamsHash},
							},
						},
					},
				}

				// Hash the agones allocation spec and store it in a map by its
				// hash for later lookup
				allocJSONBytes, err := json.Marshal(mGSSet.server.allocSpec)
				if err != nil {
					fLogger.Errorf("Unable to marshal Agones Game Server Allocation object to JSON: %v", err)
					continue
				}
				allocationSpecJSON := string(allocJSONBytes[:])

				// decorative closure just to make it easy to see which lines are encompassed by the mutex lock.
				{
					m.mutex.Lock()
					m.gameServers[allocationSpecJSON] = mGSSet
					m.mutex.Unlock()
					fLogger.Infof("Matchmaking parameters for fleet successfully initialized: %v", mGSSet)
				}

			}
		}
	}
	return nil
}

// Allocate() mocks doing an Agones game server allocation.
// The director calls UpdateMMFRequest for a match's backfill request only after
// Allocate succeeds, so a failed allocation never leaves a backfill request
// registered for a server that did not get the match. A real Agones integration
// would go further and add the new matchmaking request to the allocation's
// MetaPatch so Agones applies it atomically. Allocate therefore leaves the
// server's matchmaking requests in place when the match carries a backfill
// request, and UpdateMMFRequest replaces them.
func (m *MockAgonesIntegration) Allocate(allocationSpecJSON string, match *pb.Match) (connString string) {
	aLogger := m.Log.WithFields(logrus.Fields{"match_id": match.GetId()})

	// TODO: Here is where your backend would do any additional prep tasks for
	// server allocation in Agones

	var ok bool
	// Decorative closure to make it easier to see what is being done within the mutex lock.
	{
		m.mutex.RLock()
		_, ok = m.gameServers[allocationSpecJSON]
		m.mutex.RUnlock()
	}

	if !ok {
		aLogger.WithFields(logrus.Fields{
			"gs.allocSpec": allocationSpecJSON,
		}).Error("Unable to allocate a server that matches the provided allocation spec")

		// Return an empty connection string. The game director should
		// interpret this as an allocation failure.
		return ""
	}

	// TODO: Here is where you'd use the k8s api to request an agones game
	// server allocation in a real implementation.  If you need to add
	// match parameters, player lists, etc to your Game Server, you would
	// read it from the pb.Match message here and include it in the Agones
	// Allocation Spec. This mock does no actual allocation operation;
	// instead it just mocks out the game server metadata update that k8s
	// would send to an Agones Game Server Informer after a successful
	// Agones allocation operation.
	aLogger.Debugf("Allocating server")
	hasBackfillRequest := false
	if ext := match.GetExtensions(); ext != nil && ext[ex.MMFRequestKey] != nil {
		nextMMFRequest := &pb.MmfRequest{}
		if err := ext[ex.MMFRequestKey].UnmarshalTo(nextMMFRequest); err == nil && nextMMFRequest.GetProfile().GetName() != "" {
			hasBackfillRequest = true
		}
	}
	if !hasBackfillRequest {
		m.updateGameServerMetadata(allocationSpecJSON, []*pb.MmfRequest{})
	}
	// In a real implementation, you'd receive the agonesv1.GameServerStatus{}
	// object from kubernetes/the Agones SDK.  Here we generate a mock one instead.
	mockGameServerStatus := &agonesv1.GameServerStatus{
		Ports: []agonesv1.GameServerStatusPort{
			{
				Name: "default",
				Port: 7463,
			},
		},
		Address: "1.2.3.4",
	}

	// Make the assignment connection string out of the data received from Agones.
	connString = fmt.Sprintf("%v:%v", mockGameServerStatus.Address, mockGameServerStatus.Ports[0].Port)
	return connString
}

// GetMMFParams() returns all pb.MmfRequest messages currently tracked by the
// Agones integration. These contain all the mmf parameters for matchmaking
// function invocations that should be kicked off in the next director
// matchmaking cycle to find matches to put on game servers managed by
// Agones.
func (m *MockAgonesIntegration) GetMMFParams() []*pb.MmfRequest {
	gLogger := m.Log.WithFields(logrus.Fields{
		"component": "agones_integration",
		"operation": "get_mmf_params",
	})

	// var init
	requests := make([]*pb.MmfRequest, 0)

	// Sort the allocation specs by the number of servers they match.
	//
	// ascount is a simple struct we temporarily use to do the sorting.
	//
	// 'as' here is an abbreviation for 'allocation spec'
	type ascount struct {
		spec  string
		count int
	}
	var asByCount []ascount

	m.mutex.RLock()
	for k, v := range m.gameServers {
		asByCount = append(asByCount, ascount{spec: k, count: v.count})
	}
	m.mutex.RUnlock()

	sort.Slice(asByCount, func(x, y int) bool {
		return asByCount[x].count < asByCount[y].count
	})

	// servers with unique MmfRequests (examples: backfill, join-in-progress,
	// high-density game servers, etc) should be serviced with higher priority,
	// so add these 'more unique' (e.g. lower reference count) requests to the
	// list first.
	for _, ac := range asByCount {

		m.mutex.RLock()
		gsSet, ok := m.gameServers[ac.spec]
		m.mutex.RUnlock()

		// Its possible a given game server set was removed asynchronously, so
		// test that the retrieval succeeded before processing.
		if ok {

			for _, request := range gsSet.server.mmfParams {
				gLogger.Tracef(" retrieved MmfRequest with profile: %v", request.GetProfile().GetName())

				// retrieve existing profile extensions
				extensions := maps.Clone(request.GetProfile().GetExtensions())
				if extensions == nil {
					// make an empty extensions map if there are no extensions yet
					extensions = map[string]*anypb.Any{}
				}

				// Get number of ready servers that can accept matches from
				// this MmfRequest, and put this in the profile extensions
				// field so the mmf can read it.
				exReadyCount, err := anypb.New(&knownpb.Int32Value{Value: int32(ac.count)})
				if err != nil {
					gLogger.Errorf("Unable to convert number of ready game servers into an anypb: %v", err)
					continue
				}
				extensions[ex.AgonesGSReadyCountKey] = exReadyCount

				// add the agones allocator that returns servers asking for
				// this Profile to its extensions field.
				allocSpecJSON, err := json.Marshal(gsSet.server.allocSpec)
				if err != nil {
					gLogger.Errorf("Unable to marshal Agones Game Server Allocation object %v to JSON, discarding mmfRequest %v: %v",
						gsSet.server.allocSpec, gsSet.server.mmfParams, err)
					continue
				}
				exAllocSpecJSON, err := anypb.New(&knownpb.StringValue{Value: string(allocSpecJSON[:])})
				if err != nil {
					gLogger.Errorf("Unable to marshal Agones Game Server Allocation object %v to anyPb, discarding mmfRequest %v: %v",
						gsSet.server.allocSpec, gsSet.server.mmfParams, err)
					continue
				}
				extensions[ex.AgonesAllocatorKey] = exAllocSpecJSON

				// Copy the hash of the MMFRequests from the Agones allocation
				// spec to the MMFRequest extensions field. This is sent back
				// by the MMF in each proposed match, and is what the director
				// uses to figure out which outstanding MmfRequest resulted in
				// which match(es).
				mmfRequestHash, err := anypb.New(&knownpb.StringValue{
					Value: gsSet.server.allocSpec.Selectors[0].LabelSelector.MatchLabels[ex.MMFRequestHashKey],
				})
				if err != nil {
					gLogger.Errorf("Unable to retrieve mmfRequest from allocation spec to put into extensions, discarding mmfRequest %v: %v",
						gsSet.server.mmfParams, err)
					continue
				}
				extensions[ex.MMFRequestHashKey] = mmfRequestHash

				// Finalize the MMFParams for this game server set.
				requests = append(requests, &pb.MmfRequest{
					Mmfs: request.GetMmfs(),
					Profile: &pb.Profile{
						Name:       request.GetProfile().GetName(),
						Pools:      request.GetProfile().GetPools(),
						Extensions: extensions,
					},
				})
			} // end of mmfParam processing loop

		}
	} // end of game server processing loop
	return requests
}

// GameServerManager defines the functions required for a backend game server
// manager integration module used by our sample Director. In this example, the
// MockAgonesIntegration satisfies this interface.
type GameServerManager interface {
	Init(map[string]map[string]string, map[string]*pb.Pool, map[string][]*GameMode) error
	Allocate(string, *pb.Match) string
	GetMMFParams() []*pb.MmfRequest
	UpdateMMFRequest(string, *pb.MmfRequest) string
}

const (
	// backfillProfilePrefix is the profile-name prefix for backfill requests and
	// matches; see the naming contract documented on BackfillMMFs.
	backfillProfilePrefix = "backfill."

	// extInvalidTicketIDs is the Match.Extensions key under which om-core lists,
	// as a structpb.ListValue of strings, roster ticket IDs that failed
	// validation; om-core does not deactivate them.
	extInvalidTicketIDs = "invalid_ticket_ids"

	// extDeactivationFailedTicketIDs is the Match.Extensions key used by om-core
	// only when OM_MATCH_TICKET_DEACTIVATION_WAIT is true, listing as a
	// structpb.ListValue of strings valid roster ticket IDs whose deactivation
	// write failed; those tickets may still be active, so other MMFs can still
	// put them in matches.
	extDeactivationFailedTicketIDs = "deactivation_failed_ticket_ids"

	// defaultRandomMatchRejectPct is the default percentage of proposed matches
	// randomly rejected when RANDOM_MATCH_REJECT_PERCENT is not set.
	defaultRandomMatchRejectPct = 1
)

// MockDirector aims to have the structure of a reasonably realistic,
// feature-complete matchmaking Director that would use a game server backend
// integration to interface with game servers. OmClient, GSManager, and
// AssignmentPublisher are required; Run logs an error and returns immediately
// if any is nil.
type MockDirector struct {
	OmClient            *omclient.RestfulOMGrpcClient
	Cfg                 *viper.Viper
	Log                 *logrus.Logger
	AssignmentPublisher assignmentdistributor.Sender
	GSManager           GameServerManager
	OtelMeterPtr        *metric.Meter

	// pendingMmfRequestsByHash holds the pb.MmfRequest values sent to om-core in
	// the current cycle keyed by their MMFRequestHashKey profile extension, and
	// is rebuilt every cycle because a returned match is only valid if it was
	// made for a request that is still current.
	pendingMmfRequestsByHash map[string]*pb.MmfRequest
	statusMu                 sync.RWMutex
	cycleStatus              string
}

func (d *MockDirector) checkRequiredFields() error {
	var missing []string
	if d.OmClient == nil {
		missing = append(missing, "OmClient")
	}
	if d.GSManager == nil {
		missing = append(missing, "GSManager")
	}
	if d.AssignmentPublisher == nil {
		missing = append(missing, "AssignmentPublisher")
	}
	if len(missing) > 0 {
		return fmt.Errorf("MockDirector is missing required field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

func (d *MockDirector) configuredRandomRejectPercent() int {
	pct := defaultRandomMatchRejectPct
	if d.Cfg != nil && d.Cfg.IsSet("RANDOM_MATCH_REJECT_PERCENT") {
		pct = d.Cfg.GetInt("RANDOM_MATCH_REJECT_PERCENT")
	}
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// Status returns the most recent matchmaking cycle status summary string.
func (d *MockDirector) Status() string {
	d.statusMu.RLock()
	defer d.statusMu.RUnlock()
	return d.cycleStatus
}

// boundedProfileName bounds a profile name for use as a metric attribute.
// Profile names use reverse-DNS notation with all unique parts (timestamps,
// generation counters, hashes, match IDs) after the last dot, so stripping the
// last segment leaves a value shared by every profile of the same kind (for
// example, the backfill form becomes "backfill." plus the base name).
// https://grafana.com/blog/2022/02/15/what-are-cardinality-spikes-and-why-do-they-matter/
func boundedProfileName(name string) string {
	if idx := strings.LastIndex(name, "."); idx > 0 {
		return name[:idx]
	}
	return name
}

func decodeTicketIDListExtension(match *pb.Match, key string) ([]string, error) {
	ext := match.GetExtensions()
	if ext == nil {
		return nil, nil
	}
	raw, ok := ext[key]
	if !ok || raw == nil {
		return nil, nil
	}
	var list structpb.ListValue
	if err := raw.UnmarshalTo(&list); err != nil {
		return nil, err
	}
	vals := list.GetValues()
	if len(vals) == 0 {
		return nil, nil
	}
	ids := make([]string, len(vals))
	for i, v := range vals {
		ids[i] = v.GetStringValue()
	}
	return ids, nil
}

func invalidTicketIDs(match *pb.Match) ([]string, error) {
	return decodeTicketIDListExtension(match, extInvalidTicketIDs)
}

func deactivationFailedTicketIDs(match *pb.Match) ([]string, error) {
	return decodeTicketIDListExtension(match, extDeactivationFailedTicketIDs)
}

func statusFromInvocationError(err error, cause error) *status.Status {
	if st, ok := status.FromError(err); ok {
		return st
	}
	if errors.Is(err, MMFTimeoutError) || errors.Is(cause, MMFTimeoutError) {
		return status.New(codes.DeadlineExceeded, err.Error())
	}
	return status.FromContextError(err)
}

// extractFailedMMFNames collects non-empty "mmf_name" metadata values from any
// errdetails.ErrorInfo details on st. om-core reports each failed MMF as an
// ErrorInfo detail (reason MMF_FAILURE, domain open-match) with metadata key
// "mmf_name".
func extractFailedMMFNames(st *status.Status) []string {
	if st == nil {
		return nil
	}
	var names []string
	for _, detail := range st.Details() {
		info, ok := detail.(*errdetails.ErrorInfo)
		if !ok || info == nil {
			continue
		}
		if name := info.GetMetadata()["mmf_name"]; name != "" {
			names = append(names, name)
		}
	}
	return names
}

func hasDeactivationTimeout(match *pb.Match) (bool, error) {
	if match == nil || match.GetExtensions() == nil {
		return false, nil
	}
	timedOut, err := ex.Bool(match.GetExtensions(), ex.MatchDeactivationTimeoutKey)
	if err != nil {
		if errors.Is(err, ex.NoSuchKeyError) {
			return false, nil
		}
		return false, err
	}
	return timedOut, nil
}

func countMatchTickets(match *pb.Match) int64 {
	var total int64
	for _, roster := range match.GetRosters() {
		total += int64(len(roster.GetTickets()))
	}
	return total
}

func computeCycleBackoffFactor(numProposed, numMatchesWithAssignedTickets, numMatchesWithDeactivationTimeout int64) float64 {
	if numProposed <= 0 {
		return 1.0
	}
	contentionMatches := numMatchesWithAssignedTickets
	if numMatchesWithDeactivationTimeout > contentionMatches {
		contentionMatches = numMatchesWithDeactivationTimeout
	}
	return (float64(contentionMatches) / float64(numProposed)) + 1.0
}

// Run the MockDirector.
//
// This function is intended to be run for the lifetime of your matchmaker. It
// will loop NUM_MM_CYCLES times, each time reading the matchmaking parameters
// for your game servers from the instantiated GameServerManager 'GSManager',
// sending those requests to om-core, and processing the returned matches.
// Matches that the director decides not to put on servers are 'rejected' and
// tickets in those matches are re-activated in om-core so future matching
// attempts can include those tickets. 'Approved' matches are allocated a game
// server from 'GSManager', and the resulting game server connection strings
// are sent in assignment Rosters to the 'AssignmentPublisher'. In
// production we recommend that you handle returning game server assignments to
// game clients using a dedicated player status service. om-core has deprecated
// assignment endpoints, but since those can cause significant load that can
// impact matching performance, we don't recommend their use in production.
// (They are provided for legacy reasons.)
//
// Run returns when the configured number of cycles is done, when
// NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT consecutive cycles produce no
// match proposals, or when the context is cancelled.
func (d *MockDirector) Run(ctx context.Context) {
	if d.Log == nil {
		d.Log = logrus.New()
	}
	if d.Cfg == nil {
		d.Cfg = viper.New()
	}

	// Add fields to structured logging for this function
	logger := d.Log.WithFields(logrus.Fields{
		"app":       "matchmaker",
		"component": "game_server_director",
	})
	if err := d.checkRequiredFields(); err != nil {
		logger.Errorf("Cannot run game server director: %v", err)
		return
	}
	logger.Tracef("Initializing game server director directing matches to game servers")

	// var init
	ticketIdsToActivate := make(chan string, 1000)
	var reactivationWg sync.WaitGroup
	defer func() {
		close(ticketIdsToActivate)
		reactivationWg.Wait()
	}()

	// The re-activation goroutine stops draining on shutdown, so an unconditional
	// send could block forever.
	enqueueReactivation := func(ticketID string) bool {
		select {
		case ticketIdsToActivate <- ticketID:
			return true
		case <-ctx.Done():
			return false
		}
	}

	// Track assignments this director has made
	assignments := make(map[string]bool)
	// Track age of assignments so they can be removed from memory after a time
	assignmentTTL := make(map[time.Time][]string)
	assignmentTTLDuration := time.Duration(d.Cfg.GetInt("ASSIGNMENT_TTL_MS")) * time.Millisecond
	assignmentMemoryReclaimationDeadline := time.Now().Add(assignmentTTLDuration)

	// Read director configuration once per Run.
	randomRejectPct := d.configuredRandomRejectPercent()
	mmfInvocationTimeout := time.Duration(d.Cfg.GetInt("MMF_INVOCATION_TIMEOUT_SECS")) * time.Second

	// Initialize metrics
	if d.OtelMeterPtr != nil {
		logger.Tracef("Initializing otel metris")
		registerMetrics(d.OtelMeterPtr)
	} else {
		logger.Warnf("Unable to add game server director metrics to the OTEL configuration")
	}

	// goroutine that runs for the lifetime of the game server director to
	// re-activate rejected tickets so they appear in matchmaking pools again.
	// We'll send all ticket ids from rejected matches to the
	// ticketIdsToActivate channel.
	reactivationWg.Add(1)
	go func() {
		defer reactivationWg.Done()
		aLogger := logger.WithFields(logrus.Fields{"operation": "ticket_reactivation"})
		aLogger.Trace("requesting re-activation of rejected tickets")

		// request re-activation of rejected tickets.
		atCtx := omclient.WithActivationType(ctx, "reactivate")
		reactivationResults := make(chan omclient.ActivationResult, 1000)
		var resultsWg sync.WaitGroup
		resultsWg.Add(1)
		go func() {
			defer resultsWg.Done()
			for res := range reactivationResults {
				if res.Err != nil {
					aLogger.Errorf("ActivateTickets failed for ticket %s: %v", res.TicketId, res.Err)
					if otelTicketReactivationFailures != nil {
						otelTicketReactivationFailures.Add(ctx, 1)
					}
				}
			}
		}()

		d.OmClient.ActivateTicketsWithResults(atCtx, ticketIdsToActivate, reactivationResults)
		close(reactivationResults)
		resultsWg.Wait()
	}()

	// Kick off main matchmaking loop
	// Loop for the configured number of cycles
	lLogger := logger.WithFields(logrus.Fields{"operation": "matchmaking_loop"})
	lLogger.Debugf("Beginning matchmaking cycle, %v cycles configured", d.Cfg.GetInt("NUM_MM_CYCLES"))
	consecutiveEmptyCycles := 0
	for i := d.Cfg.GetInt("NUM_MM_CYCLES"); i > 0; i-- { // Main Matchmaking Loop
		if ctx.Err() != nil {
			lLogger.Infof("Context cancelled before cycle start: %v", ctx.Err())
			return
		}

		// Loop var init
		startTime := time.Now()
		recordCycleDuration := func() {
			if otelMatchmakingCycleDuration != nil {
				otelMatchmakingCycleDuration.Record(ctx, float64(time.Since(startTime).Nanoseconds())/1e6)
			}
		}

		// Only this cycle's requests are valid targets for returned matches.
		d.pendingMmfRequestsByHash = make(map[string]*pb.MmfRequest)

		var cycleResultsMu sync.Mutex
		var proposedMatches []*pb.Match
		matchesWithInvalidTickets := make(map[*pb.Match]bool)
		invalidTicketsThisCycle := make(map[string]bool)
		deactivationFailedByMatch := make(map[*pb.Match][]string)
		var numFeedbackResponses int64
		var numInvocationFailures int64

		// Metrics tracking
		newMMFRequestsProcessed := int64(0)
		numAcceptedMatches := int64(0)
		numTicketAssignments := int64(0)
		numRejectedMatches := int64(0)
		numRandomDropMatches := int64(0)
		numMatchesWithAssignedTickets := int64(0)
		numMatchesWithDeactivationTimeout := int64(0)
		numTicketsWithDeactivationTimeout := int64(0)
		numMatchesWithDeactivationFailed := int64(0)
		numIdsToActivate := int64(0)
		numTicketsRejected := int64(0)
		numAssignedRejectedTickets := int64(0)
		numProposedTickets := int64(0)

		// Make a fresh context for this loop. It carries an optional director-side
		// deadline (MMF_INVOCATION_TIMEOUT_SECS; 0 means none) so one stuck call
		// cannot stall the director forever.
		//
		// Cancellation in the context of matchmaking functions is complicated
		// because om-core deliberately detaches MMF work from the caller:
		// - Cancelling or timing out makes the director stop reading results and
		//   InvokeMatchmakingFunctions return, *however* om-core does not cancel
		//   the MMFs and keeps receiving their matches and deactivating their
		//   tickets after the caller has gone. Matches the director never reads are
		//   never re-activated by it, so those tickets stay inactive until
		//   om-core's ticket TTL expires (keep MMF_INVOCATION_TIMEOUT_SECS
		//   generous).
		// - MMFs keep running until they exit (always give your MMF Run() function
		//   an exit!). For more details, read the documentation on MMF design:
		//   open-match-ecosystem/v2/examples/mmf/README.md
		// - With OM_MATCH_TICKET_DEACTIVATION_WAIT=false, om-core streams a match
		//   before its tickets are deactivated, so a very fast re-activation of a
		//   rejected ticket can be overwritten by that later deactivation, leaving
		//   the ticket inactive until it expires.
		//
		// This uses context cancellation causes to help diagnose potential
		// issues. See https://github.com/golang/go/issues/56661 for more info.
		cycleCtx, cancelCycle := context.WithCancelCause(ctx)
		invCtx := cycleCtx
		cancelTimeout := func() {}
		if mmfInvocationTimeout > 0 {
			invCtx, cancelTimeout = context.WithTimeoutCause(cycleCtx, mmfInvocationTimeout, MMFTimeoutError)
		}

		handleResponse := func(resPb *pb.StreamedMmfResponse, profileName string, rLogger *logrus.Entry) {
			match := resPb.GetMatch()

			invalidIDs, err := invalidTicketIDs(match)
			if err != nil {
				rLogger.WithField("match_id", match.GetId()).Warnf("Unable to decode invalid_ticket_ids extension: %v", err)
				invalidIDs = nil
			}
			invalidSet := make(map[string]bool, len(invalidIDs))
			for _, id := range invalidIDs {
				invalidSet[id] = true
			}

			validTicketCount := 0
			for _, roster := range match.GetRosters() {
				for _, ticket := range roster.GetTickets() {
					if !invalidSet[ticket.GetId()] {
						validTicketCount++
					}
				}
			}

			if len(invalidIDs) > 0 {
				rLogger.WithFields(logrus.Fields{
					"match_id":           match.GetId(),
					"invalid_ticket_ids": invalidIDs,
				}).Warn("om-core reported invalid ticket IDs in a returned match and did not deactivate them")
			}

			// om-core forwards every MMF response including ticketless matches and
			// match-less responses; an MMF can use them to report back (e.g. in
			// extensions). Feedback is never a proposal: it is not allocated,
			// rejected, counted in proposals, or re-activated.
			if validTicketCount == 0 {
				cycleResultsMu.Lock()
				numFeedbackResponses++
				cycleResultsMu.Unlock()

				if otelMMFFeedbackResponses != nil {
					otelMMFFeedbackResponses.Add(ctx, 1,
						metric.WithAttributes(attribute.String("profile.name", profileName)),
					)
				}
				if rLogger.Logger.IsLevelEnabled(logrus.DebugLevel) {
					var extKeys []string
					for k := range match.GetExtensions() {
						extKeys = append(extKeys, k)
					}
					slices.Sort(extKeys)
					rLogger.WithFields(logrus.Fields{
						"match_id":       match.GetId(),
						"has_match":      match != nil,
						"extension_keys": extKeys,
					}).Debug("Received MMF response with no valid tickets, treating it as MMF feedback rather than a match proposal")
				}
				return
			}

			deactFailedIDs, err := deactivationFailedTicketIDs(match)
			if err != nil {
				rLogger.WithField("match_id", match.GetId()).Warnf("Unable to decode deactivation_failed_ticket_ids extension: %v", err)
				deactFailedIDs = nil
			}

			rLogger.Trace("Received match from MMF")

			// NOTE: If you have some director match processing that can
			// be done asynchronously, like reading match metadata from
			// the extensions field or constructing a ticket collision map
			// to examine later, this is a good place to kick off that work.
			cycleResultsMu.Lock()
			proposedMatches = append(proposedMatches, match)
			if len(invalidSet) > 0 {
				matchesWithInvalidTickets[match] = true
				for id := range invalidSet {
					invalidTicketsThisCycle[id] = true
				}
			}
			if len(deactFailedIDs) > 0 {
				deactivationFailedByMatch[match] = deactFailedIDs
			}
			cycleResultsMu.Unlock()
		}

		// om-core streams every match it has before reporting an MMF failure, so
		// matches received before the error are still processed.
		handleInvocationError := func(err error, profileName string, rLogger *logrus.Entry) {
			if ctx.Err() != nil {
				rLogger.Debugf("InvokeMatchmakingFunctions ended by director shutdown: %v", err)
				return
			}

			cycleResultsMu.Lock()
			numInvocationFailures++
			cycleResultsMu.Unlock()

			cause := context.Cause(invCtx)
			st := statusFromInvocationError(err, cause)
			grpcCode := st.Code().String()

			errFields := logrus.Fields{
				"grpc_code": grpcCode,
			}
			if failedMMFs := extractFailedMMFNames(st); len(failedMMFs) > 0 {
				errFields["failed_mmfs"] = failedMMFs
			}
			if cause != nil {
				errFields["cycle_cause"] = cause
			}
			rLogger.WithFields(errFields).Errorf("InvokeMatchmakingFunctions failed: %v", err)

			if otelMMFInvocationFailures != nil {
				otelMMFInvocationFailures.Add(ctx, 1,
					metric.WithAttributes(
						attribute.String("profile.name", profileName),
						attribute.String("grpc.code", grpcCode),
					),
				)
			}
		}

		// InvokeMmfs Example
		var wg sync.WaitGroup
		for _, reqPb := range d.GSManager.GetMMFParams() {
			mmfLogFields := logrus.Fields{"operation": "mmfrequest"}

			// Get the number of mmfs to be invoked by this request.
			numMMFs := len(reqPb.GetMmfs())

			rawProfileName := reqPb.GetProfile().GetName()
			profileName := boundedProfileName(rawProfileName)

			// Add additional fields to this logger to help with debugging if it is enabled.
			if logrus.IsLevelEnabled(logrus.DebugLevel) {
				mmfLogFields["num_mmfs"] = numMMFs
				mmfLogFields["profile_name"] = profileName
				mmfLogFields["num_pools"] = len(reqPb.GetProfile().GetPools())
				for _, mmf := range reqPb.GetMmfs() {
					mmfLogFields[mmf.GetName()] = mmf.GetHost()
				}
			}
			rLogger := lLogger.WithFields(mmfLogFields)
			rLogger.Debug("kicking off MMFs")

			// Save this request to the list of pending requests, indexed by hash.
			//
			// Retrieve pre-calculated hash from the request extensions instead
			// of re-calculating it here.
			mmfReqHash, err := ex.String(reqPb.GetProfile().GetExtensions(), ex.MMFRequestHashKey)
			if err != nil {
				rLogger.Errorf("Unable to retreive MmfRequest hash from profile extensions, discarding MmfRequest: %v", err)
				continue
			}
			d.pendingMmfRequestsByHash[mmfReqHash] = reqPb

			// Track metrics.
			if otelMMFsPerOMCall != nil {
				otelMMFsPerOMCall.Record(ctx, int64(numMMFs),
					metric.WithAttributes(attribute.String("profile.name", profileName)),
				)
			}

			invoke := func() {
				defer wg.Done()
				invocationResultChan := make(chan *pb.StreamedMmfResponse)
				readerDone := make(chan struct{})
				go func() {
					defer close(readerDone)
					for resPb := range invocationResultChan {
						handleResponse(resPb, profileName, rLogger)
					}
				}()
				invErr := d.OmClient.InvokeMatchmakingFunctions(invCtx, reqPb, invocationResultChan)
				<-readerDone
				if invErr != nil {
					handleInvocationError(invErr, profileName, rLogger)
				}
			}

			wg.Add(1)
			// Backfill requests can be completed before any other request is sent, so
			// tickets in backfill matches are already deactivated when other MMFs read
			// their pools.
			if d.Cfg.GetBool("WAIT_FOR_BACKFILL_MMFS") && strings.HasPrefix(rawProfileName, backfillProfilePrefix) {
				invoke()
			} else {
				go invoke()
			}
		}

		// Wait for all MMF invocations and their readers to complete; bounded by
		// the director-side deadline and om-core's own MMF timeout.
		wg.Wait()
		cancelTimeout()
		cancelCycle(MMFComplete)
		if ctx.Err() != nil {
			lLogger.Infof("Context cancelled during matchmaking cycle: %v", ctx.Err())
			return
		}

		sort.SliceStable(proposedMatches, func(a, b int) bool {
			isBackfill := func(m *pb.Match) bool {
				if m == nil || m.GetExtensions() == nil {
					return false
				}
				pName, err := ex.String(m.GetExtensions(), "profileName")
				return err == nil && strings.HasPrefix(pName, backfillProfilePrefix)
			}
			return isBackfill(proposedMatches[a]) && !isBackfill(proposedMatches[b])
		})

		// Here's where we check each match to decide if we want to
		// allocate a server for it, or reject it. (In OM1.x, this
		// functionality lived in the 'evaluator' component of Open Match.)
		//
		// Common things to check for are:
		// - player collisions among matches,
		// - matches below a specific quality score,
		// - matches that outstrip our game server capacity, etc.
		//
		// This is done in two passes: first every proposal is accepted or rejected,
		// and only then are unclaimed tickets re-activated, because a ticket can
		// appear in a rejected match and in a later accepted one in the same cycle,
		// and re-activating it on the first rejection would put an assigned player
		// back in the pool.
		lLogger.Debugf("%v proposed matches to evaluate", len(proposedMatches))

		type acceptedProposal struct {
			match         *pb.Match
			allocatorSpec string
			backfillReq   *pb.MmfRequest
		}
		var acceptedProposals []acceptedProposal
		var rejectedMatches []*pb.Match
		acceptedTicketsThisCycle := make(map[string]bool)

		rejectMatch := func(match *pb.Match) {
			numRejectedMatches++
			numTicketsRejected += countMatchTickets(match)
			rejectedMatches = append(rejectedMatches, match)
		}

	ProposalsLoop:
		for _, match := range proposedMatches {
			numProposedTickets += countMatchTickets(match)
			mLogger := lLogger.WithFields(logrus.Fields{
				"match_id": match.GetId(),
			})

			// om-core did not reserve invalid tickets, so they are not real players
			// waiting for this match; the valid ones are re-activated later.
			if matchesWithInvalidTickets[match] {
				rejectMatch(match)
				mLogger.Warn("Rejecting match containing ticket IDs om-core reported as invalid")
				continue ProposalsLoop
			}

			// Those tickets may still be active so other MMFs can put the same
			// players in other matches, risking a double assignment; re-activating a
			// ticket that is already active is harmless, so all its tickets are
			// returned to the queue.
			if failedIDs := deactivationFailedByMatch[match]; len(failedIDs) > 0 {
				numMatchesWithDeactivationFailed++
				rejectMatch(match)
				mLogger.WithField(extDeactivationFailedTicketIDs, failedIDs).Infof(
					"Rejecting match: om-core could not deactivate %d of its tickets, so they may still be active and matched elsewhere",
					len(failedIDs),
				)
				continue ProposalsLoop
			}

			deactTimedOut, deactErr := hasDeactivationTimeout(match)
			if deactErr != nil {
				mLogger.Warnf("Unable to inspect %s extension on match: %v", ex.MatchDeactivationTimeoutKey, deactErr)
			}
			if deactTimedOut {
				matchTicketCount := countMatchTickets(match)
				numMatchesWithDeactivationTimeout++
				numTicketsWithDeactivationTimeout += matchTicketCount
				mLogger.Warnf("Match %s has %s=true (%d tickets)", match.GetId(), ex.MatchDeactivationTimeoutKey, matchTicketCount)
				if d.Cfg.GetBool("REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT") {
					rejectMatch(match)
					mLogger.Warnf("Rejecting match %s due to deactivation timeout", match.GetId())
					continue ProposalsLoop
				}
			}

			// Common decision points:
			//  - if multiple matches have the same ticket ('ticket
			//  collision'), which is used? What happens to the unused matches?
			//  Completely discarded, or colliding tickets removed, and
			//  allocated with a backfill request?
			//  - Order in which the matches are allocated servers? Probably based on
			//  some kind of quality 'score' extension field provided by the MMF?
			//  - etc.
			//
			// In this mock, we simulate RANDOM_MATCH_REJECT_PERCENT percent of
			// matches (default 1 percent) being rejected by our matchmaker because
			// we don't like them or don't have available servers to host the
			// sessions right now.
			if randomRejectPct > 0 && rand.Intn(100) < randomRejectPct {
				numRandomDropMatches++
				rejectMatch(match)
				mLogger.Debug("[TEST] this match randomly selected to be rejected")
				continue ProposalsLoop
			}

			// In the match extensions, look for the hash of the MMF request
			// that resulted in the creation of this match. It is placed in the
			// pb.MmfRequest.Profile extensions field by the game server manager
			// integration, and we must code our MMF to return it to us in
			// the match extension. (This implementation has the MMF return the
			// hash instead of the entire profile to be a bit more bandwidth
			// efficient.)
			mmfParamsHash, err := extensions.String(match.GetExtensions(), ex.MMFRequestHashKey)
			if err != nil {
				rejectMatch(match)
				mLogger.Errorf("Unable to get MmfRequest hash from returned match, rejecting it: %v", err)
				continue ProposalsLoop
			}

			// Make sure the match request parameters we got back from the MMF
			// are in our list of active matchmaking requests. If somehow we're no
			// longer tracking game servers in the Game Server Manager that are
			// looking for matches that fit these parameters, discard the match -
			// we have no game servers to put it on.
			pendingReq, exists := d.pendingMmfRequestsByHash[mmfParamsHash]
			if !exists {
				rejectMatch(match)
				mLogger.Error("Unable to get pending MmfRequest, cannot return match to the game server manager; rejecting it")
				continue ProposalsLoop
			}

			// Get the specification for how to allocate a server using the
			// current game server manager integration. This is an opaque value
			// sent to the director by the game server manager integration in
			// the mmfRequest.Profile.Extensions field. The director simply
			// passes this opaque value back to the game server manager
			// integration when asking for a server.
			allocatorSpec, err := extensions.String(
				pendingReq.GetProfile().GetExtensions(),
				ex.AgonesAllocatorKey,
			)
			if err != nil {
				rejectMatch(match)
				mLogger.Errorf("Unable to retrieve game server manager allocation specification for match, rejecting it: %v", err)
				continue ProposalsLoop
			}

			// Check if this match contains any ticket IDs we've previously assigned or already accepted in this cycle, and
			// if so, reject it.
			numAssigned := int64(0)
			numSameCycleOverlap := int64(0)
			for _, roster := range match.GetRosters() {
				for _, ticket := range roster.GetTickets() {
					tid := ticket.GetId()
					if assignments[tid] {
						numAssigned++
					}
					if acceptedTicketsThisCycle[tid] {
						numSameCycleOverlap++
					}
				}
			}
			if numAssigned > 0 {
				rejectMatch(match)
				mLogger.Debugf("Rejecting match containing %v previously assigned tickets", numAssigned)
				numMatchesWithAssignedTickets++
				numAssignedRejectedTickets += numAssigned
				continue ProposalsLoop
			} else if numSameCycleOverlap > 0 {
				rejectMatch(match)
				mLogger.Debugf("Rejecting match containing %v tickets already accepted in this cycle", numSameCycleOverlap)
				continue ProposalsLoop
			}

			// See if the MMF returned new match request parameters in the
			// match extensions field, identifying a new kind of match that the
			// game server hosting this session should request, from now on.
			// It is handed to the game server manager after the server is allocated.
			//
			// Common use cases for this are backfills, join-in-progress, and
			// high-density gameservers that can host additional sessions.
			// https://agones.dev/site/docs/integration-patterns/high-density-gameservers/
			var backfillReq *pb.MmfRequest
			if mmfReqAny := match.GetExtensions()[ex.MMFRequestKey]; mmfReqAny != nil {
				nextMMFRequest := &pb.MmfRequest{}
				if err := mmfReqAny.UnmarshalTo(nextMMFRequest); err == nil && nextMMFRequest.GetProfile().GetName() != "" {
					backfillReq = nextMMFRequest
				}
			}

			for _, roster := range match.GetRosters() {
				for _, ticket := range roster.GetTickets() {
					acceptedTicketsThisCycle[ticket.GetId()] = true
				}
			}

			acceptedProposals = append(acceptedProposals, acceptedProposal{
				match:         match,
				allocatorSpec: allocatorSpec,
				backfillReq:   backfillReq,
			})
		}

		// om-core deactivates the tickets in every match it returns, so seeing
		// previously assigned tickets here means the tickets were active again when
		// a later cycle's pools were filled. Common causes are the cycle starting
		// before om-core's ticket cache replicated the earlier deactivation
		// (lengthen MM_CYCLE_MIN_DURATION_MS, or check om-core cache replication
		// lag/load), or a re-activation that raced om-core's deactivation of the
		// same ticket (see the cycle context comment). Occasional occurrences are
		// expected; a steady rate is worth investigating.
		if numAssignedRejectedTickets > 0 {
			d.Log.Warnf("%v previously assigned ticketIds were found in %v proposed matches, which were rejected",
				numAssignedRejectedTickets, numMatchesWithAssignedTickets)
		}

		// Second pass: queue unclaimed tickets for re-activation, skipping IDs
		// that are (1) previously assigned, (2) accepted this cycle, (3) already
		// queued this cycle, or (4) in this cycle's invalid-ID set (om-core would
		// just reject invalid IDs).
		queuedForReactivationThisCycle := make(map[string]bool)
		reactivateMatches := func(matchesToReactivate ...*pb.Match) bool {
			for _, m := range matchesToReactivate {
				for _, roster := range m.GetRosters() {
					for _, ticket := range roster.GetTickets() {
						tid := ticket.GetId()
						if assignments[tid] ||
							acceptedTicketsThisCycle[tid] ||
							queuedForReactivationThisCycle[tid] ||
							invalidTicketsThisCycle[tid] {
							continue
						}
						d.Log.Tracef("sending ticketid %v for re-activation", tid)
						if !enqueueReactivation(tid) {
							return false
						}
						queuedForReactivationThisCycle[tid] = true
						numIdsToActivate++
					}
				}
			}
			return true
		}

		if !reactivateMatches(rejectedMatches...) {
			lLogger.Infof("Context cancelled while queueing ticket re-activations: %v", ctx.Err())
			return
		}

		//-----------------------------

		for _, entry := range acceptedProposals {
			match := entry.match
			// TODO: Here is where you allocate the servers for your approved
			// matches, using your game server manager integration.
			//
			// The mockAgonesIntegration used in this example does not do
			// actual allocations, instead it just prints a log message saying
			// that an allocation would have taken place.
			connString := d.GSManager.Allocate(entry.allocatorSpec, match)

			// If there is a game server to send tickets to, make assignments.
			if connString == "" {
				// No valid connection string returned from the game server
				// manager, there must be an issue. Put these tickets back into the
				// matchmaking pool.
				for _, roster := range match.GetRosters() {
					for _, ticket := range roster.GetTickets() {
						delete(acceptedTicketsThisCycle, ticket.GetId())
					}
				}
				rejectMatch(match)
				if !reactivateMatches(match) {
					lLogger.Infof("Context cancelled while queueing ticket re-activations: %v", ctx.Err())
					return
				}
				continue
			}

			numAcceptedMatches++
			d.Log.Tracef("Match session assigned to game server %v", connString)

			// Now that the server is allocated, give the manager any new matchmaking
			// request the MMF attached.
			if entry.backfillReq != nil {
				lLogger.WithField("match_id", match.GetId()).Trace("Allocated a match with a backfill request attached, sending backfill request to game server manager")
				_ = d.GSManager.UpdateMMFRequest(entry.allocatorSpec, entry.backfillReq)
				newMMFRequestsProcessed++
			}

			// Stream out the assignments to the component that sends them to game
			// clients. In this example it is the mmqueue.
			for _, roster := range match.GetRosters() {
				// Generate a new assignment protobuf message
				assignment := &pb.Assignment{Connection: connString}
				// Give the assignment to this roster
				roster.Assignment = assignment

				// Attempt to send this roster assignment
				expBackoff := backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(5 * time.Second))
				err := backoff.RetryNotify(
					func() error {
						// The call we want to retry with exponential backoff and jitter
						return d.AssignmentPublisher.Send(ctx, roster)
					},
					backoff.WithContext(expBackoff, ctx),
					func(err error, bo time.Duration) {
						d.Log.Warnf("Sending match session assignment temporary failure (backoff for %v): %v", bo, err)
					},
				)
				if err == nil { // roster assignment successfully sent
					numTicketAssignments += int64(len(roster.GetTickets()))

					ttl := time.Now().Add(assignmentTTLDuration)
					// Track which tickets we've already assigned
					for _, ticket := range roster.GetTickets() {
						assignments[ticket.GetId()] = true
						assignmentTTL[ttl] = append(assignmentTTL[ttl], ticket.GetId())
					}
				} else if ctx.Err() != nil {
					// Not a publisher failure, but the allocation is still unused.
					d.Log.Warnf("Assignment for game server %v not sent because the director is shutting down: %v", connString, err)
				} else {
					// TODO: You must decide what to do in the case where
					// the match assigment could not be sent back to the
					// client, but the server was successfully allocated
					// and is waiting for players. This example just logs
					// this failure case, as there is no actual server
					// allocation. In a production matchmaker, not taking
					// action here to fix the problem would result in a
					// leaked server allocation!
					d.Log.Errorf("Leaked server allocation %v because the assignment couldn't be returned to the game clients!", connString)
				}
			}
		}

		// Expire tracked ticket assignments to keep the
		// assignments map from growing forever
		for key, value := range assignmentTTL {
			if time.Now().After(key) {
				// Loop thru tix
				for _, tid := range value {
					// Stop tracking this ticktet's assignment
					delete(assignments, tid)
				}
				// Stop tracking this ttl
				delete(assignmentTTL, key)
			}
		}

		// Periodially deep copy the assignment tracking maps.
		if time.Now().After(assignmentMemoryReclaimationDeadline) {
			assignments = maps.Clone(assignments)
			assignmentTTL = maps.Clone(assignmentTTL)

			// Make a new deadline to repeat this process.
			assignmentMemoryReclaimationDeadline = time.Now().Add(assignmentTTLDuration)
		}

		// Calculate length of time to sleep before next loop.
		// We recommend this is >= your configured OM_CACHE_IN_WAIT_TIMEOUT_MS
		//
		// Best practice is to wait a while before invoking MMFs again, to
		// allow ticket re-activations to complete. If you're getting too many
		// rejected matches because they contain tickets that have already been
		// assigned to game servers, increase this length gradually until
		// you've reached a number of rejected matches you're comfortable with.
		// Bear in mind that this length directly correlates to how long
		// players will have to wait to receive a match.
		assignedBackoffFactor := computeCycleBackoffFactor(
			int64(len(proposedMatches)),
			numMatchesWithAssignedTickets,
			numMatchesWithDeactivationTimeout,
		)
		baseMinCycleDurMs := d.Cfg.GetFloat64("MM_CYCLE_MIN_DURATION_MS")
		minCycleDur := time.Millisecond * time.Duration(int64(baseMinCycleDurMs*assignedBackoffFactor))
		sleepDur := time.Until(startTime.Add(minCycleDur))
		elapsedDuration := time.Since(startTime)

		// Record Open Telemetry metrics for this cycle. Cycle duration is recorded
		// once the cycle, including its inter-cycle sleep, is over.
		if otelTicketsRejectedPerCycle != nil {
			otelTicketsRejectedPerCycle.Record(ctx, numTicketsRejected)
		}
		if otelAssignedTicketsInMatchesPerCycle != nil {
			otelAssignedTicketsInMatchesPerCycle.Record(ctx, numAssignedRejectedTickets)
		}
		if otelTicketActivationsPerCycle != nil {
			otelTicketActivationsPerCycle.Record(ctx, numIdsToActivate)
		}
		if otelTicketAssignmentsPerCycle != nil {
			otelTicketAssignmentsPerCycle.Record(ctx, numTicketAssignments)
		}
		if otelNewMMFRequestsProcessedPerCycle != nil {
			otelNewMMFRequestsProcessedPerCycle.Record(ctx, newMMFRequestsProcessed)
		}
		if otelMatchesRejectedPerCycle != nil {
			otelMatchesRejectedPerCycle.Record(ctx, numRejectedMatches)
		}
		if otelMatchesWithAssignedTicketsPerCycle != nil {
			otelMatchesWithAssignedTicketsPerCycle.Record(ctx, numMatchesWithAssignedTickets)
		}
		if otelMatchesDeactivationTimeoutPerCycle != nil {
			otelMatchesDeactivationTimeoutPerCycle.Record(ctx, numMatchesWithDeactivationTimeout)
		}
		if otelTicketsDeactivationTimeoutPerCycle != nil {
			otelTicketsDeactivationTimeoutPerCycle.Record(ctx, numTicketsWithDeactivationTimeout)
		}
		if otelMatchesProposedPerCycle != nil {
			otelMatchesProposedPerCycle.Record(ctx, int64(len(proposedMatches)))
		}

		// Status update closure.
		{
			matchesPct := 0.0
			if len(proposedMatches) > 0 {
				matchesPct = float64(numAcceptedMatches) / float64(len(proposedMatches)) * 100.0
			}
			ticketsPct := 0.0
			if numProposedTickets > 0 {
				ticketsPct = float64(numTicketAssignments) / float64(numProposedTickets) * 100.0
			}
			cycleUsedPct := 0.0
			if minCycleDur.Milliseconds() > 0 {
				cycleUsedPct = float64(elapsedDuration.Milliseconds()) / float64(minCycleDur.Milliseconds()) * 100.0
			}

			cycleStatus := fmt.Sprintf(
				"%05d/%05d (%4.1f%%) matches accepted (%05d rejected, %05d random_drop, %05d assigned_tickets, %05d deactivation_timeout, %05d deactivation_failed)\n"+
					"%05d/%05d (%4.1f%%) tickets accepted (%05d re-activated, %05d already_assigned)\n"+
					"%05d MMF feedback responses, %05d failed InvokeMatchmakingFunctions calls\n"+
					"Cycle took %dms (minimum cycle duration calculate as %dms * %3.1fbackoff = %dms)\n"+
					"%5.1f%% of minimum cycle duration used",
				numAcceptedMatches,
				len(proposedMatches),
				matchesPct,
				numRejectedMatches,
				numRandomDropMatches,
				numMatchesWithAssignedTickets,
				numMatchesWithDeactivationTimeout,
				numMatchesWithDeactivationFailed,
				numTicketAssignments,
				numProposedTickets,
				ticketsPct,
				numIdsToActivate,
				numAssignedRejectedTickets,
				numFeedbackResponses,
				numInvocationFailures,
				elapsedDuration.Milliseconds(),
				d.Cfg.GetInt("MM_CYCLE_MIN_DURATION_MS"),
				assignedBackoffFactor,
				minCycleDur.Milliseconds(),
				cycleUsedPct,
			)

			d.statusMu.Lock()
			d.cycleStatus = cycleStatus
			d.statusMu.Unlock()

			lLogger.Info(cycleStatus)
		}

		// A failed cycle says nothing about whether tickets remain, so it neither
		// counts as empty nor resets the count.
		if numInvocationFailures > 0 {
			// Leave consecutiveEmptyCycles unchanged.
		} else if len(proposedMatches) == 0 {
			consecutiveEmptyCycles++
		} else {
			consecutiveEmptyCycles = 0
		}
		if maxEmpty := d.Cfg.GetInt("NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT"); maxEmpty > 0 && consecutiveEmptyCycles >= maxEmpty {
			lLogger.Infof("Reached %d consecutive empty matchmaking cycles (limit %d), exiting", consecutiveEmptyCycles, maxEmpty)
			recordCycleDuration()
			break
		}

		// Sleep the calculated time unless the context is cancelled.
		if sleepDur > 0 {
			sleepTimer := time.NewTimer(sleepDur)
			select {
			case <-ctx.Done():
				sleepTimer.Stop()
				recordCycleDuration()
				return
			case <-sleepTimer.C:
			}
		}
		recordCycleDuration()
	} // end of main matchmaking loop
}
