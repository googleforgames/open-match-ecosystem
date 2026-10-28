// Copyright 2024 Google LLC
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

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	knownpb "google.golang.org/protobuf/types/known/wrapperspb"
	"open-match.dev/open-match-ecosystem/v2/examples/mmf/functions/debug"
	"open-match.dev/open-match-ecosystem/v2/examples/mmf/functions/fifo"
	"open-match.dev/open-match-ecosystem/v2/examples/mmf/server"
	"open-match.dev/open-match-ecosystem/v2/internal/extensions"
)

type fakeRunServer struct {
	grpc.ServerStream
	ctx       context.Context
	chunks    []*pb.ChunkedMmfRunRequest
	recvIdx   int
	recvErr   error
	responses []*pb.StreamedMmfResponse
	sendErr   error
}

func (f *fakeRunServer) Context() context.Context {
	if f.ctx != nil {
		return f.ctx
	}
	return context.Background()
}

func (f *fakeRunServer) Recv() (*pb.ChunkedMmfRunRequest, error) {
	if f.recvErr != nil {
		return nil, f.recvErr
	}
	if f.recvIdx >= len(f.chunks) {
		return nil, io.EOF
	}
	chunk := f.chunks[f.recvIdx]
	f.recvIdx++
	return chunk, nil
}

func (f *fakeRunServer) Send(resp *pb.StreamedMmfResponse) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.responses = append(f.responses, resp)
	return nil
}

type blockingMMFService struct {
	pb.UnimplementedMatchMakingFunctionServiceServer
	entered chan struct{}
}

func (b *blockingMMFService) Run(stream pb.MatchMakingFunctionService_RunServer) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func discardLogEntry() *logrus.Entry {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return logrus.NewEntry(l)
}

func runFIFOOnce(t *testing.T, profile *pb.Profile) []*pb.Match {
	t.Helper()
	l := logrus.New()
	l.SetOutput(io.Discard)
	srv := fifo.NewWithLogger(l)
	stream := &fakeRunServer{
		chunks: []*pb.ChunkedMmfRunRequest{
			{NumChunks: 1, Profile: profile},
		},
	}
	if err := srv.Run(stream); err != nil {
		t.Fatalf("fifo.Run(%q) returned unexpected error: %v", profile.GetName(), err)
	}
	matches := make([]*pb.Match, 0, len(stream.responses))
	for _, resp := range stream.responses {
		matches = append(matches, resp.GetMatch())
	}
	return matches
}

func ticketAt(id string, created time.Time) *pb.Ticket {
	return &pb.Ticket{
		Id: id,
		Attributes: &pb.Ticket_FilterableData{
			CreationTime: timestamppb.New(created),
		},
	}
}

func sortedMatchTicketIDs(m *pb.Match) []string {
	var ids []string
	for _, roster := range m.GetRosters() {
		for _, tk := range roster.GetTickets() {
			ids = append(ids, tk.GetId())
		}
	}
	sort.Strings(ids)
	return ids
}

func reserveFreePort(t *testing.T) int32 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	port := int32(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	return port
}

func waitForPortReady(t *testing.T, port int32) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for port %d to accept connections", port)
}

func mustAnyInt32(t *testing.T, v int32) *anypb.Any {
	t.Helper()
	a, err := anypb.New(&knownpb.Int32Value{Value: v})
	if err != nil {
		t.Fatalf("failed to pack int32 any: %v", err)
	}
	return a
}

func mustAnyString(t *testing.T, v string) *anypb.Any {
	t.Helper()
	a, err := anypb.New(&knownpb.StringValue{Value: v})
	if err != nil {
		t.Fatalf("failed to pack string any: %v", err)
	}
	return a
}

func TestMMFServerFactoryDispatch(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)

	validCases := []struct {
		name  string
		input string
	}{
		{name: "empty defaults to fifo", input: ""},
		{name: "lowercase fifo", input: "fifo"},
		{name: "uppercase whitespace fifo", input: "  FIFO\t\n"},
		{name: "lowercase debug", input: "debug"},
		{name: "mixedcase whitespace debug", input: "  DeBuG  "},
	}

	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := newMMFServer(tc.input, logger)
			if err != nil {
				t.Fatalf("newMMFServer(%q) returned unexpected error: %v", tc.input, err)
			}
			if srv == nil {
				t.Fatalf("newMMFServer(%q) returned nil server", tc.input)
			}
		})
	}

	invalidInputs := []string{"unknown", "soloduel", "fifo2"}
	for _, invalid := range invalidInputs {
		t.Run("invalid_"+invalid, func(t *testing.T) {
			srv, err := newMMFServer(invalid, logger)
			if err == nil {
				t.Fatalf("newMMFServer(%q) expected error, got nil", invalid)
			}
			if srv != nil {
				t.Fatalf("newMMFServer(%q) expected nil server on error, got %v", invalid, srv)
			}
		})
	}
}

