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
	"context"

	"github.com/sirupsen/logrus"
	otelmetrics "go.opentelemetry.io/otel/metric"
)

const metricsNamePrefix = "mmqueue."

var otelLogger = logrus.WithFields(logrus.Fields{
	"app":            "matchmaker",
	"component":      "queue",
	"implementation": "otel",
})

func (q *MatchmakerQueue) ensureMetricsRegistered() {
	if q.OtelMeterPtr == nil || *q.OtelMeterPtr == nil {
		return
	}
	q.metricsOnce.Do(func() {
		q.registerMetrics()
	})
}

//nolint:cyclop // Cyclop linter sees each metric initialization as +1 cyclomatic complexity for some reason.
func (q *MatchmakerQueue) registerMetrics() {
	if q.OtelMeterPtr == nil || *q.OtelMeterPtr == nil {
		return
	}
	meter := *q.OtelMeterPtr
	var err error

	q.tpcRequestGauge, err = meter.Int64ObservableGauge(
		metricsNamePrefix+"tpc.request",
		otelmetrics.WithDescription("Number of test tickets generations attempted per cycle (default 1 second)"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	q.ticketQueuedDurationsHistogram, err = meter.Float64Histogram(
		metricsNamePrefix+"ticket.queued_durations",
		otelmetrics.WithDescription("Length of time spent by tickets in the queue"),
		otelmetrics.WithUnit("ms"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	q.ticketActivationsCounter, err = meter.Int64Counter(
		metricsNamePrefix+"ticket.activations",
		otelmetrics.WithDescription("Total number of tickets activated"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	q.ticketActivationFailuresCounter, err = meter.Int64Counter(
		metricsNamePrefix+"ticket.activations.failures",
		otelmetrics.WithDescription("Total number of ticket activations that permanently failed"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	q.ticketGenerationDurationHistogram, err = meter.Int64Histogram(
		metricsNamePrefix+"ticket.generation.cycle_duration",
		otelmetrics.WithDescription("Actual duration of ticket creation cycles"),
		otelmetrics.WithUnit("ms"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	q.tpcActualHistogram, err = meter.Int64Histogram(
		metricsNamePrefix+"tpc.actual",
		otelmetrics.WithDescription("Actual number of tickets successfully created each cycle"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	q.ticketCreationsCounter, err = meter.Int64Counter(
		metricsNamePrefix+"ticket.creations",
		otelmetrics.WithDescription("Total tickets created since startup"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	q.ticketCreationRetriesCounter, err = meter.Int64Counter(
		metricsNamePrefix+"ticket.creation.retries",
		otelmetrics.WithDescription("Total number of times that ticket creation failed and was retried"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	q.ticketAssignmentsCounter, err = meter.Int64Counter(
		metricsNamePrefix+"ticket.assignments",
		otelmetrics.WithDescription("Total ticket assignments received"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	q.ticketDeletionFailuresCounter, err = meter.Int64Counter(
		metricsNamePrefix+"ticket.deletion.failures",
		otelmetrics.WithDescription("Total ticket deletion failures"),
	)
	if err != nil {
		otelLogger.Fatal(err)
	}

	_, err = meter.RegisterCallback(
		func(_ context.Context, o otelmetrics.Observer) error {
			q.tpcMu.Lock()
			tpc := int64(q.TPC)
			q.tpcMu.Unlock()
			o.ObserveInt64(q.tpcRequestGauge, tpc)
			return nil
		},
		q.tpcRequestGauge,
	)
	if err != nil {
		otelLogger.Fatal(err)
	}
}
