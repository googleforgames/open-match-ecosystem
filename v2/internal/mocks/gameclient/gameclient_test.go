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

package gameclient

import (
	"context"
	"testing"
)

func TestSimple(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		ticket := Simple(ctx)
		if ticket == nil {
			t.Fatalf("iteration %d: expected non-nil ticket", i)
		}
		attrs := ticket.GetAttributes()
		if attrs == nil {
			t.Fatalf("iteration %d: expected non-nil ticket attributes", i)
		}

		crTime := attrs.GetCreationTime()
		if crTime == nil || !crTime.IsValid() || crTime.AsTime().IsZero() {
			t.Fatalf("iteration %d: invalid CreationTime: %v", i, crTime)
		}

		tags := attrs.GetTags()
		if len(tags) != 1 || tags[0] == "" {
			t.Fatalf("iteration %d: got tags %v, want 1 non-empty tag", i, tags)
		}

		ping, ok := attrs.GetDoubleArgs()["ping.asia-northeast1-a"]
		if !ok {
			t.Fatalf("iteration %d: missing ping.asia-northeast1-a in DoubleArgs", i)
		}
		if ping < 20 || ping >= 120 {
			t.Fatalf("iteration %d: got ping %v, want in [20, 120)", i, ping)
		}

		class, ok := attrs.GetStringArgs()["class"]
		if !ok {
			t.Fatalf("iteration %d: missing class in StringArgs", i)
		}
		switch class {
		case "tank", "healer", "dps":
		default:
			t.Fatalf("iteration %d: unexpected class %q, want tank, healer, or dps", i, class)
		}
	}
}