func TestBaseProfileName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{
			input: "dev.open-match.fleet.mode.1730000000",
			want:  "dev.open-match.fleet.mode",
		},
		{
			input: "backfill.dev.open-match.fleet.mode.1730000-3",
			want:  "dev.open-match.fleet.mode",
		},
		{
			input: "backfill.backfill.dev.open-match.fleet.mode.42",
			want:  "backfill.dev.open-match.fleet.mode",
		},
		{
			input: "my-profile",
			want:  "my-profile",
		},
		{
			input: "backfill.my-profile.123-0",
			want:  "my-profile",
		},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			if got := server.BaseProfileName(tc.input); got != tc.want {
				t.Fatalf("BaseProfileName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestGetChunkedRequestMultiChunkEmptyPoolAndSliceIsolation(t *testing.T) {
	// Allocate chunk 1 ticket slice with extra capacity to verify that subsequent
	// chunk appends do not overwrite chunk 1's backing array.
	chunk1Backing := make([]*pb.Ticket, 2, 4)
	chunk1Backing[0] = &pb.Ticket{Id: "t1"}
	chunk1Backing[1] = &pb.Ticket{Id: "t2"}
	sentinel := &pb.Ticket{Id: "sentinel-do-not-overwrite"}
	fullView := chunk1Backing[:4]
	fullView[2] = sentinel
	fullView[3] = sentinel

	chunk1 := &pb.ChunkedMmfRunRequest{
		NumChunks: 2,
		Profile: &pb.Profile{
			Name: "chunked-profile",
			Pools: map[string]*pb.Pool{
				"empty_pool": {
					Name:         "empty_pool",
					Participants: nil,
				},
				"late_pool": {
					Name:         "late_pool",
					Participants: nil,
				},
				"multi_pool": {
					Name: "multi_pool",
					Participants: &pb.Roster{
						Name:    "",
						Tickets: chunk1Backing,
					},
				},
			},
		},
	}

	chunk2 := &pb.ChunkedMmfRunRequest{
		NumChunks: 2,
		Profile: &pb.Profile{
			Name: "chunked-profile",
			Pools: map[string]*pb.Pool{
				"late_pool": {
					Name: "late_pool",
					Participants: &pb.Roster{
						Name: "late_roster",
						Tickets: []*pb.Ticket{
							{Id: "late-t1"},
						},
					},
				},
				"multi_pool": {
					Name: "multi_pool",
					Participants: &pb.Roster{
						Tickets: []*pb.Ticket{
							{Id: "t3"},
							{Id: "t4"},
						},
					},
				},
			},
		},
	}

	stream := &fakeRunServer{
		chunks: []*pb.ChunkedMmfRunRequest{chunk1, chunk2},
	}

	profile, err := server.GetChunkedRequest(stream, discardLogEntry())
	if err != nil {
		t.Fatalf("GetChunkedRequest returned unexpected error: %v", err)
	}
	if profile.GetName() != "chunked-profile" {
		t.Fatalf("got profile name %q, want %q", profile.GetName(), "chunked-profile")
	}

	// Verify empty_pool has non-nil Participants with fallback roster name and 0 tickets.
	emptyPool := profile.GetPools()["empty_pool"]
	if emptyPool == nil || emptyPool.GetParticipants() == nil {
		t.Fatalf("expected non-nil Participants for empty_pool")
	}
	if emptyPool.GetParticipants().GetName() != "empty_pool" {
		t.Fatalf("empty_pool roster name = %q, want %q", emptyPool.GetParticipants().GetName(), "empty_pool")
	}
	if len(emptyPool.GetParticipants().GetTickets()) != 0 {
		t.Fatalf("empty_pool ticket count = %d, want 0", len(emptyPool.GetParticipants().GetTickets()))
	}

	// Verify late_pool received tickets in chunk 2 after nil Participants in chunk 1.
	latePool := profile.GetPools()["late_pool"]
	if latePool == nil || len(latePool.GetParticipants().GetTickets()) != 1 {
		t.Fatalf("late_pool tickets = %v, want 1 ticket", latePool.GetParticipants().GetTickets())
	}
	if latePool.GetParticipants().GetTickets()[0].GetId() != "late-t1" {
		t.Fatalf("late_pool ticket[0] = %q, want %q", latePool.GetParticipants().GetTickets()[0].GetId(), "late-t1")
	}

	// Verify multi_pool concatenated all 4 tickets and defaulted its empty roster name to pool key.
	multiPool := profile.GetPools()["multi_pool"]
	if multiPool == nil || multiPool.GetParticipants() == nil {
		t.Fatalf("expected non-nil Participants for multi_pool")
	}
	if multiPool.GetParticipants().GetName() != "multi_pool" {
		t.Fatalf("multi_pool roster name = %q, want %q", multiPool.GetParticipants().GetName(), "multi_pool")
	}
	gotTickets := multiPool.GetParticipants().GetTickets()
	if len(gotTickets) != 4 {
		t.Fatalf("multi_pool ticket count = %d, want 4", len(gotTickets))
	}
	wantIDs := []string{"t1", "t2", "t3", "t4"}
	for i, wantID := range wantIDs {
		if gotTickets[i].GetId() != wantID {
			t.Fatalf("multi_pool ticket[%d] = %q, want %q", i, gotTickets[i].GetId(), wantID)
		}
	}

	// Verify chunk 1's slice backing array was never mutated by appending chunk 2.
	if fullView[2] != sentinel || fullView[3] != sentinel {
		t.Fatalf("chunk 1 slice backing array was mutated: fullView[2]=%v, fullView[3]=%v", fullView[2], fullView[3])
	}
}

func TestStreamRecvErrorPropagation(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	expectedErr := errors.New("simulated stream recv failure")

	t.Run("GetChunkedRequest", func(t *testing.T) {
		stream := &fakeRunServer{recvErr: expectedErr}
		prof, err := server.GetChunkedRequest(stream, discardLogEntry())
		if !errors.Is(err, expectedErr) {
			t.Fatalf("GetChunkedRequest err = %v, want %v", err, expectedErr)
		}
		if prof != nil {
			t.Fatalf("expected nil profile on error, got %v", prof)
		}
	})

	t.Run("fifo.Run", func(t *testing.T) {
		stream := &fakeRunServer{recvErr: expectedErr}
		fifoSrv := fifo.NewWithLogger(logger)
		err := fifoSrv.Run(stream)
		if !errors.Is(err, expectedErr) {
			t.Fatalf("fifo.Run err = %v, want %v", err, expectedErr)
		}
		if len(stream.responses) != 0 {
			t.Fatalf("fifo.Run sent %d responses on error, want 0", len(stream.responses))
		}
	})

	t.Run("debug.Run", func(t *testing.T) {
		stream := &fakeRunServer{recvErr: expectedErr}
		debugSrv := debug.NewWithLogger(logger)
		err := debugSrv.Run(stream)
		if !errors.Is(err, expectedErr) {
			t.Fatalf("debug.Run err = %v, want %v", err, expectedErr)
		}
		if len(stream.responses) != 0 {
			t.Fatalf("debug.Run sent %d responses on error, want 0", len(stream.responses))
		}
	})
}

func TestFIFOOrdersByCreationTime(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	profile := &pb.Profile{
		Name: "dev.open-match.fleet.mode.1",
		Pools: map[string]*pb.Pool{
			"p1": {
				Name: "p1",
				Participants: &pb.Roster{
					Name: "p1",
					Tickets: []*pb.Ticket{
						ticketAt("newest", base.Add(5*time.Second)),
						ticketAt("tie-b", base.Add(2*time.Second)),
						ticketAt("oldest", base),
					},
				},
			},
			"p2": {
				Name: "p2",
				Participants: &pb.Roster{
					Name: "p2",
					Tickets: []*pb.Ticket{
						ticketAt("second", base.Add(1*time.Second)),
						ticketAt("tie-a", base.Add(2*time.Second)),
						ticketAt("oldest", base),
					},
				},
			},
		},
		Extensions: map[string]*anypb.Any{
			"desiredNumRosters": mustAnyInt32(t, 1),
			"desiredRosterLen":  mustAnyInt32(t, 2),
			"minRosterLen":      mustAnyInt32(t, 2),
		},
	}

	matches := runFIFOOnce(t, profile)
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(matches))
	}

	wantRosters := [][]string{
		{"oldest", "second"},
		{"tie-a", "tie-b"},
	}
	for idx, want := range wantRosters {
		if len(matches[idx].GetRosters()) != 1 {
			t.Fatalf("match[%d] roster count = %d, want 1", idx, len(matches[idx].GetRosters()))
		}
		var got []string
		for _, r := range matches[idx].GetRosters() {
			for _, tk := range r.GetTickets() {
				got = append(got, tk.GetId())
			}
		}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("match[%d] tickets = %v, want %v", idx, got, want)
		}
	}
}

