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

func TestCombine(t *testing.T) {
	oneAny, err := anypb.New(wrapperspb.Int32(1))
	if err != nil {
		t.Fatalf("failed to pack wrapperspb.Int32(1): %v", err)
	}
	twoAny, err := anypb.New(wrapperspb.Int32(2))
	if err != nil {
		t.Fatalf("failed to pack wrapperspb.Int32(2): %v", err)
	}

	t.Run("b wins on conflicts and inputs are not mutated", func(t *testing.T) {
		a := map[string]*anypb.Any{
			"shared": oneAny,
			"onlyA":  oneAny,
		}
		b := map[string]*anypb.Any{
			"shared": twoAny,
			"onlyB":  twoAny,
		}

		got := Combine(a, b)
		if len(got) != 3 {
			t.Fatalf("len(got) = %d, want 3", len(got))
		}
		if got["shared"] != twoAny {
			t.Errorf("got[\"shared\"] = %p, want %p", got["shared"], twoAny)
		}
		if got["onlyA"] != oneAny {
			t.Errorf("got[\"onlyA\"] = %p, want %p", got["onlyA"], oneAny)
		}
		if got["onlyB"] != twoAny {
			t.Errorf("got[\"onlyB\"] = %p, want %p", got["onlyB"], twoAny)
		}

		if len(a) != 2 {
			t.Errorf("len(a) = %d, want 2", len(a))
		}
		if a["shared"] != oneAny {
			t.Errorf("a[\"shared\"] = %p, want %p", a["shared"], oneAny)
		}
		if len(b) != 2 {
			t.Errorf("len(b) = %d, want 2", len(b))
		}

		got["new"] = twoAny
		if _, ok := a["new"]; ok {
			t.Error("writing to result mutated input map a")
		}
	})

	t.Run("nil inputs", func(t *testing.T) {
		bothNil := Combine(nil, nil)
		if bothNil == nil {
			t.Fatal("Combine(nil, nil) = nil, want non-nil empty map")
		}
		if len(bothNil) != 0 {
			t.Errorf("len(Combine(nil, nil)) = %d, want 0", len(bothNil))
		}

		nilWithMap := Combine(nil, map[string]*anypb.Any{"onlyB": twoAny})
		if len(nilWithMap) != 1 {
			t.Errorf("len(Combine(nil, oneEntry)) = %d, want 1", len(nilWithMap))
		}
		if nilWithMap["onlyB"] != twoAny {
			t.Errorf("nilWithMap[\"onlyB\"] = %p, want %p", nilWithMap["onlyB"], twoAny)
		}

		mapWithNil := Combine(map[string]*anypb.Any{"onlyA": oneAny}, nil)
		if len(mapWithNil) != 1 {
			t.Errorf("len(Combine(oneEntry, nil)) = %d, want 1", len(mapWithNil))
		}
		if mapWithNil["onlyA"] != oneAny {
			t.Errorf("mapWithNil[\"onlyA\"] = %p, want %p", mapWithNil["onlyA"], oneAny)
		}
	})
}
