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
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

// Keys for metadata/extensions
const (
	// K8s object annotation metadata key where the JSON representation of the
	// pb.MmfRequest can be found.
	mmfRequestKey     = "open-match.dev/mmf"
	mmfProfileHashKey = "open-match.dev/profile-hash"

	// Open Match match protobuf message extensions field key where the JSON
	// representation of the agones allocation request can be found.
	agonesAllocatorKey     = "open-match.dev/agones-game-server-allocator"
	agonesAllocatorHashKey = "open-match.dev/agones-game-server-allocator-hash"

	// Key that holds the k8s label identifying if the game server is ready for (re-)allocation
	// https://agones.dev/site/docs/integration-patterns/high-density-gameservers/
	agonesGSReadyKey = "agones.dev/sdk-gs-session-ready"
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

	FleetExistsError          = errors.New("Fleet with that name already exists")
	NoSuchAllocationSpecError = errors.New("No such AllocationSpec exists")
	agonesReallocSpec         = &allocationv1.GameServerAllocationSpec{}
)

type GameMode struct {
	Name            string
	Mmfs            []*pb.MatchmakingFunctionSpec
	Pools           map[string]*pb.Pool
	ExtensionParams map[string]*anypb.Any
}
type allocOptFunc func(*allocOpts)

type allocOpts struct {
	selectors allocationv1.GameServerSelector
}

type GameServerManager interface {
	Allocate(string, *pb.Match) string
	GetMmfParams() []*pb.MmfRequest
	UpdateGameServerMetadata(string, *pb.MmfRequest)
}

type MockAgonesIntegration struct {
	// Shared logger.
	Log *logrus.Logger

	// https://github.com/googleforgames/agones/blob/eb08f0b76c6c87d7c13cdef02788ea26800a2df2/pkg/apis/agones/v1/fleet.go
	// This just mocks out real Agones fleets, holding an in-memory 'fake'
	// fleet of gameservers. In realite this would be implemented using a
	// lister for Fleet objects.
	Fleets map[string]*agonesv1.Fleet

	// A server that needs a different kind of match than that being normally
	// found for the fleet it is in (examples: backfills, join-in-progress,
	// high-density gameservers) would have an ObjectMeta.Label applied using
	// the pre-determined key of `mmfRequestKey` and the value being a hash
	// representation of the pb.MmfRequest protocol buffer message describing
	// the criteria for finding tickets for that server's unique circumstances.
	// It is recommended to actually store that MmfRequest (marshalled to JSON
	// is preferred as that is human-readable) on an ObjectMeta.Annotation on
	// the server as well and use that as the single authoritative source for
	// the matchmaking criteria for this server. If the server needs no unique
	// matchmaking criteria, the key should be empty, and the server will be
	// allocated as needed by the matchmaking criteria associated with it's
	// fleet.
	//
	// This is a greatly simplified mock.
	// In reality, this would be implemented using a lister for GameServer
	// objects.
	//
	// Actual datatypes for this are
	//   map[*allocationv1.GameServerAllocationSpec]*pb.MmfRequest
	//IndividualGameServerMatchmakingRequests sync.Map
	//GameServerMatchmakingRequests

	// Channel on which IndividualGameServerMatchmakingRequests are sent; this
	// allows us to mock out an Agones Informer on GameServer objects and write
	// event-driven code that responds to game server allocations the way a
	// real Agones integration would.
	gameServerUpdateChan chan *MockGameServerUpdate

	// datatypes for this are map[string]*allocationv1.GameServerAllocationSpec
	allocationSpecsByHash map[string]*allocationv1.GameServerAllocationSpec
	allocationSpecsCounts map[string]int
	mutex                 sync.RWMutex
}

// We'll add a Kubernetes Annotation to each Agones Fleet that describes how to
// make matches for the game servers in that fleet.
// If an individual server needs more players, we can add a Kubernetes
// Annotation to describe the kind of players it needs.
type mmfParametersAnnotation struct {
	MmfRequests []string
}

