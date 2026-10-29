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
	"maps"
	"math"
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
	"google.golang.org/protobuf/types/known/structpb"
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
	requestsByCall   [][]*pb.MmfRequest
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
	idx := s.getCalls
	s.getCalls++
	if len(s.requestsByCall) > 0 {
		if idx >= len(s.requestsByCall) {
			idx = len(s.requestsByCall) - 1
		}
		return slices.Clone(s.requestsByCall[idx])
	}
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
	cfg.Set("RANDOM_MATCH_REJECT_PERCENT", 0)
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

	wantStartMin := beforeInit.Add(-1*time.Hour - 2*time.Second)
	wantStartMax := afterInit.Add(-1*time.Hour + 2*time.Second)
	if gotStart.Before(wantStartMin) || gotStart.After(wantStartMax) {
		t.Fatalf("EveryTicket Start = %v, want between %v and %v", gotStart, wantStartMin, wantStartMax)
	}

	if gotEnd := EveryTicket.GetCreationTimeRangeFilter().GetEnd(); gotEnd != nil {
		t.Fatalf("EveryTicket End = %v, want nil (open-ended range)", gotEnd)
	}
}

// 8. ProposalsLoop Missing vs. Present Backfill Extension
func TestProposalsLoop_MissingVsPresentBackfillExtension(t *testing.T) {
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
	cfg.Set("RANDOM_MATCH_REJECT_PERCENT", 0)
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
		cfg.Set("RANDOM_MATCH_REJECT_PERCENT", 0)
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
		cfg.Set("RANDOM_MATCH_REJECT_PERCENT", 0)
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
	cfg.Set("RANDOM_MATCH_REJECT_PERCENT", 0)
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
		"deactivation_timeout, ",
		"deactivation_failed)",
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
	cfg.Set("RANDOM_MATCH_REJECT_PERCENT", 0)
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
	// Also record an invocation failure on otelMMFInvocationFailures so it appears in collected metrics.
	otelMMFInvocationFailures.Add(context.Background(), 1)

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
		"gsdirector.mmf.response.failures":                          "Number of InvokeMatchmakingFunctions calls that returned an error (transport or HTTP failure, an MMF failure reported by om-core, or the director-side deadline), by profile.name and grpc.code",
		"gsdirector.ticket.reactivations.failures":                  "Total number of ticket re-activations that permanently failed",
		"gsdirector.matchmaking_cycle.match.deactivation_timeouts":  "Number of proposed matches returned by om-core per cycle with the deactivation_timeout extension set",
		"gsdirector.matchmaking_cycle.ticket.deactivation_timeouts": "Number of tickets in proposed matches returned by om-core per cycle with the deactivation_timeout extension set",
		"gsdirector.matchmaking_cycle.match.proposals":              "Number of match proposals received from Matchmaking Functions",
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
	cfg.Set("RANDOM_MATCH_REJECT_PERCENT", 0)
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
	cfg.Set("RANDOM_MATCH_REJECT_PERCENT", 0)
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
	cfg.Set("RANDOM_MATCH_REJECT_PERCENT", 0)
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

// --- Helpers for Commit 39 tests ---

type fakeOMServer struct {
	srv     *httptest.Server
	onFetch func(w http.ResponseWriter, r *http.Request, req *pb.MmfRequest)

	mu           sync.Mutex
	activatedIDs []string
}

func newFakeOMServer(t *testing.T, onFetch func(w http.ResponseWriter, r *http.Request, req *pb.MmfRequest)) *fakeOMServer {
	t.Helper()
	f := &fakeOMServer{onFetch: onFetch}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/matches:fetch":
			body, _ := io.ReadAll(r.Body)
			var req pb.MmfRequest
			_ = protojson.Unmarshal(body, &req)
			if f.onFetch != nil {
				f.onFetch(w, r, &req)
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
			}
		case "/tickets:activate":
			body, _ := io.ReadAll(r.Body)
			var actReq pb.ActivateTicketsRequest
			_ = protojson.Unmarshal(body, &actReq)
			f.mu.Lock()
			f.activatedIDs = append(f.activatedIDs, actReq.GetTicketIds()...)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOMServer) reactivatedTickets() []string {
	f.mu.Lock()
	out := slices.Clone(f.activatedIDs)
	f.mu.Unlock()
	slices.Sort(out)
	return out
}

func buildDirectorForTest(
	t *testing.T,
	srv *httptest.Server,
	gsMgr GameServerManager,
	log *logrus.Logger,
	overrides map[string]any,
) (*MockDirector, chan *pb.Roster) {
	t.Helper()
	if log == nil {
		log = newTestLogger()
	}
	cfg := viper.New()
	cfg.Set("OM_CORE_ADDR", srv.URL)
	cfg.Set("RANDOM_MATCH_REJECT_PERCENT", 0)
	cfg.Set("NUM_MM_CYCLES", 1)
	cfg.Set("MM_CYCLE_MIN_DURATION_MS", 1)
	cfg.Set("ASSIGNMENT_TTL_MS", 60000)
	for k, v := range overrides {
		cfg.Set(k, v)
	}
	assignChan := make(chan *pb.Roster, 100)
	d := &MockDirector{
		OmClient:            omclient.New(cfg, log, srv.Client()),
		Cfg:                 cfg,
		Log:                 log,
		AssignmentPublisher: assignmentdistributor.NewChannelSender(assignChan),
		GSManager:           gsMgr,
	}
	return d, assignChan
}

func buildMMFRequestForTest(t *testing.T, profileName, hash string) *pb.MmfRequest {
	t.Helper()
	return &pb.MmfRequest{
		Mmfs: []*pb.MatchmakingFunctionSpec{Fifo},
		Profile: &pb.Profile{
			Name: profileName,
			Extensions: map[string]*anypb.Any{
				ex.MMFRequestHashKey:  mustAnyString(t, hash),
				ex.AgonesAllocatorKey: mustAnyString(t, fmt.Sprintf(`{"spec":%q}`, hash)),
			},
		},
	}
}

func buildSingleRosterMatch(t *testing.T, matchID, hash string, ticketIDs ...string) *pb.Match {
	t.Helper()
	tickets := make([]*pb.Ticket, len(ticketIDs))
	for i, id := range ticketIDs {
		tickets[i] = &pb.Ticket{Id: id}
	}
	var ext map[string]*anypb.Any
	if hash != "" {
		ext = map[string]*anypb.Any{
			ex.MMFRequestHashKey: mustAnyString(t, hash),
		}
	}
	return &pb.Match{
		Id: matchID,
		Rosters: map[string]*pb.Roster{
			"r1": {Tickets: tickets},
		},
		Extensions: ext,
	}
}

func mustAnyStringList(t *testing.T, values ...string) *anypb.Any {
	t.Helper()
	raw := make([]any, len(values))
	for i, v := range values {
		raw[i] = v
	}
	lv, err := structpb.NewList(raw)
	if err != nil {
		t.Fatalf("structpb.NewList(%v): %v", values, err)
	}
	a, err := anypb.New(lv)
	if err != nil {
		t.Fatalf("anypb.New(ListValue): %v", err)
	}
	return a
}

func collectMetricsMap(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect() error: %v", err)
	}
	byName := make(map[string]metricdata.Metrics)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			byName[m.Name] = m
		}
	}
	return byName
}

