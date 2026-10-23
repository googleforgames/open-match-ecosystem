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

package gsdirector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	otelmetrics "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	knownpb "google.golang.org/protobuf/types/known/wrapperspb"

	"open-match.dev/open-match-ecosystem/v2/internal/assignmentdistributor"
	ex "open-match.dev/open-match-ecosystem/v2/internal/extensions"
	"open-match.dev/open-match-ecosystem/v2/internal/omclient"
)

func newTestLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

func newTestMeter(t *testing.T) (*otelmetrics.Meter, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})
	meter := provider.Meter("gsdirector.test")
	return &meter, reader
}

func mustAnyString(t *testing.T, v string) *anypb.Any {
	t.Helper()
	a, err := anypb.New(&knownpb.StringValue{Value: v})
	if err != nil {
		t.Fatalf("anypb.New(StringValue(%q)): %v", v, err)
	}
	return a
}

func mustAnyBool(t *testing.T, v bool) *anypb.Any {
	t.Helper()
	a, err := anypb.New(&knownpb.BoolValue{Value: v})
	if err != nil {
		t.Fatalf("anypb.New(BoolValue(%v)): %v", v, err)
	}
	return a
}

func writeStreamedMatchResponse(t *testing.T, w http.ResponseWriter, matches ...*pb.Match) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	for _, m := range matches {
		respBytes, err := protojson.Marshal(&pb.StreamedMmfResponse{Match: m})
		if err != nil {
			t.Fatalf("protojson.Marshal(StreamedMmfResponse): %v", err)
		}
		envelope, err := json.Marshal(map[string]json.RawMessage{
			"result": respBytes,
		})
		if err != nil {
			t.Fatalf("json.Marshal(envelope): %v", err)
		}
		_, _ = w.Write(append(envelope, '\n'))
	}
}

type staticGSManager struct {
	mu               sync.Mutex
	requests         []*pb.MmfRequest
	getCalls         int
	allocateCalls    int
	updateCalls      int
	lastUpdatedParam *pb.MmfRequest
	connString       string
}

func (s *staticGSManager) Init(map[string]map[string]string, map[string]*pb.Pool, map[string][]*GameMode) error {
	return nil
}

func (s *staticGSManager) Allocate(spec string, match *pb.Match) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allocateCalls++
	if s.connString != "" {
		return s.connString
	}
	return "127.0.0.1:7777"
}

func (s *staticGSManager) GetMMFParams() []*pb.MmfRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCalls++
	return slices.Clone(s.requests)
}

func (s *staticGSManager) UpdateMMFRequest(spec string, mmfParam *pb.MmfRequest) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateCalls++
	s.lastUpdatedParam = mmfParam
	return spec + "-updated"
}

type failingSender struct {
	onSend func() error
}

func (f *failingSender) Send(ctx context.Context, roster *pb.Roster) error {
	return f.onSend()
}

func (f *failingSender) Stop() {}

// 1. Missing MMFRequestHash and Consecutive Empty Cycles Exit
func TestRun_MissingMMFRequestHashAndConsecutiveEmptyCyclesExit(t *testing.T) {
	var fetchCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/matches:fetch":
			fetchCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
		case "/tickets:activate":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	validExt := map[string]*anypb.Any{
		ex.MMFRequestHashKey:  mustAnyString(t, "valid-hash-1"),
		ex.AgonesAllocatorKey: mustAnyString(t, `{"spec":"fleet-1"}`),
	}
	gsMgr := &staticGSManager{
		requests: []*pb.MmfRequest{
			{
				// nil Profile
				Mmfs:    []*pb.MatchmakingFunctionSpec{Fifo},
				Profile: nil,
			},
			{
				// Missing MMFRequestHashKey
				Mmfs: []*pb.MatchmakingFunctionSpec{Fifo},
				Profile: &pb.Profile{
					Name:       "dev.open-match.nohash.1",
					Extensions: map[string]*anypb.Any{},
				},
			},
			{
				// Valid request
				Mmfs: []*pb.MatchmakingFunctionSpec{Fifo},
				Profile: &pb.Profile{
					Name:       "dev.open-match.valid.1",
					Extensions: validExt,
				},
			},
		},
	}

	cfg := viper.New()
	cfg.Set("OM_CORE_ADDR", srv.URL)
	cfg.Set("NUM_MM_CYCLES", 50)
	cfg.Set("NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT", 3)
	cfg.Set("MM_CYCLE_MIN_DURATION_MS", 1)
	cfg.Set("ASSIGNMENT_TTL_MS", 1000)

	log := newTestLogger()
	assignChan := make(chan *pb.Roster, 10)
	d := &MockDirector{
		OmClient:            omclient.New(cfg, log, srv.Client()),
		Cfg:                 cfg,
		Log:                 log,
		AssignmentPublisher: assignmentdistributor.NewChannelSender(assignChan),
		GSManager:           gsMgr,
	}

	done := make(chan struct{})
	go func() {
		d.Run(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run deadlocked or did not exit after consecutive empty cycles")
	}

	gsMgr.mu.Lock()
	cycles := gsMgr.getCalls
	gsMgr.mu.Unlock()
	if cycles != 3 {
		t.Fatalf("expected 3 cycles before early exit, got %d", cycles)
	}
	if got := fetchCalls.Load(); got != 3 {
		t.Fatalf("expected 3 valid /matches:fetch calls (1 per cycle), got %d", got)
	}
}

// 2. Deactivation Timeout Extension Helper
func TestHasDeactivationTimeout(t *testing.T) {
	tests := []struct {
		name    string
		match   *pb.Match
		want    bool
		wantErr bool
	}{
		{
			name:  "nil match",
			match: nil,
			want:  false,
		},
		{
			name:  "nil extensions",
			match: &pb.Match{Id: "m-nil-ext"},
			want:  false,
		},
		{
			name: "absent key",
			match: &pb.Match{
				Id:         "m-absent",
				Extensions: map[string]*anypb.Any{"other": mustAnyString(t, "val")},
			},
			want: false,
		},
		{
			name: "boolean true",
			match: &pb.Match{
				Id: "m-true",
				Extensions: map[string]*anypb.Any{
					ex.MatchDeactivationTimeoutKey: mustAnyBool(t, true),
				},
			},
			want: true,
		},
		{
			name: "boolean false",
			match: &pb.Match{
				Id: "m-false",
				Extensions: map[string]*anypb.Any{
					ex.MatchDeactivationTimeoutKey: mustAnyBool(t, false),
				},
			},
			want: false,
		},
		{
			name: "wrong anypb payload type",
			match: &pb.Match{
				Id: "m-wrong-type",
				Extensions: map[string]*anypb.Any{
					ex.MatchDeactivationTimeoutKey: mustAnyString(t, "not-a-bool"),
				},
			},
			want:    false,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hasDeactivationTimeout(tc.match)
			if (err != nil) != tc.wantErr {
				t.Fatalf("hasDeactivationTimeout() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("hasDeactivationTimeout() = %v, want %v", got, tc.want)
			}
		})
	}
}

// 3. Match Ticket Counter
func TestCountMatchTickets(t *testing.T) {
	tests := []struct {
		name  string
		match *pb.Match
		want  int64
	}{
		{
			name:  "nil match",
			match: nil,
			want:  0,
		},
		{
			name:  "empty rosters",
			match: &pb.Match{Id: "m-empty", Rosters: map[string]*pb.Roster{}},
			want:  0,
		},
		{
			name: "multiple rosters",
			match: &pb.Match{
				Id: "m-multi",
				Rosters: map[string]*pb.Roster{
					"r1": {Tickets: []*pb.Ticket{{Id: "t1"}, {Id: "t2"}}},
					"r2": {Tickets: []*pb.Ticket{{Id: "t3"}, {Id: "t4"}, {Id: "t5"}}},
					"r3": {Tickets: nil},
				},
			},
			want: 5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := countMatchTickets(tc.match); got != tc.want {
				t.Fatalf("countMatchTickets() = %d, want %d", got, tc.want)
			}
		})
	}
}

