package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"open-match.dev/open-match/pkg/pb"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	allocationv1 "agones.dev/agones/pkg/apis/allocation/v1"
	"agones.dev/agones/pkg/client/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

func main() {
	for {
		if err := run(); err != nil {
			fmt.Println(err.Error())
		}
	}
}

func createOMBackendClient() (pb.BackendClient, func() error) {
	conn, err := grpc.Dial("om-backend.open-match.svc.cluster.local:50505", grpc.WithInsecure())
	if err != nil {
		panic(err)
	}
	return pb.NewBackendClient(conn), conn.Close
}

func createAgonesClient() *versioned.Clientset {
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	agonesClient, err := versioned.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	return agonesClient
}

// Customize the backend.FetchMatches request, the default one will return all tickets in the statestore
func createOMFetchMatchesRequest() *pb.FetchMatchesRequest {
	return &pb.FetchMatchesRequest{
		// om-function:50502 -> the internal hostname & port number of the MMF service in our Kubernetes cluster
		Config: &pb.FunctionConfig{
			Host: "om-function",
			Port: 50502,
			Type: pb.FunctionConfig_GRPC,
		},
		Profiles: []*pb.MatchProfile{
			{
				Name:  "get-all",
				Pools: []*pb.Pool{},
			},
		},
	}
}

func createAgonesGameServerAllocation() *allocationv1.GameServerAllocation {
	return &allocationv1.GameServerAllocation{
		Spec: allocationv1.GameServerAllocationSpec{
			Required: metav1.LabelSelector{
				MatchLabels: map[string]string{agonesv1.FleetNameLabel: "simple-udp"},
			},
		},
	}
}

func createOMAssignTicketRequest(match *pb.Match, gsa *allocationv1.GameServerAllocation) *pb.AssignTicketsRequest {
	tids := []string{}
	for _, t := range match.GetTickets() {
		tids = append(tids, t.GetId())
	}

	return &pb.AssignTicketsRequest{
		TicketIds: tids,
		Assignment: &pb.Assignment{
			Connection: fmt.Sprintf("%s:%d", gsa.Status.Address, gsa.Status.Ports[0].Port),
		},
	}
}

func run() error {
	bc, closer := createOMBackendClient()
	defer closer()

	agonesClient := createAgonesClient()

	stream, err := bc.FetchMatches(context.Background(), createOMFetchMatchesRequest())
	if err != nil {
		fmt.Printf("Director: fail to get response stream from backend.FetchMatches call, desc: %s\n", err.Error())
		return err
	}

	// Read the FetchMatches response. Each loop fetches an available game match that satisfies the match profiles.
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		gsa, err := agonesClient.AllocationV1().GameServerAllocations("default").Create(createAgonesGameServerAllocation())
		if err != nil {
			return err
		}
		if gsa.Status.State != allocationv1.GameServerAllocationAllocated {
			fmt.Printf("failed to allocate game server.\n")
			continue
		}

		if _, err = bc.AssignTickets(context.Background(), createOMAssignTicketRequest(resp.GetMatch(), gsa)); err != nil {
			// Corner case where we allocated a game server for players who left the queue after some waiting time.
			// Note that we may still leak some game servers when tickets got assigned but players left the queue before game frontend announced the assignments.
			if err = agonesClient.AgonesV1().GameServers("default").Delete(gsa.Status.GameServerName, &metav1.DeleteOptions{}); err != nil {
				return err
			}
		}

	}

	time.Sleep(time.Second * 5)
	return nil
}