func sumCounterTotal(t *testing.T, metrics map[string]metricdata.Metrics, name string) int64 {
	t.Helper()
	m, ok := metrics[name]
	if !ok {
		return 0
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q data = %T, want metricdata.Sum[int64]", name, m.Data)
	}
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total
}

func sumInt64Histogram(t *testing.T, metrics map[string]metricdata.Metrics, name string) int64 {
	t.Helper()
	m, ok := metrics[name]
	if !ok {
		return 0
	}
	hist, ok := m.Data.(metricdata.Histogram[int64])
	if !ok {
		t.Fatalf("metric %q data = %T, want metricdata.Histogram[int64]", name, m.Data)
	}
	var total int64
	for _, dp := range hist.DataPoints {
		total += dp.Sum
	}
	return total
}

func dataPointAttrString(dp metricdata.DataPoint[int64], key string) string {
	for _, kv := range dp.Attributes.ToSlice() {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}

type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// New Test 1: Ticket in a rejected then accepted match is not re-activated (G1)
func TestRun_TicketInRejectedThenAcceptedMatchNotReactivated(t *testing.T) {
	const reqHash = "hash-g1"
	fakeOM := newFakeOMServer(t, func(w http.ResponseWriter, _ *http.Request, _ *pb.MmfRequest) {
		rejectedMatch := buildSingleRosterMatch(t, "m-rejected", reqHash, "t-shared", "t-rejected-only")
		rejectedMatch.Extensions[ex.MatchDeactivationTimeoutKey] = mustAnyBool(t, true)
		acceptedMatch := buildSingleRosterMatch(t, "m-accepted", reqHash, "t-shared", "t-accepted-only")
		writeStreamedMatchResponse(t, w, rejectedMatch, acceptedMatch)
	})

	gsMgr := &staticGSManager{
		requests: []*pb.MmfRequest{buildMMFRequestForTest(t, "dev.open-match.APAC.SoloDuel.1", reqHash)},
	}
	d, assignChan := buildDirectorForTest(t, fakeOM.srv, gsMgr, nil, map[string]any{
		"REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT": true,
	})

	d.Run(context.Background())

	if len(assignChan) != 1 {
		t.Fatalf("assigned rosters = %d, want 1", len(assignChan))
	}
	gotReactivated := fakeOM.reactivatedTickets()
	wantReactivated := []string{"t-rejected-only"}
	if !slices.Equal(gotReactivated, wantReactivated) {
		t.Fatalf("reactivated tickets = %v, want %v", gotReactivated, wantReactivated)
	}
}

// New Test 2: Shutdown with a full re-activation channel does not hang (G2)
func TestRun_ShutdownWithFullReactivationChannelDoesNotHang(t *testing.T) {
	testDone := make(chan struct{})
	defer close(testDone)

	firstActivateStarted := make(chan struct{})
	var firstActivateOnce sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/matches:fetch":
			_, _ = io.ReadAll(r.Body)
			tickets := make([]string, 1500)
			for i := range tickets {
				tickets[i] = fmt.Sprintf("t-%04d", i)
			}
			// No request hash -> rejected in Pass 1.
			m := buildSingleRosterMatch(t, "m-1500", "", tickets...)
			writeStreamedMatchResponse(t, w, m)
		case "/tickets:activate":
			_, _ = io.ReadAll(r.Body)
			firstActivateOnce.Do(func() {
				close(firstActivateStarted)
			})
			select {
			case <-r.Context().Done():
			case <-testDone:
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	gsMgr := &staticGSManager{
		requests: []*pb.MmfRequest{buildMMFRequestForTest(t, "dev.open-match.APAC.SoloDuel.1", "hash-g2")},
	}
	d, _ := buildDirectorForTest(t, srv, gsMgr, nil, map[string]any{
		"OM_CORE_MAX_CONCURRENT_ACTIVATION_CALLS": 1,
		"OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL": 1,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(runDone)
	}()

	select {
	case <-firstActivateStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first /tickets:activate call to start")
	}

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Run hung on shutdown with a full re-activation channel")
	}
}

// New Test 3: Pending MMF requests are rebuilt each cycle (G3)
func TestRun_PendingMMFRequestsRebuiltEachCycle(t *testing.T) {
	var cycle atomic.Int32
	fakeOM := newFakeOMServer(t, func(w http.ResponseWriter, _ *http.Request, _ *pb.MmfRequest) {
		c := cycle.Add(1)
		ticketID := fmt.Sprintf("c%d-t1", c)
		m := buildSingleRosterMatch(t, fmt.Sprintf("m-c%d", c), "hash-cycle-1", ticketID)
		writeStreamedMatchResponse(t, w, m)
	})

	gsMgr := &staticGSManager{
		requestsByCall: [][]*pb.MmfRequest{
			{buildMMFRequestForTest(t, "dev.open-match.APAC.SoloDuel.1", "hash-cycle-1")},
			{buildMMFRequestForTest(t, "dev.open-match.APAC.SoloDuel.2", "hash-cycle-2")},
		},
	}
	d, assignChan := buildDirectorForTest(t, fakeOM.srv, gsMgr, nil, map[string]any{
		"NUM_MM_CYCLES": 2,
	})

	d.Run(context.Background())

	if len(assignChan) != 1 {
		t.Fatalf("assigned rosters = %d, want 1 (cycle 1 only)", len(assignChan))
	}
	roster := <-assignChan
	if len(roster.GetTickets()) != 1 || roster.GetTickets()[0].GetId() != "c1-t1" {
		t.Fatalf("assigned roster tickets = %+v, want [c1-t1]", roster.GetTickets())
	}

	gotReactivated := fakeOM.reactivatedTickets()
	wantReactivated := []string{"c2-t1"}
	if !slices.Equal(gotReactivated, wantReactivated) {
		t.Fatalf("reactivated tickets = %v, want %v", gotReactivated, wantReactivated)
	}

	gotPendingKeys := slices.Sorted(maps.Keys(d.pendingMmfRequestsByHash))
	wantPendingKeys := []string{"hash-cycle-2"}
	if !slices.Equal(gotPendingKeys, wantPendingKeys) {
		t.Fatalf("pending request keys = %v, want %v", gotPendingKeys, wantPendingKeys)
	}
}

// New Test 4: Random match reject percent end-to-end
func TestRun_RandomMatchRejectPercentEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name            string
		pct             int
		wantAssigned    int
		wantReactivated int
	}{
		{name: "percent 0 accepts all", pct: 0, wantAssigned: 20, wantReactivated: 0},
		{name: "percent 100 rejects all", pct: 100, wantAssigned: 0, wantReactivated: 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const reqHash = "hash-rand"
			fakeOM := newFakeOMServer(t, func(w http.ResponseWriter, _ *http.Request, _ *pb.MmfRequest) {
				matches := make([]*pb.Match, 20)
				for i := range matches {
					matches[i] = buildSingleRosterMatch(t, fmt.Sprintf("m-%d", i), reqHash, fmt.Sprintf("t-%02d", i))
				}
				writeStreamedMatchResponse(t, w, matches...)
			})

			gsMgr := &staticGSManager{
				requests: []*pb.MmfRequest{buildMMFRequestForTest(t, "dev.open-match.APAC.SoloDuel.1", reqHash)},
			}
			d, assignChan := buildDirectorForTest(t, fakeOM.srv, gsMgr, nil, map[string]any{
				"RANDOM_MATCH_REJECT_PERCENT": tc.pct,
			})

			d.Run(context.Background())

			if got := len(assignChan); got != tc.wantAssigned {
				t.Fatalf("assigned rosters = %d, want %d", got, tc.wantAssigned)
			}
			if got := len(fakeOM.reactivatedTickets()); got != tc.wantReactivated {
				t.Fatalf("reactivated tickets = %d, want %d", got, tc.wantReactivated)
			}
		})
	}
}

