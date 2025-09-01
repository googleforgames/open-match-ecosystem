// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mmf

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"open-match.dev/open-match/pkg/pb"
)

func TestCheckQuality(t *testing.T) {
	t.Run("no tickets", func(t *testing.T) {
		q := computeQuality(nil)
		require.Equal(t, math.Inf(-1), q)
	})
	t.Run("one ticket", func(t *testing.T) {
		q := computeQuality([]*pb.Ticket{
			{
				SearchFields: &pb.SearchFields{
					DoubleArgs: map[string]float64{
						"mmr": 3,
					},
				},
			},
		})
		require.Equal(t, 0.0, q)
	})
}
