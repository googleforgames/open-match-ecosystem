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
//
// NOTE: WIP, this is a testbed right now.
package gsdirector

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	"github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/zeebo/xxh3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
	knownpb "google.golang.org/protobuf/types/known/wrapperspb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156
	"google.golang.org/protobuf/types/known/anypb"
	_ "google.golang.org/protobuf/types/known/wrapperspb"

	"open-match.dev/open-match-ecosystem/v2/internal/extensions"
	ex "open-match.dev/open-match-ecosystem/v2/internal/extensions"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

var (
	logger            = &logrus.Logger{}
	statusUpdateMutex sync.RWMutex
	tickets           sync.Map

	// pool with a set of filters to find all players. Really only
	// useful in development and testing; Always use some meaningful
	// filters in production matchmaking Pools!
	EveryTicket = &pb.Pool{
		Name: "everyone",
		CreationTimeRangeFilter: &pb.Pool_CreationTimeRangeFilter{
			Start: timestamppb.New(time.Now().Add(time.Hour * -1)),
			End:   timestamppb.New(time.Now().Add(time.Hour * 100)),
		},
	}

	// Pool with a set of filters to get tickets where the player selected a
	// tank class
	Tanks = &pb.Pool{Name: "tank",
		StringEqualsFilters: []*pb.Pool_StringEqualsFilter{
			&pb.Pool_StringEqualsFilter{StringArg: "class", Value: "tank"}}}

	// Pool with a set of filters to get where the player selected a dps class
	DPS = &pb.Pool{Name: "dps",
		StringEqualsFilters: []*pb.Pool_StringEqualsFilter{
			&pb.Pool_StringEqualsFilter{StringArg: "class", Value: "dps"}}}

	// Pool with a set of filters to get where the player selected a healer class
	Healers = &pb.Pool{Name: "healer",
		StringEqualsFilters: []*pb.Pool_StringEqualsFilter{
			&pb.Pool_StringEqualsFilter{StringArg: "class", Value: "healer"}}}

	// Matchmaking function specifications
	Fifo = &pb.MatchmakingFunctionSpec{
		Name: "FIFO",
		Host: "http://localhost",
		Port: 50090,
		Type: pb.MatchmakingFunctionSpec_GRPC,
	}
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
		"asia-northeast1-a": []*GameMode{SoloDuel},
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
		"asia-northeast1-a": &pb.Pool{
			DoubleRangeFilters: []*pb.Pool_DoubleRangeFilter{
				&pb.Pool_DoubleRangeFilter{
					DoubleArg: "ping.asia-northeast1-a",
					Minimum:   1,
					Maximum:   120,
				},
			},
		},
		// All other zones aren't used currently and are included for
		// instructional purposes.
		"asia-northeast3-b":      &pb.Pool{},
		"asia-south2-c":          &pb.Pool{},
		"australia-southeast1-c": &pb.Pool{},
		"europe-central2-b":      &pb.Pool{},
		"europe-west3-a":         &pb.Pool{},
		"me-central2-a":          &pb.Pool{},
		"us-east4-b":             &pb.Pool{},
		"us-west2-c":             &pb.Pool{},
		"southamerica-east1-c":   &pb.Pool{},
		"africa-south1-b":        &pb.Pool{},
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
		"APAC": map[string]string{
			"JP_Tokyo":  "asia-northeast1-a",
			"KR_Seoul":  "asia-northeast3-b",
			"IN_Delhi":  "asia-south2-c",
			"AU_Sydney": "australia-southeast1-c",
		},
		"EMEA": map[string]string{
			"PL_Warsaw":    "europe-central2-b",
			"DE_Frankfurt": "europe-west3-a",
			"SA_Dammam":    "me-central2-a",
		},
		"NA": map[string]string{
			"US_Ashburn":    "us-east4-b",
			"US_LosAngeles": "us-west2-c",
		},
		"SA": map[string]string{
			"BR_SaoPaulo": "southamerica-east1-c",
		},
		"Africa": map[string]string{
			"ZA_Johannesburg": "africa-south1-b",
		},
	}

	NoSuchAllocationSpecError = errors.New("No such AllocationSpec exists")
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
		update.gs.allocSpec = m.gameServers[defunctAllocationSpecJSON].server.allocSpec
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
// allocate a server which can host a game sessions created by the input
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

	//r := &T{MmfReqs: []string{string(reqBytes[:]), string(reqBytes[:])}}
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
			allocationv1.GameServerSelector{
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

// ProcessGameServerUpdatesis a mock for Agones Integration code you'd write to
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

	m.Log.Debug("Initializing matching parameters")

	// appendPoolFilters copies the filters from src pool to dest pool, allowing us
	// to combine filters from different pools to create pools dynamically.
	//
	// NOTE: If the destination contains a CreationTimeFilter and the source does
	// not, the destination CreationTimeFilter will be preserved.
	appendPoolFilters := func(dest *pb.Pool, src *pb.Pool) *pb.Pool {
		dest.TagPresentFilters = append(dest.TagPresentFilters, src.GetTagPresentFilters()...)
		dest.DoubleRangeFilters = append(dest.DoubleRangeFilters, src.GetDoubleRangeFilters()...)
		dest.StringEqualsFilters = append(dest.StringEqualsFilters, src.GetStringEqualsFilters()...)

		// Check that the source has a creation time filter before copying to avoid
		// an empty src filter overwriting an existing dest filter. This can only
		// happen with this type of filter, as all others are contained in arrays.
		if src.GetCreationTimeRangeFilter() != nil {
			dest.CreationTimeRangeFilter = src.GetCreationTimeRangeFilter()
		}
		return dest
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
				fLogger := logger.WithFields(logrus.Fields{
					"fleet": fleetName,
				})
				fLogger.Debug("Adding matching parameters to fleet")

				// Make an array of all the matchmaking profiles that apply to
				// this fleet, one profile per game mode.
				mpa := &mmfParametersAnnotation{mmfParams: make([]string, 0)} // String version to attach to k8s metadata
				mGSSet.server.mmfParams = make([]*pb.MmfRequest, 0)           // protobuf version to send to Open Match.

				// retrieve filters for this specific datacenter.
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

					// Generate sets of filters (held in pb.Pool structures) that define the kinds of
					// tickets this game mode in this zone wants to use for matching.
					composedPools := map[string]*pb.Pool{}
					for pname, thisModePool := range mode.Pools {
						composedName := fmt.Sprintf("%v.%v", fleetName, pname)
						// Make a new pool with a name combining the fleet and the game mode's pool name
						composedPool := &pb.Pool{Name: composedName}

						// Add filters to this pool
						composedPool = appendPoolFilters(composedPool, zonePool)
						appendPoolFilters(composedPool, thisModePool)
						composedPools[composedName] = composedPool
					}

					// Add this mmf request to our list of parameters
					mGSSet.server.mmfParams = append(mGSSet.server.mmfParams, &pb.MmfRequest{
						Mmfs: mode.MMFs,
						Profile: &pb.Profile{
							Name:       profileName,
							Pools:      composedPools,
							Extensions: mode.ExtensionParams, // Include all the user-defined data in the game mode extensions field.
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
				mmfParamsJSON, err := json.Marshal(mpa)
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
						allocationv1.GameServerSelector{
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
// If the game server instance being allocated needs to include a new
// matchmaking request, that needs to be sent via the UpdateMMFRequest()
// function before Allocate() is called, as Agones sets the metadata atomically
// on game server allocation.
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
	m.updateGameServerMetadata(allocationSpecJSON, []*pb.MmfRequest{})
	// In a real implementation, you'd receive the agonesv1.GameServerStatus{}
	// object from kubernetes/the Agones SDK.  Here we generate a mock one instead.
	mockGameServerStatus := &agonesv1.GameServerStatus{
		Ports: []agonesv1.GameServerStatusPort{
			agonesv1.GameServerStatusPort{
				Name: "default",
				Port: 7463},
		},
		Address:  "1.2.3.4",
		NodeName: "node-name",
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
				extensions := request.GetProfile().GetExtensions()
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

// MockDirector aims to have the structure of a reasonably realistic,
// feature-complete matchmaking Director that would use a game server backend
// integration to interface with game servers.
type MockDirector struct {
	OmClient                 *omclient.RestfulOMGrpcClient
	Cfg                      *viper.Viper
	Log                      *logrus.Logger
	AssignmentsChan          chan *pb.Roster
	GSManager                GameServerManager
	OtelMeterPtr             *metric.Meter
	pendingMmfRequestsByHash map[string]*pb.MmfRequest
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
// are sent in assignment Rosters to the 'AssignmentChan' channel.  In
// production we recommend that you handle returning game server assignments to
// game clients using a dedicated player status service. om-core has deprecated
// assignment endpoints, but since those can cause significant load that can
// impact matching performance, we don't recommend their use in production.
// (They are provided for legacy reasons.)
func (d *MockDirector) Run(ctx context.Context) {

	// Add fields to structured logging for this function
	logger := d.Log.WithFields(logrus.Fields{
		"app":       "matchmaker",
		"component": "game_server_director",
	})
	logger.Tracef("Initializing game server director directing matches to game servers")

	// var init
	d.pendingMmfRequestsByHash = make(map[string]*pb.MmfRequest)
	ticketIdsToActivate := make(chan string, 10000)
	var startTime time.Time
	var proposedMatches []*pb.Match
	var matches map[*pb.Match]string
	assignments := make(map[string]bool)
	assignmentTTL := make(map[time.Time][]string)

	// Initialize metrics
	if d.OtelMeterPtr != nil {
		logger.Tracef("Initializing otel metris")
		registerMetrics(d.OtelMeterPtr)
	}

	// goroutine that runs for the lifetime of the game server director to
	// re-activate rejected tickets so they appear in matchmaking pools again.
	// We'll send all ticket ids from rejected matches to the
	// ticketIdsToActivate channel.
	go func() {
		aLogger := logger.WithFields(logrus.Fields{"operation": "ticket_reactivation"})
		aLogger.Trace("requesting re-activation of rejected tickets")

		// request re-activation of rejected tickets.
		atCtx := context.WithValue(ctx, "activationType", "re-activate")
		d.OmClient.ActivateTickets(atCtx, ticketIdsToActivate)
	}()

	// Kick off main matchmaking loop
	// Loop for the configured number of cycles
	lLogger := logger.WithFields(logrus.Fields{"operation": "matchmaking_loop"})
	lLogger.Debugf("Beginning matchmaking cycle, %v cycles configured", d.Cfg.GetInt("NUM_MM_CYCLES"))
	for i := d.Cfg.GetInt("NUM_MM_CYCLES"); i > 0; i-- { // Main Matchmaking Loop

		// Loop var init
		startTime = time.Now()
		proposedMatches = []*pb.Match{}
		matches = map[*pb.Match]string{}

		// Metrics tracking
		newMMFRequestsProcessed := int64(0)
		numTicketAssignments := int64(0)
		numRejectedMatches := int64(0)
		numIdsToActivate := int64(0)

		// Make a fresh context for this loop.
		//
		// Cancellation in the context of matchmaking functions is a
		// bit complicated:
		// - Cancelling the context causes OM to stop processing matches
		// returned by your MMF(s) and return immediately.
		// - *HOWEVER*, MMFs invoked by OM don't respect context cancellation
		// (by design).  Although any additional matches returned by an MMF
		// after the context is cancelled are silently dropped, your MMF
		// will keep  processing until it exits (so, make sure you always
		// have an exit for your MMF Run() function!). For more details
		// about why this is useful, read the documentation on MMF design.
		//
		// NOTE: OM won't take longer than its configured timeout to return
		// matches (determined by the OM_MMF_TIMEOUT_SECS config var in
		// your om-core deployment). The
		// `rClient.InvokeMatchmakingFunctions()` will always return.
		//
		// TODO: make timeout configurable
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)

		// Make a channel-of-channels that contains the response channels for all the
		// concurrent calls we'll make to OM.
		fanInChan := make(chan chan *pb.StreamedMmfResponse)

		// Launch asynchronous match result processing function.
		//
		// We make a separate reponse channel for every InvokeMMF call - see
		// the 'rClient.InvokeMatchmakingFunctions()' call below.  Each of
		// those channels is sent to this match result processing goroutine on
		// the 'fanInChan' channel (it is a channel-of-channels). This allows
		// all results from all MMFs in one cycle to be processed in one code
		// path at the expense of complexity.
		wg := sync.WaitGroup{}
		go func() {

			// For every channel received on fanInChan, kick off a
			// goroutine to read all matches returned on it.
			//
			// This goroutine pauses here until the
			// 'rClient.InvokeMatchmakingFunctions()' calls below start
			// returning matches.
			for resChan := range fanInChan {
				lLogger.Trace("received mmf invocation response channel on fanInChan")

				// Asynchronously read all matches from this channel.
				go func() {
					for resPb := range resChan {

						// Process this returned match.
						if resPb == nil {
							// Something went wrong.
							lLogger.Trace("StreamedMmfResponse protobuf was nil!")
							otelMMFResponseFailures.Add(ctx, 1)
						} else {
							// We got a match result.
							lLogger.Trace("Received match from MMF")

							// NOTE: If you have some director match processing that can
							// be done asynchronously, like reading match metadata from
							// the extensions field or constructing a ticket collision map
							// to examine later, this is a good place to kick off that work.
							proposedMatches = append(proposedMatches, resPb.GetMatch())
						} // end of else clause

					} // end of loop over all matches on this individual channel.

					// Mark the MMF invocation that is returning
					// matches on this channel as complete.
					wg.Done()

				}() // end of asynchronous individual channel processing goroutine.

			} // end of loop over channel of channels.

		}() // end of asynchronous channel fan-in goroutine.

		// InvokeMmfs Example
		requests := d.GSManager.GetMMFParams()
		for _, reqPb := range requests {
			wg.Add(1)
			mmfLogFields := logrus.Fields{"operation": "mmfrequest"}

			// Get the number of mmfs to be invoked by this request.
			numMMFs := len(reqPb.GetMmfs())

			// By convention, profile names should use reverse-DNS notation
			// https://en.wikipedia.org/wiki/Reverse_domain_name_notation This
			// helps with metric attribute/log field cardinality, as we can record
			// profile names alongside metric readings after stripping off the
			// most-unique portion.
			profileName := reqPb.GetProfile().GetName()
			i := strings.LastIndex(profileName, ".")
			if i > 0 {
				profileName = profileName[0:i]
			}

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
			mmfReqHash, err := ex.String(reqPb.Profile.GetExtensions(), ex.MMFRequestHashKey)
			if err != nil {
				rLogger.Errorf("Unable to retreive MmfRequest hash from profile extensions, discarding MmfRequest: %v", err)
				continue
			}
			d.pendingMmfRequestsByHash[mmfReqHash] = reqPb

			// Make a channel for responses from this MMF invocation.
			invocationResultChan := make(chan *pb.StreamedMmfResponse)

			// Tell the fan-in goroutine to monitor this new channel for matches.
			fanInChan <- invocationResultChan

			// Invoke matchmaking functions, match results are sent back
			// using the channel we just created.
			go d.OmClient.InvokeMatchmakingFunctions(ctx, reqPb, invocationResultChan)

			// Track metrics.
			otelMMFsPerOMCall.Record(ctx, int64(numMMFs),
				metric.WithAttributes(attribute.String("profile.name", profileName)),
			)

		}

		// Wait for all results from all MMFs. OM won't take longer than its configured
		// timeout to return matches (determined by the OM_MMF_TIMEOUT_SECS config var
		// in your om-core deployment).
		wg.Wait()
		// wg.Wait() returns after all invocationResultChans are empty, at the end of the
		// synchronous individual channel processing goroutine above. We can then safely close
		// the fanInChan so the asynchronous channel fan-in goroutine knows it can exit as
		// no more match results are coming in this cycle.
		close(fanInChan)

		// Here's where we check each match to decide if we want to
		// allocate a server for it, or reject it.  (In OM1.x, this
		// functionality lived in the 'evaluator' component of Open Match.)
		//
		// Common things to check for are:
		// - player collisions among matches,
		// - matches below a specific quality score,
		// - matches that outstrip our game server capacity, etc.
		lLogger.Debugf("%v proposed matches to evaluate", len(proposedMatches))

		// Here is where you loop over all the returned match proposals and make decisions.
		//
		// Here's a simple local function to 'reject' a proposed match by adding all the
		// tickets in it to the channel of ticket ids to reactivate in OM.
		rejectMatch := func(match *pb.Match) {
			numRejectedMatches++
			for _, roster := range match.GetRosters() {
				for _, ticket := range roster.GetTickets() {
					// queue this ticket ID to be re-activated
					d.Log.Tracef("sending ticketid %v for re-activation", ticket.GetId())
					ticketIdsToActivate <- ticket.GetId()
					otelTicketsRejected.Add(ctx, 1)
				}
				numIdsToActivate += int64(len(roster.GetTickets()))
			}
		}

	ProposalsLoop:
		for _, match := range proposedMatches {
			mLogger := lLogger.WithFields(logrus.Fields{
				"match_id": match.Id,
			})
			// Common decision points:
			//  - if multiple matches have the same ticket ('ticket
			//  collision'), which is used? What happens to the unused matches?
			//  Completely discarded, or colliding tickets removed, and
			//  allocated with a backfill request?
			//  - Order in which the matches are allocated servers? Probably based on
			//  some kind of quality 'score' extension field provided by the MMF?
			//  - etc.
			//
			// In this mock, we simulate 1 in 100 matches being rejected by our
			// matchmaker because we don't like them or don't have available
			// servers to host the sessions right now.
			if rand.Intn(100) <= 1 {
				mLogger.Warn("[TEST] this match randomly selected to be rejected")
				rejectMatch(match)
				continue ProposalsLoop
			}

			// In the match extensions, look for the hash of the MMF request
			// that resulted in the creation of this match. It is placed in the
			// pb.MmfRequest.Profile extensions field by the game server manager
			// integration, and we have to code our MMF to return it to us in
			// the match extension. (This implementation has the MMF return the
			// hash instead of the entire profile to be a bit more bandwidth
			// efficient.)
			mmfParamsHash, err := extensions.String(match.Extensions, ex.MMFRequestHashKey)
			if err != nil {
				// Couldn't get the hash, reject match.
				mLogger.Errorf("Unable to get MmfRequest hash from returned match, rejecting: %v", err)
				rejectMatch(match)
				continue ProposalsLoop
			}

			// Make sure the match request parameters we got back from the MMF
			// are in our list of active matchmaking requests. If somehow we're no
			// longer tracking game servers in the Game Server Manager that are
			// looking for matches that fit these parameters, discard the match -
			// we have no game servers to put it on.
			if _, exists := d.pendingMmfRequestsByHash[mmfParamsHash]; !exists {
				// No request with that hash in our list, reject match.
				mLogger.Error("Unable to get pending MmfRequest, cannot return match, rejecting")
				rejectMatch(match)
				continue ProposalsLoop
			}

			// Get the specification for how to allocate a server using the
			// current game server manager integration. This is an opaque value
			// sent to the director by the game server manager integration in
			// the mmfRequest.Profile.Extensions field. The director simply
			// passes this opaque value back to the game server manager
			// integration when asking for a server.
			allocatorSpec, err := extensions.String(
				d.pendingMmfRequestsByHash[mmfParamsHash].GetProfile().GetExtensions(),
				ex.AgonesAllocatorKey,
			)
			if err != nil {
				mLogger.Errorf("Unable to retrieve allocation specification for game server manager for returned match, rejecting: %v", err)
				rejectMatch(match)
				continue ProposalsLoop
			}

			// Check if this match contains any ticket IDs we've previously assigned, and
			// if so, reject it.
			for _, roster := range match.GetRosters() {
				for _, ticket := range roster.GetTickets() {
					// queue this ticket ID to be re-activated
					if _, previouslyAssigned := assignments[ticket.GetId()]; previouslyAssigned {
						mLogger.Errorf("ticketid %v previously assigned, rejecting", ticket.GetId())
						rejectMatch(match)
						continue ProposalsLoop
					}

				}
			}

			// See if the MMF returned new match request parameters in the
			// match extensions field, identifying a new kind of match that the
			// game server hosting this session should request, from now on.
			//
			// Common use cases for this are backfills, join-in-progress, and
			// high-density gameservers that can host additional sessions.
			// https://agones.dev/site/docs/integration-patterns/high-density-gameservers/
			nextMMFRequest := &pb.MmfRequest{}
			err = match.Extensions[ex.MMFRequestKey].UnmarshalTo(nextMMFRequest)
			if err == nil && nextMMFRequest.GetProfile().GetName() != "" {
				mLogger.Trace("Approved a match with a backfill request attached, sending backfill request to game server manager")
				allocatorSpec = d.GSManager.UpdateMMFRequest(allocatorSpec, nextMMFRequest)
				newMMFRequestsProcessed++
			}

			// This proposed match passed all our decision points and validation checks,
			// lets add it to the map of matches we want to allocate a server for.
			//acceptedMatchesCounter.Add(ctx, 1)
			matches[match] = allocatorSpec
		}

		for match, allocatorSpec := range matches {
			// TODO: Here is where you allocate the servers for your approved
			// matches,  using your game server manager integration.
			//
			// The mockAgonesIntegration used in this example does not do
			// actual allocations, instead it just prints a log message saying
			// that an allocation would have taken place.
			connString := d.GSManager.Allocate(allocatorSpec, match)

			// If there is a game server to send tickets to, make assignments.
			if connString != "" {
				d.Log.Tracef("Match session assigned to game server %v", connString)

				// Stream out the assignments to the mmqueue
				for _, roster := range match.GetRosters() {
					// queue this ticket ID to be re-activated
					d.AssignmentsChan <- roster
					numTicketAssignments += int64(len(roster.GetTickets()))

					// TODO
					for _, ticket := range roster.GetTickets() {
						assignments[ticket.GetId()] = true
						ttl := time.Now().Add(time.Second * 600)
						if _, nilArray := assignmentTTL[ttl]; nilArray {
							assignmentTTL[ttl] = make([]string, 0)
						}
						assignmentTTL[ttl] = append(assignmentTTL[ttl], ticket.GetId())
					}

				}

			} else {
				// No valid connection string returned from the game server
				// manager, there must be an issue. Put these tickets back into the
				// matchmaking pool.
				rejectMatch(match)
			}

		}

		// TODO: temp hack to test assignment match rejection
		// This is terribly inefficient, only for testing
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

		// Calculate length of time to sleep before next loop.
		// TODO: proper exp BO + jitter
		// First, wait until ticket (re-)activations are complete
		// sleepDur := time.Until(startTime.Add(time.Millisecond * 1000)) // >= OM_CACHE_IN_WAIT_TIMEOUT_MS for efficiency's sake
		sleepDur := time.Until(startTime.Add(time.Second * 5)) // >= OM_CACHE_IN_WAIT_TIMEOUT_MS for efficiency's sake
		dur := time.Now().Sub(startTime)
		//sleepDur = (time.Millisecond * 1000) - dur

		// Record metrics for this cycle
		otelMatchesRejectedPerCycle.Record(ctx, numRejectedMatches)
		otelTicketActivationsPerCycle.Record(ctx, numIdsToActivate)
		otelNewMMFRequestsProcessedPerCycle.Record(ctx, newMMFRequestsProcessed)
		otelTicketAssignmentsPerCycle.Record(ctx, numTicketAssignments)
		otelMatchesProposedPerCycle.Record(ctx, int64(len(proposedMatches)))
		otelMatchmakingCycleDuration.Record(ctx, float64(dur.Microseconds()/1000))

		// Status update closure. This is currently just for development
		// debugging and may be removed or made into a separate function in
		// future versions
		{
			numCycles := fmt.Sprintf("/%v", d.Cfg.GetInt("NUM_MM_CYCLES"))
			if d.Cfg.GetInt("NUM_MM_CYCLES") > 600000 { // ~600k seconds is roughly a week
				numCycles = "/--" // Number is so big as to be meaningless, just don't display it
			}

			// Write the status of this cycle to the logger.
			cycleStatus := fmt.Sprintf("Cycle %06d%v: %4d/%4d matches accepted, next invocation in %v ms", d.Cfg.GetInt("NUM_MM_CYCLES")+1-i, numCycles, len(matches), len(proposedMatches), sleepDur.Milliseconds())
			lLogger.Info(cycleStatus)
		}

		// Sleep the calculated time.
		time.Sleep(sleepDur)

		// Release resources associated with this context.
		cancel()
	} // end of main matchmaking loop
}