// 4. Cycle Backoff Factor Calculation
func TestComputeCycleBackoffFactor(t *testing.T) {
	tests := []struct {
		name        string
		numProposed int64
		numAssigned int64
		numDeact    int64
		want        float64
	}{
		{
			name:        "zero proposed returns 1.0",
			numProposed: 0,
			numAssigned: 0,
			numDeact:    0,
			want:        1.0,
		},
		{
			name:        "zero proposed with non-zero counters returns 1.0",
			numProposed: 0,
			numAssigned: 3,
			numDeact:    2,
			want:        1.0,
		},
		{
			name:        "negative proposed returns 1.0",
			numProposed: -5,
			numAssigned: 2,
			numDeact:    1,
			want:        1.0,
		},
		{
			name:        "assigned greater than deactivation timeout",
			numProposed: 10,
			numAssigned: 4,
			numDeact:    1,
			want:        1.4,
		},
		{
			name:        "deactivation timeout greater than assigned",
			numProposed: 10,
			numAssigned: 2,
			numDeact:    7,
			want:        1.7,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeCycleBackoffFactor(tc.numProposed, tc.numAssigned, tc.numDeact)
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("computeCycleBackoffFactor() returned %v (must not be NaN/Inf)", got)
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("computeCycleBackoffFactor() = %v, want %v", got, tc.want)
			}
		})
	}
}

// 5. GameMode.SetBackfillMMFs
func TestGameMode_SetBackfillMMFs(t *testing.T) {
	gm := &GameMode{
		Name: "CustomSoloDuel",
	}
	runtimeMMF := &pb.MatchmakingFunctionSpec{
		Name: "FIFO_CLOUD_RUN",
		Host: "https://mmf-fifo-xyz.a.run.app",
		Port: 443,
		Type: pb.MatchmakingFunctionSpec_GRPC,
	}

	if err := gm.SetBackfillMMFs(runtimeMMF); err != nil {
		t.Fatalf("SetBackfillMMFs() error: %v", err)
	}
	if gm.ExtensionParams == nil {
		t.Fatal("ExtensionParams was not initialized")
	}

	decodedReq, err := ex.MMFRequest(gm.ExtensionParams, ex.MMFRequestKey)
	if err != nil {
		t.Fatalf("ex.MMFRequest() error: %v", err)
	}
	if len(decodedReq.GetMmfs()) != 1 {
		t.Fatalf("expected 1 backfill MMF, got %d", len(decodedReq.GetMmfs()))
	}
	gotMMF := decodedReq.GetMmfs()[0]
	if gotMMF.GetName() != runtimeMMF.Name || gotMMF.GetHost() != runtimeMMF.Host || gotMMF.GetPort() != runtimeMMF.Port {
		t.Fatalf("got MMF %+v, want %+v", gotMMF, runtimeMMF)
	}
}

