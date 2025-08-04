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

	"cloud.google.com/go/pubsub"
	"github.com/google/uuid"
	"github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

// PubSubPublisher sends assignments to a Google Cloud Pub/Sub topic.
type PubSubPublisher struct {
	client *pubsub.Client
	topic  *pubsub.Topic
	log    *logrus.Logger
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

	// Connect to the Pub/Sub topic
	topic := client.Topic(topicID)
	if exists, err := topic.Exists(ctx); err != nil || !exists {
		log.Fatalf("Pubsub topic '%s' produced an error or does not exist. Topic must be created and its ID specified in the config at startup time: %v", topicID, err)
	}

	// Instantiate the Pub/Sub receiver
	return &PubSubPublisher{client: client, topic: topic, log: log}
}

func (p *PubSubPublisher) Send(ctx context.Context, roster *pb.Roster) error {
	// Marshal the protobuf message into bytes
	data, err := proto.Marshal(roster)
	if err != nil {
		p.log.Errorf("Failed to marshal roster: %v", err)
		return err
	}

	msg := &pubsub.Message{Data: data}
	result := p.topic.Publish(ctx, msg)

	// Block until the result is returned and log any errors.
	if _, err := result.Get(ctx); err != nil {
		p.log.Errorf("Failed to publish message to Pub/Sub: %v", err)
		return err
	}
	return nil
}

func (p *PubSubPublisher) Stop() {
	// cleanup function for Pub/Sub
	p.log.Infof("Cleaning up assignment pubsub client, stopping topic %s", p.topic.ID())
	p.client.Close() // close the client connection.
}

// PubSubSubscriber receives assignments from a Google Cloud Pub/Sub subscription.
type PubSubSubscriber struct {
	client       *pubsub.Client
	subscription *pubsub.Subscription
	log          *logrus.Logger
}

func NewPubSubSubscriber(projectID string, topicID string, log *logrus.Logger) *PubSubSubscriber {
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

	// Connect to the Pub/Sub topic
	topic := client.Topic(topicID)
	if exists, err := topic.Exists(ctx); err != nil || !exists {
		log.Fatalf("Pubsub topic '%s' produced an error or does not exist. Topic must be created and its ID specified in the config at startup time: %v", topicID, err)
	}

	// Unique subscription name for each application instance.
	subID := fmt.Sprintf("mmqueue-%s-sub", uuid.NewString())
	log.Infof("Creating subscription: %s", subID)
	sub, err := client.CreateSubscription(ctx, subID, pubsub.SubscriptionConfig{
		Topic:       topic,
		AckDeadline: 20 * time.Second,
		// The subscription will be automatically deleted by GCP after 24 hours
		// of inactivity. This is a safeguard against orphaned resources if
		// the app crashes without cleaning up.
		ExpirationPolicy: 24 * time.Hour,
	})
	if err != nil {
		log.Fatalf("Failed to create Pub/Sub subscription: %v", err)
	}

	return &PubSubSubscriber{client: client, subscription: sub, log: log}
}

func (r *PubSubSubscriber) Receive(ctx context.Context, handler func(ctx context.Context, roster *pb.Roster)) error {
	return r.subscription.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
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
	r.log.Infof("Cleaning up assignment pubsub subscription %s...", r.subscription.ID())

	// Use a new, short-lived context to ensure cleanup runs.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := r.subscription.Delete(cleanupCtx); err != nil {
		r.log.Errorf("Failed to delete subscription %s: %v", r.subscription.ID(), err)
	} else {
		r.log.Infof("Successfully deleted subscription %s", r.subscription.ID())
	}

	// close the client connection
	r.client.Close()

}
