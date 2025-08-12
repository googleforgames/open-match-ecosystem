### **Assignment Distributor using Pub/Sub**

In a production environment, reliably delivering game server assignments from the director to the matchmaking queue is critical. While the sample includes an implementation using Go channels, this is only suitable for local development \- a distributed and scalable system requires a more robust solution. Google Cloud Pub/Sub is an excellent choice for this use case, offering a number of advantages that align with the demands of a modern matchmaking system.

#### **Why Pub/Sub is a Good Fit**

* **Decoupling of Services**: Pub/Sub allows the director and matchmaking queue to operate independently. The director can publish assignments without needing to know how many matchmaking queue instances are running or where they are located. This loose coupling makes the system more resilient and easier to maintain.
* **Scalability**: Pub/Sub is a fully managed service that can handle millions of messages per second. As your player base grows, you can scale your matchmaking queue instances horizontally, and each instance will receive the assignments from the Pub/Sub topic. This ensures that your assignment delivery system won't become a bottleneck.  Note that the Pub/Sub sample code is focused on simplicity of implementation. If your game is operating at truly huge scale, or you are trying to build a matchmaking system for use by an entire game platform, you will likely want to improve this pattern using a form of sharding topics so that every queue instance is only responsible for a subset of possible assignments.
* **Reliability and Durability**: Pub/Sub provides at-least-once message delivery, ensuring that assignments are not lost in transit. If a matchmaking queue instance goes down, Pub/Sub will retain the messages until another instance can process them. This durability is essential for ensuring that players are not dropped from the matchmaking process.
* **Asynchronous Communication**: The asynchronous nature of Pub/Sub allows the director to publish assignments and move on to its next task without waiting for the matchmaking queue to process them. This improves the overall throughput and responsiveness of the matchmaking system.

#### **An Improvement Over the Open Match 1 Approach**

The shift to a Pub/Sub-based assignment distributor is a significant architectural improvement over the model used in Open Match 1 (OM1). The new approach fundamentally addresses the scalability and complexity issues inherent in the previous design.

In OM1, each game client was responsible for retrieving its own assignment by maintaining a persistent long-polling gRPC stream with the `om-frontend` service. This client-side polling pattern created two major problems:

1. **Scalability Bottleneck**: The `om-frontend` service had to manage a massive number of concurrent connections—one for every player in the matchmaking pool. This consumed substantial resources and became a significant bottleneck as the player count grew.
2. **Complex Client Logic**: Game clients needed to implement and manage the logic for this long-polling loop, making the client-side code more complex and stateful.

The OM2 model, using Pub/Sub, replaces this inefficient pattern with a centralized, push-based system. The responsibility for handling assignments is moved from thousands of individual clients to your own scalable backend services. The director, now the sole publisher of assignments, sends the results to a Pub/Sub topic. Your matchmaking queue instances subscribe to this topic and handle pushing the notification to the relevant clients through your game's existing online services.

This new [architecture](architecture.svg) is vastly more scalable, simplifies game client logic, and aligns with modern best practices for distributed systems by cleanly decoupling the assignment delivery mechanism from the core matchmaking service. While OM2 provides legacy endpoints that replicate the old polling behavior to ease migration, they are deprecated and should not be used in a production environment.

### **Core Components**

The Pub/Sub implementation of the assignment distributor consists of two main components:

* **PubSubPublisher**: This component, used by the director, sends assignments to a Google Cloud Pub/Sub topic. It's important to note that the publisher does **not** create the topic itself; it assumes the topic has already been created and its ID is provided at startup.
* **PubSubSubscriber**: This component, used by the matchmaking queue, receives assignments from a Google Cloud Pub/Sub subscription. For each instance of the subscriber, a unique subscription is created to the specified topic. This subscription is automatically deleted after 24 hours of inactivity to prevent orphaned resources in case of an application crash.

### **Workflow**

#### **1\. Initialization**

The PubSubPublisher is initialized with a Google Cloud Project ID, a Pub/Sub Topic ID, and a logger. It then creates a Pub/Sub client and connects to the specified topic. This sample shows how to implement this alongside the existing Golang channel code so you can select which you want to use at runtime.

*Example from v2/examples/standalone/gsdirector/main.go*:

