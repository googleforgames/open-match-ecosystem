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
	"math/rand/v2"
	_ "net/http"
	_ "net/http/pprof"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
)

// Simple generates a mock game client matchmaking request.
func Simple(ctx context.Context) *pb.Ticket {

	// Make a fake game client matchmaking request
	crTime := time.Now()
	s := strconv.FormatInt(crTime.UnixNano(), 10)
	minPing := 20
	maxPing := 120
	ticket := &pb.Ticket{
		Attributes: &pb.Ticket_FilterableData{
			// Make mock game client attributes.
			Tags: []string{s},
			DoubleArgs: map[string]float64{
				// mock out a ping to this datacenter between 20-250ms
				"ping.asia-northeast1-a": float64(rand.IntN(maxPing-minPing) + minPing),
			},
			CreationTime: timestamppb.New(crTime),
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
