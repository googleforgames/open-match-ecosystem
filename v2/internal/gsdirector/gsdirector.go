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

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
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

	// If the MMF used by this game mode needs to make a backfill request for
	// additional tickets, it should use these MMFs to service that backfill
	// request. (The pb.MmfRequest.Profile will be generated at backfill
	// request time according to what kinds of tickets are required).
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
			ex.MMFRequestKey: BackfillMMFs,
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
			ex.MMFRequestKey: BackfillMMFs,
		}),
	}

	// Nested map structure defining which game modes are available in which
	// data centers. This mocks out one Agones fleet in each region. It is
	// assumed that game servers specified in that fleet.GameServerTemplate are
	// able to host a match session from any of the provided game modes.
	//
	// There are many ways you could do this, this approach organizes the
	// fleets into geographic 'categories' and drills all the way down to which
	// game mode is available in each zonal fleet.
	// e.g. FleetConfig[category][region][GCP_zone] = [gamemode,...]
	//
	// In a production system, where you get this information is based on
	// how you operate your infrastructure. For example:
	//  - read this from your infrastructure-as-code repository (e.g. where
	//  you've checked in something like terraform code)
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
	// Name of this game mode, human-readable string.
	Name string
	// List of MMFs to use when matching for this game mode.
	MMFs []*pb.MatchmakingFunctionSpec
	// Pools filled with filters used to find tickets that can play this game mode.
	Pools map[string]*pb.Pool
	// Additional user-defined parameters for the matchmaker or MMF.
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

// UpdateMMFRequest() is called by the director associate a new mmfRequest with
// the server sescribed by the provided allocation spec. This is commonly used
// to add a matchmaking request with new parameters to a server for which a
// session already exists (e.g. scenarios like backfill, join-in-progress, and
// high-density gameservers). The new allocation spec that will match servers
// that can handle matches resulting from this mmfRequest is returned to the
// director.
func (m *MockAgonesIntegration) UpdateMMFRequest(defunctAllocationSpecJSON string, mmfParam *pb.MmfRequest) string {
	// TODO: this is broken, WIP
	//newAllocSpec, err := allocationSpecFromMMFRequests([]*pb.MmfRequest{mmfParam}, m.Log)
	newAllocSpec, err := allocationSpecFromMMFRequests([]*pb.MmfRequest{}, m.Log)
	if err != nil {
		m.Log.Errorf("Failure to generate new Agones Allocation Spec from provided MmfRequest: %v", err)
	}
	newAllocSpecJSON, err := json.Marshal(newAllocSpec)
	if err != nil {
		m.Log.Errorf("Failure to marshal new Agones Allocation Spec to JSON: %v", err)
	}

	m.updateGameServerMetadata(defunctAllocationSpecJSON, []*pb.MmfRequest{mmfParam})
	return string(newAllocSpecJSON[:])
}