// These are generated to mock out situations where Agones would update a k8s
// gameserver object.
//
// To mock a 'delete':
//
//	MatchmakingRequest = nil
//	DefunctAllocatorSpec = hash of allocationv1.GameServerAllocationSpec(marshalled to JSON) that describes how to select a server to delete
//
// To mock a 'create':
//
//	MatchmakingRequest = pb.MmfRequest describing matches this server wants to host
//	DefunctAllocatorSpec = nil
//
// To mock an 'update':
//
//	MatchmakingRequest       = pb.MmfRequest describing matches this server wants to host
//	DefunctAllocatorSpecHash = hash of allocationv1.GameServerAllocationSpec(marshalled to JSON) that describes how to select the server as it exists before the update
type MockGameServerUpdate struct {
	MatchmakingRequests      []*pb.MmfRequest
	DefunctAllocatorSpecHash string
}

// UpdateGameServerMetadata is a mock function that adds metadata to
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
func (m *MockAgonesIntegration) UpdateGameServerMetadata(defunctAllocationSpecHash string, mmfReqs []*pb.MmfRequest) {
	m.gameServerUpdateChan <- &MockGameServerUpdate{
		MatchmakingRequests:      mmfReqs,
		DefunctAllocatorSpecHash: defunctAllocationSpecHash,
	}
}

