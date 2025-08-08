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
// function.  This sample uses the 'fifo' matching function to create 1v1
// matches, using a first-in-first-out (FIFO) strategy.
//
// A typical approach if you wish to write your mmf in golang would be to make
// a copy of the open-match.dev/open-match-ecosystem/v2/mmf/functions/fifo
// directory, write your own matchmaking logic in the 'Run' function based on
// your game's requirements, rename the module it according to what it does,
// and then compile this main program using your function in place of fifo.
//
// A typical production deployment would put that compiled binary into a
// continer image to serve from a serverless platform like Cloud Run or
// kNative, or a kubernetes deployment with a service in front of it.
package main

import (
	"github.com/spf13/viper"
	mmf "open-match.dev/open-match-ecosystem/v2/examples/mmf/functions/debug"
	"open-match.dev/open-match-ecosystem/v2/examples/mmf/server"
	"open-match.dev/open-match-ecosystem/v2/internal/logging"
)

func main() {

	// Init viper config with default port
	cfg := viper.New()
	cfg.SetDefault("PORT", 8080)
	cfg.AutomaticEnv()

	// Override these with env vars when doing local development.
	// Suggested values in that case are "text", "debug", and "false",
	// respectively
	cfg.SetDefault("LOGGING_FORMAT", "json")
	cfg.SetDefault("LOGGING_LEVEL", "info")
	cfg.SetDefault("LOG_CALLER", "false")

	// Initialize logging
	logger := logging.NewSharedLogger(cfg)

	customMatchingLogic := mmf.NewWithLogger(logger)

	// Start a gRPC server that handles MMF requests
	server.StartServer(cfg.GetInt32("PORT"), customMatchingLogic, logger)
}