func TestFIFOMatchIDsAndBackfillNaming(t *testing.T) {
	emptyMmfReqAny, err := anypb.New(&pb.MmfRequest{})
	if err != nil {
		t.Fatalf("failed to pack empty MmfRequest: %v", err)
	}

	buildProfile := func(name string) *pb.Profile {
		return &pb.Profile{
			Name: name,
			Pools: map[string]*pb.Pool{
				"p1": {
					Name: "p1",
					Participants: &pb.Roster{
						Name: "p1",
						Tickets: []*pb.Ticket{
							{Id: "a"},
							{Id: "b"},
							{Id: "c"},
							{Id: "d"},
							{Id: "e"},
						},
					},
				},
			},
			Extensions: map[string]*anypb.Any{
				"desiredNumRosters":      mustAnyInt32(t, 1),
				"desiredRosterLen":       mustAnyInt32(t, 2),
				"minRosterLen":           mustAnyInt32(t, 1),
				extensions.MMFRequestKey: emptyMmfReqAny,
			},
		}
	}

	run1 := runFIFOOnce(t, buildProfile("dev.open-match.fleet.mode.111"))
	run2 := runFIFOOnce(t, buildProfile("dev.open-match.fleet.mode.222"))
	if len(run1) != 3 {
		t.Fatalf("run1 produced %d matches, want 3", len(run1))
	}
	if len(run2) != 3 {
		t.Fatalf("run2 produced %d matches, want 3", len(run2))
	}

	seenMatchIDs := make(map[string]struct{})
	seenRosterNames := make(map[string]struct{})
	const wantMatchPrefix = "simple-fifo-matchfunction.dev.open-match.fleet.mode."

	for _, batch := range [][]*pb.Match{run1, run2} {
		for _, m := range batch {
			id := m.GetId()
			if _, dup := seenMatchIDs[id]; dup {
				t.Fatalf("duplicate match ID %q", id)
			}
			seenMatchIDs[id] = struct{}{}

			if !strings.HasPrefix(id, wantMatchPrefix) {
				t.Fatalf("match ID = %q, want prefix %q", id, wantMatchPrefix)
			}
			rem := strings.TrimPrefix(id, wantMatchPrefix)
			if rem == "" || strings.Contains(rem, ".") {
				t.Fatalf("match ID suffix = %q, want non-empty dot-free string", rem)
			}

			for rName := range m.GetRosters() {
				if _, dup := seenRosterNames[rName]; dup {
					t.Fatalf("duplicate roster name %q", rName)
				}
				seenRosterNames[rName] = struct{}{}
			}
		}
	}

	// Third match of the first run (1 ticket) must carry a bounded backfill profile.
	bfReq1, err := extensions.MMFRequest(run1[2].GetExtensions(), extensions.MMFRequestKey)
	if err != nil {
		t.Fatalf("expected backfill MmfRequest on partial match, got err: %v", err)
	}
	bfProf1 := bfReq1.GetProfile()
	const wantBfPrefix = "backfill.dev.open-match.fleet.mode."
	if !strings.HasPrefix(bfProf1.GetName(), wantBfPrefix) {
		t.Fatalf("backfill profile name = %q, want prefix %q", bfProf1.GetName(), wantBfPrefix)
	}
	bfSuffix1 := strings.TrimPrefix(bfProf1.GetName(), wantBfPrefix)
	if bfSuffix1 == "" || strings.Contains(bfSuffix1, ".") {
		t.Fatalf("backfill profile suffix = %q, want non-empty dot-free string", bfSuffix1)
	}
	lastDot := strings.LastIndex(bfProf1.GetName(), ".")
	if gotTruncated := bfProf1.GetName()[:lastDot]; gotTruncated != "backfill.dev.open-match.fleet.mode" {
		t.Fatalf("truncated backfill profile name = %q, want %q", gotTruncated, "backfill.dev.open-match.fleet.mode")
	}

	// Run the backfill profile itself as another partial round; verify no nested backfill prefixes.
	bfProf1.Pools["p1"].Participants = &pb.Roster{
		Name:    "p1",
		Tickets: []*pb.Ticket{{Id: "z"}},
	}
	bfProf1.Extensions[extensions.MMFRequestKey] = emptyMmfReqAny
	bfProf1.Extensions["desiredRosterLen"] = mustAnyInt32(t, 2)

	bfMatches := runFIFOOnce(t, bfProf1)
	if len(bfMatches) != 1 {
		t.Fatalf("backfill run produced %d matches, want 1", len(bfMatches))
	}
	bfMatch := bfMatches[0]
	if _, dup := seenMatchIDs[bfMatch.GetId()]; dup {
		t.Fatalf("duplicate match ID on backfill run: %q", bfMatch.GetId())
	}
	for rName := range bfMatch.GetRosters() {
		if _, dup := seenRosterNames[rName]; dup {
			t.Fatalf("duplicate roster name on backfill run: %q", rName)
		}
	}

	const wantBfMatchPrefix = "simple-fifo-matchfunction.backfill.dev.open-match.fleet.mode."
	if !strings.HasPrefix(bfMatch.GetId(), wantBfMatchPrefix) {
		t.Fatalf("backfill match ID = %q, want prefix %q", bfMatch.GetId(), wantBfMatchPrefix)
	}
	bfMatchRem := strings.TrimPrefix(bfMatch.GetId(), wantBfMatchPrefix)
	if bfMatchRem == "" || strings.Contains(bfMatchRem, ".") {
		t.Fatalf("backfill match ID suffix = %q, want non-empty dot-free string", bfMatchRem)
	}

	bfReq2, err := extensions.MMFRequest(bfMatch.GetExtensions(), extensions.MMFRequestKey)
	if err != nil {
		t.Fatalf("expected second-round backfill MmfRequest, got err: %v", err)
	}
	bfProf2Name := bfReq2.GetProfile().GetName()
	if !strings.HasPrefix(bfProf2Name, wantBfPrefix) {
		t.Fatalf("second-round backfill profile name = %q, want prefix %q", bfProf2Name, wantBfPrefix)
	}
	bfSuffix2 := strings.TrimPrefix(bfProf2Name, wantBfPrefix)
	if bfSuffix2 == "" || strings.Contains(bfSuffix2, ".") {
		t.Fatalf("second-round backfill profile suffix = %q, want non-empty dot-free string (no nested prefixes)", bfSuffix2)
	}
}

