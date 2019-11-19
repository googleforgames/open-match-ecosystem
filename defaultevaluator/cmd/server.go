// Copyright 2019 Google LLC
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
	"fmt"
	"io"
	"net"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"open-match.dev/open-match/pkg/pb"
)

var (
	logger = logrus.WithFields(logrus.Fields{
		"app":       "evaluator",
		"component": "evaluator.server",
	})
)

const port = 50508

// EvaluatorService implements pb.EvaluatorServer.
type EvaluatorService struct {
}

func main() {
	evalService := EvaluatorService{}
	server := grpc.NewServer()
	pb.RegisterEvaluatorServer(server, &evalService)
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		logger.WithFields(logrus.Fields{
			"error": err.Error(),
			"port":  port,
		}).Fatal("net.Listen() error")
	}

	logger.WithFields(logrus.Fields{
		"port": port,
	}).Info("TCP net listener initialized")

	err = server.Serve(ln)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"error": err.Error(),
		}).Fatal("gRPC serve() error")
	}
}

// Evaluate is the implementation of the gRPC call defined in api/evaluator.proto.
func (s *EvaluatorService) Evaluate(stream pb.Evaluator_EvaluateServer) error {
	var proposals = []*pb.Match{}
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		proposals = append(proposals, req.GetMatch())
	}

	logger.WithFields(logrus.Fields{
		"proposals": proposals,
	}).Trace("proposals received by the evaluator")

	results, err := evaluate(proposals)
	if err != nil {
		return status.Error(codes.Aborted, err.Error())
	}

	for _, result := range results {
		if err := stream.Send(&pb.EvaluateResponse{Match: result}); err != nil {
			return err
		}
	}

	logger.WithFields(logrus.Fields{
		"results": results,
	}).Trace("matches accepted by the evaluator")
	return nil
}
