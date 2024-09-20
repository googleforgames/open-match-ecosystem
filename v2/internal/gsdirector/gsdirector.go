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
	"sync"
	"time"

	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	"github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/zeebo/xxh3"
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
	cycleStatus       string
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
	Dps = &pb.Pool{Name: "dps",
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
		Port: 50080,
		Type: pb.MatchmakingFunctionSpec_GRPC,
	}

	// Game mode specifications
	SoloDuel = &GameMode{
		Name:  "SoloDuel",
		Mmfs:  []*pb.MatchmakingFunctionSpec{Fifo},
		Pools: map[string]*pb.Pool{"all": EveryTicket},
		ExtensionParams: extensions.AnypbIntMap(map[string]int32{
			"desiredNumRosters": 1,
			"desiredRosterLen":  4,
			"minRosterLen":      2,
		}),
	}

	// Nested map structure defining which game modes are available in which
	// data centers. This mocks out one Agones fleet in each region.
	//
	// There are many ways you could do this, this approach organizes the
	// fleets into geographic 'categories' and drills all the way down to which
	// game mode is available in each zonal fleet.
	// e.g. FleetConfig[category][region][GCP_zone] = [gamemode,...]
	//
	// In a production system, where you get this information is based on
	// how you operate your infrastructure. For example:
	// - read this from your infrastructure-as-code repository (e.g. using terraform)
	// - query your infrastructure layer directly (e.g. using gcloud)
	FleetConfig = map[string]map[string]map[string][]*GameMode{
		"APAC": map[string]map[string][]*GameMode{
			"JP_Tokyo": map[string][]*GameMode{
				"asia-northeast1-a": []*GameMode{
					SoloDuel,
				},
			},
		},
	}
	//"APAC": map[string]map[string][]*GameMode{
	//"KR_Seoul":  "asia-northeast3-b",
	//"IN_Delhi":  "asia-south2-c",
	//"AU_Sydney": "australia-southeast1-c",

	//"EMEA": map[string]map[string][]*GameMode{
	//	"PL_Warsaw":    "europe-central2-b",
	//	"DE_Frankfurt": "europe-west3-a",
	//	"SA_Dammam":    "me-central2-a",
	//},

	//"NA": map[string]map[string][]*GameMode{
	//	"US_Ashburn":    "us-east4-b",
	//	"US_LosAngeles": "us-west2-c",
	//},

	//"SA": map[string]map[string][]*GameMode{
	//	"BR_SaoPaulo": "southamerica-east1-c",
	//},

	//"Africa": map[string]map[string][]*GameMode{
	//	"ZA_Johannesburg": "africa-south1-b",
	//},

	NoSuchAllocationSpecError = errors.New("No such AllocationSpec exists")
)

// GameMode holds data about how to create an Open Match pb.MmfRequest that
// will make the kind of matches desired by this game mode.
type GameMode struct {
	Name            string
	Mmfs            []*pb.MatchmakingFunctionSpec
	Pools           map[string]*pb.Pool
	ExtensionParams map[string]*anypb.Any
}

type MockAgonesIntegration struct {
	// Shared logger.
	Log *logrus.Logger

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
	gameServerUpdateChan chan *mockGameServer
}

// We'll add a Kubernetes Annotation to each Agones Game Server that describes
// how to make matches for that game server.  For example, when a game server
// is created by a fleet scaling event or an individual server needs more
// players, we can add a Kubernetes Annotation to describe the kind of players
// it needs.
type mmfParametersAnnotation struct {
	mmfParams []string
}

// mockGameServer mocks out the parts of an Agones GameServer object that our
// mock Agones Integration interacts with.  They are both the storage format,
// and the update event message format (to mock out situations where an Agones
// Game Server Informer would CREATE/DELETE/UPDATE the k8s object).
//
// To mock a 'DELETE':
//
//	mmfParams = nil
//	allocatorSpec = allocationv1.GameServerAllocationSpec that describes how to select a server to DELETE
//
// To mock a 'CREATE':
//
//	mmfParams = array of pb.MmfRequests describing matches this server wants to host
//	allocatorSpec = nil
//
// To mock an 'UPDATE':
//
//	mmfParams = array of pb.MmfRequests describing matches this server wants to host
//	allocatorSpec = allocationv1.GameServerAllocationSpec that describes how to select the server as it exists before the UPDATE
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

	m.gameServerUpdateChan <- &mockGameServer{
		// If there was no next matchmaking request returned from the MMF, then
		// the input mmfReq variable will be nil.
		//mmfParams: &mmfParametersAnnotation{mmfParams: mmfReqs},
		mmfParams: mmfParams,
		allocSpec: m.gameServers[defunctAllocationSpecJSON].server.allocSpec,
	}
}