// 6. Multi-Fleet Extensions and Pool Slice Isolation
func TestMockAgonesIntegration_MultiFleetIsolation(t *testing.T) {
	sharedPoolExtKey := "poolSharedKey"
	sharedModeExtKey := "modeSharedKey"

	sharedTemplatePool := &pb.Pool{
		Name: "shared_pool",
		TagPresentFilters: []*pb.Pool_TagPresentFilter{
			{Tag: "base-tag"},
		},
		DoubleRangeFilters: []*pb.Pool_DoubleRangeFilter{
			{DoubleArg: "skill", Minimum: 0, Maximum: 100},
		},
		StringEqualsFilters: []*pb.Pool_StringEqualsFilter{
			{StringArg: "mode", Value: "ranked"},
		},
		Extensions: map[string]*anypb.Any{
			sharedPoolExtKey: mustAnyString(t, "basePoolVal"),
		},
	}

	sharedGameMode := &GameMode{
		Name:  "SharedMode",
		MMFs:  []*pb.MatchmakingFunctionSpec{Fifo},
		Pools: map[string]*pb.Pool{"main": sharedTemplatePool},
		ExtensionParams: map[string]*anypb.Any{
			sharedModeExtKey: mustAnyString(t, "baseModeVal"),
		},
	}

	multiFleetConfig := map[string]map[string]string{
		"APAC": {
			"JP_Tokyo": "asia-northeast1-a",
		},
		"NA": {
			"US_Ashburn": "us-east4-b",
		},
	}
	multiZonePools := map[string]*pb.Pool{
		"asia-northeast1-a": {
			DoubleRangeFilters: []*pb.Pool_DoubleRangeFilter{
				{DoubleArg: "ping.asia-northeast1-a", Minimum: 1, Maximum: 120},
			},
			Extensions: map[string]*anypb.Any{
				"zoneKey": mustAnyString(t, "asia-northeast1-a"),
			},
		},
		"us-east4-b": {
			DoubleRangeFilters: []*pb.Pool_DoubleRangeFilter{
				{DoubleArg: "ping.us-east4-b", Minimum: 1, Maximum: 120},
			},
			Extensions: map[string]*anypb.Any{
				"zoneKey": mustAnyString(t, "us-east4-b"),
			},
		},
	}
	multiModesInZone := map[string][]*GameMode{
		"asia-northeast1-a": {sharedGameMode},
		"us-east4-b":        {sharedGameMode},
	}

	agones := &MockAgonesIntegration{Log: newTestLogger()}
	if err := agones.Init(multiFleetConfig, multiZonePools, multiModesInZone); err != nil {
		t.Fatalf("agones.Init() failed: %v", err)
	}

	params := agones.GetMMFParams()
	if len(params) != 2 {
		t.Fatalf("expected 2 fleet MmfRequests, got %d", len(params))
	}

	req0 := params[0]
	req1 := params[1]

	hash0, err := ex.String(req0.GetProfile().GetExtensions(), ex.MMFRequestHashKey)
	if err != nil || hash0 == "" {
		t.Fatalf("req0 missing MMFRequestHashKey: %v", err)
	}
	hash1, err := ex.String(req1.GetProfile().GetExtensions(), ex.MMFRequestHashKey)
	if err != nil || hash1 == "" {
		t.Fatalf("req1 missing MMFRequestHashKey: %v", err)
	}
	if hash0 == hash1 {
		t.Fatalf("expected distinct MMFRequestHashKey across fleets, both got %q", hash0)
	}

	alloc0, err := ex.String(req0.GetProfile().GetExtensions(), ex.AgonesAllocatorKey)
	if err != nil || alloc0 == "" {
		t.Fatalf("req0 missing AgonesAllocatorKey: %v", err)
	}
	alloc1, err := ex.String(req1.GetProfile().GetExtensions(), ex.AgonesAllocatorKey)
	if err != nil || alloc1 == "" {
		t.Fatalf("req1 missing AgonesAllocatorKey: %v", err)
	}
	if alloc0 == alloc1 {
		t.Fatalf("expected distinct AgonesAllocatorKey across fleets, both got %q", alloc0)
	}

	// Verify sharedGameMode.ExtensionParams was not polluted with Agones keys.
	if _, polluted := sharedGameMode.ExtensionParams[ex.AgonesAllocatorKey]; polluted {
		t.Fatal("sharedGameMode.ExtensionParams was mutated with AgonesAllocatorKey")
	}

	// Verify pool extensions and filter slices are isolated across fleets and template pool.
	if _, polluted := sharedTemplatePool.Extensions["zoneKey"]; polluted {
		t.Fatal("sharedTemplatePool.Extensions was mutated with zoneKey")
	}
	if len(sharedTemplatePool.DoubleRangeFilters) != 1 {
		t.Fatalf("sharedTemplatePool.DoubleRangeFilters length mutated to %d", len(sharedTemplatePool.DoubleRangeFilters))
	}

	for _, p := range req0.GetProfile().GetPools() {
		p.TagPresentFilters[0] = &pb.Pool_TagPresentFilter{Tag: "mutated-tag"}
		p.DoubleRangeFilters[0] = &pb.Pool_DoubleRangeFilter{DoubleArg: "mutated"}
		p.StringEqualsFilters[0] = &pb.Pool_StringEqualsFilter{StringArg: "mutated"}
		p.Extensions["mutatedExt"] = mustAnyString(t, "mutated")
	}
	for _, p := range req1.GetProfile().GetPools() {
		if p.TagPresentFilters[0].GetTag() == "mutated-tag" {
			t.Fatal("req1 TagPresentFilters shares backing array with req0")
		}
		if p.DoubleRangeFilters[0].GetDoubleArg() == "mutated" {
			t.Fatal("req1 DoubleRangeFilters shares backing array with req0")
		}
		if p.StringEqualsFilters[0].GetStringArg() == "mutated" {
			t.Fatal("req1 StringEqualsFilters shares backing array with req0")
		}
		if _, exists := p.Extensions["mutatedExt"]; exists {
			t.Fatal("req1 Pool.Extensions shares map with req0")
		}
	}

	// Verify both fleets can allocate independently.
	if conn := agones.Allocate(alloc0, &pb.Match{Id: "match-fleet-0"}); conn == "" {
		t.Fatal("Allocate failed for fleet 0")
	}
	if conn := agones.Allocate(alloc1, &pb.Match{Id: "match-fleet-1"}); conn == "" {
		t.Fatal("Allocate failed for fleet 1")
	}
}

// 7. Dynamic Refresh of EveryTicket.CreationTimeRangeFilter
func TestMockAgonesIntegration_DynamicRefreshEveryTicketCreationTimeRangeFilter(t *testing.T) {
	staleStart := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	staleEnd := time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)
	EveryTicket.CreationTimeRangeFilter = &pb.Pool_CreationTimeRangeFilter{
		Start: timestamppb.New(staleStart),
		End:   timestamppb.New(staleEnd),
	}

	agones := &MockAgonesIntegration{Log: newTestLogger()}
	beforeInit := time.Now()
	if err := agones.Init(FleetConfig, ZonePools, GameModesInZone); err != nil {
		t.Fatalf("agones.Init() failed: %v", err)
	}
	afterInit := time.Now()

	gotStart := EveryTicket.GetCreationTimeRangeFilter().GetStart().AsTime()
	gotEnd := EveryTicket.GetCreationTimeRangeFilter().GetEnd().AsTime()

	wantStartMin := beforeInit.Add(-1*time.Hour - 2*time.Second)
	wantStartMax := afterInit.Add(-1*time.Hour + 2*time.Second)
	if gotStart.Before(wantStartMin) || gotStart.After(wantStartMax) {
		t.Fatalf("EveryTicket Start = %v, want between %v and %v", gotStart, wantStartMin, wantStartMax)
	}

	wantEndMin := beforeInit.Add(100*time.Hour - 2*time.Second)
	wantEndMax := afterInit.Add(100*time.Hour + 2*time.Second)
	if gotEnd.Before(wantEndMin) || gotEnd.After(wantEndMax) {
		t.Fatalf("EveryTicket End = %v, want between %v and %v", gotEnd, wantEndMin, wantEndMax)
	}
}