// New Test 5: Random match reject percent config
func TestMockDirector_RandomMatchRejectPercentConfig(t *testing.T) {
	tests := []struct {
		name string
		set  bool
		val  int
		want int
	}{
		{name: "unset returns default 1", set: false, want: 1},
		{name: "explicit 0 returns 0", set: true, val: 0, want: 0},
		{name: "25 returns 25", set: true, val: 25, want: 25},
		{name: "150 clamps to 100", set: true, val: 150, want: 100},
		{name: "-5 clamps to 0", set: true, val: -5, want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := viper.New()
			if tc.set {
				cfg.Set("RANDOM_MATCH_REJECT_PERCENT", tc.val)
			}
			d := &MockDirector{Cfg: cfg}
			if got := d.configuredRandomRejectPercent(); got != tc.want {
				t.Fatalf("configuredRandomRejectPercent() = %d, want %d", got, tc.want)
			}
		})
	}
}

// New Test 6: Ticketless responses are feedback, invalid IDs handled
func TestRun_TicketlessFeedbackResponsesAndInvalidTicketIDs(t *testing.T) {
	const reqHash = "hash-feedback"
	meterPtr, reader := newTestMeter(t)

	fakeOM := newFakeOMServer(t, func(w http.ResponseWriter, _ *http.Request, _ *pb.MmfRequest) {
		ticketlessMatch := &pb.Match{
			Id: "m-ticketless",
			Extensions: map[string]*anypb.Any{
				"mmfFeedbackKey": mustAnyString(t, "feedback-value"),
			},
		}
		realMatch := buildSingleRosterMatch(t, "m-real", reqHash, "t-real")
		partialInvalid := buildSingleRosterMatch(t, "m-partial-invalid", reqHash, "t-partial-valid", "bad-id-1")
		partialInvalid.Extensions["invalid_ticket_ids"] = mustAnyStringList(t, "bad-id-1")
		allInvalid := buildSingleRosterMatch(t, "m-all-invalid", reqHash, "bad-id-1")
		allInvalid.Extensions["invalid_ticket_ids"] = mustAnyStringList(t, "bad-id-1")

		// Final response has nil Match.
		writeStreamedMatchResponse(t, w, ticketlessMatch, realMatch, partialInvalid, allInvalid, nil)
	})

	logBuf := &lockedLogBuffer{}
	log := logrus.New()
	log.SetOutput(logBuf)
	log.SetLevel(logrus.DebugLevel)

	gsMgr := &staticGSManager{
		requests: []*pb.MmfRequest{buildMMFRequestForTest(t, "dev.open-match.feedback.1", reqHash)},
	}
	d, assignChan := buildDirectorForTest(t, fakeOM.srv, gsMgr, log, nil)
	d.OtelMeterPtr = meterPtr

	d.Run(context.Background())

	if len(assignChan) != 1 {
		t.Fatalf("assigned rosters = %d, want 1", len(assignChan))
	}
	gsMgr.mu.Lock()
	allocCalls := gsMgr.allocateCalls
	gsMgr.mu.Unlock()
	if allocCalls != 1 {
		t.Fatalf("Allocate calls = %d, want 1", allocCalls)
	}

	gotReactivated := fakeOM.reactivatedTickets()
	wantReactivated := []string{"t-partial-valid"}
	if !slices.Equal(gotReactivated, wantReactivated) {
		t.Fatalf("reactivated tickets = %v, want %v", gotReactivated, wantReactivated)
	}

	metrics := collectMetricsMap(t, reader)
	if got := sumCounterTotal(t, metrics, "gsdirector.mmf.feedback_responses"); got != 3 {
		t.Fatalf("gsdirector.mmf.feedback_responses total = %d, want 3", got)
	}
	fbData := metrics["gsdirector.mmf.feedback_responses"].Data.(metricdata.Sum[int64])
	for _, dp := range fbData.DataPoints {
		if gotProf := dataPointAttrString(dp, "profile.name"); gotProf != "dev.open-match.feedback" {
			t.Fatalf("feedback_responses profile.name = %q, want %q", gotProf, "dev.open-match.feedback")
		}
	}

	if got := sumInt64Histogram(t, metrics, "gsdirector.matchmaking_cycle.match.proposals"); got != 2 {
		t.Fatalf("gsdirector.matchmaking_cycle.match.proposals sum = %d, want 2", got)
	}
	if got := sumInt64Histogram(t, metrics, "gsdirector.matchmaking_cycle.match.rejections"); got != 1 {
		t.Fatalf("gsdirector.matchmaking_cycle.match.rejections sum = %d, want 1", got)
	}

	logs := logBuf.String()
	for _, wantSub := range []string{"MMF feedback", "mmfFeedbackKey", "did not deactivate them", "bad-id-1"} {
		if !strings.Contains(logs, wantSub) {
			t.Fatalf("logs missing substring %q; got:\n%s", wantSub, logs)
		}
	}
}

