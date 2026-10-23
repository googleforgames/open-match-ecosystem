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

package extensions

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestBool(t *testing.T) {
	trueAny, err := anypb.New(wrapperspb.Bool(true))
	if err != nil {
		t.Fatalf("failed to pack true wrapperspb.Bool: %v", err)
	}
	falseAny, err := anypb.New(wrapperspb.Bool(false))
	if err != nil {
		t.Fatalf("failed to pack false wrapperspb.Bool: %v", err)
	}
	intAny, err := anypb.New(wrapperspb.Int32(42))
	if err != nil {
		t.Fatalf("failed to pack wrapperspb.Int32: %v", err)
	}

	t.Run("extracts true boolean value", func(t *testing.T) {
		ex := map[string]*anypb.Any{
			MatchDeactivationTimeoutKey: trueAny,
		}
		got, err := Bool(ex, MatchDeactivationTimeoutKey)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got {
			t.Errorf("got %v, want true", got)
		}
	})

	t.Run("extracts false boolean value", func(t *testing.T) {
		ex := map[string]*anypb.Any{
			MatchDeactivationTimeoutKey: falseAny,
		}
		got, err := Bool(ex, MatchDeactivationTimeoutKey)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got {
			t.Errorf("got %v, want false", got)
		}
	})

	t.Run("returns NoSuchKeyError when key is missing", func(t *testing.T) {
		ex := map[string]*anypb.Any{}
		got, err := Bool(ex, MatchDeactivationTimeoutKey)
		if !errors.Is(err, NoSuchKeyError) {
			t.Fatalf("got error %v, want %v", err, NoSuchKeyError)
		}
		if got {
			t.Errorf("got %v, want false", got)
		}
	})

	t.Run("returns NoSuchKeyError when extension map is nil", func(t *testing.T) {
		var ex map[string]*anypb.Any
		got, err := Bool(ex, MatchDeactivationTimeoutKey)
		if !errors.Is(err, NoSuchKeyError) {
			t.Fatalf("got error %v, want %v", err, NoSuchKeyError)
		}
		if got {
			t.Errorf("got %v, want false", got)
		}
	})

	t.Run("returns unmarshal error when Any wraps non-bool type", func(t *testing.T) {
		ex := map[string]*anypb.Any{
			MatchDeactivationTimeoutKey: intAny,
		}
		got, err := Bool(ex, MatchDeactivationTimeoutKey)
		if err == nil {
			t.Fatal("expected unmarshal error, got nil")
		}
		if errors.Is(err, NoSuchKeyError) {
			t.Fatalf("got NoSuchKeyError, want unmarshal error: %v", err)
		}
		if got {
			t.Errorf("got %v, want false", got)
		}
	})
}