// Init initializes the fleets the Agones backend mocks according to the
// provided fleetConfig, and starts a mock Kubernetes Informer that
// asynchronously watches for changes to Agones Game Server objects.
// https://agones.dev/site/docs/guides/access-api/#best-practice-using-informers-and-listers
func (m *MockAgonesIntegration) Init(fleetConfig map[string]map[string]map[string][]*GameMode) (err error) {

	// Map init
	m.allocationSpecsByHash = make(map[string]*allocationv1.GameServerAllocationSpec)
	m.allocationSpecsCounts = make(map[string]int)

	// channel used to simulate a Kubernetes informer event-driven pattern.
	m.gameServerUpdateChan = make(chan *MockGameServerUpdate)
	// This is a mock for code you'd write to handle update/create/delete events from an Agones
	// GameServer object listener
	go func() {

		var xxHash [16]byte

		for update := range m.gameServerUpdateChan {
			if update.DefunctAllocatorSpecHash != "" {
				// This is mocking out processing the data that a k8s informer
				// event handler would get as the outgoing object spec in a
				// delete or update operation. Since k8s is effectively removing this
				// oject, we have to stop tracking it in our agones integration code.
				m.mutex.Lock()
				// Make sure there's an existing reference count for this allocation spec
				if count, countExists := m.allocationSpecsCounts[update.DefunctAllocatorSpecHash]; countExists {
					// If removing one reference count would result in at least one remaining reference, then decrement
					if count-1 > 0 {
						m.allocationSpecsCounts[update.DefunctAllocatorSpecHash]--
						// If removing one reference count would make the reference count < 1, remove this allocation spec from memory.
					} else {
						delete(m.allocationSpecsCounts, update.DefunctAllocatorSpecHash)
						if _, exists := m.allocationSpecsByHash[update.DefunctAllocatorSpecHash]; exists {
							delete(m.allocationSpecsByHash, update.DefunctAllocatorSpecHash)
						}
					}
					// done with our updates; let other goroutines access the allocator specs.
					m.mutex.Unlock()
				} else {
					// no updates to make; release the lock while we log an error.
					m.mutex.Unlock()
					m.Log.Errorf("Error: %v", NoSuchAllocationSpecError)
				}
			}
			if len(update.MatchmakingRequests) > 0 {
				// This is mocking out processing the data that a k8s informer
				// event handler would receive as the incoming object spec in a
				// create or update operation. We now want to track this new
				// object spec in our agones integration code.
				mmfParams := &mmfParametersAnnotation{MmfRequests: make([]string, 0)}
				for _, mmfReq := range update.MatchmakingRequests {
					//Convert Mmf Request to JSON.
					mmfReqJSON, err := protojson.Marshal(mmfReq)
					if err != nil {
						m.Log.Errorf("Failed to marshal MMF parameters into JSON: %v", err)
						continue
					}
					// Add JSON string of this Mmf Request to the list of parameters to annotate this game server with.
					mmfParams.MmfRequests = append(mmfParams.MmfRequests, string(mmfReqJSON[:]))
				}

				// Make sure we successfully generated a JSON string out of at least one Mmf Request.
				if len(mmfParams.MmfRequests) == 0 {
					m.Log.Errorf("Failed to marshal MMF parameters into JSON: %v", err)
					continue
				}

				// generate new allocspec based on input Mmf Requests
				mmfReqJSON, err := json.Marshal(mmfParams)
				if err != nil {
					m.Log.Errorf("Unable to marshal MmfRequests annotation to JSON: %v", err)
					continue
				}
				xxHash = xxh3.Hash128Seed(mmfReqJSON, 0).Bytes()

				// Make an allocation spec that will find a server with a label
				// in the mmfRequestKey (defined as a const at the top of this
				// file) with a value that is the hash of the Matching Request,
				// and that will add the Matching Request as an annotation to
				// the game server upon allocation.
				newAllocatorSpec := &allocationv1.GameServerAllocationSpec{
					Selectors: []allocationv1.GameServerSelector{
						allocationv1.GameServerSelector{
							LabelSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{
									mmfRequestKey: hex.EncodeToString(xxHash[:]),
								},
							},
						},
					},
					MetaPatch: allocationv1.MetaPatch{
						// Attach the matchmaking request that can be used to
						// make matches this server wants to host as a k8s
						// annotation.
						Annotations: map[string]string{
							mmfRequestKey: string(mmfReqJSON[:]),
						},
					},
				}

				newAllocatorSpecJSON, err := json.Marshal(newAllocatorSpec)
				if err != nil {
					m.Log.Errorf("Unable to marshal Agones Game Server Allocation object to JSON: %v", err)
					continue
				}
				xxHash = xxh3.Hash128Seed(newAllocatorSpecJSON, 0).Bytes()
				newAllocatorSpecHash := hex.EncodeToString(xxHash[:])

				// Finally, write this allocation spec (representing a server) to memory and increment the
				// reference count of number of servers matching this allocation spec.
				m.mutex.Lock()
				m.allocationSpecsByHash[newAllocatorSpecHash] = newAllocatorSpec
				m.allocationSpecsCounts[newAllocatorSpecHash]++
				m.mutex.Unlock()
			}
		}
	}()

	// Initialize the matchmaking parameters of the mock Agones fleets
	// specified in the fleetConfig
	m.Fleets = make(map[string]*agonesv1.Fleet)
	for category, regions := range fleetConfig {
		for region, zones := range regions {
			for zone, modes := range zones {

				// Construct a fleet name based on the FleetConfig
				fleetName := fmt.Sprintf("%s/%s@%s", category, region, zone)
				fLogger := logger.WithFields(logrus.Fields{
					"fleet": fleetName,
				})

				// Generate an Agones Game Server Allocation that can allocate a ready server from this fleet.
				agonesAllocSpec := &allocationv1.GameServerAllocationSpec{
					Selectors: []allocationv1.GameServerSelector{
						allocationv1.GameServerSelector{
							LabelSelector: metav1.LabelSelector{
								MatchLabels: map[string]string{"agones.dev/fleet": fleetName},
							},
						},
					},
				}

				fleetAllocatorJSON, err := json.Marshal(agonesAllocSpec)
				if err != nil {
					fLogger.Errorf("Unable to marshal Agones Game Server Allocation object to JSON: %v", err)
					continue
				}

				// Hash the agones allocation spec and store it in a map by its hash for later lookup
				xxHash := xxh3.Hash128Seed(fleetAllocatorJSON, 0).Bytes()
				allocHash := hex.EncodeToString(xxHash[:])
				m.mutex.Lock()
				m.allocationSpecsByHash[allocHash] = agonesAllocSpec
				m.allocationSpecsCounts[allocHash] = math.MaxInt32
				m.mutex.Unlock()

				// Make an Open Match protobuf message extension that holds
				// the hash of the agones allocation spec. This will be
				// attached to the profile so the game server director can
				// look up the proper agones allocation spec to use when
				// allocating game servers for the resulting matches.
				agonesAllocatorHashValue, err := anypb.New(&knownpb.StringValue{Value: allocHash})
				if err != nil {
					fLogger.Errorf("Unable to create Any protobuf message to hold the Agones Game Server Allocation object's hash: %v", err)
					continue
				}
				fleetEx := map[string]*anypb.Any{
					agonesAllocatorHashKey: agonesAllocatorHashValue,
				}
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
				mmfParams := &mmfParametersAnnotation{MmfRequests: make([]string, len(modes))}
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
						CopyPoolFilters(zonePool, composedPool)
						CopyPoolFilters(pool, composedPool)
						composedPools[composedName] = composedPool
					}

					// Combine the Profile extensions defined for this mode
					// with those defined for this fleet. If they both contain
					// a value at the same key, the Fleet one will take
					// precedence.
					modeAndFleetEx := mode.ExtensionParams
					for key, value := range fleetEx {
						modeAndFleetEx[key] = value
					}

					// Marshal this mmf request to a JSON string
					profile, err := protojson.Marshal(&pb.MmfRequest{
						Mmfs: mode.Mmfs,
						Profile: &pb.Profile{
							Name:       profileName,
							Pools:      composedPools,
							Extensions: modeAndFleetEx,
						},
					})
					if err != nil {
						fLogger.WithFields(logrus.Fields{
							"game_mode": mode,
						}).Errorf("Failed to marshal MMF parameters into JSON: %v", err)
						return err
					}

					// Add the matchmaking profile for this game mode to this
					// fleet
					mmfParams.MmfRequests = append(mmfParams.MmfRequests, string(profile[:]))
				}

				// Marshal the array of profiles used by game modes in this fleet into JSON
				annotation, err := json.Marshal(mmfParams)
				if err != nil {
					fLogger.Errorf("Failed to marshal MMF parameters annotation into JSON: %v", err)
					return err
				}

				// Successfully have all the parameters we need to matchmake
				// for this fleet, ready to create it.
				// Refer to the Agones documentation for more information about Fleets.
				thisFleet := &agonesv1.Fleet{}
				thisFleet.ApplyDefaults()
				thisFleet.ObjectMeta = metav1.ObjectMeta{
					// Attach the matchmaking profiles for this fleet's
					// game modes to the fleet as a k8s annotation.
					Annotations: map[string]string{
						mmfRequestKey: string(annotation[:]),
					},
				}
				m.Fleets[fleetName] = thisFleet
			}
		}
	}
	return nil
}