// New Test 7: Match with failed deactivations is rejected
func TestRun_MatchWithFailedDeactivationsRejected(t *testing.T) {
	const reqHash = "hash-deact-fail"
	fakeOM := newFakeOMServer(t, func(w http.ResponseWriter, _ *http.Request, _ *pb.MmfRequest) {
		deactFailedMatch := buildSingleRosterMatch(t, "m-deact-failed", reqHash, "t-deact-ok", "t-deact-failed")
		deactFailedMatch.Extensions["deactivation_failed_ticket_ids"] = mustAnyStringList(t, "t-deact-failed")
		okMatch := buildSingleRosterMatch(t, "m-ok", reqHash, "t-ok")
		writeStreamedMatchResponse(t, w, deactFailedMatch, okMatch)
	})

	logBuf := &lockedLogBuffer{}
	log := logrus.New()
	log.SetOutput(logBuf)
	log.SetLevel(logrus.InfoLevel)
	log.SetFormatter(&logrus.JSONFormatter{})

	gsMgr := &staticGSManager{
		requests: []*pb.MmfRequest{buildMMFRequestForTest(t, "dev.open-match.APAC.SoloDuel.1", reqHash)},
	}
	d, assignChan := buildDirectorForTest(t, fakeOM.srv, gsMgr, log, nil)

	d.Run(context.Background())

	if len(assignChan) != 1 {
		t.Fatalf("assigned rosters = %d, want 1", len(assignChan))
	}
	roster := <-assignChan
	if len(roster.GetTickets()) != 1 || roster.GetTickets()[0].GetId() != "t-ok" {
		t.Fatalf("assigned roster tickets = %+v, want [t-ok]", roster.GetTickets())
	}

	gotReactivated := fakeOM.reactivatedTickets()
	wantReactivated := []string{"t-deact-failed", "t-deact-ok"}
	if !slices.Equal(gotReactivated, wantReactivated) {
		t.Fatalf("reactivated tickets = %v, want %v", gotReactivated, wantReactivated)
	}

	if !strings.Contains(d.Status(), "00001 deactivation_failed") {
		t.Fatalf("Status() = %q, missing '00001 deactivation_failed'", d.Status())
	}

	foundInfoLine := false
	for _, line := range strings.Split(logBuf.String(), "\n") {
		if strings.Contains(line, `"level":"info"`) &&
			strings.Contains(line, "could not deactivate 1 of its tickets") &&
			strings.Contains(line, "t-deact-failed") {
			foundInfoLine = true
			break
		}
	}
	if !foundInfoLine {
		t.Fatalf("expected INFO log line with 'could not deactivate 1 of its tickets' and 't-deact-failed', got:\n%s", logBuf.String())
	}
}

