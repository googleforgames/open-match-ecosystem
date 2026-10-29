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

package omclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestDeactivateTickets_RequestShape(t *testing.T) {
	var calls atomic.Int32
	var gotMethod string
	var gotPath string
	var gotIDs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed reading body: %v", err)
		}
		var req pb.DeactivateTicketsRequest
		if err := protojson.Unmarshal(body, &req); err != nil {
			t.Errorf("failed unmarshalling DeactivateTicketsRequest: %v", err)
		}
		gotIDs = slices.Clone(req.GetTicketIds())

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	rc, _ := newTestClient(t, srv.URL)
	if err := rc.DeactivateTickets(context.Background(), []string{"a", "b", "a", ""}); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 HTTP request, got %d", got)
	}
	if gotMethod != http.MethodDelete {
		t.Fatalf("expected DELETE method, got %q", gotMethod)
	}
	if gotPath != "/tickets:deactivate" {
		t.Fatalf("expected path /tickets:deactivate, got %q", gotPath)
	}
	if !slices.Equal(gotIDs, []string{"a", "b"}) {
		t.Fatalf("expected deduplicated ticket IDs [a b], got %v", gotIDs)
	}
}

func TestDeactivateTickets_EmptyIsNoOp(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rc, _ := newTestClient(t, srv.URL)
	if err := rc.DeactivateTickets(context.Background(), nil); err != nil {
		t.Fatalf("expected nil error on empty slice, got %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("expected 0 HTTP requests for nil slice, got %d", got)
	}
}

func TestDeactivateTickets_RetryClassification(t *testing.T) {
	t.Run("503 Unavailable retried then succeeds", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write(marshalStatusWithViolations(t, codes.Unavailable, "catching up", nil))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer srv.Close()

		rc, _ := newTestClient(t, srv.URL)
		if err := rc.DeactivateTickets(context.Background(), []string{"t1"}); err != nil {
			t.Fatalf("expected nil error after retry, got %v", err)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("expected 2 HTTP requests, got %d", got)
		}
	})

	t.Run("400 InvalidArgument fails immediately without retry", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(marshalStatusWithViolations(t, codes.InvalidArgument, "invalid ticket id", map[string]string{
				"t1": "invalid ticket id",
			}))
		}))
		defer srv.Close()

		rc, _ := newTestClient(t, srv.URL)
		err := rc.DeactivateTickets(context.Background(), []string{"t1"})
		if err == nil {
			t.Fatal("expected non-nil error on 400 InvalidArgument")
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("expected 1 HTTP request without retry, got %d", got)
		}
	})
}
