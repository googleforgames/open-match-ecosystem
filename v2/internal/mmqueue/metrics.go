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
	otelTicketAssignments                  otelmetrics.Int64Counter
	otelTicketCreationRetries              otelmetrics.Int64Counter
	otelTicketCreations                    otelmetrics.Int64Counter
	otelTicketsGeneratedPerSecond          otelmetrics.Int64ObservableGauge
	otelTicketGenerationsAchievedPerSecond otelmetrics.Int64Histogram
	otelActivationsPerCall                 otelmetrics.Int64Histogram
)

//nolint:cyclop // Cyclop linter sees each metric initialization as +1 cyclomatic complexity for some reason.
func registerMetrics(meterPointer *otelmetrics.Meter) {
	meter := *meterPointer
	var err error

	// Initialize all declared Metrics
	otelTicketsGeneratedPerSecond, err = meter.Int64ObservableGauge(
		metricsNamePrefix+"tps.request",
		otelmetrics.WithDescription("Number of test tickets generations attempted per second"),
		otelmetrics.WithUnit("tickets/second"),
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

	otelTicketGenerationsAchievedPerSecond, err = meter.Int64Histogram(
		metricsNamePrefix+"tps.actual",
		otelmetrics.WithDescription("Actual number of tickets successfully created each second"),
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

}