// New Test 8: InvokeMatchmakingFunctions error is recorded
func TestRun_InvokeMatchmakingFunctionsErrorRecorded(t *testing.T) {
	t.Run("stream error recorded and failed cycles do not count as empty", func(t *testing.T) {
		const reqHash = "hash-err"
		const errEnvelope = `{"error":{"code":13,"message":"1 of 1 matchmaking functions failed: FIFO: boom","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MMF_FAILURE","domain":"open-match","metadata":{"mmf_name":"FIFO","grpc_code":"Unavailable","message":"boom"}}]}}` + "\n"

		meterPtr, reader := newTestMeter(t)
		var cycle atomic.Int32
		fakeOM := newFakeOMServer(t, func(w http.ResponseWriter, _ *http.Request, _ *pb.MmfRequest) {
			c := cycle.Add(1)
			if c == 1 {
				m := buildSingleRosterMatch(t, "m-before-err", reqHash, "t-pre-err")
				writeStreamedMatchResponse(t, w, m)
				_, _ = w.Write([]byte(errEnvelope))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(errEnvelope))
		})

		logBuf := &lockedLogBuffer{}
		log := logrus.New()
		log.SetOutput(logBuf)
		log.SetLevel(logrus.InfoLevel)
		log.SetFormatter(&logrus.JSONFormatter{})

		gsMgr := &staticGSManager{
			requests: []*pb.MmfRequest{buildMMFRequestForTest(t, "dev.open-match.APAC.SoloDuel.1", reqHash)},
		}
		d, assignChan := buildDirectorForTest(t, fakeOM.srv, gsMgr, log, map[string]any{
			"NUM_MM_CYCLES": 3,
			"NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT": 1,
		})
		d.OtelMeterPtr = meterPtr

		d.Run(context.Background())

		if len(assignChan) != 1 {
			t.Fatalf("assigned rosters = %d, want 1 (pre-error match)", len(assignChan))
		}

		gsMgr.mu.Lock()
		getCalls := gsMgr.getCalls
		gsMgr.mu.Unlock()
		if getCalls != 3 {
			t.Fatalf("GetMMFParams calls = %d, want 3 (failed cycles must not count as empty)", getCalls)
		}

		metrics := collectMetricsMap(t, reader)
		if got := sumCounterTotal(t, metrics, "gsdirector.mmf.response.failures"); got != 3 {
			t.Fatalf("gsdirector.mmf.response.failures total = %d, want 3", got)
		}
		failSum := metrics["gsdirector.mmf.response.failures"].Data.(metricdata.Sum[int64])
		for _, dp := range failSum.DataPoints {
			if gotCode := dataPointAttrString(dp, "grpc.code"); gotCode != "Internal" {
				t.Fatalf("failure data point grpc.code = %q, want %q", gotCode, "Internal")
			}
		}

		logs := logBuf.String()
		if !strings.Contains(logs, `"failed_mmfs"`) || !strings.Contains(logs, "FIFO") {
			t.Fatalf("logs missing failed_mmfs with FIFO; got:\n%s", logs)
		}
	})

	t.Run("empty cycles without error exit after 1 cycle", func(t *testing.T) {
		fakeOM := newFakeOMServer(t, func(w http.ResponseWriter, _ *http.Request, _ *pb.MmfRequest) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
		})

		gsMgr := &staticGSManager{
			requests: []*pb.MmfRequest{buildMMFRequestForTest(t, "dev.open-match.APAC.SoloDuel.1", "hash-empty")},
		}
		d, _ := buildDirectorForTest(t, fakeOM.srv, gsMgr, nil, map[string]any{
			"NUM_MM_CYCLES": 3,
			"NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT": 1,
		})

		d.Run(context.Background())

		gsMgr.mu.Lock()
		getCalls := gsMgr.getCalls
		gsMgr.mu.Unlock()
		if getCalls != 1 {
			t.Fatalf("GetMMFParams calls = %d, want 1", getCalls)
		}
	})
}

