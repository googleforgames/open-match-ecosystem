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
package mmqueue

import (
	"github.com/sirupsen/logrus"
	otelmetrics "go.opentelemetry.io/otel/metric"
)

const metricsNamePrefix = "mmqueue."

var (
	otelLogger = logrus.WithFields(logrus.Fields{
		"app":            "matchmaker",
		"component":      "queue",
		"implementation": "otel",
	})

	// Metric variable declarations are global, so they can be accessed directly in the application code.
	// Metrics populated by the grpc function implementations in main.go.
	otelTicketAssignments                 otelmetrics.Int64Counter
	otelTicketCreationRetries             otelmetrics.Int64Counter
	otelTicketCreations                   otelmetrics.Int64Counter
	otelTicketDeletionFailures            otelmetrics.Int64Counter
	otelTicketsGeneratedPerCycle          otelmetrics.Int64ObservableGauge
	otelTicketGenerationsAchievedPerCycle otelmetrics.Int64Histogram
	otelTicketGenerationCycleDurations    otelmetrics.Int64Histogram
	otelActivationsPerCall                otelmetrics.Int64Histogram
	otelTicketQueuedDurations             otelmetrics.Float64Histogram
)

//nolint:cyclop // Cyclop linter sees each metric initialization as +1 cyclomatic complexity for some reason.
func registerMetrics(meterPointer *otelmetrics.Meter) {
	meter := *meterPointer
	var err error

	// Initialize all declared Metrics
	otelTicketsGeneratedPerCycle, err = meter.Int64ObservableGauge(
		metricsNamePrefix+"tpc.request",
		otelmetrics.WithDescription("Number of test tickets generations attempted per cycle (default 1 second)"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelTicketQueuedDurations, err = meter.Float64Histogram(
		metricsNamePrefix+"ticket.queued_durations",
		otelmetrics.WithDescription("Length of time spent by tickets in the queue"),
		otelmetrics.WithUnit("ms"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelActivationsPerCall, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.activation.requests",
		otelmetrics.WithDescription("Total tickets set to active, so they appear in pools"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelTicketGenerationCycleDurations, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.generation.cycle_duration",
		otelmetrics.WithDescription("Actual duration of ticket creation cycles"),
		otelmetrics.WithUnit("ms"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}
	otelTicketGenerationsAchievedPerCycle, err = meter.Int64Histogram(
		metricsNamePrefix+"tpc.actual",
		otelmetrics.WithDescription("Actual number of tickets successfully created each cycle"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelTicketCreations, err = meter.Int64Counter(
		metricsNamePrefix+"ticket.creations",
		otelmetrics.WithDescription("Total tickets created since startup"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelTicketCreationRetries, err = meter.Int64Counter(
		metricsNamePrefix+"ticket.creation.retries",
		otelmetrics.WithDescription("Total number of times that ticket creation failed and was retried"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelTicketAssignments, err = meter.Int64Counter(
		metricsNamePrefix+"ticket.assignments",
		otelmetrics.WithDescription("Total ticket assignments received"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	otelTicketDeletionFailures, err = meter.Int64Counter(
		metricsNamePrefix+"ticket.deletion.failures",
		otelmetrics.WithDescription("Total ticket deletion failures"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

}
