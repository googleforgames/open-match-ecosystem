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

package logging

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

func TestSharedLoggerUsableBeforeInit(t *testing.T) {
	shared := GetSharedLogger()
	if shared.GetLevel() != logrus.InfoLevel {
		t.Fatalf("got level %v, want %v", shared.GetLevel(), logrus.InfoLevel)
	}
	if _, ok := shared.Formatter.(*logrus.JSONFormatter); !ok {
		t.Fatalf("got formatter %T, want *logrus.JSONFormatter", shared.Formatter)
	}

	origOut := shared.Out
	var buf bytes.Buffer
	shared.SetOutput(&buf)
	defer shared.SetOutput(origOut)

	shared.Info("early message")
	if !strings.Contains(buf.String(), "early message") {
		t.Fatalf("expected buffer to contain %q, got %q", "early message", buf.String())
	}
}

func TestNewSharedLoggerAppliesConfigInPlace(t *testing.T) {
	shared := GetSharedLogger()
	origOut := shared.Out
	var buf bytes.Buffer
	shared.SetOutput(&buf)
	defer func() {
		shared.SetOutput(origOut)
		shared.SetFormatter(cloudRunJSONFormatter())
		shared.SetLevel(logrus.InfoLevel)
	}()

	cfg := viper.New()
	cfg.Set("LOGGING_FORMAT", "text")
	cfg.Set("LOGGING_LEVEL", "debug")

	configured := NewSharedLogger(cfg)
	if configured != shared {
		t.Fatalf("NewSharedLogger returned %p, want %p", configured, shared)
	}
	if configured.GetLevel() != logrus.DebugLevel {
		t.Fatalf("got level %v, want %v", configured.GetLevel(), logrus.DebugLevel)
	}
	if _, ok := configured.Formatter.(*logrus.TextFormatter); !ok {
		t.Fatalf("got formatter %T, want *logrus.TextFormatter", configured.Formatter)
	}
	if !strings.Contains(buf.String(), "format text") {
		t.Fatalf("expected buffer to contain %q, got %q", "format text", buf.String())
	}
}