// New Test 9: Director-side deadline (G6)
func TestRun_DirectorSideDeadline(t *testing.T) {
	testDone := make(chan struct{})
	defer close(testDone)

	meterPtr, reader := newTestMeter(t)
	fakeOM := newFakeOMServer(t, func(_ http.ResponseWriter, r *http.Request, _ *pb.MmfRequest) {
		select {
		case <-r.Context().Done():
		case <-testDone:
		}
	})

	gsMgr := &staticGSManager{
		requests: []*pb.MmfRequest{buildMMFRequestForTest(t, "dev.open-match.APAC.SoloDuel.1", "hash-deadline")},
	}
	d, _ := buildDirectorForTest(t, fakeOM.srv, gsMgr, nil, map[string]any{
		"MMF_INVOCATION_TIMEOUT_SECS": 1,
	})
	d.OtelMeterPtr = meterPtr

	start := time.Now()
	d.Run(context.Background())
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("Run took %v, want < 5s", elapsed)
	}

	metrics := collectMetricsMap(t, reader)
	if got := sumCounterTotal(t, metrics, "gsdirector.mmf.response.failures"); got != 1 {
		t.Fatalf("gsdirector.mmf.response.failures total = %d, want 1", got)
	}
	failSum := metrics["gsdirector.mmf.response.failures"].Data.(metricdata.Sum[int64])
	for _, dp := range failSum.DataPoints {
		if gotCode := dataPointAttrString(dp, "grpc.code"); gotCode != "DeadlineExceeded" {
			t.Fatalf("failure data point grpc.code = %q, want %q", gotCode, "DeadlineExceeded")
		}
	}
}

