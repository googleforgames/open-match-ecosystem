package assignmentdistributor

import (
	"context"
	"cloud.google.com/go/pubsub"
	"google.golang.org/protobuf/proto"
	"github.com/sirupsen/logrus"
	"github.com/googleforgames/open-match2/v2/pkg/pb"
)

// PubSubPublisher sends assignments to a Google Cloud Pub/Sub topic.
type PubSubPublisher struct {
	topic *pubsub.Topic
	log   *logrus.Logger
}

func NewPubSubPublisher(client *pubsub.Client, topicID string, log *logrus.Logger) *PubSubPublisher {
	topic := client.Topic(topicID)
	return &PubSubPublisher{topic: topic, log: log}
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
	p.topic.Stop()
}

// PubSubSubscriber receives assignments from a Google Cloud Pub/Sub subscription.
type PubSubSubscriber struct {
	subscription *pubsub.Subscription
	log          *logrus.Logger
}

func NewPubSubSubscriber(sub *pubsub.Subscription, log *logrus.Logger) *PubSubSubscriber {
	return &PubSubSubscriber{subscription: sub, log: log}
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
