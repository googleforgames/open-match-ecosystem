// Copyright 2024 Google LLC
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
package gsdirector

import (
	"github.com/sirupsen/logrus"
	otelmetrics "go.opentelemetry.io/otel/metric"
)

const metricsNamePrefix = "gsdirector."

var (
	otelLogger = logrus.WithFields(logrus.Fields{
		"app":            "matchmaker",
		"component":      "director",
		"implementation": "otel",
	})

	// Metric variable declarations are global, so they can be accessed directly in the application code.
	// Metrics populated by the grpc function implementations in main.go.
	otelTicketsRejected                 otelmetrics.Int64Counter
	otelMMFResponseFailures             otelmetrics.Int64Counter
	otelTicketActivationsPerCycle       otelmetrics.Int64Histogram
	otelTicketAssignmentsPerCycle       otelmetrics.Int64Histogram
	otelNewMMFRequestsProcessedPerCycle otelmetrics.Int64Histogram
	otelMatchesRejectedPerCycle         otelmetrics.Int64Histogram
	otelMatchesProposedPerCycle         otelmetrics.Int64Histogram
	otelMMFsPerOMCall                   otelmetrics.Int64Histogram
	otelMatchmakingCycleDuration        otelmetrics.Float64Histogram
)

//nolint:cyclop // Cyclop linter sees each metric initialization as +1 cyclomatic complexity for some reason.
func registerMetrics(meterPointer *otelmetrics.Meter) {
	meter := *meterPointer
	var err error

	// Initialize all declared Metrics
	otelTicketsRejected, err = meter.Int64Counter(
		metricsNamePrefix+"tickets.rejected",
		otelmetrics.WithDescription("Total tickets rejected"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelMMFResponseFailures, err = meter.Int64Counter(
		metricsNamePrefix+"mmf.reponse.failures",
		otelmetrics.WithDescription("Total MMF response stream errors"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelTicketActivationsPerCycle, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.reactivations",
		otelmetrics.WithDescription("Number of tickets re-activated per cycle due to rejected matches, server allocation failures, etc"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelTicketAssignmentsPerCycle, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.assignments",
		otelmetrics.WithDescription("Number of ticket assignments created per cycle"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelNewMMFRequestsProcessedPerCycle, err = meter.Int64Histogram(
		metricsNamePrefix+"mmf.newrequests",
		otelmetrics.WithDescription("Number of new MMF requests received from the MMF (backfills, join-in-progress, high density game servers, etc) per cycle"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelMatchesRejectedPerCycle, err = meter.Int64Histogram(
		metricsNamePrefix+"matchmaking_cycle.match.rejections",
		otelmetrics.WithDescription("Total tickets set to active, so they appear in pools"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelMatchesProposedPerCycle, err = meter.Int64Histogram(
		metricsNamePrefix+"matchmaking_cycle.match.proposals",
		otelmetrics.WithDescription("Total tickets set to active, so they appear in pools"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelMMFsPerOMCall, err = meter.Int64Histogram(
		metricsNamePrefix+"mmf.count",
		otelmetrics.WithDescription("Number of mmfs requested per OM InvokeMatchmakingFunctions() call"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelMatchmakingCycleDuration, err = meter.Float64Histogram(
		metricsNamePrefix+"matchmaking_cycle.duration",
		otelmetrics.WithDescription("Length of matchmaking cycles"),
		otelmetrics.WithUnit("ms"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}
}