func (m *MockAgonesIntegration) UpdateMMFRequest(defunctAllocationSpecJSON string, mmfParam *pb.MmfRequest) string {
	m.updateGameServerMetadata(defunctAllocationSpecJSON, []*pb.MmfRequest{mmfParam})
	// TODO: move the processgameserverudpate section that makes the allocator spec to a new function, and make it called by the UpdateMMFRequest (so it can be returned) and the init function
	return //JSON of the new allocationSpec
}

// ProcessGameServerUpdatesis a mock for Agones Integration code you'd write to
// handle UPDATE/CREATE/DELETE events from an Agones GameServer object listener
// https://agones.dev/site/docs/guides/access-api/#best-practice-using-informers-and-listers
func (m *MockAgonesIntegration) processGameServerUpdates() {

	var xxHash [16]byte

	for update := range m.gameServerUpdateChan {

		// This mocks processing the data that a k8s informer event
		// handler would get as the 'old' game server in a DELETE or
		// UPDATE operation. Since k8s is effectively removing this
		// server, we have to stop tracking it in our agones
		// integration code.
		if update.allocSpec != nil {
			// convert allocationv1.GameServerAllocationSpec to JSON.
			allocSpecJSON, err := json.Marshal(update.allocSpec)
			if err != nil {
				m.Log.Errorf("Unable to marshal Agones Game Server Allocation object to JSON: %v", err)
				continue
			}
			// Generate hash of the JSON.
			xxHash = xxh3.Hash128Seed(allocSpecJSON, 0).Bytes()
			allocSpecHash := hex.EncodeToString(xxHash[:])
			m.mutex.Lock()
			// Make sure there's an existing reference count for this allocation spec
			if gsSet, exists := m.gameServers[allocSpecHash]; exists {
				if gsSet.count-1 > 0 {
					// If removing one reference count would result in at least
					// one remaining reference, then decrement
					m.gameServers[allocSpecHash].count--
				} else {
					// If removing one reference count would make the reference
					// count < 1, remove this allocation spec from memory.
					delete(m.gameServers, allocSpecHash)
				}
				// done with our updates; let other goroutines access the allocator specs.
				m.mutex.Unlock()
			} else {
				// no updates to make; release the lock while we log an error.
				m.mutex.Unlock()
				m.Log.Errorf("Error: %v", NoSuchAllocationSpecError)
			}
		}

		// This mocks processing the data that a k8s informer event
		// handler would receive as 'new' game server with an attached
		// MMF request in a CREATE or UPDATE operation. We now want to
		// track this new object spec in our agones integration code.
		if update.mmfParams != nil {
			// Add JSON string of this Mmf Request to the list of requests to annotate this game server with.
			mpa := &mmfParametersAnnotation{mmfParams: make([]string, len(update.mmfParams))}

			for _, mmfParam := range update.mmfParams {
				// Marshal this mmf request to a JSON string
				mmfParamJSON, err := protojson.Marshal(mmfParam)
				if err != nil {
					m.Log.Errorf("Failed to marshal MMF parameters into JSON, discarding: %v", err)
					continue
				}

				// Add this set of matchmaking parameters
				mpa.mmfParams = append(mpa.mmfParams, string(mmfParamJSON[:]))
			}

			// Convert the mmfParams to JSON, and get a hash of that JSON
			mmfParamsAnnotationJSON, err := json.Marshal(mpa)
			if err != nil {
				m.Log.Errorf("Unable to marshal MmfRequests annotation to JSON: %v", err)
				continue
			}
			xxHash = xxh3.Hash128Seed(mmfParamsAnnotationJSON, 0).Bytes()

			// Make an allocation spec that will match a game server with a
			// label under the key `ex.MMFRequestKey` with a value equal to the
			// hash of the new MMF Requests, and that will add the MMF Requests
			// as an annotation to the game server upon allocation.
			newAllocatorSpec := &allocationv1.GameServerAllocationSpec{
				Selectors: []allocationv1.GameServerSelector{
					allocationv1.GameServerSelector{
						LabelSelector: metav1.LabelSelector{
							// TODO: move to lists, and have one hash for every mmfRequest
							MatchLabels: map[string]string{
								ex.MMFRequestKey: hex.EncodeToString(xxHash[:]),
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
			}

			// Now hash the allocation spec we just created to use as a key.
			newAllocatorSpecJSON, err := json.Marshal(newAllocatorSpec)
			if err != nil {
				m.Log.Errorf("Unable to marshal Agones Game Server Allocation object to JSON: %v", err)
				continue
			}
			xxHash = xxh3.Hash128Seed(newAllocatorSpecJSON, 0).Bytes()
			newAllocatorSpecHash := hex.EncodeToString(xxHash[:])

			// Decorative closure to make it easy to see what happens inside the mutex lock.
			{
				// Finally, track this game server in memory
				m.mutex.Lock()
				// Create if it doesn't exist
				if _, exists := m.gameServers[newAllocatorSpecHash]; !exists {
					m.gameServers[newAllocatorSpecHash] = &mockGameServerSet{
						count:  0,
						server: mockGameServer{},
					}
				}
				// Track alloc spec by hash
				m.gameServers[newAllocatorSpecHash].server.allocSpec = newAllocatorSpec
				m.gameServers[newAllocatorSpecHash].server.mmfParams = update.mmfParams
				// Increment reference count
				m.gameServers[newAllocatorSpecHash].count++
				m.mutex.Unlock()
			}
		}
	}
}

// Init initializes the fleets the Agones backend mocks according to the
// provided fleetConfig, and starts a mock Kubernetes Informer that
// asynchronously watches for changes to Agones Game Server objects.
// https://agones.dev/site/docs/guides/access-api/#best-practice-using-informers-and-listers
func (m *MockAgonesIntegration) Init(fleetConfig map[string]map[string]map[string][]*GameMode) (err error) {

	// copyPoolFilters copies the filters from src pool to dest pool, allowing us
	// to combine filters from different pools to create pools dynamically.
	//
	// NOTE: If the destination contains a CreationTimeFilter and the source does
	// not, the destination CreationTimeFilter will be preserved.
	copyPoolFilters := func(src *pb.Pool, dest *pb.Pool) {
		dest.TagPresentFilters = append(dest.TagPresentFilters, src.GetTagPresentFilters()...)
		dest.DoubleRangeFilters = append(dest.DoubleRangeFilters, src.GetDoubleRangeFilters()...)
		dest.StringEqualsFilters = append(dest.StringEqualsFilters, src.GetStringEqualsFilters()...)

		// Check that the source has a creation time filter before copying to avoid
		// an empty src filter overwriting an existing dest filter. This can only
		// happen with this type of filter, as all others are contained in arrays.
		if src.GetCreationTimeRangeFilter() != nil {
			dest.CreationTimeRangeFilter = src.GetCreationTimeRangeFilter()
		}
		return
	}
	// In-memory game server state tracking maps init
	m.gameServers = make(map[string]*mockGameServerSet)

	// channel + goroutine used to simulate a Kubernetes informer event-driven pattern.
	m.gameServerUpdateChan = make(chan *mockGameServer)
	go m.processGameServerUpdates()

	// Initialize the matchmaking parameters of the mock Agones fleets
	// specified in the fleetConfig
	var xxHash [16]byte
	for category, regions := range fleetConfig {
		for region, zones := range regions {
			for zone, modes := range zones {

				// Construct a fleet name based on the FleetConfig
				fleetName := fmt.Sprintf("%s/%s@%s", category, region, zone)
				fLogger := logger.WithFields(logrus.Fields{
					"fleet": fleetName,
				})

				// Construct a pool with filters to find players near this zone.
				zonePool := &pb.Pool{
					DoubleRangeFilters: []*pb.Pool_DoubleRangeFilter{
						&pb.Pool_DoubleRangeFilter{
							DoubleArg: fmt.Sprintf("ping.%v", zone),
							Minimum:   1,
							Maximum:   120,
						},
					},
				}

				// Make an array of all the matchmaking profiles that apply to
				// this fleet, one profile per game mode
				mpa := &mmfParametersAnnotation{mmfParams: make([]string, len(modes))}
				mmfParams := make([]*pb.MmfRequest, len(modes))
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

					// For every pool this game mode requests, also include the
					// filters for this fleet.
					composedPools := map[string]*pb.Pool{}
					for pname, pool := range mode.Pools {
						// Make a new pool with a name combining the fleet and the game mode's pool name
						composedName := fmt.Sprintf("%v.%v", fleetName, pname)
						composedPool := &pb.Pool{Name: composedName}
						// Add filters to this pool
						copyPoolFilters(zonePool, composedPool)
						copyPoolFilters(pool, composedPool)
						composedPools[composedName] = composedPool
					}

					// Add this mmf request to our list of parameters
					mmfParams := append(mmfParams, &pb.MmfRequest{
						Mmfs: mode.Mmfs,
						Profile: &pb.Profile{
							Name:       profileName,
							Pools:      composedPools,
							Extensions: mode.ExtensionParams,
						},
					})

					// Marshal this mmf request to a JSON string
					paramsJSON, err := protojson.Marshal(mmfParams[len(mmfParams)-1])
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
				allocSpec := &allocationv1.GameServerAllocationSpec{
					Selectors: []allocationv1.GameServerSelector{
						allocationv1.GameServerSelector{
							LabelSelector: metav1.LabelSelector{
								// TODO: move to lists, and have one hash for every mmfRequest
								MatchLabels: map[string]string{ex.MMFRequestHashKey: mmfParamsHash},
							},
						},
					},
				}

				// Now we have all the parameters we need to matchmake for this
				// fleet.  This mock does not actually create Agones Fleets. In
				// a real integration you'd need some code that creates Fleets
				// in kubernetes, and specifies the MmfRequest in the Game
				// Server Template Spec, so all game servers made by fleet
				// scaling events will have the proper MmfRequest attached to
				// them. Something like this:
				//
				//thisFleet := &agonesv1.Fleet{
				//	Spec: agonesv1.FleetSpec{
				//		Template: agonesv1.GameServerTemplateSpec{
				//			ObjectMeta: metav1.ObjectMeta{
				//				// Attach the matchmaking profiles for this fleet's
				//				// game modes to the fleet as a k8s annotation.
				//				Annotations: map[string]string{
				//					ex.MMFRequestKey: string(mmfParamsJSON[:]),
				//				},
				// TODO: move to lists, and have one hash for each mmfRequest
				//				Labels: map[string]string{
				//					ex.MMFRequestHashKey: mmfParamsHash,

				//				},
				//			},
				//		},
				//	},
				//}
				// createFleetOnCluster(thisCluster, thisFleet)

				// Hash the agones allocation spec and store it in a map by its
				// hash for later lookup
				allocJSON, err := json.Marshal(allocSpec)
				if err != nil {
					fLogger.Errorf("Unable to marshal Agones Game Server Allocation object to JSON: %v", err)
					continue
				}
				xxHash = xxh3.Hash128Seed(allocJSON, 0).Bytes()
				allocHash := hex.EncodeToString(xxHash[:])

				// decorative closure just to make it easy to see which lines are encompassed by the mutex lock.
				{
					m.mutex.Lock()
					m.gameServers[allocHash] = &mockGameServerSet{
						server: mockGameServer{
							allocSpec: allocSpec,
							mmfParams: mmfParams,
						},
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
					m.mutex.Unlock()
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
func (m *MockAgonesIntegration) Allocate(allocationSpecJSON string, match *pb.Match) (err error) {
	// Here is where your backend would do any additional prep tasks for server allocation in Agones
	m.Log.Debugf("Allocating server for match %v", match.GetId())

	var ok bool
	// Decorative closure to make it easier to see what is being done within the mutex lock.
	{
		m.mutex.RLock()
		_, ok = m.gameServers[allocationSpecJSON]
		m.mutex.RUnlock()
	}

	if !ok {
		m.Log.Errorf(" invalid allocation criteria for match %v!", match.GetId())
		return NoSuchAllocationSpecError
	} else {
		// TODO: Here is where you'd use the k8s api to request an agones game
		// server allocation in a real implementation.  If you need to add
		// match parameters, player lists, etc to your Game Server, you would
		// read it from the pb.Match message here and include it in the Agones
		// Allocation Spec. This mock does no actual allocation operation;
		// instead it just mocks out the game server metadata update that k8s
		// would send to an Agones Game Server Informer after a successful
		// Agones allocation operation.
		m.Log.Debugf("Allocating server for match %v", match.GetId())
		m.Log.Tracef(" match %v allocation criteria %v", match.GetId(), allocationSpecJSON)
		m.updateGameServerMetadata(allocationSpecJSON, []*pb.MmfRequest{})
	}
	return nil
}

// hashcount is a simple struct used to sort the agones allocation specs
// currently in memory by the number of servers that would be returned by each
// spec.
type hashcount struct {
	hash  string
	count int
}

// GetMMFParams() returns all pb.MmfRequest messages currently tracked by the
// Agones integration. These contain all the parameters for matchmaking
// function invocations that should be kicked off in the next director
// matchmaking cycle in order to find matches to put on game servers managed by
// Agones.
func (m *MockAgonesIntegration) GetMMFParams() (requests []*pb.MmfRequest) {

	// Sort the allocation specs by the number of servers they match (e.g.
	// their reference count)
	var ashByCount []hashcount // 'ash' = allocation spec hash

	m.mutex.RLock()
	for k, v := range m.gameServers {
		ashByCount = append(ashByCount, hashcount{hash: k, count: v.count})
	}
	m.mutex.RUnlock()

	sort.Slice(ashByCount, func(x, y int) bool {
		return ashByCount[x].count < ashByCount[y].count
	})

	// servers with unique MmfRequests (examples: backfill, join-in-progress,
	// high-density game servers, etc) should be serviced with higher priority,
	// so add these 'more unique' (e.g. lower reference count) requests to the
	// list first.
	for _, hc := range ashByCount {
		if gsSet, ok := m.gameServers[hc.hash]; ok {

			// Each fleet could have multiple different MmfRequests associated with
			// it, to allow it to run multiple different kinds of matchmaking
			// concurrently (common with undifferentiated server fleets)
			m.Log.Tracef("retrieved '%v' MmfRequests from k8s annotation.", len(gsSet.server.mmfParams))
			for _, request := range gsSet.server.mmfParams {

				m.Log.Tracef(" retrieved MmfRequest: %v", request)

				// retrieve existing profile extensions
				extensions := request.GetProfile().GetExtensions()

				// Get number of ready servers that can accept matches from
				// this MmfRequest, and put this in the profile extensions
				// field so the mmf can read it.
				exReadyCount, err := anypb.New(&knownpb.Int32Value{Value: int32(hc.count)})
				if err != nil {
					m.Log.Errorf("Unable to convert number of ready game servers into an anypb: %v", err)
					continue
				}
				extensions[ex.AgonesGSReadyCountKey] = exReadyCount

				// add the agones allocator that returns servers asking for
				// this Profile to its extensions field.
				// TODO: this could be optimized by keeping the JSON version of
				// the allocSpec around in memory when we hash it the first
				// time, but this will do for now
				allocSpecJSON, err := json.Marshal(gsSet.server.allocSpec)
				if err != nil {
					m.Log.Errorf("Unable to marshal Agones Game Server Allocation object %v to JSON, discarding mmfRequest %v: %v",
						gsSet.server.allocSpec, gsSet.server.mmfParams, err)
					continue
				}
				exAllocSpecJSON, err := anypb.New(&knownpb.StringValue{Value: string(allocSpecJSON[:])})
				if err != nil {
					m.Log.Errorf("Unable to marshal Agones Game Server Allocation object %v to anyPb, discarding mmfRequest %v: %v",
						gsSet.server.allocSpec, gsSet.server.mmfParams, err)
					continue
				}
				extensions[ex.AgonesAllocatorKey] = exAllocSpecJSON

				requests = append(requests, &pb.MmfRequest{
					Mmfs: request.Mmfs,
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
// manager integration module used by our sample Director.
type GameServerManager interface {
	Allocate(string, *pb.Match) string
	GetMmfParams() []*pb.MmfRequest
	UpdateMMFRequest(string, *pb.MmfRequest) string
}

// MockDirector aims to have the structure of a reasonably realistic,
// feature-complete matchmaking Director that would use a game server backend
// integration to interface with game servers.
type MockDirector struct {
	OmClient                 *omclient.RestfulOMGrpcClient
	Cfg                      *viper.Viper
	Log                      *logrus.Logger
	Assignments              sync.Map
	gsManager                GameServerManager
	pendingMmfRequestsByHash map[string]*pb.MmfRequest
}

func (d *MockDirector) Run(ctx context.Context) {

	// Add fields to structured logging for this function
	logger := d.Log.WithFields(logrus.Fields{
		"component": "game_server_director",
		"operation": "matchmaking_loop",
	})
	logger.Tracef("Directing matches to game servers")

	// local var init
	ticketIdsToActivate := make(chan string, 10000)
	var startTime time.Time
	var proposedMatches []*pb.Match
	var matches map[*pb.Match]string

	// Kick off main matchmaking loop
	// Loop for the configured number of cycles
	logger.Debugf("Beginning matchmaking cycle, %v cycles configured", d.Cfg.GetInt("NUM_MM_CYCLES"))
	for i := d.Cfg.GetInt("NUM_MM_CYCLES"); i > 0; i-- { // Main Matchmaking Loop

		// Loop var init
		startTime = time.Now()
		proposedMatches = []*pb.Match{}
		matches = map[*pb.Match]string{}

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

		// Make a channel that collects all the results channels for all the
		// concurrent calls we'll make to OM.
		fanInChan := make(chan chan *pb.StreamedMmfResponse)

		// Launch asynchronous match result processing function.
		//
		// We make a separate results channel for every InvokeMMF call
		// - see the 'rClient.InvokeMatchmakingFunctions()' call below.
		// Each of those results channels is sent to this match result
		// processing goroutine on the 'fanInChan' channel.
		//
		// This is a complex goroutine so it's over-commented.
		wg := sync.WaitGroup{}
		go func() {

			// For every channel received on fanInChan, kick off a
			// goroutine to read all matches returned on it.
			//
			// This goroutine pauses here until the
			// 'rClient.InvokeMatchmakingFunctions()' calls below start
			// returning matches.
			for resChan := range fanInChan {
				logger.Trace("received invocationResultChan on fanInChan")

				// Asynchronously read all matches from this channel.
				go func() {
					for resPb := range resChan {

						// Process this returned match.
						if resPb == nil {
							// Something went wrong.
							logger.Trace("StreamedMmfResponse protobuf was nil!")
						} else {
							// We got a match result.
							logger.Trace("Received match from MMF")

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
		requests := d.gsManager.GetMmfParams()
		for _, reqPb := range requests {
			wg.Add(1)
			logger.Debugf("Kicking off MMFs for Profile '%s' with %v pools", reqPb.GetProfile().GetName(), len(reqPb.GetProfile().GetPools()))

			// Make a channel for responses from this individual call.
			invocationResultChan := make(chan *pb.StreamedMmfResponse)

			// Tell the fan-in goroutine to monitor this new channel for matches.
			fanInChan <- invocationResultChan

			// Invoke matchmaking functions, match results are sent back
			// using the channel we just created.
			go d.OmClient.InvokeMatchmakingFunctions(ctx, reqPb, invocationResultChan)

		}

		// Wait for all results from all MMFs. OM won't take longer than its configured
		// timeout to return matches (determined by the OM_MMF_TIMEOUT_SECS config var
		// in your om-core deployment).
		wg.Wait()
		close(fanInChan)

		// Here's where we check each match to decide if we want to
		// allocate a server for it, or reject it.  (In OM1.x, this
		// functionality lived in the 'evaluator' component of Open Match.)
		//
		// Common things to check for are:
		// - player collisions among matches,
		// - matches below a specific quality score,
		// - matches that outstrip our game server capacity, etc.
		numRejectedTickets := 0
		logger.Debugf("%v proposed matches to evaluate", len(proposedMatches))

		// Simple local function to 'reject' a proposed match by adding all the
		// tickets in it to the list of tickets to reactivate in OM.
		rejectMatch := func(match *pb.Match) {
			//rejectedMatchesCounter.Add(ctx, 1)
			for _, roster := range match.GetRosters() {
				for _, ticket := range roster.GetTickets() {
					// queue this ticket ID to be re-activated
					ticketIdsToActivate <- ticket.GetId()
					numRejectedTickets++
				}
			}
		}

		// Here is where you loop over all the returned match proposals and make decisions.
		for _, match := range proposedMatches {
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
				rejectMatch(match)
				continue
			}

			// Get the hash of the MMF request that resulted in the creation of
			// this match from the match extensions. It is placed in the
			// pb.MmfRequest.Profile extensions field by the Agones
			// Integration, and we have to code our MMF to return it to us in
			// the match extension. We have the MMF return the hash instead
			// of the entire profile to be a bit more bandwidth efficient.
			mmfParamsHash, err := extensions.String(match.Extensions, ex.MMFRequestHashKey)
			if err != nil {
				// Couldn't get the hash, reject match.
				logger.WithFields(logrus.Fields{
					"match_id": match.Id,
				}).Error("Unable to get Profile hash from returned match, rejecting match: %v")
				rejectMatch(match)
				continue
			}

			// Make sure these match request parameters are in our list of active
			// matchmaking requests
			if _, exists := d.pendingMmfRequestsByHash[mmfParamsHash]; !exists {
				// No request with that hash in our list, reject match.
				rejectMatch(match)
				continue
			}

			// Get the specification for how to allocate a server using the
			// current game server manager integration. This is an opaque value
			// sent to us by the game server manager integration that we simply
			// pass back to it when we're ready to get a server.
			allocatorSpec, err := extensions.String(
				d.pendingMmfRequestsByHash[mmfParamsHash].GetProfile().GetExtensions(),
				ex.AgonesAllocatorKey,
			)
			if err != nil {
				rejectMatch(match)
				continue
			}

			// See if the MMF included a new set of match request parameters.
			// Common use cases for this are backfills, join-in-progress, and
			// high-density gameservers that can host additional sessions, but
			// now need to change their ticket pool filters (to, for example,
			// limit the tickets under consideration to those who want to play
			// the game mode or map the server currently has loaded).
			nextMMFParams, err := extensions.String(match.Extensions, ex.MMFRequestKey)
			if err == nil {
				// there is a new matching request; let the game server manager
				// know about it, and ask it for a new allocation specification
				// that will add this matching request to the game server upon
				// successful allocation.
				allocatorSpec = d.gsManager.UpdateMMFRequest(allocatorSpec, nextMMFParams)
			}

			// This proposed match passed all our decision points and validation checks,
			// lets add it to the map of matches we want to allocate a server for.
			//acceptedMatchesCounter.Add(ctx, 1)
			matches[match] = allocatorSpec
		}

		for match, allocatorSpec := range matches {
			// Here is where you would actually allocate the server in Agones
			// The mock just prints a log message telling you when an
			// allocation would take place.
			d.gsManager.Allocate(allocatorSpec, match)
		}

		// Re-activate the rejected tickets so they appear in matchmaking pools again.
		d.OmClient.ActivateTickets(ctx, ticketIdsToActivate)

		// Status update closure. This is currently just for development
		// debugging and may be removed or made into a separate function in
		// future versions
		{
			// Print status for this cycle
			logger.Trace("Acquiring lock to write cycleStatus")

			numCycles := fmt.Sprintf("/%v", d.Cfg.GetInt("NUM_MM_CYCLES"))
			if d.Cfg.GetInt("NUM_MM_CYCLES") > 600000 { // ~600k seconds is roughly a week
				numCycles = "/--" // Number is so big as to be meaningless, just don't display it
			}

			// Lock the mutex while updating the status for this cycle so
			// it can't be read while we're writing it.
			statusUpdateMutex.Lock()
			cycleStatus = fmt.Sprintf("Cycle %06d%v: %4d/%4d matches accepted", d.Cfg.GetInt("NUM_MM_CYCLES")+1-i, numCycles, len(matches), len(proposedMatches))
			statusUpdateMutex.Unlock()

			// Write the status of this cycle to the logger.
			logger.Info(cycleStatus)
		}

		// Sleep before next loop.
		// TODO: proper exp BO + jitter
		sleepDur := time.Until(startTime.Add(time.Millisecond * 1000)) // >= OM_CACHE_IN_WAIT_TIMEOUT_MS for efficiency's sake
		dur := time.Now().Sub(startTime)
		sleepDur = (time.Millisecond * 1000) - dur
		time.Sleep(sleepDur)
		time.Sleep(3 * time.Second)

		// Release resources associated with this context.
		cancel()
	} // end of main matchmaking loop
}