func TestFIFOBackfillCapacityUnevenRosters(t *testing.T) {
	emptyMmfReqAny, err := anypb.New(&pb.MmfRequest{})
	if err != nil {
		t.Fatalf("failed to pack empty MmfRequest: %v", err)
	}

	var tickets []*pb.Ticket
	for i := 1; i <= 7; i++ {
		tickets = append(tickets, &pb.Ticket{Id: fmt.Sprintf("t%d", i)})
	}

	profile := &pb.Profile{
		Name: "dev.open-match.fleet.teams.1",
		Pools: map[string]*pb.Pool{
			"p1": {
				Name: "p1",
				Participants: &pb.Roster{
					Name:    "p1",
					Tickets: tickets,
				},
			},
		},
		Extensions: map[string]*anypb.Any{
			"desiredNumRosters":      mustAnyInt32(t, 2),
			"desiredRosterLen":       mustAnyInt32(t, 4),
			"minRosterLen":           mustAnyInt32(t, 2),
			extensions.MMFRequestKey: emptyMmfReqAny,
		},
	}

	matches := runFIFOOnce(t, profile)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}

	gotIDs := sortedMatchTicketIDs(matches[0])
	if len(gotIDs) != 7 {
		t.Fatalf("match ticket count = %d (%v), want 7", len(gotIDs), gotIDs)
	}

	bfReq, err := extensions.MMFRequest(matches[0].GetExtensions(), extensions.MMFRequestKey)
	if err != nil {
		t.Fatalf("expected backfill MmfRequest on partial match, got err: %v", err)
	}
	bfExt := bfReq.GetProfile().GetExtensions()

	for _, key := range []string{"desiredNumRosters", "desiredRosterLen", "minRosterLen"} {
		val, err := extensions.Int32(bfExt, key)
		if err != nil || val != 1 {
			t.Fatalf("backfill %s = %d (err=%v), want 1", key, val, err)
		}
	}
}

