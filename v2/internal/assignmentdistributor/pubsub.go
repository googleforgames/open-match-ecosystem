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
func NewPubSubPublisher(projectID string, topicID string, log *logrus.Logger) *PubSubPublisher {

	// Get Pub/Sub specific configuration
	if projectID == "" || topicID == "" {
		log.Fatalln("For 'pubsub' assignment distribution, Google Cloud Project ID and an existing Topic ID to use must be provided")
	}

	// Create a Pub/Sub client
	ctx := context.Background()
	client, err := pubsub.NewClient(ctx, projectID)
	if err != nil {
		log.Fatalf("Failed to initialize pubsub client to receive assignments: %v", err)
	}

	// Connect to the Pub/Sub topic & instantiate the Pub/Sub receiver
	publisher := client.Publisher(fmt.Sprintf("projects/%v/topics/%v", projectID, topicID))
	return &PubSubPublisher{client: client, publisher: publisher, log: log}
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

func NewPubSubSubscriber(projectID string, topicID string, log *logrus.Logger) *PubSubSubscriber {
	// Get Pub/Sub specific configuration
	if projectID == "" || topicID == "" {
		log.Fatalln("For 'pubsub' assignment distribution, Google Cloud Project ID and an existing Topic ID to use must be provided")
	}

	// Create a Pub/Sub client
	ctx, cancel := context.WithCancel(context.Background())
	client, err := pubsub.NewClient(ctx, projectID)
	if err != nil {
		log.Fatalf("Failed to initialize pubsub client to receive assignments: %v", err)
	}

	// Unique subscription name for each application instance.
	tName := fmt.Sprintf("projects/%v/topics/%v", projectID, topicID)
	sName := fmt.Sprintf("projects/%v/subscriptions/mmqueue-%s-sub", projectID, uuid.NewString())
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
		log.Fatalf("Failed to create Pub/Sub subscription: %v", err)
	}

	return &PubSubSubscriber{
		client:    client,
		sub:       client.Subscriber(sub.GetName()),
		ctxCancel: cancel,
		log:       log,
	}
}

// Note: This uses pubsub's streaming pull feature. This feature has properties
// that may be surprising. Please take a look at
// https://cloud.google.com/pubsub/docs/pull#streamingpull for more details on
// how streaming pull behaves compared to the synchronous pull method.
func (r *PubSubSubscriber) Receive(ctx context.Context, handler func(ctx context.Context, roster *pb.Roster)) error {
	return r.sub.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
		var roster pb.Roster
		if err := proto.Unmarshal(msg.Data, &roster); err != nil {
			r.log.Errorf("Failed to unmarshal roster from Pub/Sub message: %v", err)
			msg.Nack() // Nack the message so it can be redelivered
			return
		}

		handler(ctx, &roster)
		msg.Ack() // Ack the message to confirm processing
	})
}

// cleanup function for Pub/Sub
func (r *PubSubSubscriber) Stop() {
	r.log.Infof("Cleaning up assignment pubsub subscription %s...", r.sub.String())

	// Use a new, short-lived context to ensure subscription cleanup runs.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	// Cancelling the subscriber's context is the idiomatic way to quit listening to a subscription.
	r.ctxCancel()

	// close the client connection
	r.client.Close()

}
