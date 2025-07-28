// Copyright 2025 Google LLC
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
package assignmentdistributor

import (
	"context"

	"github.com/googleforgames/open-match2/v2/pkg/pb"
)

// Sender defines the interface for sending roster assignments.
type Sender interface {
	Send(ctx context.Context, roster *pb.Roster) error
	Stop() // Tells the distributor that your code is permanently done sending assignments.
}

// Receiver defines the interface for receiving roster assignments.
type Receiver interface {
	Receive(ctx context.Context, handler func(ctx context.Context, roster *pb.Roster)) error
	Stop() // Tells the distributor that your code is permanently done receiving assignments.
}