func TestFIFOOverlappingPoolDeduplicationFullAndBackfillMatch(t *testing.T) {
	var logBuf bytes.Buffer
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)
	logger.SetOutput(&logBuf)

	bfReqAny, err := anypb.New(&pb.MmfRequest{})
	if err != nil {
		t.Fatalf("failed to pack MmfRequest: %v", err)
	}

	// Pool A has 4 tickets (t1..t4); Pool B has 5 tickets (t3..t7), overlapping on t3 and t4.
	// Total unique tickets = 7.
	// With desiredNumRosters=1, desiredRosterLen=4, minRosterLen=3:
	// - Match 0 is a full match (4 tickets, score = 100).
	// - Match 1 is a partial/backfill match (3 tickets, score = 25, backfill profile desiredRosterLen = 1, minRosterLen = 1).
	profile := &pb.Profile{
		Name: "fifo-overlap-test",
		Pools: map[string]*pb.Pool{
			"poolA": {
				Name: "poolA",
				Participants: &pb.Roster{
					Name: "poolA",
					Tickets: []*pb.Ticket{
						{Id: "t1"},
						{Id: "t2"},
						{Id: "t3"},
						{Id: "t4"},
					},
				},
			},
			"poolB": {
				Name: "poolB",
				Participants: &pb.Roster{
					Name: "poolB",
					Tickets: []*pb.Ticket{
						{Id: "t3"},
						{Id: "t4"},
						{Id: "t5"},
						{Id: "t6"},
						{Id: "t7"},
					},
				},
			},
		},
		Extensions: map[string]*anypb.Any{
			"desiredNumRosters":      mustAnyInt32(t, 1),
			"desiredRosterLen":       mustAnyInt32(t, 4),
			"minRosterLen":           mustAnyInt32(t, 3),
			extensions.MMFRequestKey: bfReqAny,
		},
	}

	stream := &fakeRunServer{
		chunks: []*pb.ChunkedMmfRunRequest{
			{
				NumChunks: 1,
				Profile:   profile,
			},
		},
	}

	fifoSrv := fifo.NewWithLogger(logger)
	if err := fifoSrv.Run(stream); err != nil {
		t.Fatalf("fifo.Run returned unexpected error: %v", err)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "Found 4 tickets in pool poolA") {
		t.Errorf("expected per-pool log 'Found 4 tickets in pool poolA', got logs:\n%s", logs)
	}
	if !strings.Contains(logs, "Found 5 tickets in pool poolB") {
		t.Errorf("expected per-pool log 'Found 5 tickets in pool poolB', got logs:\n%s", logs)
	}
	if !strings.Contains(logs, "Matching among 7 tickets from 2 provided pools") {
		t.Errorf("expected deduplicated ticket count log 'Matching among 7 tickets from 2 provided pools', got logs:\n%s", logs)
	}
	if !strings.Contains(logs, "with roster of 4 tickets") {
		t.Errorf("expected full match log 'with roster of 4 tickets', got logs:\n%s", logs)
	}

	if len(stream.responses) != 2 {
		t.Fatalf("got %d matches, want 2", len(stream.responses))
	}

	// Verify Match 0 (full match: score 100, 4 tickets).
	fullMatch := stream.responses[0].GetMatch()
	fullScore, err := extensions.Int32(fullMatch.GetExtensions(), "score")
	if err != nil || fullScore != 100 {
		t.Fatalf("full match score = %d (err=%v), want 100", fullScore, err)
	}
	fullTicketCount := 0
	seenAcrossMatches := make(map[string]struct{})
	for _, r := range fullMatch.GetRosters() {
		for _, tk := range r.GetTickets() {
			fullTicketCount++
			if _, dup := seenAcrossMatches[tk.GetId()]; dup {
				t.Fatalf("duplicate ticket %q in full match", tk.GetId())
			}
			seenAcrossMatches[tk.GetId()] = struct{}{}
		}
	}
	if fullTicketCount != 4 {
		t.Fatalf("full match ticket count = %d, want 4", fullTicketCount)
	}

	// Verify Match 1 (partial match: score 25, 3 tickets, backfill profile with desiredRosterLen = 1).
	partialMatch := stream.responses[1].GetMatch()
	partialScore, err := extensions.Int32(partialMatch.GetExtensions(), "score")
	if err != nil || partialScore != 25 {
		t.Fatalf("partial match score = %d (err=%v), want 25", partialScore, err)
	}
	partialTicketCount := 0
	for _, r := range partialMatch.GetRosters() {
		for _, tk := range r.GetTickets() {
			partialTicketCount++
			if _, dup := seenAcrossMatches[tk.GetId()]; dup {
				t.Fatalf("duplicate ticket %q across matches", tk.GetId())
			}
			seenAcrossMatches[tk.GetId()] = struct{}{}
		}
	}
	if partialTicketCount != 3 {
		t.Fatalf("partial match ticket count = %d, want 3", partialTicketCount)
	}

	gotBfReq, err := extensions.MMFRequest(partialMatch.GetExtensions(), extensions.MMFRequestKey)
	if err != nil {
		t.Fatalf("expected backfill MmfRequest on partial match, got err: %v", err)
	}
	bfProf := gotBfReq.GetProfile()
	if !strings.HasPrefix(bfProf.GetName(), "backfill.") {
		t.Fatalf("backfill profile name = %q, want prefix 'backfill.'", bfProf.GetName())
	}
	bfDesiredLen, err := extensions.Int32(bfProf.GetExtensions(), "desiredRosterLen")
	if err != nil || bfDesiredLen != 1 {
		t.Fatalf("backfill desiredRosterLen = %d (err=%v), want 1", bfDesiredLen, err)
	}
	bfMinLen, err := extensions.Int32(bfProf.GetExtensions(), "minRosterLen")
	if err != nil || bfMinLen != 1 {
		t.Fatalf("backfill minRosterLen = %d (err=%v), want 1", bfMinLen, err)
	}

	// Verify original request profile extensions were not mutated.
	origDesiredLen, err := extensions.Int32(profile.GetExtensions(), "desiredRosterLen")
	if err != nil || origDesiredLen != 4 {
		t.Fatalf("original profile desiredRosterLen was mutated to %d (err=%v), want 4", origDesiredLen, err)
	}
	if _, hasScore := profile.GetExtensions()["score"]; hasScore {
		t.Fatalf("original profile extensions map was mutated with 'score'")
	}

	// Verify partial match with nil profile Extensions (and absent MMFRequestKey) still streams with score=25 without panicking.
	nilExtStream := &fakeRunServer{
		chunks: []*pb.ChunkedMmfRunRequest{
			{
				NumChunks: 1,
				Profile: &pb.Profile{
					Name:       "fifo-nil-ext-profile",
					Extensions: nil,
					Pools: map[string]*pb.Pool{
						"p1": {
							Name: "p1",
							Participants: &pb.Roster{
								Name: "p1",
								Tickets: []*pb.Ticket{
									{Id: "tk1"},
									{Id: "tk2"},
								},
							},
						},
					},
				},
			},
		},
	}
	if err := fifoSrv.Run(nilExtStream); err != nil {
		t.Fatalf("fifo.Run with nil Extensions returned unexpected error: %v", err)
	}
	if len(nilExtStream.responses) != 1 {
		t.Fatalf("fifo.Run with nil Extensions and 2 tickets sent %d matches, want 1", len(nilExtStream.responses))
	}
}