// 8. ProposalsLoop Missing vs. Present Backfill Extension
func TestProposalsLoop_MissingVsPresentBackfillExtension(t *testing.T) {
	rand.Seed(1)

	agones := &MockAgonesIntegration{Log: newTestLogger()}
	if err := agones.Init(FleetConfig, ZonePools, GameModesInZone); err != nil {
		t.Fatalf("agones.Init() failed: %v", err)
	}

	backfillReq := &pb.MmfRequest{
		Mmfs: []*pb.MatchmakingFunctionSpec{Fifo},
		Profile: &pb.Profile{
			Name: "backfill.partial-match-1",
		},
	}
	backfillAny, err := anypb.New(backfillReq)
	if err != nil {
		t.Fatalf("anypb.New(backfillReq): %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/matches:fetch":
			body, _ := io.ReadAll(r.Body)
			var req pb.MmfRequest
			_ = protojson.Unmarshal(body, &req)
			hash, _ := ex.String(req.GetProfile().GetExtensions(), ex.MMFRequestHashKey)

			matchNoBackfill := &pb.Match{
				Id: "match-no-backfill",
				Rosters: map[string]*pb.Roster{
					"r1": {Tickets: []*pb.Ticket{{Id: "t-full-1"}, {Id: "t-full-2"}}},
				},
				Extensions: map[string]*anypb.Any{
					ex.MMFRequestHashKey: mustAnyString(t, hash),
				},
			}
			matchWithBackfill := &pb.Match{
				Id: "match-with-backfill",
				Rosters: map[string]*pb.Roster{
					"r1": {Tickets: []*pb.Ticket{{Id: "t-part-1"}, {Id: "t-part-2"}}},
				},
				Extensions: map[string]*anypb.Any{
					ex.MMFRequestHashKey: mustAnyString(t, hash),
					ex.MMFRequestKey:     backfillAny,
				},
			}
			writeStreamedMatchResponse(t, w, matchNoBackfill, matchWithBackfill)
		case "/tickets:activate":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := viper.New()
	cfg.Set("OM_CORE_ADDR", srv.URL)
	cfg.Set("NUM_MM_CYCLES", 1)
	cfg.Set("MM_CYCLE_MIN_DURATION_MS", 1)
	cfg.Set("ASSIGNMENT_TTL_MS", 60000)

	log := newTestLogger()
	assignChan := make(chan *pb.Roster, 10)
	d := &MockDirector{
		OmClient:            omclient.New(cfg, log, srv.Client()),
		Cfg:                 cfg,
		Log:                 log,
		AssignmentPublisher: assignmentdistributor.NewChannelSender(assignChan),
		GSManager:           agones,
	}

	d.Run(context.Background())

	if len(assignChan) != 2 {
		t.Fatalf("expected 2 rosters assigned, got %d", len(assignChan))
	}

	// Verify MockAgonesIntegration preserved the backfill MMF request after Allocate.
	nextCycleParams := agones.GetMMFParams()
	foundBackfill := false
	for _, p := range nextCycleParams {
		if p.GetProfile().GetName() == "backfill.partial-match-1" {
			foundBackfill = true
			break
		}
	}
	if !foundBackfill {
		t.Fatalf("expected backfill.partial-match-1 in next cycle GetMMFParams(), got %+v", nextCycleParams)
	}
}

// 9. Inter-Cycle Sleep Cancellation and Re-Activation Flush
func TestRun_InterCycleSleepCancellationAndReactivationFlush(t *testing.T) {
	t.Run("cancel interrupts 30s inter-cycle sleep immediately", func(t *testing.T) {
		cycleFetched := make(chan struct{})
		var fetchOnce sync.Once
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/matches:fetch":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fetchOnce.Do(func() {
					close(cycleFetched)
				})
			case "/tickets:activate":
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		agones := &MockAgonesIntegration{Log: newTestLogger()}
		_ = agones.Init(FleetConfig, ZonePools, GameModesInZone)

		cfg := viper.New()
		cfg.Set("OM_CORE_ADDR", srv.URL)
		cfg.Set("NUM_MM_CYCLES", 2)
		cfg.Set("MM_CYCLE_MIN_DURATION_MS", 30000)
		cfg.Set("ASSIGNMENT_TTL_MS", 60000)

		log := newTestLogger()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		d := &MockDirector{
			OmClient:            omclient.New(cfg, log, srv.Client()),
			Cfg:                 cfg,
			Log:                 log,
			AssignmentPublisher: assignmentdistributor.NewChannelSender(make(chan *pb.Roster, 10)),
			GSManager:           agones,
		}

		runDone := make(chan struct{})
		go func() {
			d.Run(ctx)
			close(runDone)
		}()

		select {
		case <-cycleFetched:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for first cycle fetch")
		}

		// Give the cycle a few milliseconds to enter the 30s sleep timer.
		time.Sleep(30 * time.Millisecond)
		cancelStart := time.Now()
		cancel()

		select {
		case <-runDone:
			if elapsed := time.Since(cancelStart); elapsed > 500*time.Millisecond {
				t.Fatalf("Run took %v to exit after cancel, expected < 500ms", elapsed)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not exit promptly when context was cancelled during inter-cycle sleep")
		}
	})

	t.Run("normal exit closes ticketIdsToActivate and flushes rejected tickets", func(t *testing.T) {
		var activatedMu sync.Mutex
		var reactivatedTickets []string

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/matches:fetch":
				body, _ := io.ReadAll(r.Body)
				var req pb.MmfRequest
				_ = protojson.Unmarshal(body, &req)
				hash, _ := ex.String(req.GetProfile().GetExtensions(), ex.MMFRequestHashKey)

				timedOutMatch := &pb.Match{
					Id: "m-timed-out",
					Rosters: map[string]*pb.Roster{
						"r1": {Tickets: []*pb.Ticket{{Id: "flush-t1"}, {Id: "flush-t2"}}},
					},
					Extensions: map[string]*anypb.Any{
						ex.MMFRequestHashKey:           mustAnyString(t, hash),
						ex.MatchDeactivationTimeoutKey: mustAnyBool(t, true),
					},
				}
				writeStreamedMatchResponse(t, w, timedOutMatch)
			case "/tickets:activate":
				body, _ := io.ReadAll(r.Body)
				var actReq pb.ActivateTicketsRequest
				_ = protojson.Unmarshal(body, &actReq)
				activatedMu.Lock()
				reactivatedTickets = append(reactivatedTickets, actReq.GetTicketIds()...)
				activatedMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		agones := &MockAgonesIntegration{Log: newTestLogger()}
		_ = agones.Init(FleetConfig, ZonePools, GameModesInZone)

		cfg := viper.New()
		cfg.Set("OM_CORE_ADDR", srv.URL)
		cfg.Set("NUM_MM_CYCLES", 1)
		cfg.Set("MM_CYCLE_MIN_DURATION_MS", 1)
		cfg.Set("REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT", true)
		cfg.Set("ASSIGNMENT_TTL_MS", 60000)

		log := newTestLogger()
		d := &MockDirector{
			OmClient:            omclient.New(cfg, log, srv.Client()),
			Cfg:                 cfg,
			Log:                 log,
			AssignmentPublisher: assignmentdistributor.NewChannelSender(make(chan *pb.Roster, 10)),
			GSManager:           agones,
		}

		d.Run(context.Background())

		activatedMu.Lock()
		got := slices.Clone(reactivatedTickets)
		activatedMu.Unlock()

		slices.Sort(got)
		want := []string{"flush-t1", "flush-t2"}
		if !slices.Equal(got, want) {
			t.Fatalf("reactivated tickets flushed before Run exit = %v, want %v", got, want)
		}
	})
}

// 10. Concurrency-Safe Status Summary
func TestDirector_ConcurrencySafeStatusSummary(t *testing.T) {
	rand.Seed(1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/matches:fetch":
			body, _ := io.ReadAll(r.Body)
			var req pb.MmfRequest
			_ = protojson.Unmarshal(body, &req)
			hash, _ := ex.String(req.GetProfile().GetExtensions(), ex.MMFRequestHashKey)

			m := &pb.Match{
				Id: "status-match",
				Rosters: map[string]*pb.Roster{
					"r1": {Tickets: []*pb.Ticket{{Id: fmt.Sprintf("t-%d", time.Now().UnixNano())}}},
				},
				Extensions: map[string]*anypb.Any{
					ex.MMFRequestHashKey:           mustAnyString(t, hash),
					ex.MatchDeactivationTimeoutKey: mustAnyBool(t, true),
				},
			}
			writeStreamedMatchResponse(t, w, m)
		case "/tickets:activate":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	agones := &MockAgonesIntegration{Log: newTestLogger()}
	_ = agones.Init(FleetConfig, ZonePools, GameModesInZone)

	cfg := viper.New()
	cfg.Set("OM_CORE_ADDR", srv.URL)
	cfg.Set("NUM_MM_CYCLES", 3)
	cfg.Set("MM_CYCLE_MIN_DURATION_MS", 2)
	cfg.Set("REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT", true)
	cfg.Set("ASSIGNMENT_TTL_MS", 60000)

	log := newTestLogger()
	d := &MockDirector{
		OmClient:            omclient.New(cfg, log, srv.Client()),
		Cfg:                 cfg,
		Log:                 log,
		AssignmentPublisher: assignmentdistributor.NewChannelSender(make(chan *pb.Roster, 10)),
		GSManager:           agones,
	}

	stopReaders := make(chan struct{})
	var readerWg sync.WaitGroup
	readerWg.Add(2)
	for range 2 {
		go func() {
			defer readerWg.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
					_ = d.Status()
				}
			}
		}()
	}

	d.Run(context.Background())
	close(stopReaders)
	readerWg.Wait()

	status := d.Status()
	expectedSubstrings := []string{
		"matches accepted",
		"deactivation_timeout)",
		"tickets accepted",
		"Cycle took ",
		"minimum cycle duration calculate as ",
		"% of minimum cycle duration used",
	}
	for _, sub := range expectedSubstrings {
		if !strings.Contains(status, sub) {
			t.Fatalf("Status() = %q, missing expected substring %q", status, sub)
		}
	}
}

