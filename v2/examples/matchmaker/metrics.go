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
package main

import (
	"log"
	_ "net/http"
	_ "net/http/pprof"

	"go.opentelemetry.io/otel/exporters/prometheus"
	otelmetrics "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
)

var (
	// Metric variable declarations are global, so they can be access directly in the application code.
	createdTicketCounter   otelmetrics.Int64Counter
	assignedTicketCounter  otelmetrics.Int64Counter
	acceptedMatchesCounter otelmetrics.Int64Counter
	rejectedMatchesCounter otelmetrics.Int64Counter
	activeTicketsGauge     otelmetrics.Int64ObservableGauge
	metricsNamePrefix      = "matchmaker_example-"
)

func registerMetrics() {
	var err error

	// The exporter embeds a default OpenTelemetry Reader and
	// implements prometheus.Collector, allowing it to be used as
	// both a Reader and Collector.
	exporter, err := prometheus.New()
	if err != nil {
		log.Fatal(err)
	}
	provider := metric.NewMeterProvider(metric.WithReader(exporter))
	meter := provider.Meter("open-match.dev/matchmaker_example")

	// Initialize all declared Metrics
	createdTicketCounter, err = meter.Int64Counter(
		metricsNamePrefix+"tickets_created",
		otelmetrics.WithDescription("Total tickets created"),
	)
	if err != nil {
		log.Fatal(err)
	}
	assignedTicketCounter, err = meter.Int64Counter(
		metricsNamePrefix+"tickets_assigned",
		otelmetrics.WithDescription("Total tickets assigned"),
	)
	if err != nil {
		log.Fatal(err)
	}
	acceptedMatchesCounter, err = meter.Int64Counter(
		metricsNamePrefix+"matches_accepted",
		otelmetrics.WithDescription("Total matches accepted"),
	)
	if err != nil {
		log.Fatal(err)
	}
	rejectedMatchesCounter, err = meter.Int64Counter(
		metricsNamePrefix+"matches_rejected",
		otelmetrics.WithDescription("Total matches rejected"),
	)
	if err != nil {
		log.Fatal(err)
	}

	// This is the equivalent of prometheus.NewCounterVec
	//activeTicketsGauge, err = meter.Int64ObservableGauge(
	//	metricsNamePrefix+"tickets_active",
	//	otelmetrics.WithDescription("active tickets"),
	//)
	//if err != nil {
	//	log.Fatal(err)
	//}
	//_, err = meter.RegisterCallback(func(_ context.Context, o otelmetrics.Observer) error {
	//	o.ObserveInt64(activeTicketsGauge, activeTicketCounter.Load())
	//	return nil
	//}, activeTicketsGauge)
	//if err != nil {
	//	log.Fatal(err)
	//}

	// Start the prometheus HTTP server and pass the exporter Collector to it
	//go func() {
	//	log.Printf("serving metrics at localhost:2233/metrics")
	//	http.Handle("/metrics", promhttp.Handler())
	//	err := http.ListenAndServe(":2233", nil) //nolint:gosec // Ignoring G114: Use of net/http serve function that has no support for setting timeouts.
	//	if err != nil {
	//		fmt.Printf("error serving http: %v", err)
	//		return
	//	}
	//}()

}
