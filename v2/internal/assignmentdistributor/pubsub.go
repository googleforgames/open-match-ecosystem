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
//
// The Pub/Sub assignment distributor returns assignments using Google Cloud
// Pub/Sub.  This is a basic sample of how to use a distributed messaging
// service to deliver assignments from your matchmaker 'director' (the
// component that allocates servers), and the matchmaker 'queue' (the component
// that connects to the game client). It is not tested at production scale and
// should be thoroughly hardened before use in production.
//
// It does not create or delete topics for the publisher (It assumes the topic
// ID you send to it has already been created.), but it does create and delete
// a unique subscription for each subscriber.
package assignmentdistributor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/google/uuid"
	"github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// PubSubPublisher sends assignments to a Google Cloud Pub/Sub topic.
type PubSubPublisher struct {
	client    *pubsub.Client
	publisher *pubsub.Publisher
	log       *logrus.Logger
}

// NewPubSubPublisher does not create or delete topics for the publisher (It assumes the topic
// ID you send to it has already been created.)
//
// On failure it logs the error and returns it rather than exiting the process, so the caller
// can shut down cleanly (flushing metrics and stopping anything it already started) before exiting.
func NewPubSubPublisher(projectID string, topicID string, log *logrus.Logger) (*PubSubPublisher, error) {
	if projectID == "" || topicID == "" {
		err := errors.New("for 'pubsub' assignment distribution, Google Cloud Project ID and an existing Topic ID to use must be provided")
		log.Error(err)
		return nil, err
	}

	ctx := context.Background()
	client, err := pubsub.NewClient(ctx, projectID)
	if err != nil {
		wrappedErr := fmt.Errorf("failed to initialize pubsub client to send assignments: %w", err)
		log.Error(wrappedErr)
		return nil, wrappedErr
	}

	publisher := client.Publisher("projects/" + projectID + "/topics/" + topicID)
	return &PubSubPublisher{client: client, publisher: publisher, log: log}, nil
}

func (p *PubSubPublisher) Send(ctx context.Context, roster *pb.Roster) error {
	// Marshal the protobuf message into bytes
	data, err := proto.Marshal(roster)
	if err != nil {
		p.log.Errorf("Failed to marshal roster: %v", err)
		return err
	}

	// Publish, return any error received.
	msg := &pubsub.Message{Data: data}
	results := p.publisher.Publish(ctx, msg)
	if _, err := results.Get(ctx); err != nil {
		p.log.Errorf("Pubsub topic '%s' produced an error or does not exist. Topic must be created and its ID specified in the config at startup time: %v", p.publisher.String(), err)
		return err
	}
	return nil
}

func (p *PubSubPublisher) Stop() {
	// cleanup function for Pub/Sub
	p.log.Infof("Cleaning up assignment pubsub client, stopping topic %s", p.publisher.String())
	p.publisher.Stop()
	p.client.Close() // close the client connection.
}

// PubSubSubscriber receives assignments from a Google Cloud Pub/Sub subscription.
type PubSubSubscriber struct {
	client    *pubsub.Client
	sub       *pubsub.Subscriber
	ctxCancel context.CancelFunc
	log       *logrus.Logger
}

// NewPubSubSubscriber creates a unique subscription to the given topic for this instance.
//
// On failure it logs the error and returns it rather than exiting the process, so the caller
// can shut down cleanly (flushing metrics and stopping anything it already started) before exiting.
func NewPubSubSubscriber(projectID string, topicID string, log *logrus.Logger) (*PubSubSubscriber, error) {
	if projectID == "" || topicID == "" {
		err := errors.New("for 'pubsub' assignment distribution, Google Cloud Project ID and an existing Topic ID to use must be provided")
		log.Error(err)
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	client, err := pubsub.NewClient(ctx, projectID)
	if err != nil {
		cancel()
		wrappedErr := fmt.Errorf("failed to initialize pubsub client to receive assignments: %w", err)
		log.Error(wrappedErr)
		return nil, wrappedErr
	}

	// Unique subscription name for each application instance.
	tName := "projects/" + projectID + "/topics/" + topicID
	sName := fmt.Sprintf("projects/%s/subscriptions/mmqueue-%s-sub", projectID, uuid.NewString())
	log.Infof("Creating subscription: '%s' for topic: '%s'", sName, tName)
	sub, err := client.SubscriptionAdminClient.CreateSubscription(ctx,
		&pubsubpb.Subscription{
			// The subscription will be automatically deleted by GCP after 24 hours
			// of inactivity. This is a safeguard against orphaned resources if
			// the app crashes without cleaning up.
			ExpirationPolicy: &pubsubpb.ExpirationPolicy{
				Ttl: durationpb.New(24 * time.Hour),
			},
			Name:  sName,
			Topic: tName,
		},
	)
	if err != nil {
		_ = client.Close()
		cancel()
		wrappedErr := fmt.Errorf("failed to create Pub/Sub subscription %s for topic %s: %w", sName, tName, err)
		log.Error(wrappedErr)
		return nil, wrappedErr
	}

	return &PubSubSubscriber{
		client:    client,
		sub:       client.Subscriber(sub.GetName()),
		ctxCancel: cancel,
		log:       log,
	}, nil
}

// Receive listens for assignment messages on this subscriber's subscription and
// calls handler for each valid roster. Receive returns when its context is cancelled.
//
// Every subscriber has its own subscription, so every mmqueue instance receives
// every assignment, including those for other instances' tickets, which the
// handler is expected to ignore. This keeps the sample simple, but means each
// instance processes all assignment traffic; see the README for ways to shard
// it at larger scale.
//
// Note: This uses pubsub's streaming pull feature. This feature has properties
// that may be surprising. Please take a look at
// https://cloud.google.com/pubsub/docs/pull#streamingpull for more details on
// how streaming pull behaves compared to the synchronous pull method.
func (r *PubSubSubscriber) Receive(ctx context.Context, handler func(ctx context.Context, roster *pb.Roster)) error {
	return r.sub.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
		var roster pb.Roster
		if err := proto.Unmarshal(msg.Data, &roster); err != nil {
			// A message that cannot be parsed now never will be, so Ack it to
			// discard it; a Nack would have Pub/Sub redeliver it immediately,
			// forever. In production, configure a dead-letter topic on the
			// subscription to keep such messages for analysis.
			r.log.Errorf("Discarding Pub/Sub message %s: cannot unmarshal roster: %v", msg.ID, err)
			msg.Ack()
			return
		}

		handler(ctx, &roster)
		msg.Ack() // Ack the message to confirm processing
	})
}

// Stop deletes this subscriber's subscription and closes the client. Callers
// should stop receiving first, by cancelling the context passed to Receive.
func (r *PubSubSubscriber) Stop() {
	r.log.Infof("Cleaning up assignment pubsub subscription %s...", r.sub.String())

	// Use a fresh context because the application's context is usually already
	// cancelled during shutdown. Keep the timeout short so cleanup fits in
	// Cloud Run's shutdown grace period (the example mains give this step 3
	// seconds); if deletion fails, the subscription's 24-hour expiration policy
	// removes it eventually.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := r.client.SubscriptionAdminClient.DeleteSubscription(
		cleanupCtx,
		&pubsubpb.DeleteSubscriptionRequest{Subscription: r.sub.String()},
	)
	if err != nil {
		r.log.Errorf("Failed to delete subscription %s: %v", r.sub.String(), err)
	} else {
		r.log.Infof("Successfully deleted subscription %s", r.sub.String())
	}

	// Cancel the context the client was created with, then close the client connection.
	r.ctxCancel()
	r.client.Close()
}