// 11. Director Metrics Names, Descriptions, and Recording
func TestDirector_MetricsNamesDescriptionsAndRecording(t *testing.T) {
	rand.Seed(1)

	meterPtr, reader := newTestMeter(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/matches:fetch":
			body, _ := io.ReadAll(r.Body)
			var req pb.MmfRequest
			_ = protojson.Unmarshal(body, &req)
			hash, _ := ex.String(req.GetProfile().GetExtensions(), ex.MMFRequestHashKey)

			m := &pb.Match{
				Id: "metrics-match-1",
				Rosters: map[string]*pb.Roster{
					"r1": {Tickets: []*pb.Ticket{{Id: "bad-ticket-1"}, {Id: "bad-ticket-2"}}},
				},
				Extensions: map[string]*anypb.Any{
					ex.MMFRequestHashKey:           mustAnyString(t, hash),
					ex.MatchDeactivationTimeoutKey: mustAnyBool(t, true),
				},
			}
			writeStreamedMatchResponse(t, w, m)
		case "/tickets:activate":
			// Return HTTP 400 Bad Request so reactivation permanently fails for the 2 tickets.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":3,"message":"invalid ticket"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	agones := &MockAgonesIntegration{Log: newTestLogger()}
	_ = agones.Init(FleetConfig, ZonePools, GameModesInZone)

	cfg := viper.New()
	cfg.Set("OM_CORE_ADDR", srv.URL)
	cfg.Set("NUM_MM_CYCLES", 1)
	cfg.Set("MM_CYCLE_MIN_DURATION_MS", 5)
	cfg.Set("REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT", true)
	cfg.Set("ASSIGNMENT_TTL_MS", 60000)

	log := newTestLogger()
	d := &MockDirector{
		OmClient:            omclient.New(cfg, log, srv.Client()),
		Cfg:                 cfg,
		Log:                 log,
		AssignmentPublisher: assignmentdistributor.NewChannelSender(make(chan *pb.Roster, 10)),
		GSManager:           agones,
		OtelMeterPtr:        meterPtr,
	}

	d.Run(context.Background())
	// Also record a response failure on otelMMFResponseFailures so it appears in collected metrics.
	otelMMFResponseFailures.Add(context.Background(), 1)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect() error: %v", err)
	}

	metricsByName := make(map[string]metricdata.Metrics)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			metricsByName[m.Name] = m
		}
	}

	if _, hasMisspelled := metricsByName["gsdirector.mmf.reponse.failures"]; hasMisspelled {
		t.Fatal("found misspelled metric gsdirector.mmf.reponse.failures")
	}

	wantDescriptions := map[string]string{
		"gsdirector.mmf.response.failures":                       "Total MMF response stream errors",
		"gsdirector.ticket.reactivations.failures":               "Total number of ticket re-activations that permanently failed",
		"gsdirector.matchmaking_cycle.match.deactivation_timeouts":  "Number of proposed matches returned by om-core per cycle with the deactivation_timeout extension set",
		"gsdirector.matchmaking_cycle.ticket.deactivation_timeouts": "Number of tickets in proposed matches returned by om-core per cycle with the deactivation_timeout extension set",
		"gsdirector.matchmaking_cycle.match.proposals":           "Number of match proposals received from Matchmaking Functions",
	}

	for name, wantDesc := range wantDescriptions {
		gotMetric, ok := metricsByName[name]
		if !ok {
			t.Fatalf("expected metric %q to be registered and collected", name)
		}
		if gotMetric.Description != wantDesc {
			t.Fatalf("metric %q description = %q, want %q", name, gotMetric.Description, wantDesc)
		}
	}

	// Verify counter values and histogram sums.
	reactFailSum := metricsByName["gsdirector.ticket.reactivations.failures"].Data.(metricdata.Sum[int64])
	if len(reactFailSum.DataPoints) == 0 || reactFailSum.DataPoints[0].Value != 2 {
		t.Fatalf("gsdirector.ticket.reactivations.failures = %+v, want 2", reactFailSum.DataPoints)
	}

	matchDeactHist := metricsByName["gsdirector.matchmaking_cycle.match.deactivation_timeouts"].Data.(metricdata.Histogram[int64])
	if len(matchDeactHist.DataPoints) == 0 || matchDeactHist.DataPoints[0].Sum != 1 {
		t.Fatalf("gsdirector.matchmaking_cycle.match.deactivation_timeouts sum = %+v, want 1", matchDeactHist.DataPoints)
	}

	ticketDeactHist := metricsByName["gsdirector.matchmaking_cycle.ticket.deactivation_timeouts"].Data.(metricdata.Histogram[int64])
	if len(ticketDeactHist.DataPoints) == 0 || ticketDeactHist.DataPoints[0].Sum != 2 {
		t.Fatalf("gsdirector.matchmaking_cycle.ticket.deactivation_timeouts sum = %+v, want 2", ticketDeactHist.DataPoints)
	}

	proposalsHist := metricsByName["gsdirector.matchmaking_cycle.match.proposals"].Data.(metricdata.Histogram[int64])
	if len(proposalsHist.DataPoints) == 0 || proposalsHist.DataPoints[0].Sum != 1 {
		t.Fatalf("gsdirector.matchmaking_cycle.match.proposals sum = %+v, want 1", proposalsHist.DataPoints)
	}
}