// New Test 10: Director-side deadline mid-stream (G6)
func TestRun_DirectorSideDeadlineMidStream(t *testing.T) {
	testDone := make(chan struct{})
	defer close(testDone)

	const reqHash = "hash-deadline-mid"
	meterPtr, reader := newTestMeter(t)
	var cycle atomic.Int32

	fakeOM := newFakeOMServer(t, func(w http.ResponseWriter, r *http.Request, _ *pb.MmfRequest) {
		c := cycle.Add(1)
		if c == 1 {
			m := buildSingleRosterMatch(t, "m-mid-1", reqHash, "t-mid-1")
			writeStreamedMatchResponse(t, w, m)
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-testDone:
		}
	})

	gsMgr := &staticGSManager{
		requests: []*pb.MmfRequest{buildMMFRequestForTest(t, "dev.open-match.APAC.SoloDuel.1", reqHash)},
	}
	d, assignChan := buildDirectorForTest(t, fakeOM.srv, gsMgr, nil, map[string]any{
		"NUM_MM_CYCLES":                               3,
		"MMF_INVOCATION_TIMEOUT_SECS":                 1,
		"NUM_CONSECUTIVE_EMPTY_MM_CYCLES_BEFORE_QUIT": 1,
	})
	d.OtelMeterPtr = meterPtr

	d.Run(context.Background())

	if len(assignChan) != 1 {
		t.Fatalf("assigned rosters = %d, want 1", len(assignChan))
	}
	gsMgr.mu.Lock()
	getCalls := gsMgr.getCalls
	gsMgr.mu.Unlock()
	if getCalls != 3 {
		t.Fatalf("GetMMFParams calls = %d, want 3", getCalls)
	}

	metrics := collectMetricsMap(t, reader)
	if got := sumCounterTotal(t, metrics, "gsdirector.mmf.response.failures"); got != 3 {
		t.Fatalf("gsdirector.mmf.response.failures total = %d, want 3", got)
	}
	failSum := metrics["gsdirector.mmf.response.failures"].Data.(metricdata.Sum[int64])
	for _, dp := range failSum.DataPoints {
		if gotCode := dataPointAttrString(dp, "grpc.code"); gotCode != "DeadlineExceeded" {
			t.Fatalf("failure data point grpc.code = %q, want %q", gotCode, "DeadlineExceeded")
		}
	}
}

