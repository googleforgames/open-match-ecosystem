// Copyright 2025 Google LLC
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

package omclient

import (
	"context"
	"fmt"
	"net/http"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/encoding/protojson"
)

// DeactivateTickets moves the given tickets to om-core's inactive state, so
// they stop appearing in the pools sent to matchmaking functions. Call it when
// a player should no longer be matched: they cancelled, disconnected, or it is
// unknown whether an earlier ActivateTickets call took effect.
//
// om-core has no DeleteTicket RPC. Deactivation is how a ticket is taken out of
// matchmaking, and om-core deletes the ticket itself when it expires
// (OM_CACHE_TICKET_TTL_MS after creation).
//
// Deactivating an already inactive ticket is harmless, so the call is safe to
// retry.
//
// om-core's grpc-gateway maps DeactivateTickets to HTTP DELETE on
// /tickets:deactivate, with the DeactivateTicketsRequest as the JSON body (see
// open-match2's proto/api.yaml).
//
// Each attempt is bounded by OM_CORE_REQUEST_TIMEOUT_MS. Transport errors,
// timeouts, 429s and 5xx responses (including om-core's partial-failure
// Internal status) are retried with exponential backoff for up to three attempt
// timeouts; other errors, such as InvalidArgument for malformed or unknown
// ticket IDs, are returned immediately.
func (rc *RestfulOMGrpcClient) DeactivateTickets(ctx context.Context, ticketIDs []string) error {
	ids := deduplicateTicketIDs(ticketIDs)
	if len(ids) == 0 {
		return nil
	}

	logger := rc.Log.WithFields(logrus.Fields{
		"component": "open_match_client",
		"operation": "proxy_DeactivateTickets",
	})

	reqBuf, err := protojson.Marshal(&pb.DeactivateTicketsRequest{TicketIds: ids})
	if err != nil {
		return fmt.Errorf("cannot marshal DeactivateTicketsRequest: %w", err)
	}

	reqTimeout := rc.requestTimeout()
	targetAddr := rc.Cfg.GetString("OM_CORE_ADDR")

	attemptCall := func() error {
		attemptCtx, cancelAttempt := context.WithTimeout(ctx, reqTimeout)
		defer cancelAttempt()

		resp, err := rc.Do(attemptCtx, http.MethodDelete, targetAddr, "/tickets:deactivate", reqBuf)
		if err != nil {
			return fmt.Errorf("DeactivateTickets request failed: %w", err)
		}

		body, err := readAllBody(*resp, logger)
		if err != nil {
			return fmt.Errorf("DeactivateTickets cannot read response body: %w", err)
		}

		if resp.StatusCode == http.StatusOK {
			return nil
		}

		statusErr := statusErrorFromJSON(body, http.StatusText(resp.StatusCode))
		callErr := fmt.Errorf("DeactivateTickets failed: HTTP %d: %w", resp.StatusCode, statusErr)
		if !isRetryableStatus(resp.StatusCode, body) {
			return backoff.Permanent(callErr)
		}
		return callErr
	}

	bOff := backoff.WithContext(
		backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(3*reqTimeout)),
		ctx,
	)
	err = backoff.RetryNotify(
		attemptCall,
		bOff,
		func(err error, bo time.Duration) {
			logger.Warnf("DeactivateTickets temporary failure (backoff for %v): %v", bo, err)
		},
	)
	if err != nil {
		logger.Errorf("DeactivateTickets failed for %d tickets: %v", len(ids), err)
		return err
	}

	logger.Tracef("DeactivateTickets complete for %d tickets", len(ids))
	return nil
}