```go
// Initialize assignment distribution
var publisher assignmentdistributor.Sender
switch cfg.GetString("ASSIGNMENT_DISTRIBUTION_PATH") {
case "channel":
    log.Info("Using Go channels for assignment distribution")
    log.Error("Using Go channels for assignment distribution isn't possible unless the matchmaking queue and game server director are running in the same process, which should only be done when doing active local development. Since this is a standalone game server director process, the go channel will never be read, so assignments are effectively discarded with this configuration.")
    assignmentsChan := make(chan *pb.Roster)
    publisher = assignmentdistributor.NewChannelSender(assignmentsChan)
case "pubsub":
    fallthrough // default is 'pubsub' in the standalone mmqueue
default:
    // note: if using pubsub to send assignments from your director to your
    // matchmaking queue, make sure you have sufficient quota for
    // subscriptions in your GCP project. Each instance of the mmqueue will
    // make a unique topic subscription.
    log.Println("Using Google Cloud Pub/Sub for assignment distribution")

    // Instantiate the Pub/Sub receiver
    publisher = assignmentdistributor.NewPubSubPublisher(
        cfg.GetString("GCP_PROJECT_ID"),
        cfg.GetString("ASSIGNMENT_TOPIC_ID"),
        log,
    )
}
```

The PubSubSubscriber is also initialized with a Project ID, Topic ID, and logger. It creates a Pub/Sub client and then creates a new, unique subscription to the topic. This sample shows how to implement this alongside the existing Golang channel code so you can select which you want to use at runtime.

*Example from v2/examples/standalone/mmqueue/main.go*:

```go
// Initialize assignment distribution
var receiver assignmentdistributor.Receiver
switch cfg.GetString("ASSIGNMENT_DISTRIBUTION_PATH") {
case "channel":
    log.Info("Using Go channels for assignment distribution")
    log.Error("Using Go channels for assignment distribution isn't possible unless the matchmaking queue and game server director are running in the same process, which should only be done when doing active local development. Since this is a standalone matchmaking queue process, there is no game server director sending data on the go channel - assignments are effectively discarded with this configuration.")
    assignmentsChan := make(chan *pb.Roster)
    receiver = assignmentdistributor.NewChannelReceiver(assignmentsChan)
case "pubsub":
    fallthrough // default is 'pubsub' in the standalone mmqueue
default:
    // note: if using pubsub to send assignments from your director to your
    // matchmaking queue, make sure you have sufficient quota for
    // subscriptions in your GCP project. Each instance of the mmqueue will
    // make a unique topic subscription.
    log.Println("Using Google Cloud Pub/Sub for assignment distribution")

    // Instantiate the Pub/Sub receiver
    receiver = assignmentdistributor.NewPubSubSubscriber(
        cfg.GetString("GCP_PROJECT_ID"),
        cfg.GetString("ASSIGNMENT_TOPIC_ID"),
        log,
    )
}
```

#### **2\. Sending Assignments**

When the director has a roster to send, it calls the Send method of the PubSubPublisher. The Send method marshals the roster protobuf message into bytes and publishes it to the Pub/Sub topic.

*Example from v2/internal/assignmentdistributor/pubsub.go*:

```go
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
```

#### **3\. Receiving Assignments**

The matchmaking queue calls the Receive method of the PubSubSubscriber, passing in a handler function. The Receive method uses Pub/Sub's streaming pull feature to listen for messages on its unique subscription. When a message is received, it's unmarshaled back into a roster protobuf message, and the handler function is then executed with the received roster.

*Example from v2/internal/assignmentdistributor/pubsub.go*:

```go
func (r *PubSubSubscriber) Receive(ctx context.Context, handler func(ctx context.Context, roster *pb.Roster)) error {
    return r.sub.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
        var roster pb.Roster
        if err := proto.Unmarshal(msg.Data, &roster); err != nil {
            r.log.Errorf("Failed to unmarshal roster from Pub/Sub message: %v", err)
            msg.Nack() // Nack the message so it can be redelivered
            return
        }

        handler(ctx, &roster)
        // NOTE: In a production system, you likely only want to ack the message after you have successfully delivered the assignment to the game client at least once.
        msg.Ack() // Ack the message to confirm processing.
    })
}
```

#### **4\. Shutdown**

When the application is shutting down, the Stop method is called on both the publisher and subscriber. The PubSubPublisher stops the publisher and closes the client connection. The subscription will be automatically deleted after 24 hours due to the default configuration, but this should only be relied upon to prevent 'leaking' subscriptions \- best practices are to have the PubSubSubscriber delete its unique subscription, cancel its context to stop listening, and close the client connection before the application stops.

*Example from v2/internal/assignmentdistributor/pubsub.go*:

```go

func (p *PubSubPublisher) Stop() {
    // cleanup function for Pub/Sub
    p.log.Infof("Cleaning up assignment pubsub client, stopping topic %s", p.publisher.String())
    p.publisher.Stop()
    p.client.Close() // close the client connection.
}

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
```
