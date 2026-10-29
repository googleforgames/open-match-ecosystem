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

// This sample is a reference to demonstrate serving a golang matchmaking
// function using gRPC, and can be used as a starting point for your match
// function. By default it serves the 'fifo' matching function, which makes
// matches using a first-in-first-out (FIFO) strategy, with the number and size
// of rosters coming from the profile's extensions (see functions/fifo); setting
// MMF_FUNCTION to "debug" serves the debug function instead.
//
// A typical approach if you wish to write your mmf in golang would be to make
// a copy of the open-match.dev/open-match-ecosystem/v2/mmf/functions/fifo
// directory, write your own matchmaking logic in the 'Run' function based on
// your game's requirements, rename the module according to what it does,
// and then compile this main program using your function in place of fifo.
//
// A typical production deployment would put that compiled binary into a
// container image to serve from a serverless platform like Cloud Run or
// kNative, or a kubernetes deployment with a service in front of it.
package main

import (
	"context"
	"fmt"
	"os/signal"
	"strings"
	"syscall"

	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"open-match.dev/open-match-ecosystem/v2/examples/mmf/functions/debug"
	"open-match.dev/open-match-ecosystem/v2/examples/mmf/functions/fifo"
	"open-match.dev/open-match-ecosystem/v2/examples/mmf/server"
	"open-match.dev/open-match-ecosystem/v2/internal/logging"
)

func newMMFServer(funcName string, logger *logrus.Logger) (pb.MatchMakingFunctionServiceServer, error) {
	switch strings.ToLower(strings.TrimSpace(funcName)) {
	case "", "fifo":
		return fifo.NewWithLogger(logger), nil
	case "debug":
		return debug.NewWithLogger(logger), nil
	default:
		return nil, fmt.Errorf("unknown MMF_FUNCTION %q (expected 'fifo' or 'debug')", funcName)
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Init viper config with default port.
	// Cloud Run and the GKE manifest set PORT explicitly; the 50090 default is
	// for local runs: it avoids om-core's default port (8080) and matches the
	// standalone gsdirector's default SOLODUEL_PORT, so the two find each other
	// with no config.
	cfg := viper.New()
	cfg.SetDefault("PORT", 50090)
	cfg.SetDefault("MMF_FUNCTION", "fifo")

	// Override these with env vars when doing local development.
	// Suggested values in that case are "text", "debug", and "true",
	// respectively (LOG_CALLER set to "true" adds the calling file and line
	// number to every log line).
	cfg.SetDefault("LOGGING_FORMAT", "json")
	cfg.SetDefault("LOGGING_LEVEL", "info")
	cfg.SetDefault("LOG_CALLER", "false")
	cfg.AutomaticEnv()

	// Initialize logging
	logger := logging.NewSharedLogger(cfg)

	customMatchingLogic, err := newMMFServer(cfg.GetString("MMF_FUNCTION"), logger)
	if err != nil {
		logger.Fatal(err)
	}

	// Start a gRPC server that handles MMF requests
	if err := server.StartServerWithContext(ctx, cfg.GetInt32("PORT"), customMatchingLogic, logger); err != nil {
		logger.Fatal(err)
	}
}