// allocationSpecFromMMFRequests() generates a new allocation spec that will
// allocate a server which can host a game sessions created by the input
// mmfRequests.
// TODO: move to lists if they become able to delete at allocation time https://agones.dev/site/docs/guides/counters-and-lists/
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
					MatchLabels: map[string]string{
						ex.MMFRequestHashKey: hex.EncodeToString(xxHash[:]),
					},
				},
			},
		},
		// TODO: broken, WIP
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

	//var xxHash [16]byte

	for update := range m.gameServerUpdateChan {

		pLogger.WithFields(logrus.Fields{"update": update}).Trace("Received game server update")
		// This mocks processing the data that a k8s informer event
		// handler would get as the 'old' game server in a DELETE or
		// UPDATE operation. Since k8s is effectively removing this
		// server, we have to stop tracking it in our agones
		// integration code.
		if update.allocSpec != nil {
			asLogger := pLogger.WithFields(logrus.Fields{
				"alloc_spec": update.allocSpec.Selectors[0].LabelSelector.MatchLabels,
			})
			asLogger.Trace(" game server update contains an alloc spec to remove from matching")
			// convert allocationv1.GameServerAllocationSpec to JSON.
			allocSpecJSONBytes, err := json.Marshal(update.allocSpec)
			if err != nil {
				asLogger.Errorf("Unable to marshal Agones Game Server Allocation object to JSON: %v", err)
				continue
			}
			// Generate hash of the JSON.
			//xxHash = xxh3.Hash128Seed(allocSpecJSONBytes, 0).Bytes()
			//allocSpecHash := hex.EncodeToString(xxHash[:])
			allocSpecJSON := string(allocSpecJSONBytes[:])

			m.mutex.Lock()
			// Make sure there's an existing reference count for this allocation spec
			if gsSet, exists := m.gameServers[allocSpecJSON]; exists {
				if gsSet.count-1 > 0 {
					asLogger.Trace("  decremented the number of game servers managed by Agones matching this alloc spec")
					// If removing one reference count would result in at least
					// one remaining reference, then decrement
					m.gameServers[allocSpecJSON].count--
				} else {
					asLogger.Trace("  removed last server matching this allocation spec from matching, so also removing this alloc spec from matching")
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
		if update.mmfParams != nil && len(update.mmfParams) > 0 {
			newAllocatorSpec, err := allocationSpecFromMMFRequests(update.mmfParams, m.Log)
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
				m.gameServers[newAllocatorSpecJSON].server.mmfParams = update.mmfParams
				// Increment reference count
				m.gameServers[newAllocatorSpecJSON].count++
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

	m.Log.Debug("Initializing matching parameters")

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
				// this fleet, one profile per game mode.
				mpa := &mmfParametersAnnotation{mmfParams: make([]string, 0)} // String version to attach to k8s metadata
				mGSSet.server.mmfParams = make([]*pb.MmfRequest, 0)           // protobuf version to send to Open Match.
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
				//xxHash = xxh3.Hash128Seed(allocJSONBytes, 0).Bytes()
				//allocHash := hex.EncodeToString(xxHash[:])
				allocJSON := string(allocJSONBytes[:])

				// decorative closure just to make it easy to see which lines are encompassed by the mutex lock.
				{
					m.mutex.Lock()
					m.gameServers[allocJSON] = mGSSet
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
	// Here is where your backend would do any additional prep tasks for server allocation in Agones

	var ok bool
	// Decorative closure to make it easier to see what is being done within the mutex lock.
	{
		m.mutex.RLock()
		_, ok = m.gameServers[allocationSpecJSON]
		m.mutex.RUnlock()
	}

	if !ok {
		m.Log.Errorf(" No game servers that fit allocation criteria for match %v: %v", match.GetId(), allocationSpecJSON)
		return ""
	}
	// TODO: Here is where you'd use the k8s api to request an agones game
	// server allocation in a real implementation.  If you need to add
	// match parameters, player lists, etc to your Game Server, you would
	// read it from the pb.Match message here and include it in the Agones
	// Allocation Spec. This mock does no actual allocation operation;
	// instead it just mocks out the game server metadata update that k8s
	// would send to an Agones Game Server Informer after a successful
	// Agones allocation operation, and returns a placeholder connection
	// string.
	m.Log.Debugf("Allocating server for match %v", match.GetId())
	m.updateGameServerMetadata(allocationSpecJSON, []*pb.MmfRequest{})
	mockGameServerStatus := &agonesv1.GameServerStatus{
		Ports: []agonesv1.GameServerStatusPort{
			agonesv1.GameServerStatusPort{
				Name: "default",
				Port: 7463},
		},
		Address:  "1.2.3.4",
		NodeName: "node-name",
	}
	connString = fmt.Sprintf("%v:%v", mockGameServerStatus.Address, mockGameServerStatus.Ports[0].Port)
	return
}

// hashcount is a simple struct used to sort the agones allocation specs
// currently in memory by the number of servers that would be returned by each
// spec.
type ascount struct {
	spec  string
	count int
}

// GetMMFParams() returns all pb.MmfRequest messages currently tracked by the
// Agones integration. These contain all the parameters for matchmaking
// function invocations that should be kicked off in the next director
// matchmaking cycle in order to find matches to put on game servers managed by
// Agones.
func (m *MockAgonesIntegration) GetMMFParams() []*pb.MmfRequest {
	gLogger := m.Log.WithFields(logrus.Fields{
		"component": "agones_integration",
		"operation": "get_mmf_params",
	})

	requests := make([]*pb.MmfRequest, 0)
	// Sort the allocation specs by the number of servers they match (e.g.
	// their reference count)
	var asByCount []ascount // 'as' = allocation spec

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

		if ok {

			for _, request := range gsSet.server.mmfParams {
				gLogger.Tracef(" retrieved MmfRequest with profile: %v", request.GetProfile().GetName())

				// retrieve existing profile extensions
				extensions := request.GetProfile().GetExtensions()
				if extensions == nil {
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
				// TODO: this could be optimized by keeping the JSON version of
				// the allocSpec around in memory when we hash it the first
				// time, but this will do for now
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
					gLogger.Errorf("Unable to put mmfRequest hash into extensions, discarding mmfRequest %v: %v",
						gsSet.server.mmfParams, err)
					continue
				}
				extensions[ex.MMFRequestHashKey] = mmfRequestHash

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
// manager integration module used by our sample Director.
type GameServerManager interface {
	Init(map[string]map[string]map[string][]*GameMode) error
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
	assignments              map[string]string
	GSManager                GameServerManager
	pendingMmfRequestsByHash map[string]*pb.MmfRequest
	assignmentMutex          sync.RWMutex
}

// GetAssignments() returns a map containing the connection string for every
// input ticketID it finds in the in-memory map of assignments.
func (d *MockDirector) GetAssignments(ticketIds []string) map[string]string {
	out := make(map[string]string)
	// Get Assignments can be called asynchronously, so we need to use a mutex when reading
	// from the assignments map.
	d.assignmentMutex.RLock()
	for _, id := range ticketIds {
		if connString, exists := d.assignments[id]; exists {
			out[id] = connString
		}
	}
	d.assignmentMutex.RUnlock()
	return out
}

func (d *MockDirector) Run(ctx context.Context) {

	// Var init
	d.assignments = make(map[string]string)
	d.pendingMmfRequestsByHash = make(map[string]*pb.MmfRequest)

	// Add fields to structured logging for this function
	logger := d.Log.WithFields(logrus.Fields{
		"component": "game_server_director",
		"operation": "matchmaking_loop",
	})
	logger.Tracef("Initializing loop directing matches to game servers")

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
		requests := d.GSManager.GetMMFParams()
		for _, reqPb := range requests {
			wg.Add(1)

			mmfLogFields := logrus.Fields{
				"num_mmfs":     len(reqPb.GetMmfs()),
				"profile_name": reqPb.GetProfile().GetName(),
				"num_pools":    len(reqPb.GetProfile().GetPools()),
			}
			for _, mmf := range reqPb.GetMmfs() {
				mmfLogFields[mmf.GetName()] = mmf.GetHost()
			}
			logger.WithFields(mmfLogFields).Debug("kicking off MMFs")

			// Add this request to the list of pending requests, indexed by hash.
			mmfReqHash, err := ex.String(reqPb.Profile.GetExtensions(), ex.MMFRequestHashKey)
			if err != nil {
				logger.Errorf("Unable to retreive MmfRequest hash from profile extensions, discarding MmfRequest: %v", err)
				continue
			}
			d.pendingMmfRequestsByHash[mmfReqHash] = reqPb

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
			mLogger := logger.WithFields(logrus.Fields{
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
				continue
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
				continue
			}

			// Make sure the match request parameters we got back from the MMF
			// are in our list of active matchmaking requests.
			if _, exists := d.pendingMmfRequestsByHash[mmfParamsHash]; !exists {
				// No request with that hash in our list, reject match.
				mLogger.Error("Unable to get pending MmfRequest, cannot return match, rejecting")
				rejectMatch(match)
				continue
			}

			// Get the specification for how to allocate a server using the
			// current game server manager integration. This is an opaque value
			// sent to the director by the game server manager integration in
			// the mmfRequest.Profile.Extensions field the director simply
			// passes back to the game server manager integration when asking
			// for a server.
			allocatorSpec, err := extensions.String(
				d.pendingMmfRequestsByHash[mmfParamsHash].GetProfile().GetExtensions(),
				ex.AgonesAllocatorKey,
			)
			if err != nil {
				mLogger.Errorf("Unable to retrieve allocation specification for game server manager for returned match, rejecting: %v", err)
				rejectMatch(match)
				continue
			}

			// See if the MMF returned new match request parameters in the
			// match extensions field.
			//
			// Common use cases for this are backfills, join-in-progress, and
			// high-density gameservers that can host additional sessions.
			// https://agones.dev/site/docs/integration-patterns/high-density-gameservers/
			nextMMFRequest := &pb.MmfRequest{}
			err = match.Extensions[ex.MMFRequestKey].UnmarshalTo(nextMMFRequest)
			if err == nil && nextMMFRequest.GetProfile().GetName() != "" {
				mLogger.Info("Approved a match with a backfill request attached, sending backfill request details to game server manager")
				allocatorSpec = d.GSManager.UpdateMMFRequest(allocatorSpec, nextMMFRequest)
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
			// The mockAgonesIntegration just prints a log message saying that
			// an allocation would take place.
			connString := d.GSManager.Allocate(allocatorSpec, match)

			// If there is a game server to send tickets to, make assignments.
			if connString != "" {
				d.Log.Tracef("Match session assigned to game server %v", connString)

				// Assignments can be read asynchronously, so we must use a mutex
				// when updating the assignment map.
				d.assignmentMutex.Lock()
				for _, roster := range match.GetRosters() {
					for _, ticket := range roster.GetTickets() {
						d.assignments[ticket.GetId()] = connString
					}
				}
				d.assignmentMutex.Unlock()
			} else {
				// No valid connection string returned from the game server
				// manager, there must be an issue. Put these tickets back into the
				// matchmaking pool.
				rejectMatch(match)
			}

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
