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
// In this game client mocking module, the 'game client' sends an Open match
// protobuf `ticket` message directly. However, in a real game client, you'd
// probably use whatever communication protocol/format (JSON string, binary
// encoding, custom format, etc) your game engine or dev kit encourages, and
// parse it into the Open Match protobuf `ticket` protobuf in your matchmaker
// queue.

package gameclient

import (
	"context"
	"math"
	"math/rand/v2"
	// Register pprof HTTP handlers for profiling (revive: blank-imports).
	// revive:disable-next-line:blank-imports
	_ "net/http/pprof"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
)

type weightedZone struct {
	name          string
	cumWeight     int
	adjacentZones []string
	minOffsetMs   int
	offsetSpanMs  int
}

// regionalZones models the global multiplayer population across the 11
// datacenter zones in gsdirector.FleetConfig (APAC 44%, EMEA 28%, NA 17%,
// SA 8%, and Africa 3%) and the intra-continental fibre latency offsets to
// physically nearby neighbour zones.
var regionalZones = []weightedZone{
	{name: "asia-northeast3-b", cumWeight: 18, adjacentZones: []string{"asia-northeast1-a"}, minOffsetMs: 74, offsetSpanMs: 60},           // KR Seoul, 18%
	{name: "europe-west3-a", cumWeight: 34, adjacentZones: []string{"europe-central2-b"}, minOffsetMs: 72, offsetSpanMs: 60},              // DE Frankfurt, 16%
	{name: "asia-northeast1-a", cumWeight: 49, adjacentZones: []string{"asia-northeast3-b"}, minOffsetMs: 74, offsetSpanMs: 60},           // JP Tokyo, 15%
	{name: "us-east4-b", cumWeight: 60, adjacentZones: []string{"us-west2-c"}, minOffsetMs: 76, offsetSpanMs: 60},                         // US Ashburn, 11%
	{name: "europe-central2-b", cumWeight: 69, adjacentZones: []string{"europe-west3-a"}, minOffsetMs: 72, offsetSpanMs: 60},              // PL Warsaw, 9%
	{name: "southamerica-east1-c", cumWeight: 77, adjacentZones: nil, minOffsetMs: 0, offsetSpanMs: 0},                                    // BR Sao Paulo, 8%
	{name: "asia-south2-c", cumWeight: 84, adjacentZones: []string{"me-central2-a"}, minOffsetMs: 76, offsetSpanMs: 60},                   // IN Delhi, 7%
	{name: "us-west2-c", cumWeight: 90, adjacentZones: []string{"us-east4-b"}, minOffsetMs: 76, offsetSpanMs: 60},                         // US Los Angeles, 6%
	{name: "australia-southeast1-c", cumWeight: 94, adjacentZones: nil, minOffsetMs: 0, offsetSpanMs: 0},                                  // AU Sydney, 4%
	{name: "me-central2-a", cumWeight: 97, adjacentZones: []string{"europe-west3-a", "asia-south2-c"}, minOffsetMs: 76, offsetSpanMs: 60}, // SA Dammam, 3%
	{name: "africa-south1-b", cumWeight: 100, adjacentZones: nil, minOffsetMs: 0, offsetSpanMs: 0},                                        // ZA Johannesburg, 3%
}

// Simple generates a mock game client matchmaking request with realistic
// regional latency and Gaussian MMR skill attributes.
func Simple(ctx context.Context) *pb.Ticket {
	homeZone := regionalZones[0]
	zoneRoll := rand.IntN(100)
	for _, candidate := range regionalZones {
		if candidate.cumWeight > zoneRoll {
			homeZone = candidate
			break
		}
	}

	doubleArgs := make(map[string]float64, 2+len(homeZone.adjacentZones))

	// Home zone ping in [20, 85] ms is always within the 100 ms limit of the
	// zone pool filter.
	homePing := float64(20 + rand.IntN(66))
	doubleArgs["ping."+homeZone.name] = homePing

	// Neighbour pings give border players in connected regions (roughly 20% to
	// 35%) a path to an adjacent datacenter.
	for _, adj := range homeZone.adjacentZones {
		doubleArgs["ping."+adj] = homePing + float64(homeZone.minOffsetMs+rand.IntN(homeZone.offsetSpanMs))
	}

	// Gaussian MMR skill rating (mean 1500, sigma 250), rounded to the nearest
	// integer and clamped to [500, 2750].
	mmr := math.Round(rand.NormFloat64()*250 + 1500)
	if mmr < 500 {
		mmr = 500
	} else if mmr > 2750 {
		mmr = 2750
	}
	doubleArgs["mmr"] = mmr

	// No CreationTime or Tags are set here because the matchmaking queue sets
	// the creation time (the platform decides when a player entered
	// matchmaking) and none of the example pools filter on tags.
	ticket := &pb.Ticket{
		Attributes: &pb.Ticket_FilterableData{
			DoubleArgs: doubleArgs,
		},
	}

	// Matching Attribute example: this fake game type needs to match based
	// on what character the player has chosen.
	//
	// For this mock, randomly select a character archetype this fake player
	// wants to play
	var selectedClass string
	switch rand.IntN(5) + 1 { // 1d5
	case 1:
		selectedClass = "tank"
	case 2:
		selectedClass = "healer"
	default: // 60% of players will select dps
		selectedClass = "dps"
	}
	ticket.Attributes.StringArgs = map[string]string{"class": selectedClass}

	return ticket
}
