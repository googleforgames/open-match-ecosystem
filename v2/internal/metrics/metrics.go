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
package metrics

import (
	"context"
	"fmt"
	"net/http"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/detectors/gcp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/prometheus"
	otelmetrics "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"

	"open-match.dev/open-match-ecosystem/v2/internal/logging"
)

// ServiceVersion is the default OpenTelemetry service version attribute.
const ServiceVersion = "2.1.0"

var (
	// Uses the shared application logger, so it follows the LOGGING_LEVEL /
	// LOGGING_FORMAT the application configures with logging.NewSharedLogger,
	// which updates this same logger in place.
	otelLogger = logging.GetSharedLogger().WithFields(logrus.Fields{
		"app":            "matchmaker",
		"component":      "metrics",
		"implementation": "otel",
	})
)

// InitializeOtel sets up the Open Telemetry endpoint to send data to the
// observability backend you've configured (by default, Google Cloud
// Observability Suite). serviceName is recorded as the service.name resource
// attribute so each application's metrics can be told apart, and it can still
// be overridden with the OTEL_SERVICE_NAME environment variable.
// NOTE: the OTEL modules have a set of environment variables they read from
// directly to decide what host/port their endpoint runs on. For more details,
// see
// https://opentelemetry.io/docs/languages/sdk-configuration/otlp-exporter/
func InitializeOtel(serviceName string) (*otelmetrics.Meter, func(context.Context) error) {
	ctx := context.Background()

	// Get resource attributes
	res, err := resource.New(
		context.Background(),
		// Use the GCP resource detector to detect information about the GCP platform
		resource.WithDetectors(gcp.NewDetector()),
		// Discover and provide information about the OpenTelemetry SDK used.
		resource.WithTelemetrySDK(),
		// Open Match attributes
		resource.WithAttributes(
			semconv.ServiceNamespaceKey.String("matchmaker"),
			semconv.ServiceNameKey.String(serviceName),
			semconv.ServiceVersionKey.String(ServiceVersion),
		),
		// Discover and provide attributes from OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME environment variables.
		resource.WithFromEnv(),
	)
	if errors.Is(err, resource.ErrPartialResource) || errors.Is(err, resource.ErrSchemaURLConflict) {
		otelLogger.Println(err) // Log non-fatal issues.
	} else if err != nil {
		otelLogger.Errorf("Failed to create open telemetry resource: %v", err)

	}

	// Init otel exporter for use by the collector sidecar.
	// Gets its configuration directly from OTEL env vars.
	// https://opentelemetry.io/docs/languages/sdk-configuration/otlp-exporter/
	// In the default case (cloud run with collector as a sidecar), the default
	// values work fine.
	exporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithInsecure())
	if err != nil {
		otelLogger.Fatal(err)
	}

	// Otel meter and meterprovider init
	provider := metric.NewMeterProvider(metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exporter)))
	meter := provider.Meter("matchmaker.example")

	return &meter, provider.Shutdown
}

// InitializeOtelWithLocalProm runs a default OpenTelemetry Reader and
// implements prometheus.Collector, allowing it to be used as both a Reader and
// Collector. http://localhost:<port>/metrics . For use with local development.
// serviceName is recorded as the service.name resource attribute, and metrics
// are served from their own ServeMux so the metrics port exposes only /metrics.
func InitializeOtelWithLocalProm(port int, serviceName string) (*otelmetrics.Meter, func(context.Context) error) {
	// Get resource attributes
	res, err := resource.New(
		context.Background(),
		// Use the GCP resource detector to detect information about the GCP platform
		resource.WithDetectors(gcp.NewDetector()),
		// Discover and provide information about the OpenTelemetry SDK used.
		resource.WithTelemetrySDK(),
		// Open Match attributes
		resource.WithAttributes(
			semconv.ServiceNamespaceKey.String("matchmaker"),
			semconv.ServiceNameKey.String(serviceName),
			semconv.ServiceVersionKey.String(ServiceVersion),
		),
		// Discover and provide attributes from OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME environment variables.
		resource.WithFromEnv(),
	)
	if errors.Is(err, resource.ErrPartialResource) || errors.Is(err, resource.ErrSchemaURLConflict) {
		otelLogger.Println(err) // Log non-fatal issues.
	} else if err != nil {
		otelLogger.Errorf("Failed to create open telemetry resource: %v", err)

	}

	// This exporter embeds a default OpenTelemetry Reader and
	// implements prometheus.Collector
	exporter, err := prometheus.New()
	if err != nil {
		otelLogger.Fatal(err)
	}

	// Start the prometheus HTTP server and pass the exporter Collector to it
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		otelLogger.Infof("serving metrics at localhost:%v/metrics", port)
		err := http.ListenAndServe(fmt.Sprintf(":%v", port), mux) //nolint:gosec // Ignoring G114: Use of net/http serve function that has no support for setting timeouts.
		if err != nil {
			fmt.Printf("error serving http: %v", err)
			return
		}
	}()

	// Otel meter and meterprovider init
	provider := metric.NewMeterProvider(metric.WithResource(res), metric.WithReader(exporter))
	meter := provider.Meter("matchmaker.example")

	return &meter, provider.Shutdown
}