// Allocate mocks doing an Agones game server allocation.
//func (m *MockAgonesIntegration) Allocate(match *pb.Match) (err error) {
//	// Convert match extension field at specified key to a string
//	// representation of an Agones allocation request
//	stringAllocReq := &knownpb.StringValue{}
//	if err = match.GetExtensions()[agonesAllocatorMatchExtensionKey].UnmarshalTo(stringAllocReq); err != nil {
//		m.Log.Errorf("failure to read the allocation extension field from match %v: %v", match.GetId(), err)
//		return err
//	}
//
//	// Marshal the string into an Agones allocation request
//	agonesAllocReq := &allocationv1.GameServerAllocation{}
//	if err = json.Unmarshal([]byte(stringAllocReq.Value), agonesAllocReq); err != nil {
//		m.Log.Errorf("failure to read the allocation extension field from match %v: %v", match.GetId(), err)
//		return err
//	}
//
//	m.Log.Tracef(" match %v allocation criteria %v", match.GetId(), stringAllocReq.Value)
//	return nil
//}

// Allocate mocks doing an Agones game server allocation.
// If the game server instance being allocated needs to initiate a backfill
// profile, that profile needs to be created in the AllocationSpec annotation
// metadata before this function is called, as Agones sets the metadata
// atomically on game server allocation.
func (m *MockAgonesIntegration) Allocate(allocationSpecHash string, match *pb.Match) (err error) {
	// Here is where your backend would do any additional prep tasks for server allocation in Agones
	//*allocationv1.GameServerAllocationSpec
	m.Log.Debugf("Allocating server for match %v", match.GetId())
	allocSpec, ok := m.allocationSpecsByHash.LoadAndDelete(allocationSpecHash)
	if !ok {
		m.Log.Errorf(" no allocation criteria found for match %v!", match.GetId())
		return NoSuchAllocationSpecError
	} else {
		// TODO: Here is where you'd use the k8s api to request an agones game server allocation in a real implementation.
		m.Log.Debugf("Allocating server for match %v", match.GetId())
		m.Log.Tracef(" match %v allocation criteria %v", match.GetId(), allocSpec)
	}
	return nil
}

