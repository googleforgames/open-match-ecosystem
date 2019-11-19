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
	"math"
	"sort"

	"github.com/golang/protobuf/ptypes"
	"github.com/sirupsen/logrus"
	"open-match.dev/open-match-ecosystem/defaultevaluator"
	"open-match.dev/open-match/pkg/pb"
)

type matchExt struct {
	match *pb.Match
	inp   *defaultevaluator.DefaultEvaluationCriteria
}

func evaluate(proposals []*pb.Match) ([]*pb.Match, error) {
	matches := make([]*matchExt, 0, len(proposals))
	nilEvlautionInputs := 0

	for _, m := range proposals {
		// Evaluation criteria is optional, but sort it lower than any matches which
		// provided criteria.
		inp := &defaultevaluator.DefaultEvaluationCriteria{
			Score: math.Inf(-1),
		}

		if a, ok := m.Extensions["evaluation_input"]; ok {
			err := ptypes.UnmarshalAny(a, inp)
			if err != nil {
				logger.WithFields(logrus.Fields{
					"match_id": m.MatchId,
					"error":    err,
				}).Error("Failed to unmarshal match's DefaultEvaluationCriteria.  Rejecting match.")
				continue
			}
		} else {
			nilEvlautionInputs++
		}
		matches = append(matches, &matchExt{
			match: m,
			inp:   inp,
		})
	}

	if nilEvlautionInputs > 0 {
		logger.WithFields(logrus.Fields{
			"count": nilEvlautionInputs,
		}).Info("Some matches don't have the optional field evaluation_input set.")
	}

	sort.Sort(byScore(matches))

	d := decollider{
		ticketsUsed: make(map[string]*collidingMatch),
	}

	for _, m := range matches {
		d.maybeAdd(m)
	}

	return d.results, nil
}

type collidingMatch struct {
	id    string
	score float64
}

type decollider struct {
	results     []*pb.Match
	ticketsUsed map[string]*collidingMatch
}

func (d *decollider) maybeAdd(m *matchExt) {
	for _, t := range m.match.GetTickets() {
		if cm, ok := d.ticketsUsed[t.Id]; ok {
			logger.WithFields(logrus.Fields{
				"match_id":              m.match.GetMatchId(),
				"ticket_id":             t.GetId(),
				"match_score":           m.inp.GetScore(),
				"colliding_match_id":    cm.id,
				"colliding_match_score": cm.score,
			}).Info("Higher quality match with colliding ticket found. Rejecting match.")
			return
		}
	}

	for _, t := range m.match.GetTickets() {
		d.ticketsUsed[t.Id] = &collidingMatch{
			id:    m.match.GetMatchId(),
			score: m.inp.GetScore(),
		}
	}

	d.results = append(d.results, m.match)
}

type byScore []*matchExt

func (m byScore) Len() int {
	return len(m)
}

func (m byScore) Swap(i, j int) {
	m[i], m[j] = m[j], m[i]
}

func (m byScore) Less(i, j int) bool {
	return m[i].inp.Score > m[j].inp.Score
}
