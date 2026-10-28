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
package main

import (
	"testing"
	"time"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
)

func TestBuildSegmentedFleetConfigShape(t *testing.T) {
	buildSegmentedFleetConfig([]*pb.MatchmakingFunctionSpec{mmfFifo})

	if len(gameModesInZone) == 0 {
		t.Fatal("expected gameModesInZone to be non-empty")
	}
	if len(gameModesInZone) != len(zonePools) {
		t.Fatalf("len(gameModesInZone) = %d, want %d (len(zonePools))", len(gameModesInZone), len(zonePools))
	}

	for zoneKey, modes := range gameModesInZone {
		if len(modes) != 1 {
			t.Fatalf("zone %q has %d game modes, want 1", zoneKey, len(modes))
		}
		mode := modes[0]
		if len(mode.MMFs) != 1 || mode.MMFs[0] != mmfFifo {
			t.Errorf("zone %q game mode %q MMFs = %v, want single element mmfFifo", zoneKey, mode.Name, mode.MMFs)
		}

		tierPool, ok := mode.Pools["mmr_tier"]
		if !ok || tierPool == nil {
			t.Fatalf("zone %q game mode %q is missing pool under key \"mmr_tier\"", zoneKey, mode.Name)
		}

		// A creation-time window captured at startup eventually stops matching
		// new tickets on a long-running instance, so tier pools must leave
		// CreationTimeRangeFilter nil.
		if tierPool.GetCreationTimeRangeFilter() != nil {
			t.Errorf("zone %q tier pool %q CreationTimeRangeFilter = %v, want nil", zoneKey, tierPool.GetName(), tierPool.GetCreationTimeRangeFilter())
		}
	}
}

func TestWaitBoundedCompletesInTime(t *testing.T) {
	if !waitBounded(1*time.Second, func() {}) {
		t.Fatal("waitBounded returned false for an immediate function, want true")
	}
}

func TestWaitBoundedTimesOut(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	if waitBounded(10*time.Millisecond, func() { <-release }) {
		t.Fatal("waitBounded returned true for a blocked function, want false")
	}
}
