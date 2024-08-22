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
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	"github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
	knownpb "google.golang.org/protobuf/types/known/wrapperspb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	// Required for protojson to correctly parse JSON when unmarshalling to protobufs that contain
	// 'well-known types' https://github.com/golang/protobuf/issues/1156
	"google.golang.org/protobuf/types/known/anypb"
	_ "google.golang.org/protobuf/types/known/wrapperspb"

	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

// Keys for metadata/extensions

// K8s object annotation metadata key where the JSON representation of the
// pb.MmfRequest can be found.
const mmfRequestKey = "open-match.dev/mmf"

// Open Match match protobuf message extensions field key where the JSON
// representation of the agones allocation request can be found.
const agonesAllocatorMatchExtensionKey = "agones.dev/GameServerAllocation"

type GameMode struct {
	Name            string
	Mmfs            []*pb.MatchmakingFunctionSpec
	Pools           map[string]*pb.Pool
	ExtensionParams map[string]*anypb.Any
}

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
		ExtensionParams: anypbIntMap(map[string]int32{
			"desiredRosterLen": 4,
			"minRosterLen":     2,
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

	FleetExistsError = errors.New("Fleet with that name already exists")
)

type MockAgonesIntegration struct {
	// https://github.com/googleforgames/agones/blob/eb08f0b76c6c87d7c13cdef02788ea26800a2df2/pkg/apis/agones/v1/fleet.go
	// This just mocks out real Agones fleets, holding an in-memory 'fake'
	// fleet of gameservers
	Fleets map[string]*agonesv1.Fleet

	// In the format GameServersRequestingMorePlayers[labelValue], where
	// 'labelValue' represents the backfill MmfRequest params for the server.
	// So, for example, any server that needs a backfill
	// would have an ObjectMeta.Label applied using the pre-determined key of
	// `mmfRequestKey` and the value being a JSON representation of the
	// pb.MmfRequest protocol buffer message.
	//
	// This is a greatly simplified mock.
	// In reality, this would be implemented using a lister for GameServers
	// looking for the server with a matching `labelValue` string at some
	// pre-determined k8s Label key we've chosen to hold server backfill
	GameServersRequestingMorePlayers map[string]*agonesv1.GameServer

	// Shared logger.
	Log *logrus.Logger
}

// We'll add a Kubernetes Annotation to each Agones Fleet that describes how to
// make matches for the game servers in that fleet.
// If an individual server needs more players, we can add a Kubernetes
// Annotation to describe the kind of players it needs.
type mmfParametersAnnotation struct {
	MmfRequests []string
}

func (m *MockAgonesIntegration) InitializeMmfParams(fleetConfig map[string]map[string]map[string][]*GameMode) error {

	// Initialize fleets and the matchmaking parameters for each
	m.Fleets = make(map[string]*agonesv1.Fleet)
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
				mmfParams := &mmfParametersAnnotation{MmfRequests: make([]string, len(modes))}
				for _, mode := range modes {

					// For every pool this game mode requests, also include the filters for this fleet.
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

					// Marshal this mmf request to a JSON string
					profile, err := protojson.Marshal(&pb.MmfRequest{
						Mmfs: mode.Mmfs,
						Profile: &pb.Profile{
							Name:       profileName,
							Pools:      composedPools,
							Extensions: mode.ExtensionParams,
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
				thisFleet := &agonesv1.Fleet{}
				thisFleet.ApplyDefaults()
				thisFleet.ObjectMeta = metav1.ObjectMeta{
					// Attach the matchmaking profiles for this fleet's
					// game modes to the fleet as a k8s annotation.
					Annotations: map[string]string{
						mmfRequestKey: string(annotation[:]),
					},
				}

				// Refer to the Agones documentation for more information about Fleets.
				m.Fleets[fleetName] = thisFleet

			}
		}
	}
	return nil
}

// AllocateGameServer mocks doing an Agones game server allocation.
// The two arguments mock using k8s Labels to initiate and fulfill a backfill
// for the server:
// - `existingRequest` mocks out adding custom metadata at the label key
// `mmfRequestKey` (const defined above) at allocation time. We use this to add
// a JSON representation of a pb.MmfRequest protobuf message to the server that
// the director should read and use to invoke MMFs to get more players for this
// server.
// -`newRequest` mocks out using a LabelSelector on the label key `mmfRequestKey`
// (const defined above), specifying that we want to do an allocation of a
// server with this value at that key. We use this to find the server on which
// the backfill match belongs after the Director has received the results from
// Open Match. The backfill MmfRequest is removed in this process. If you want
// to continue to backfill the server, you should specify an updated MmfRequest
// in the `addValue` param in the same call.
//
// https://agones.dev/site/docs/integration-patterns/high-density-gameservers/
// for more details about how Agones expects you allocate the same
// allocated server multiple times.  server := &agonesv1.GameServer{}
func (m *MockAgonesIntegration) AllocateAndBackfillGameServer(existingRequest string, newRequest string) {
	// Simulate a k8s label selector.
	// This mocks out adding more players to an existing server with an
	// outstanding backfill MmfRequest.
	var serverExists bool
	if existingRequest != "" {
		// This is a greatly simplified mock.
		//
		// In reality, we would be checking the value at a pre-defined
		// key in ObjectMeta.Labels for this k8s resource using a lister in order to find the server
		// with this backfill MmfRequest
		if _, serverExists = m.GameServersRequestingMorePlayers[existingRequest]; serverExists {
			// Found the server with this label value. Remove the existing
			// backfill MmfRequest since it has been fulfilled.
			delete(m.GameServersRequestingMorePlayers, existingRequest)
		}
	}

	if newRequest != "" {
		// Simulate 'allocating' a game server in the provided fleet, with the
		// provided value for the label at some pre-defined key.
		// This mocks out allocating a server that includes a backfill MmfRequest
		// to add more players to it.
		m.GameServersRequestingMorePlayers[newRequest] = &agonesv1.GameServer{}
	}

}

func (m *MockAgonesIntegration) Allocate(match *pb.Match) (err error) {
	// Convert match extension field at specified key to a string
	// representation of an Agones allocation request
	stringAllocReq := &knownpb.StringValue{}
	if err = match.GetExtensions()[agonesAllocatorMatchExtensionKey].UnmarshalTo(stringAllocReq); err != nil {
		m.Log.Errorf("failure to read the allocation extension field from match %v: %v", match.GetId(), err)
		return err
	}

	// Marshal the string into an Agones allocation request
	agonesAllocReq := &allocationv1.GameServerAllocation{}
	if err = json.Unmarshal([]byte(stringAllocReq.Value), agonesAllocReq); err != nil {
		m.Log.Errorf("failure to read the allocation extension field from match %v: %v", match.GetId(), err)
		return err
	}

	// Here is where you'd actually use the k8s api to request an agones game server allocation.
	m.Log.Debugf("Allocating server for match %v using allocation criteria %v", match.GetId(), stringAllocReq.Value)
	return nil
}

func (m *MockAgonesIntegration) GetMmfParams() (requests []*pb.MmfRequest) {

	var err error

	// servers that need backfills should be serviced with higher priority
	//
	// In a real Agones integration, you'd write an informer + lister that
	// allow you to query all existing Agones GameServers for a backfill
	// MmfRequest label in order to minimize impact to the k8s control plane:
	// https://agones.dev/site/docs/guides/access-api/#best-practice-using-informers-and-listers
	for mmfRequest, _ := range m.GameServersRequestingMorePlayers {

		// Try to marshal the request from string back into a pb.MmfRequest
		reqPb := &pb.MmfRequest{}
		err = protojson.Unmarshal([]byte(mmfRequest), reqPb)
		if err != nil {
			m.Log.Error("cannot unmarshal server matchmaking request back into protobuf, ignoring...")
			continue
		}

		// Successfully got the backfill request, add it to the list of MMF
		// requests to send to Open Match.
		requests = append(requests, reqPb)
	}
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

		// Generate an Agones Game Server Allocation object that can allocate a ready server from this fleet.
		agonesAllocReq := &allocationv1.GameServerAllocation{}
		agonesAllocReq.ApplyDefaults()
		fleetSelector := allocationv1.GameServerSelector{}
		fleetSelector.LabelSelector = metav1.LabelSelector{
			MatchLabels: map[string]string{"agones.dev/fleet": fleetName},
		}
		agonesAllocReq.Spec.Selectors = append(agonesAllocReq.Spec.Selectors, fleetSelector)
		fleetAllocatorJSON, err := json.Marshal(agonesAllocReq)
		if err != nil {
			log.Errorf("Unable to marshal Agones Game Server Allocation object to JSON: %v", err)
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

				// Get number of ready servers, and put this in the profile extensions
				// field so the mmf can read it.
				exReadyReplicas, err := anypb.New(&knownpb.Int32Value{Value: fleet.Status.ReadyReplicas})
				if err != nil {
					log.Errorf("Unable to convert number of ready replicas into an anypb: %v", err)
					continue
				}
				reqPb.Profile.Extensions = map[string]*anypb.Any{
					"readyServerCount": exReadyReplicas,
				}

				// Put the fleet allocator into the profile extensions. We'll need to
				// write our MMF so it propogates this value into the extensions
				// field of returned matches, so we can send those matches to the
				// correct fleet when they come back from Open Match.
				exAgonesAllocator, err := anypb.New(&knownpb.StringValue{Value: string(fleetAllocatorJSON)})
				if err != nil {
					log.Errorf("Unable to convert number of ready replicas into an anypb: %v", err)
					continue
				}
				reqPb.Profile.Extensions = map[string]*anypb.Any{
					agonesAllocatorMatchExtensionKey: exAgonesAllocator,
				}
				requests = append(requests, reqPb)
			}
		}
	}
	return requests
}