func (m *MockAgonesIntegration) GetMmfParams() (requests []*pb.MmfRequest) {

	var err error

	// servers with unique MmfRequests (examples: backfill, join-in-progress,
	// high-density game servers, etc) should be serviced with higher priority,
	// so add them to the request list first.
	//
	// This sync.Map.Range() is the equivalent to this kind of loop over a
	// normal golang map:
	// for mmfRequest, _ := m.IndividualGameServerMatchmakingRequests {
	//		requests = append(requests, mmfRequest.(*pb.MmfRequest))
	// }
	// See the sync.Map documentation for more info.
	m.IndividualGameServerMatchmakingRequests.Range(func(mmfRequest, _ interface{}) bool {
		requests = append(requests, mmfRequest.(*pb.MmfRequest))
		return true
	})

	//		// Try to marshal the request from string back into a pb.MmfRequest
	//		//reqPb := &pb.MmfRequest{}
	//		//err = protojson.Unmarshal([]byte(mmfRequest), reqPb)
	//		//if err != nil {
	//		//	m.Log.Error("cannot unmarshal server matchmaking request back into protobuf, ignoring...")
	//		//	continue
	//		//}
	//
	//		// Generate an Agones Game Server Allocation object that can select this server for re-allocation.
	//		agonesReallocSpec := &allocationv1.GameServerAllocationSpec{
	//			Selectors: []allocationv1.GameServerSelector{
	//				allocationv1.GameServerSelector{
	//					metav1.LabelSelector{
	//						MatchLabels: map[string]string{
	//							agonesGSReadyKey: "true",
	//							// TODO: this needs to be a hash or something, labels can only be 63 chars?
	//							//mmfRequestKey: mmfRequest,
	//						},
	//					},
	//				},
	//			},
	//		}
	//		backfillRequestingGameServerSelector := allocationv1.GameServerSelector{}
	//		backfillRequestingGameServerSelector.LabelSelector = metav1.LabelSelector{
	//			MatchLabels: map[string]string{
	//				agonesGSReadyKey: "true",
	//				// TODO: this needs to be a hash or something, labels can only be 63 chars?
	//				//mmfRequestKey: mmfRequest,
	//			},
	//		}
	//		agonesReallocSpec.Selectors = append(agonesReallocSpec.Selectors, backfillRequestingGameServerSelector)

	// TODO: remove after testing
	//		agonesReallocatorJSON, err := json.Marshal(agonesReallocReq)
	//		if err != nil {
	//			logger.Errorf("Unable to marshal Agones Game Server Allocation object to JSON: %v", err)
	//			continue
	//		}
	//
	//		exAgonesReallocator, err := anypb.New(&knownpb.StringValue{Value: string(agonesReallocatorJSON)})
	//		if err != nil {
	//			logger.Errorf("Unable to convert number of ready replicas into an anypb: %v", err)
	//			continue
	//		}
	//		reqPb.Profile.Extensions = map[string]*anypb.Any{
	//			agonesAllocatorMatchExtensionKey: exAgonesReallocator,
	//		}
	//		requests = append(requests, reqPb)

	m.Log.Tracef("retrieved %v backfill MmfRequests from servers", len(requests))

	for fleetName, fleet := range m.Fleets {
		log := m.Log.WithFields(logrus.Fields{"fleet_name": fleetName})
		log.Trace("Attempting to get fleet MmfRequests")

		// get the mmf request used to populate this fleet
		//
		// In a real Agones integration, you'd write an informer + lister that
		// allow you to query all fleet annotations with minimal impact on the
		// k8s control plane:
		// https://agones.dev/site/docs/guides/access-api/#best-practice-using-informers-and-listers
		jsonRequests := &mmfParametersAnnotation{}
		err = json.Unmarshal([]byte(fleet.ObjectMeta.Annotations[mmfRequestKey]), jsonRequests)
		if err != nil {
			log.Errorf("failure to read the mmf annotation from fleet %v: %v", fleetName, err)
			continue
		}

		// Each fleet could have multiple different MmfRequests associated with
		// it, to allow it to run multiple different kinds of matchmaking
		// concurrently (common with undifferentiated server fleets)
		log.Tracef("retrieved '%v' fleet MmfRequests from k8s annotation.", len(jsonRequests.MmfRequests))
		for _, request := range jsonRequests.MmfRequests {

			// For some reason I'm not sure about, the json.Unmarshal is giving
			// an mmfParametersAnnotation{} struct with one empty item in the
			// mmfParametersAnnotation.MmfRequests array, so we skip if we get
			// an empty entry.
			if request != "" {

				log.Tracef("retrieved MmfRequest: %v", request)

				reqPb := &pb.MmfRequest{}
				err = protojson.Unmarshal([]byte(request), reqPb)
				if err != nil {
					log.Errorf("cannot unmarshal fleet matchmaking request back into protobuf: %v", err)
					continue
				}

				// Get number of ready servers that can accept matches from
				// this MmfRequest, and put this in the profile extensions
				// field so the mmf can read it.
				exReadyReplicas, err := anypb.New(&knownpb.Int32Value{Value: fleet.Status.ReadyReplicas})
				if err != nil {
					log.Errorf("Unable to convert number of ready replicas into an anypb: %v", err)
					continue
				}
				reqPb.Profile.Extensions = map[string]*anypb.Any{
					"readyServerCount": exReadyReplicas,
				}
			}
		}
	}
	return requests
}