func TestDebugMMFDiagnosticMatchAndExtensionIsolation(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	debugSrv := debug.NewWithLogger(logger)

	t.Run("non_nil_extensions_not_mutated", func(t *testing.T) {
		origExtensions := map[string]*anypb.Any{
			"customParam": mustAnyString(t, "value1"),
		}
		profile := &pb.Profile{
			Name:       "debug-test-profile",
			Extensions: origExtensions,
			Pools: map[string]*pb.Pool{
				"p1": {
					Name: "p1",
					Participants: &pb.Roster{
						Name:    "p1",
						Tickets: []*pb.Ticket{{Id: "t1"}},
					},
				},
			},
		}

		stream := &fakeRunServer{
			chunks: []*pb.ChunkedMmfRunRequest{
				{NumChunks: 1, Profile: profile},
			},
		}

		if err := debugSrv.Run(stream); err != nil {
			t.Fatalf("debug.Run returned unexpected error: %v", err)
		}
		if len(stream.responses) != 1 {
			t.Fatalf("debug.Run sent %d matches, want 1", len(stream.responses))
		}

		match := stream.responses[0].GetMatch()
		if len(match.GetRosters()) != 0 {
			t.Fatalf("debug match rosters count = %d, want 0", len(match.GetRosters()))
		}
		poolCount, err := extensions.Int32(match.GetExtensions(), "debug.pool-ticket-count/p1")
		if err != nil || poolCount != 1 {
			t.Fatalf("debug match debug.pool-ticket-count/p1 = %d (err=%v), want 1", poolCount, err)
		}
		if _, hasScore := match.GetExtensions()["score"]; hasScore {
			t.Fatalf("debug match unexpectedly contains 'score' extension")
		}
		profName, err := extensions.String(match.GetExtensions(), "profileName")
		if err != nil || profName != "debug-test-profile" {
			t.Fatalf("debug match profileName = %q (err=%v), want %q", profName, err, "debug-test-profile")
		}
		customVal, err := extensions.String(match.GetExtensions(), "customParam")
		if err != nil || customVal != "value1" {
			t.Fatalf("debug match customParam = %q (err=%v), want %q", customVal, err, "value1")
		}
		const wantPrefix = "profile-debug-matchfunction.debug-test-profile."
		if !strings.HasPrefix(match.GetId(), wantPrefix) {
			t.Fatalf("debug match ID = %q, want prefix %q", match.GetId(), wantPrefix)
		}
		rem := strings.TrimPrefix(match.GetId(), wantPrefix)
		if rem == "" || strings.Contains(rem, ".") {
			t.Fatalf("debug match ID remainder = %q, want non-empty dot-free string", rem)
		}

		// Ensure original extensions map was not mutated.
		if len(origExtensions) != 1 {
			t.Fatalf("original profile Extensions mutated: got %d keys, want 1", len(origExtensions))
		}
	})

	t.Run("nil_extensions_initialized_safely", func(t *testing.T) {
		profile := &pb.Profile{
			Name:       "debug-nil-ext-profile",
			Extensions: nil,
		}
		stream := &fakeRunServer{
			chunks: []*pb.ChunkedMmfRunRequest{
				{NumChunks: 1, Profile: profile},
			},
		}

		if err := debugSrv.Run(stream); err != nil {
			t.Fatalf("debug.Run with nil Extensions returned unexpected error: %v", err)
		}
		if len(stream.responses) != 1 {
			t.Fatalf("debug.Run sent %d matches, want 1", len(stream.responses))
		}
		match := stream.responses[0].GetMatch()
		if match.GetId() == "" {
			t.Fatalf("expected non-empty match ID")
		}
		if len(match.GetRosters()) != 0 {
			t.Fatalf("debug match rosters count = %d, want 0", len(match.GetRosters()))
		}
		mmfName, err := extensions.String(match.GetExtensions(), "mmfName")
		if err != nil || mmfName != "profile-debug-matchfunction" {
			t.Fatalf("debug match mmfName = %q (err=%v), want %q", mmfName, err, "profile-debug-matchfunction")
		}
	})
}