// 12. Backfill Priority, Same-Cycle vs. Cross-Cycle Collision Rejection, and Selective Re-Activation
func TestRun_BackfillPrioritySameCycleVsCrossCycleCollisionAndSelectiveReactivation(t *testing.T) {
	rand.Seed(1)

	meterPtr, reader := newTestMeter(t)

	var cycleNum atomic.Int32
	var activatedMu sync.Mutex
	var reactivatedByCycle [][]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/matches:fetch":
			body, _ := io.ReadAll(r.Body)
			var req pb.MmfRequest
			_ = protojson.Unmarshal(body, &req)
			hash, _ := ex.String(req.GetProfile().GetExtensions(), ex.MMFRequestHashKey)

			c := cycleNum.Add(1)
			if c == 1 {
				// Cycle 1: Standard fleet match comes FIRST in stream, backfill match comes SECOND,
				// and both share ticket "t-shared". Because backfill.* is prioritized first via sort.SliceStable,
				// the backfill match ("t-shared", "t-bf-only") must be accepted, and the standard fleet match
				// ("t-shared", "t-unclaimed-same-cycle") must be rejected as a same-cycle overlap (NOT cross-cycle assigned),
				// re-activating ONLY "t-unclaimed-same-cycle" and never "t-shared".
				stdMatch := &pb.Match{
					Id: "std-match-cycle-1",
					Rosters: map[string]*pb.Roster{
						"r1": {Tickets: []*pb.Ticket{{Id: "t-shared"}, {Id: "t-unclaimed-same-cycle"}}},
					},
					Extensions: map[string]*anypb.Any{
						ex.MMFRequestHashKey: mustAnyString(t, hash),
						"profileName":        mustAnyString(t, "dev.open-match.APAC.SoloDuel.1"),
					},
				}
				backfillMatch := &pb.Match{
					Id: "bf-match-cycle-1",
					Rosters: map[string]*pb.Roster{
						"r1": {Tickets: []*pb.Ticket{{Id: "t-shared"}, {Id: "t-bf-only"}}},
					},
					Extensions: map[string]*anypb.Any{
						ex.MMFRequestHashKey: mustAnyString(t, hash),
						"profileName":        mustAnyString(t, "backfill.session-1"),
					},
				}
				writeStreamedMatchResponse(t, w, stdMatch, backfillMatch)
			} else {
				// Cycle 2: Propose a match containing "t-shared" (assigned in Cycle 1) and "t-unclaimed-cross-cycle".
				// Must be rejected as cross-cycle assigned collision, incrementing assigned rejection metrics,
				// and re-activating ONLY "t-unclaimed-cross-cycle".
				crossCycleMatch := &pb.Match{
					Id: "std-match-cycle-2",
					Rosters: map[string]*pb.Roster{
						"r1": {Tickets: []*pb.Ticket{{Id: "t-shared"}, {Id: "t-unclaimed-cross-cycle"}}},
					},
					Extensions: map[string]*anypb.Any{
						ex.MMFRequestHashKey: mustAnyString(t, hash),
						"profileName":        mustAnyString(t, "dev.open-match.APAC.SoloDuel.2"),
					},
				}
				writeStreamedMatchResponse(t, w, crossCycleMatch)
			}
		case "/tickets:activate":
			body, _ := io.ReadAll(r.Body)
			var actReq pb.ActivateTicketsRequest
			_ = protojson.Unmarshal(body, &actReq)
			activatedMu.Lock()
			reactivatedByCycle = append(reactivatedByCycle, slices.Clone(actReq.GetTicketIds()))
			activatedMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	gsMgr := &staticGSManager{
		requests: []*pb.MmfRequest{
			{
				Mmfs: []*pb.MatchmakingFunctionSpec{Fifo},
				Profile: &pb.Profile{
					Name: "dev.open-match.APAC.SoloDuel.1",
					Extensions: map[string]*anypb.Any{
						ex.MMFRequestHashKey:  mustAnyString(t, "hash-fleet-1"),
						ex.AgonesAllocatorKey: mustAnyString(t, `{"spec":"fleet-1"}`),
					},
				},
			},
		},
	}

	cfg := viper.New()
	cfg.Set("OM_CORE_ADDR", srv.URL)
	cfg.Set("NUM_MM_CYCLES", 2)
	cfg.Set("MM_CYCLE_MIN_DURATION_MS", 300)
	cfg.Set("ASSIGNMENT_TTL_MS", 60000)

	log := newTestLogger()
	assignChan := make(chan *pb.Roster, 10)
	d := &MockDirector{
		OmClient:            omclient.New(cfg, log, srv.Client()),
		Cfg:                 cfg,
		Log:                 log,
		AssignmentPublisher: assignmentdistributor.NewChannelSender(assignChan),
		GSManager:           gsMgr,
		OtelMeterPtr:        meterPtr,
	}

	d.Run(context.Background())

	// Only the backfill match from Cycle 1 should have been assigned.
	if len(assignChan) != 1 {
		t.Fatalf("expected 1 assigned roster across both cycles, got %d", len(assignChan))
	}
	assignedRoster := <-assignChan
	var assignedIDs []string
	for _, tk := range assignedRoster.GetTickets() {
		assignedIDs = append(assignedIDs, tk.GetId())
	}
	wantAssigned := []string{"t-shared", "t-bf-only"}
	if !slices.Equal(assignedIDs, wantAssigned) {
		t.Fatalf("assigned ticket IDs = %v, want %v (backfill match should win priority)", assignedIDs, wantAssigned)
	}

	// Verify only unclaimed tickets were re-activated, never "t-shared" or "t-bf-only".
	activatedMu.Lock()
	var allReactivated []string
	for _, batch := range reactivatedByCycle {
		allReactivated = append(allReactivated, batch...)
	}
	activatedMu.Unlock()

	slices.Sort(allReactivated)
	wantReactivated := []string{"t-unclaimed-cross-cycle", "t-unclaimed-same-cycle"}
	if !slices.Equal(allReactivated, wantReactivated) {
		t.Fatalf("all reactivated tickets = %v, want %v", allReactivated, wantReactivated)
	}

	// Verify cross-cycle assigned rejection metrics recorded only Cycle 2's collision (sum = 1 match, 1 ticket).
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect() error: %v", err)
	}
	metricsByName := make(map[string]metricdata.Metrics)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			metricsByName[m.Name] = m
		}
	}

	matchAssignedRej := metricsByName["gsdirector.matchmaking_cycle.match.rejections.assigned"].Data.(metricdata.Histogram[int64])
	if len(matchAssignedRej.DataPoints) == 0 || matchAssignedRej.DataPoints[0].Sum != 1 {
		t.Fatalf("gsdirector.matchmaking_cycle.match.rejections.assigned sum = %+v, want 1 (same-cycle overlap must not increment assigned rejections)", matchAssignedRej.DataPoints)
	}

	ticketAssignedRej := metricsByName["gsdirector.ticket.rejections.assigned"].Data.(metricdata.Histogram[int64])
	if len(ticketAssignedRej.DataPoints) == 0 || ticketAssignedRej.DataPoints[0].Sum != 1 {
		t.Fatalf("gsdirector.ticket.rejections.assigned sum = %+v, want 1", ticketAssignedRej.DataPoints)
	}
}