type MockDirector struct {
	OmClient                 *omclient.RestfulOMGrpcClient
	Cfg                      *viper.Viper
	Log                      *logrus.Logger
	Assignments              sync.Map
	GameServerManager        *MockAgonesIntegration
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

							// NOTE: If you have some match processing that can
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
		requests := d.GameServerManager.GetMmfParams()
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

			// Find the profile of this match (by hash) in the list of
			// active matchmaking requests we are running
			//
			// Get the profile hash from the match extensions field
			profileHash, err := extensions.String(match.Extensions, mmfProfileHashKey)
			if err != nil {
				// Couldn't get the hash, reject match.
				logger.WithFields(logrus.Fields{
					"match_id": match.Id,
				}).Error("Unable to get Profile hash from returned match, rejecting match: %v")
				rejectMatch(match)
				continue
			}

			// Make sure the match profile is in our list  of active
			// matchmaking requests
			if _, exists := d.pendingMmfRequestsByHash[profileHash]; !exists {
				// No profile with that hash in our list, reject match.
				rejectMatch(match)
				continue
			}

			// This proposed match passed all our decision points and validation checks,
			// lets add it to the map of matches we want to allocate a server for.
			//acceptedMatchesCounter.Add(ctx, 1)
			allocatorSpec, err := extensions.String(d.pendingMmfRequestsByHash[profileHash].GetProfile().GetExtensions(), agonesAllocatorHashKey)
			if err != nil {
				rejectMatch(match)
				continue
			}
			matches[match] = allocatorSpec
		}

		for match, allocatorSpec := range matches {
			// Here is where you would actually allocate the server in Agones
			// The mocks just print a log message telling you when an
			// allocation would take place.
			//d.GameServerManager.Allocate(match, &allocationv1.GameServerAllocationSpec{})
			d.GameServerManager.Allocate(allocatorSpec, match)
		}

		// Re-activate the rejected tickets so they appear in matchmaking pools again.
		d.OmClient.ActivateTickets(ctx, ticketIdsToActivate)

		// Status update closure. This may be removed or made into a separate
		// function in future versions
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

// CopyPoolFilters copies the filters from src pool to dest pool, allowing us
// to combine filters from different pools to create pools dynamically.
//
// NOTE: If the destination contains a CreationTimeFilter and the source does
// not, the destination CreationTimeFilter will be preserved.
func CopyPoolFilters(src *pb.Pool, dest *pb.Pool) {
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