func TestStartServerWithContextListenErrorAndGracefulStop(t *testing.T) {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	mmfSrv := fifo.NewWithLogger(logger)

	t.Run("invalid_port_returns_error", func(t *testing.T) {
		err := server.StartServerWithContext(context.Background(), -1, mmfSrv, logger)
		if err == nil {
			t.Fatalf("StartServerWithContext with port -1 expected error, got nil")
		}
	})

	t.Run("graceful_stop_on_context_cancel", func(t *testing.T) {
		port := reserveFreePort(t)

		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			errCh <- server.StartServerWithContext(ctx, port, mmfSrv, logger)
		}()

		waitForPortReady(t, port)
		cancel()

		select {
		case serveErr := <-errCh:
			if serveErr != nil {
				t.Fatalf("StartServerWithContext returned unexpected error on graceful stop: %v", serveErr)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for StartServerWithContext to return after ctx cancellation")
		}
	})

	t.Run("concurrent_servers_with_own_loggers", func(t *testing.T) {
		port1 := reserveFreePort(t)
		port2 := reserveFreePort(t)
		if port2 == port1 {
			port2 = reserveFreePort(t)
		}

		logger1 := logrus.New()
		logger1.SetOutput(io.Discard)
		logger2 := logrus.New()
		logger2.SetOutput(io.Discard)

		srv1 := fifo.NewWithLogger(logger1)
		srv2 := fifo.NewWithLogger(logger2)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		errCh1 := make(chan error, 1)
		errCh2 := make(chan error, 1)
		go func() {
			errCh1 <- server.StartServerWithContext(ctx, port1, srv1, logger1)
		}()
		go func() {
			errCh2 <- server.StartServerWithContext(ctx, port2, srv2, logger2)
		}()

		waitForPortReady(t, port1)
		waitForPortReady(t, port2)

		cancel()

		for i, ch := range []chan error{errCh1, errCh2} {
			select {
			case err := <-ch:
				if err != nil {
					t.Fatalf("server %d returned unexpected error: %v", i+1, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out waiting for server %d to stop", i+1)
			}
		}
	})

	t.Run("hung_run_force_stopped_after_deadline", func(t *testing.T) {
		if testing.Short() {
			t.Skip("skipping graceful-stop timeout test in short mode")
		}

		port := reserveFreePort(t)
		blocker := &blockingMMFService{
			entered: make(chan struct{}, 1),
		}
		srvLogger := logrus.New()
		srvLogger.SetOutput(io.Discard)

		srvCtx, srvCancel := context.WithCancel(context.Background())
		defer srvCancel()

		errCh := make(chan error, 1)
		go func() {
			errCh <- server.StartServerWithContext(srvCtx, port, blocker, srvLogger)
		}()

		waitForPortReady(t, port)

		conn, err := grpc.NewClient(
			fmt.Sprintf("127.0.0.1:%d", port),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			t.Fatalf("grpc.NewClient failed: %v", err)
		}
		defer func() { _ = conn.Close() }()

		rpcCtx, rpcCancel := context.WithCancel(context.Background())
		defer rpcCancel()

		client := pb.NewMatchMakingFunctionServiceClient(conn)
		runStream, err := client.Run(rpcCtx)
		if err != nil {
			t.Fatalf("client.Run failed: %v", err)
		}
		if err := runStream.Send(&pb.ChunkedMmfRunRequest{
			NumChunks: 1,
			Profile:   &pb.Profile{Name: "p"},
		}); err != nil {
			t.Fatalf("runStream.Send failed: %v", err)
		}

		select {
		case <-blocker.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for blocking Run handler to be entered")
		}

		cancelAt := time.Now()
		srvCancel()

		select {
		case serveErr := <-errCh:
			if serveErr != nil {
				t.Fatalf("StartServerWithContext returned unexpected error: %v", serveErr)
			}
			elapsed := time.Since(cancelAt)
			if elapsed < 5*time.Second {
				t.Fatalf("StartServerWithContext returned in %v, expected at least 5s graceful wait", elapsed)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("StartServerWithContext did not force-stop within 15s")
		}
	})
}