type MockDirector struct {
	OmClient          *omclient.RestfulOMGrpcClient
	Cfg               *viper.Viper
	Log               *logrus.Logger
	Assignments       sync.Map
	GameServerManager *MockAgonesIntegration
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
	var matches, proposedMatches []*pb.Match

	// Kick off main matchmaking loop
	// Loop for the configured number of cycles
	logger.Debugf("Beginning matchmaking cycle, %v cycles configured", d.Cfg.GetInt("NUM_MM_CYCLES"))
	for i := d.Cfg.GetInt("NUM_MM_CYCLES"); i > 0; i-- { // Main Matchmaking Loop

		// Loop var init
		startTime = time.Now()
		proposedMatches = []*pb.Match{}
		matches = []*pb.Match{}

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
		// NOTE: This is a complex goroutine so it's over-commented.
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
		for _, match := range proposedMatches {
			// simulate 1 in 100 matches being rejected by our matchmaker
			// because we don't like them or don't have available servers to
			// host the sessions right now.
			if rand.Intn(100) <= 1 {
				for _, roster := range match.GetRosters() {
					for _, ticket := range roster.GetTickets() {
						// queue this ticket ID to be re-activated
						ticketIdsToActivate <- ticket.GetId()
						numRejectedTickets++
					}
				}
				//rejectedMatchesCounter.Add(ctx, 1)
			} else {
				//acceptedMatchesCounter.Add(ctx, 1)
				matches = append(matches, match)
			}
		}

		for _, match := range matches {
			// Here is where you would actually allocate the server in Agones
			// This is just a mock, so it instead just prints a log message.
			d.GameServerManager.Allocate(match)
		}

		// Re-activate the rejected tickets so they appear in matchmaking pools again.
		d.OmClient.ActivateTickets(ctx, ticketIdsToActivate)

		// Status update closure.
		{
			// Print status for this cycle
			logger.Trace("Acquiring lock to write cycleStatus")

			numCycles := fmt.Sprintf("/%v", d.Cfg.GetInt("NUM_MM_CYCLES"))
			// ~600k seconds is roughly a week
			if d.Cfg.GetInt("NUM_MM_CYCLES") > 600000 {
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

// Make a map[string]int32 into a map of 'any' protobuf messages, suitable to
// send in the extensions field of a Open Match protobuf message.
func anypbIntMap(in map[string]int32) (out map[string]*anypb.Any) {
	for key, value := range in {
		anyValue, err := anypb.New(&knownpb.Int32Value{Value: value})
		if err != nil {
			panic(err)
		}
		out[key] = anyValue
	}
	return
}