// 13. Multi-Ticket Roster Assignment TTL Expiration
func TestRun_MultiTicketRosterAssignmentTTLExpiration(t *testing.T) {
	rand.Seed(1)

	var cycleNum atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/matches:fetch":
			body, _ := io.ReadAll(r.Body)
			var req pb.MmfRequest
			_ = protojson.Unmarshal(body, &req)
			hash, _ := ex.String(req.GetProfile().GetExtensions(), ex.MMFRequestHashKey)

			c := cycleNum.Add(1)
			switch c {
			case 1, 3:
				// In Cycle 1 and Cycle 3, propose the exact same 4-ticket roster.
				m := &pb.Match{
					Id: fmt.Sprintf("match-cycle-%d", c),
					Rosters: map[string]*pb.Roster{
						"r1": {
							Tickets: []*pb.Ticket{
								{Id: "ttl-t1"},
								{Id: "ttl-t2"},
								{Id: "ttl-t3"},
								{Id: "ttl-t4"},
							},
						},
					},
					Extensions: map[string]*anypb.Any{
						ex.MMFRequestHashKey: mustAnyString(t, hash),
					},
				}
				writeStreamedMatchResponse(t, w, m)
			case 2:
				// Wait for the 15ms assignment TTL to expire during Cycle 2 so Cycle 2 expires
				// all 4 tickets from assignments and compacts the maps via maps.Clone.
				time.Sleep(35 * time.Millisecond)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
			}
		case "/tickets:activate":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	gsMgr := &staticGSManager{
		requests: []*pb.MmfRequest{
			{
				Mmfs: []*pb.MatchmakingFunctionSpec{Fifo},
				Profile: &pb.Profile{
					Name: "dev.open-match.APAC.SoloDuel.1",
					Extensions: map[string]*anypb.Any{
						ex.MMFRequestHashKey:  mustAnyString(t, "hash-ttl"),
						ex.AgonesAllocatorKey: mustAnyString(t, `{"spec":"fleet-ttl"}`),
					},
				},
			},
		},
	}

	cfg := viper.New()
	cfg.Set("OM_CORE_ADDR", srv.URL)
	cfg.Set("NUM_MM_CYCLES", 3)
	cfg.Set("MM_CYCLE_MIN_DURATION_MS", 5)
	cfg.Set("ASSIGNMENT_TTL_MS", 15)

	log := newTestLogger()
	assignChan := make(chan *pb.Roster, 10)
	d := &MockDirector{
		OmClient:            omclient.New(cfg, log, srv.Client()),
		Cfg:                 cfg,
		Log:                 log,
		AssignmentPublisher: assignmentdistributor.NewChannelSender(assignChan),
		GSManager:           gsMgr,
	}

	d.Run(context.Background())

	// Both Cycle 1 and Cycle 3 matches (4 tickets each) must have been accepted because all 4 tickets
	// in the multi-ticket roster expired from assignments during Cycle 2.
	if len(assignChan) != 2 {
		t.Fatalf("expected 2 rosters assigned (Cycle 1 and Cycle 3 after TTL expiry), got %d", len(assignChan))
	}
}

