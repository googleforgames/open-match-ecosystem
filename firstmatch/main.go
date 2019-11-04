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
	"time"

	"open-match.dev/open-match-ecosystem/demoui"
)

func main() {
	demoui.Run(map[string]func(demoui.SetFunc){
		"clients":  runClients,
		"director": runDirector,
		"uptime":   runUptime,
		"mmf":      runMmf,
	})
}

func runUptime(update demoui.SetFunc) {
	start := time.Now()
	for now := range time.Tick(time.Second) {
		update(int64(now.Sub(start).Seconds()))
	}
}
