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
	"io"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

func newDiscardLogger() *logrus.Logger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	return log
}

func TestPubSubSubscriberAcksUnparseableMessages(t *testing.T) {
	srv := pstest.NewServer()
	t.Cleanup(func() {
		_ = srv.Close()
	})
	t.Setenv("PUBSUB_EMULATOR_HOST", srv.Addr)

	const (
		projectID = "test-project"
		topicID   = "assignments"
		topicPath = "projects/test-project/topics/assignments"
	)

	if _, err := srv.GServer.CreateTopic(context.Background(), &pubsubpb.Topic{Name: topicPath}); err != nil {
		t.Fatalf("CreateTopic(%q) failed: %v", topicPath, err)
	}

	sub, err := NewPubSubSubscriber(projectID, topicID, newDiscardLogger())
	if err != nil {
		t.Fatalf("NewPubSubSubscriber failed: %v", err)
	}
	t.Cleanup(sub.Stop)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan string, 4)
	recvDone := make(chan error, 1)
	go func() {
		recvDone <- sub.Receive(ctx, func(_ context.Context, roster *pb.Roster) {
			received <- roster.GetName()
		})
	}()

	// Invalid protobuf wire bytes: length-delimited field 1 (tag 0x0a) whose
	// declared length of 5 exceeds the single following byte (0x01).
	poisonPayload := []byte{0x0a, 0x05, 0x01}
	poisonID := srv.Publish(topicPath, poisonPayload, nil)

	validPayload, err := proto.Marshal(&pb.Roster{Name: "valid-roster"})
	if err != nil {
		t.Fatalf("proto.Marshal failed: %v", err)
	}
	srv.Publish(topicPath, validPayload, nil)

	select {
	case got := <-received:
		if got != "valid-roster" {
			t.Fatalf("received roster %q, want %q", got, "valid-roster")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for valid-roster")
	}

	deadline := time.Now().Add(10 * time.Second)
	poisonAcked := false
	for time.Now().Before(deadline) {
		if msg := srv.Message(poisonID); msg != nil && msg.Acks > 0 {
			poisonAcked = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !poisonAcked {
		t.Fatalf("poison message %q was never acked", poisonID)
	}

	cancel()
	select {
	case err := <-recvDone:
		if err != nil {
			t.Fatalf("Receive returned unexpected error after context cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Receive to return after context cancellation")
	}

	select {
	case extra := <-received:
		t.Fatalf("unexpected extra roster delivered to handler: %q", extra)
	default:
	}
}

func TestPubSubConstructorsMissingConfigReturnError(t *testing.T) {
	log := newDiscardLogger()

	pub, err := NewPubSubPublisher("", "topic", log)
	if err == nil {
		t.Fatal("NewPubSubPublisher with empty project ID returned nil error, want non-nil")
	}
	if pub != nil {
		t.Fatalf("NewPubSubPublisher with empty project ID returned non-nil publisher: %v", pub)
	}

	sub, err := NewPubSubSubscriber("project", "", log)
	if err == nil {
		t.Fatal("NewPubSubSubscriber with empty topic ID returned nil error, want non-nil")
	}
	if sub != nil {
		t.Fatalf("NewPubSubSubscriber with empty topic ID returned non-nil subscriber: %v", sub)
	}
}
