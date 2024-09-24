// Copyright 2019 Google LLC
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
package extensions

// Keys for metadata/extensions
const (
	// K8s object annotation metadata key where the JSON representation of the
	// pb.MmfRequest can be found.
	MMFRequestKey     = "open-match.dev/mmf-request"
	MMFRequestHashKey = "open-match.dev/mmf-request-hash"

	// Open Match match protobuf message extensions field key where the JSON
	// representation of the agones allocation request can be found.
	AgonesAllocatorKey = "open-match.dev/agones-game-server-allocator"

	// Key that holds the k8s label identifying that the game server is ready
	// for (re-)allocation
	// https://agones.dev/site/docs/integration-patterns/high-density-gameservers/
	AgonesGSReadyKey      = "agones.dev/sdk-gs-session-ready"
	AgonesGSReadyCountKey = "open-match.dev/agones-game-server-ready-count"
)
