// Copyright 2019 Google LLC
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

// Package extensions contains golang implementations for common convenience
// utility functionality for working with maps of anypb (map[string]*anypb.Any).
// Open Match uses protocol buffer messages as its IDL (https://protobuf.dev/).
// Nearly every message type that is not a simple request/response wrapper has an
// 'extensions' field which is a map of anypb, this package simplifies working
// with those in golang.
package extensions

import (
	"errors"

	"github.com/googleforgames/open-match2/v2/pkg/pb"
	"google.golang.org/protobuf/types/known/anypb"
	knownpb "google.golang.org/protobuf/types/known/wrapperspb"
)

var (
	NoSuchKeyError = errors.New("Extension field contains no such key")
)

// Combine concatinates two map[string]*anypb.Any maps. If the two maps contain
// values at the same key, the value in map 'b' takes precedent.
func Combine(a map[string]*anypb.Any, b map[string]*anypb.Any) map[string]*anypb.Any {
	for k, v := range b {
		a[k] = v
	}
	return a
}

// String() is a simple helper function to retrieve the string stored in the
// extensions field at the provided key, and return it as a standard golang string value.
func String(ex map[string]*anypb.Any, exKey string) (out string, err error) {
	stringPb := &knownpb.StringValue{}
	if exValue, ok := ex[exKey]; ok {
		err = exValue.UnmarshalTo(stringPb)
		if err != nil {
			return "", err
		}
		return stringPb.Value, err
	}
	return "", NoSuchKeyError
}

// Bool() is a simple helper function to retrieve the bool stored in the
// extensions field at the provided key, and return it as a standard golang bool value.
func Bool(ex map[string]*anypb.Any, exKey string) (out bool, err error) {
	boolPb := &knownpb.BoolValue{}
	if exValue, ok := ex[exKey]; ok {
		err = exValue.UnmarshalTo(boolPb)
		if err != nil {
			return false, err
		}
		return boolPb.Value, err
	}
	return false, NoSuchKeyError
}

// Int32() is a simple helper function to retrieve the int32 stored in the
// extensions field at the provided key, and return it as a standard golang int value.
func Int32(ex map[string]*anypb.Any, exKey string) (out int, err error) {
	int32Pb := &knownpb.Int32Value{}
	if exValue, ok := ex[exKey]; ok {
		err = exValue.UnmarshalTo(int32Pb)
		if err != nil {
			return 0, err
		}
		return int(int32Pb.Value), err
	}
	return 0, NoSuchKeyError
}

// MMFRequests is a simple helper function to extract an array of matchmaking
// requests from the extensions field of an OM2 protobuf message. If no MmfRequest
// exists at the given key, it will return a nil value and extensions.NoSuchKeyError.
func MMFRequest(ex map[string]*anypb.Any, exKey string) (*pb.MmfRequest, error) {
	var err error
	out := &pb.MmfRequest{}

	// Get the extension value that holds the mmf requests.
	if exValue, ok := ex[exKey]; ok {
		err = exValue.UnmarshalTo(out)
		if err != nil {
			return nil, err
		}
		return out, err
	}
	return nil, NoSuchKeyError
}

// AnypbIntMap() takes a golang map[string]int32 and creates an identical map
// of 'any' protobuf messages, suitable to send in the extensions field of a
// Open Match protobuf message.
func AnypbIntMap(in map[string]int32) map[string]*anypb.Any {
	out := make(map[string]*anypb.Any)
	for key, value := range in {
		anyValue, err := anypb.New(&knownpb.Int32Value{Value: value})
		if err != nil {
			panic(err)
		}
		out[key] = anyValue
	}
	return out
}