// New Test 11: Missing required fields
func TestRun_MissingRequiredFields(t *testing.T) {
	logBuf := &lockedLogBuffer{}
	log := logrus.New()
	log.SetOutput(logBuf)

	d := &MockDirector{
		Log: log,
		Cfg: viper.New(),
	}

	done := make(chan struct{})
	go func() {
		d.Run(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly when required fields were missing")
	}

	logs := logBuf.String()
	for _, wantField := range []string{"OmClient", "GSManager", "AssignmentPublisher"} {
		if !strings.Contains(logs, wantField) {
			t.Fatalf("logs missing required field name %q; got:\n%s", wantField, logs)
		}
	}
}

// New Test 12: Bounded profile name
func TestBoundedProfileName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{
			in:   "dev.open-match.APAC/JP_Tokyo@asia-northeast1-a.SoloDuel.1700000000",
			want: "dev.open-match.APAC/JP_Tokyo@asia-northeast1-a.SoloDuel",
		},
		{
			in:   "backfill.dev.open-match.APAC.SoloDuel.abc123",
			want: "backfill.dev.open-match.APAC.SoloDuel",
		},
		{
			in:   "nodots",
			want: "nodots",
		},
		{
			in:   "",
			want: "",
		},
		{
			in:   ".leading",
			want: ".leading",
		},
	}

	for _, tc := range tests {
		if got := boundedProfileName(tc.in); got != tc.want {
			t.Errorf("boundedProfileName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
