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

package metrics

import (
	"context"
	"testing"
	"time"
)

func TestServiceVersionAndInitializeOtel(t *testing.T) {
	if ServiceVersion != "2.1.0" {
		t.Fatalf("got ServiceVersion %q, want %q", ServiceVersion, "2.1.0")
	}

	meterPtr, shutdown := InitializeOtel("metrics-test")
	if meterPtr == nil || *meterPtr == nil {
		t.Fatal("expected non-nil meter pointer from InitializeOtel")
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown function from InitializeOtel")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = shutdown(ctx)
}