// 14. AssignmentPublisher Backoff Context Cancellation
func TestRun_AssignmentPublisherBackoffContextCancellation(t *testing.T) {
	rand.Seed(1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/matches:fetch":
			body, _ := io.ReadAll(r.Body)
			var req pb.MmfRequest
			_ = protojson.Unmarshal(body, &req)
			hash, _ := ex.String(req.GetProfile().GetExtensions(), ex.MMFRequestHashKey)

			m := &pb.Match{
				Id: "backoff-cancel-match",
				Rosters: map[string]*pb.Roster{
					"r1": {Tickets: []*pb.Ticket{{Id: "bo-t1"}}},
				},
				Extensions: map[string]*anypb.Any{
					ex.MMFRequestHashKey: mustAnyString(t, hash),
				},
			}
			writeStreamedMatchResponse(t, w, m)
		case "/tickets:activate":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	gsMgr := &staticGSManager{
		requests: []*pb.MmfRequest{
			{
				Mmfs: []*pb.MatchmakingFunctionSpec{Fifo},
				Profile: &pb.Profile{
					Name: "dev.open-match.APAC.SoloDuel.1",
					Extensions: map[string]*anypb.Any{
						ex.MMFRequestHashKey:  mustAnyString(t, "hash-bo"),
						ex.AgonesAllocatorKey: mustAnyString(t, `{"spec":"fleet-bo"}`),
					},
				},
			},
		},
	}

	cfg := viper.New()
	cfg.Set("OM_CORE_ADDR", srv.URL)
	cfg.Set("NUM_MM_CYCLES", 1)
	cfg.Set("MM_CYCLE_MIN_DURATION_MS", 1)
	cfg.Set("ASSIGNMENT_TTL_MS", 60000)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sendAttempts atomic.Int32
	publisher := &failingSender{
		onSend: func() error {
			sendAttempts.Add(1)
			cancel()
			return errors.New("simulated transient publisher failure")
		},
	}

	log := newTestLogger()
	d := &MockDirector{
		OmClient:            omclient.New(cfg, log, srv.Client()),
		Cfg:                 cfg,
		Log:                 log,
		AssignmentPublisher: publisher,
		GSManager:           gsMgr,
	}

	start := time.Now()
	d.Run(ctx)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("AssignmentPublisher retry backoff took %v after context cancel, expected < 500ms", elapsed)
	}
	if got := sendAttempts.Load(); got != 1 {
		t.Fatalf("AssignmentPublisher.Send attempts = %d, want 1", got)
	}
}

// 15. MockAgonesIntegration Logger Configuration and Nil Fallback
func TestMockAgonesIntegration_LoggerConfigurationAndNilFallback(t *testing.T) {
	t.Run("logs through configured Log field", func(t *testing.T) {
		var buf bytes.Buffer
		customLog := logrus.New()
		customLog.SetOutput(&buf)
		customLog.SetLevel(logrus.DebugLevel)

		agones := &MockAgonesIntegration{
			Log: customLog,
		}
		if err := agones.Init(FleetConfig, ZonePools, GameModesInZone); err != nil {
			t.Fatalf("agones.Init() error: %v", err)
		}

		output := buf.String()
		if !strings.Contains(output, "Initializing matching parameters") {
			t.Fatalf("expected custom logger to contain 'Initializing matching parameters', got %q", output)
		}
		if !strings.Contains(output, "Matchmaking parameters for fleet successfully initialized") {
			t.Fatalf("expected custom logger to contain fleet initialization log, got %q", output)
		}
	})

	t.Run("defaults Log when nil", func(t *testing.T) {
		agones := &MockAgonesIntegration{
			Log: nil,
		}
		if err := agones.Init(FleetConfig, ZonePools, GameModesInZone); err != nil {
			t.Fatalf("agones.Init() with nil Log error: %v", err)
		}
		if agones.Log == nil {
			t.Fatal("expected agones.Log to be initialized when nil")
		}
	})
}
