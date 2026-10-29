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
//
// package omclient is a thin client to do grpc-gateway RESTful HTTP gRPC calls
// with affordances for retry with exponential backoff + jitter.
package omclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	// defaultRequestTimeout bounds each attempt of a unary om-core call
	// (CreateTicket, ActivateTickets, DeactivateTickets, ValidateConnection) when
	// OM_CORE_REQUEST_TIMEOUT_MS is unset or non-positive. It is applied per
	// attempt with a context timeout rather than through http.Client.Timeout,
	// because the director shares one http.Client between these unary calls and
	// the long-lived InvokeMatchmakingFunctions stream, so that client cannot
	// have an overall timeout. Without a per-attempt bound, a black-holed
	// connection would hold an activation slot (see
	// OM_CORE_MAX_CONCURRENT_ACTIVATION_CALLS) forever.
	defaultRequestTimeout = 5 * time.Second

	// tokenMintTimeout bounds how long a request waits for a Cloud Run ID token.
	// Minting talks to the metadata server, so if that is slow, requests fail
	// fast (and their callers retry) instead of hanging.
	tokenMintTimeout = 10 * time.Second

	// defaultMaxConcurrentActivationCalls is the maximum number of concurrent
	// activation batch calls when OM_CORE_MAX_CONCURRENT_ACTIVATION_CALLS is
	// unset or non-positive.
	defaultMaxConcurrentActivationCalls = 10
)

var MMFsComplete = errors.New("Open Match HTTP client detected that the stream of matches from OM was complete and cancelled the context")

// ActivationResult reports the activation outcome for a single ticket ID.
type ActivationResult struct {
	TicketId string
	Err      error
}

type activationTypeContextKey struct{}

// WithActivationType returns a copy of ctx labelled with the activation type
// (for example "activate" for a ticket's first activation, or "reactivate" when
// a director returns tickets from a rejected match to the pool).
// ActivateTicketsWithResults includes the label in its logs so the two kinds of
// activation can be told apart.
func WithActivationType(ctx context.Context, activationType string) context.Context {
	return context.WithValue(ctx, activationTypeContextKey{}, activationType)
}

type tokenSourceFactory func(ctx context.Context, audience string) (oauth2.TokenSource, error)

func defaultTokenSourceFactory(ctx context.Context, audience string) (oauth2.TokenSource, error) {
	return idtoken.NewTokenSource(ctx, audience)
}

// audienceMintingSource creates a new ID token source for its audience every
// time it is asked for a token. It is always wrapped in an
// oauth2.ReuseTokenSource, which caches the token until it expires and makes
// concurrent callers share a single mint instead of each minting their own.
type audienceMintingSource struct {
	factory  tokenSourceFactory
	audience string
}

func (m audienceMintingSource) Token() (*oauth2.Token, error) {
	src, err := m.factory(context.Background(), m.audience)
	if err != nil {
		return nil, fmt.Errorf("Cloud Run service account authentication requires token source, but couldn't get one. idtoken.NewTokenSource: %w", err)
	}
	tok, err := src.Token()
	if err != nil {
		return nil, fmt.Errorf("Cloud Run service account authentication requires a token, but couldn't get one. TokenSource.Token: %w", err)
	}
	return tok, nil
}

type audienceTokenEntry struct {
	source oauth2.TokenSource
}

func newAudienceTokenEntry(factory tokenSourceFactory, audience string) *audienceTokenEntry {
	return &audienceTokenEntry{
		source: oauth2.ReuseTokenSource(nil, audienceMintingSource{
			factory:  factory,
			audience: audience,
		}),
	}
}

// oidcTokenCache holds one caching token source per audience (om-core URL).
// Its mutex only guards the map; tokens are minted outside the lock, so one
// slow mint for an audience never blocks requests to other audiences.
type oidcTokenCache struct {
	mu      sync.Mutex
	factory tokenSourceFactory
	entries map[string]*audienceTokenEntry
}

func newOIDCTokenCache(factory tokenSourceFactory) *oidcTokenCache {
	return &oidcTokenCache{
		factory: factory,
		entries: make(map[string]*audienceTokenEntry),
	}
}

func (c *oidcTokenCache) lookup(ctx context.Context, audience string) (*oauth2.Token, *audienceTokenEntry, error) {
	c.mu.Lock()
	entry, ok := c.entries[audience]
	if !ok {
		entry = newAudienceTokenEntry(c.factory, audience)
		c.entries[audience] = entry
	}
	c.mu.Unlock()

	tok, err := fetchTokenBounded(ctx, entry.source)
	if err != nil {
		return nil, nil, err
	}
	return tok, entry, nil
}

func (c *oidcTokenCache) refresh(ctx context.Context, audience string, rejected *audienceTokenEntry) (*oauth2.Token, error) {
	c.mu.Lock()
	entry, ok := c.entries[audience]
	if !ok || entry == rejected {
		entry = newAudienceTokenEntry(c.factory, audience)
		c.entries[audience] = entry
	}
	c.mu.Unlock()

	return fetchTokenBounded(ctx, entry.source)
}

// fetchTokenBounded fetches a token from src while bounding how long the caller
// waits. oauth2 token sources take no context, so the call runs in its own
// goroutine; if the mint hangs, that goroutine finishes whenever the mint does,
// and later requests share its result through the reusable token source.
func fetchTokenBounded(ctx context.Context, src oauth2.TokenSource) (*oauth2.Token, error) {
	mintCtx, cancel := context.WithTimeout(ctx, tokenMintTimeout)
	defer cancel()

	type tokenOutcome struct {
		tok *oauth2.Token
		err error
	}
	resCh := make(chan tokenOutcome, 1)
	go func() {
		tok, err := src.Token()
		resCh <- tokenOutcome{tok: tok, err: err}
	}()

	select {
	case res := <-resCh:
		return res.tok, res.err
	case <-mintCtx.Done():
		return nil, fmt.Errorf("timed out waiting for a Cloud Run ID token: %w", mintCtx.Err())
	}
}

// RestfulOMGrpcClient calls om-core's gRPC API through its grpc-gateway
// HTTP/JSON endpoints and should be constructed with New.
//
// Authentication:
//   - When running on Cloud Run (detected by the K_REVISION environment
//     variable, which Cloud Run always sets and which reaches the client
//     through Cfg), every request carries a Google-signed ID token for the
//     om-core service, so a Cloud Run om-core that requires authentication
//     accepts it.
//   - Elsewhere (local runs, GKE) no token is sent.
//   - The token's audience is OM_CORE_ADDR, with any trailing "/" ignored. On
//     Cloud Run, OM_CORE_ADDR must therefore be om-core's exact https service
//     URL, and the calling service account needs the Cloud Run Invoker role on
//     it.
type RestfulOMGrpcClient struct {
	Client     *http.Client
	Log        *logrus.Logger
	Cfg        *viper.Viper
	tokenCache *oidcTokenCache
}

// New constructs a RestfulOMGrpcClient initialized with a concurrency-safe per-audience OIDC token cache.
func New(cfg *viper.Viper, log *logrus.Logger, client *http.Client) *RestfulOMGrpcClient {
	return &RestfulOMGrpcClient{
		Client:     client,
		Log:        log,
		Cfg:        cfg,
		tokenCache: newOIDCTokenCache(defaultTokenSourceFactory),
	}
}

func (rc *RestfulOMGrpcClient) requestTimeout() time.Duration {
	if ms := rc.Cfg.GetInt("OM_CORE_REQUEST_TIMEOUT_MS"); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return defaultRequestTimeout
}

// isRetryableStatus reports whether an om-core error response is worth retrying
// (the server was overloaded or temporarily unavailable, rather than rejecting
// the request itself). grpc-gateway maps gRPC codes to HTTP statuses
// (Unavailable to 503, ResourceExhausted to 429, Internal to 500,
// DeadlineExceeded to 504, Aborted to 409, InvalidArgument to 400), and the
// body carries the gRPC status, which is checked too.
func isRetryableStatus(httpStatus int, body []byte) bool {
	if httpStatus == http.StatusRequestTimeout || httpStatus == http.StatusTooManyRequests {
		return true
	}
	if httpStatus >= http.StatusInternalServerError && httpStatus != http.StatusNotImplemented {
		return true
	}
	grpcCode, _, _ := parseActivationErrorBody(body)
	switch grpcCode {
	case codes.Unavailable, codes.ResourceExhausted, codes.Aborted, codes.DeadlineExceeded:
		return true
	default:
		return false
	}
}

// statusErrorFromJSON decodes a google.rpc.Status (as written by grpc-gateway)
// into a gRPC status error, preserving any error details, and falls back to
// codes.Unknown with the raw body as the message if it cannot be decoded.
func statusErrorFromJSON(body []byte, fallbackMsg string) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 {
		var rpcStatus spb.Status
		unmarshalOpts := protojson.UnmarshalOptions{DiscardUnknown: true}
		if err := unmarshalOpts.Unmarshal(trimmed, &rpcStatus); err == nil && rpcStatus.GetCode() != 0 {
			return status.ErrorProto(&rpcStatus)
		}
	}
	msg := string(trimmed)
	if msg == "" {
		msg = fallbackMsg
	}
	return status.Error(codes.Unknown, msg)
}

// CreateTicket Example
// Your platform services layer should take the matchmaking request, add
// attributes your platform services have authority over (ex: MMR, ELO,
// inventory, etc), then call Open Match core on the client's behalf to make a
// matchmaking ticket. In this sense, Open Match sees your platform services
// layer as a 'proxy' for the player's game client.
//
// CreateTicket makes a single attempt, bounded by OM_CORE_REQUEST_TIMEOUT_MS,
// and is meant to be called with backoff.Retry. Errors that retrying cannot fix
// are wrapped in backoff.Permanent; transport errors, timeouts, 429s and 5xx
// responses are returned as-is so they get retried.
//
// Retrying CreateTicket is a trade-off: om-core has no idempotency key, so if
// an attempt times out after om-core has already stored the ticket, the retry
// creates a second ticket. The duplicate is never activated, so it is never
// matched, and om-core expires it after its ticket TTL, but om-core's
// ticket-creation count will be slightly higher than the caller's.
func (rc *RestfulOMGrpcClient) CreateTicket(ctx context.Context, ticket *pb.Ticket) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, rc.requestTimeout())
	defer cancel()

	logger := rc.Log.WithFields(logrus.Fields{
		"component": "open_match_client",
		"operation": "proxy_CreateTicket",
	})

	// Put the ticket into open match CreateTicket request protobuf message.
	reqPb := &pb.CreateTicketRequest{Ticket: ticket}
	resPb := &pb.CreateTicketResponse{}

	// Marshal request into json
	buf, err := protojson.Marshal(reqPb)
	if err != nil {
		// Mark as a permanent error so the backoff library doesn't retry this (invalid input)
		err := backoff.Permanent(err)
		logger.WithFields(logrus.Fields{
			"pb_message": "CreateTicketsRequest",
		}).Errorf("cannot marshal proto to json")
		return "", err
	}

	// HTTP version of the gRPC CreateTicket() call
	resp, err := rc.Post(ctx, rc.Cfg.GetString("OM_CORE_ADDR"), "/tickets", buf)
	if err != nil {
		reqErr := fmt.Errorf("CreateTicket request failed: %w", err)
		logger.WithFields(logrus.Fields{
			"CreateTicketsRequest": reqPb,
		}).Error(reqErr)
		return "", reqErr
	}

	// Include headers in structured logging when using trace log level [slow]
	headerFields := logrus.Fields{}
	if rc.Log.IsLevelEnabled(logrus.TraceLevel) {
		for key, value := range resp.Header {
			headerFields[key] = value
		}
	}
	logger = logger.WithFields(headerFields)

	// Read & close HTTP response body
	body, err := readAllBody(*resp, logger)
	if err != nil {
		// Mark as a permanent error so the backoff library doesn't retry this REST call
		err := backoff.Permanent(err)
		logger.WithFields(logrus.Fields{
			"pb_message": "CreateTicketsResponse",
			"error":      err,
		}).Errorf("cannot read http response body")
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		statusErr := statusErrorFromJSON(body, http.StatusText(resp.StatusCode))
		callErr := fmt.Errorf("CreateTicket failed: HTTP %d: %w", resp.StatusCode, statusErr)
		logger.WithFields(logrus.Fields{
			"CreateTicketsRequest": reqPb,
		}).Error(callErr)
		if !isRetryableStatus(resp.StatusCode, body) {
			return "", backoff.Permanent(callErr)
		}
		return "", callErr
	}

	// Success, unmarshal json back into protobuf
	err = protojson.Unmarshal(body, resPb)
	if err != nil {
		// Mark as a permanent error so the backoff library doesn't retry this REST call
		err := backoff.Permanent(err)
		logger.WithFields(headerFields).WithFields(logrus.Fields{
			"response_body": string(body),
			"pb_message":    "CreateTicketsResponse",
			"error":         err,
		}).Errorf("cannot unmarshal http response body back into protobuf")
		return "", err
	}
	if resPb.GetTicketId() == "" {
		// Mark as a permanent error so the backoff library doesn't retry this REST call
		err := backoff.Permanent(errors.New("CreateTicket returned empty result"))
		logger.Error(err)
		return "", err
	}

	// Successful ticket creation
	logger.Tracef("CreateTicket %v complete", resPb.TicketId)
	return resPb.TicketId, nil
}

func parseActivationErrorBody(body []byte) (codes.Code, map[string]string, bool) {
	if len(bytes.TrimSpace(body)) == 0 {
		return codes.Unknown, nil, false
	}

	var rpcStatus spb.Status
	unmarshalOpts := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshalOpts.Unmarshal(body, &rpcStatus); err != nil {
		return codes.Unknown, nil, false
	}

	st := status.FromProto(&rpcStatus)
	var perTicketErrors map[string]string
	for _, detail := range st.Details() {
		badReq, ok := detail.(*errdetails.BadRequest)
		if !ok || badReq == nil {
			continue
		}
		for _, violation := range badReq.GetFieldViolations() {
			if violation == nil {
				continue
			}
			ticketID := strings.TrimPrefix(violation.GetField(), "ticket_id/")
			if perTicketErrors == nil {
				perTicketErrors = make(map[string]string)
			}
			perTicketErrors[ticketID] = violation.GetDescription()
		}
	}

	return st.Code(), perTicketErrors, len(perTicketErrors) > 0
}

func deduplicateTicketIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	var unique []string
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}

// ActivateTickets delegates to ActivateTicketsWithResults with a nil results channel.
func (rc *RestfulOMGrpcClient) ActivateTickets(ctx context.Context, ticketIdsToActivate <-chan string) {
	rc.ActivateTicketsWithResults(ctx, ticketIdsToActivate, nil)
}

// ActivateTicketsWithResults takes a channel of ticket IDs awaiting activation,
// batches them up to OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL or 250ms, calls
// /tickets:activate with per-ticket status parsing and subset retries, and
// reports per-ticket outcomes to resultsChan when non-nil. At most
// OM_CORE_MAX_CONCURRENT_ACTIVATION_CALLS batches are in flight at once.
func (rc *RestfulOMGrpcClient) ActivateTicketsWithResults(ctx context.Context, ticketIdsToActivate <-chan string, resultsChan chan<- ActivationResult) {
	caller := "unspecified"
	if label, ok := ctx.Value(activationTypeContextKey{}).(string); ok && label != "" {
		caller = label
	}
	logger := rc.Log.WithFields(logrus.Fields{
		"component": "open_match_client",
		"operation": "proxy_ActivateTickets",
		"caller":    caller,
	})

	sendResult := func(ticketID string, err error) {
		if resultsChan == nil {
			return
		}
		select {
		case resultsChan <- ActivationResult{TicketId: ticketID, Err: err}:
		case <-ctx.Done():
		}
	}

	batchChan := make(chan []string)
	go func() {
		bLogger := logger.WithFields(logrus.Fields{
			"suboperation": "create_batches",
		})

		// This value must be no larger than om-core's OM_MAX_STATE_UPDATES_PER_CALL,
		// because om-core rejects larger requests with InvalidArgument and every
		// full batch would fail permanently.
		maxBatchSize := 500
		if configuredMax := rc.Cfg.GetInt("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL"); configuredMax > 0 {
			maxBatchSize = configuredMax
		}

		numBatches := 0
		senderActive := true
		for senderActive {
			deadline := time.NewTimer(250 * time.Millisecond)
			activationBatch := make([]string, 0)
			batchComplete := false

			for !batchComplete {
				select {
				case <-ctx.Done():
					senderActive = false
					batchComplete = true
				case ticketId, ok := <-ticketIdsToActivate:
					if !ok {
						senderActive = false
						batchComplete = true
						break
					}
					activationBatch = append(activationBatch, ticketId)
					if len(activationBatch) >= maxBatchSize {
						batchComplete = true
					}
				case <-deadline.C:
					batchComplete = true
				}
			}
			deadline.Stop()

			if len(activationBatch) > 0 {
				numBatches++
				bLogger.Tracef("queueing batch of %v ticket ids to send for activation", len(activationBatch))
				select {
				case batchChan <- activationBatch:
				case <-ctx.Done():
					senderActive = false
				}
			}
		}

		bLogger.Tracef("processed %v batches before the sender stopped sending ticket ids", numBatches)
		close(batchChan)
	}()

	maxConcurrent := defaultMaxConcurrentActivationCalls
	if configuredConc := rc.Cfg.GetInt("OM_CORE_MAX_CONCURRENT_ACTIVATION_CALLS"); configuredConc > 0 {
		maxConcurrent = configuredConc
	}
	// When all slots are in use, the loop below stops reading from batchChan,
	// which blocks the batching goroutine, which in turn stops draining the
	// caller's ticket-ID channel. That propagates backpressure to the caller
	// instead of letting in-flight activation calls grow without bound when
	// om-core slows down.
	sem := make(chan struct{}, maxConcurrent)

	var activationWg sync.WaitGroup
	for batch := range batchChan {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			continue
		}

		activationWg.Add(1)
		go func(thisBatch []string) {
			defer activationWg.Done()
			defer func() { <-sem }()

			pendingBatch := deduplicateTicketIDs(slices.Clone(thisBatch))
			if len(pendingBatch) == 0 {
				return
			}

			reqTimeout := rc.requestTimeout()

			activateTicketCall := func() error {
				if len(pendingBatch) == 0 {
					return nil
				}

				reqPb, err := protojson.Marshal(&pb.ActivateTicketsRequest{TicketIds: pendingBatch})
				if err != nil {
					logger.WithFields(logrus.Fields{
						"pb_message": "ActivateTicketsRequest",
					}).Errorf("cannot marshal protobuf to json")
					return backoff.Permanent(err)
				}

				// Each attempt is bounded so a black-holed connection cannot hold
				// this activation slot forever.
				attemptCtx, cancelAttempt := context.WithTimeout(ctx, reqTimeout)
				defer cancelAttempt()

				logger.Tracef("ActivateTicket call with %v tickets: %v...", len(pendingBatch), pendingBatch[0])
				resp, err := rc.Post(attemptCtx, rc.Cfg.GetString("OM_CORE_ADDR"), "/tickets:activate", reqPb)
				var respBody []byte
				if resp != nil && resp.Body != nil {
					respBody, _ = io.ReadAll(resp.Body)
					_ = resp.Body.Close()
				}
				if err != nil {
					logger.Errorf("ActivateTickets attempt failed: %v", err)
					return err
				}

				if resp.StatusCode == http.StatusOK {
					for _, id := range pendingBatch {
						sendResult(id, nil)
					}
					pendingBatch = nil
					return nil
				}

				grpcCode, ticketErrs, hasTicketDetails := parseActivationErrorBody(respBody)
				httpErr := fmt.Errorf("%s (%d)", http.StatusText(resp.StatusCode), resp.StatusCode)

				switch {
				case hasTicketDetails && (resp.StatusCode == http.StatusBadRequest || grpcCode == codes.InvalidArgument):
					var retrySlice []string
					var permFailure error
					for _, id := range pendingBatch {
						desc, failed := ticketErrs[id]
						if !failed {
							sendResult(id, nil)
							continue
						}
						if strings.Contains(desc, "Unable to update ticket") || strings.Contains(desc, "code "+"= Internal") {
							retrySlice = append(retrySlice, id)
							continue
						}
						permErr := fmt.Errorf("permanent ticket activation failure for %s: %s", id, desc)
						permFailure = permErr
						logger.Errorf("ActivateTickets permanent failure for ticket %s: %s", id, desc)
						sendResult(id, permErr)
					}
					pendingBatch = retrySlice
					if len(pendingBatch) > 0 {
						logger.Errorf("ActivateTickets attempt had transient failures for %d tickets: %v", len(pendingBatch), httpErr)
						return httpErr
					}
					if permFailure != nil {
						return backoff.Permanent(permFailure)
					}
					return backoff.Permanent(httpErr)

				case !hasTicketDetails && (resp.StatusCode == http.StatusBadRequest || grpcCode == codes.InvalidArgument):
					// Callers get the plain error; the Permanent wrapper is only a signal to RetryNotify.
					logger.Errorf("ActivateTickets permanent failure: %v", httpErr)
					for _, id := range pendingBatch {
						sendResult(id, httpErr)
					}
					pendingBatch = nil
					return backoff.Permanent(httpErr)

				case hasTicketDetails && (resp.StatusCode == http.StatusInternalServerError || grpcCode == codes.Internal):
					var retrySlice []string
					for _, id := range pendingBatch {
						if _, failed := ticketErrs[id]; !failed {
							sendResult(id, nil)
						} else {
							retrySlice = append(retrySlice, id)
						}
					}
					pendingBatch = retrySlice
					if len(pendingBatch) == 0 {
						return nil
					}
					logger.Errorf("ActivateTickets attempt failed for %d tickets: %v", len(pendingBatch), httpErr)
					return httpErr

				case !isRetryableStatus(resp.StatusCode, respBody):
					// Other client errors (401/403 after Do's token handling, 404 wrong
					// OM_CORE_ADDR, 413 batch too large) fail the same way on every retry.
					logger.Errorf("ActivateTickets permanent failure: %v", httpErr)
					for _, id := range pendingBatch {
						sendResult(id, httpErr)
					}
					pendingBatch = nil
					return backoff.Permanent(httpErr)

				default:
					logger.Errorf("ActivateTickets attempt failed: %v", httpErr)
					return httpErr
				}
			}

			// Retry for long enough that an attempt that hit the per-attempt
			// timeout still gets retried; activation is idempotent in om-core,
			// so retrying a call that actually succeeded is safe.
			bOff := backoff.WithContext(
				backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(3*reqTimeout)),
				ctx,
			)
			err := backoff.RetryNotify(
				activateTicketCall,
				bOff,
				func(err error, bo time.Duration) {
					logger.Errorf("ActivateTicket temporary failure (backoff for %v): %v", bo, err)
				},
			)
			if err != nil {
				logger.Errorf("ActivateTickets failed: %v", err)
				for _, id := range pendingBatch {
					sendResult(id, err)
				}
			}
			logger.Trace("ActivateTickets complete")
		}(batch)
	}

	activationWg.Wait()
}

// streamedGatewayEnvelope is one newline-delimited JSON message from om-core's
// grpc-gateway server-streaming endpoint, either a result message or a final
// error message carrying a google.rpc.Status when the RPC ends with an error.
type streamedGatewayEnvelope struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// InvokeMatchmakingFunctions calls om-core's InvokeMatchmakingFunctions RPC
// over the HTTP grpc-gateway and sends every streamed response to respChan
// exactly as om-core returned it, including responses whose Match has no
// tickets and responses with no Match at all. The channel is always closed
// before return. The returned error reports transport, marshalling or decoding
// failures, a non-200 HTTP response (as om-core's gRPC status), or a final
// error message at the end of the stream (how om-core reports that one or more
// MMFs failed, returned as that gRPC status with details, with matches received
// before it still delivered). This lets callers tell "the call failed" apart
// from "no matches were made".
func (rc *RestfulOMGrpcClient) InvokeMatchmakingFunctions(ctx context.Context, req *pb.MmfRequest, respChan chan *pb.StreamedMmfResponse) error {
	// Make sure the output channel always gets closed when this function is finished.
	defer close(respChan)

	logger := rc.Log.WithFields(logrus.Fields{
		"component": "open_match_client",
		"operation": "proxy_InvokeMatchmakingFunctions",
		"url":       rc.Cfg.GetString("OM_CORE_ADDR") + "/matches:fetch",
	})

	// Have to cancel the context to tell the om-core server we're done reading from the stream.
	streamCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(MMFsComplete)

	// Marshal protobuf request to JSON for grpc-gateway HTTP call
	reqPb, err := protojson.Marshal(req)
	if err != nil {
		marshalErr := fmt.Errorf("cannot marshal MmfRequest to json: %w", err)
		logger.WithFields(logrus.Fields{
			"pb_message": "MmfRequest",
		}).Error(marshalErr)
		return marshalErr
	}
	logger.Trace("Marshalled MmfRequest protobuf message to JSON")

	// HTTP version of gRPC InvokeMatchmakingFunctions() call
	logger.Trace("Calling InvokeMatchmakingFunctions()")
	resp, err := rc.Post(streamCtx, rc.Cfg.GetString("OM_CORE_ADDR"), "/matches:fetch", reqPb)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		callErr := fmt.Errorf("InvokeMatchmakingFunctions call failed: %w", err)
		logger.Error(callErr)
		return callErr
	}
	if resp == nil || resp.Body == nil {
		nilErr := errors.New("InvokeMatchmakingFunctions call failed: nil response")
		logger.Error(nilErr)
		return nilErr
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		statusErr := statusErrorFromJSON(body, http.StatusText(resp.StatusCode))
		logger.Errorf("InvokeMatchmakingFunctions call failed with HTTP %d: %v", resp.StatusCode, statusErr)
		return statusErr
	}
	defer resp.Body.Close()

	logger.Tracef("InvokeMatchmakingFunctions call successful: %v", resp.StatusCode)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	unmarshalOpts := protojson.UnmarshalOptions{DiscardUnknown: true}

	var streamErr error
	var decodeErrCount int

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var envelope streamedGatewayEnvelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			decodeErrCount++
			logger.WithFields(logrus.Fields{
				"pb_message":    "StreamedMmfResponse",
				"http_response": string(line),
				"error":         err,
			}).Errorf("cannot unmarshal http response envelope from json")
			continue
		}

		trimmedErr := bytes.TrimSpace(envelope.Error)
		if len(trimmedErr) > 0 && string(trimmedErr) != "null" {
			streamErr = statusErrorFromJSON(trimmedErr, "InvokeMatchmakingFunctions stream ended with an error")
			logger.Errorf("InvokeMatchmakingFunctions stream error: %v", streamErr)
			continue
		}

		trimmedResult := bytes.TrimSpace(envelope.Result)
		if len(trimmedResult) == 0 || string(trimmedResult) == "null" {
			continue
		}

		respPb := &pb.StreamedMmfResponse{}
		if err := unmarshalOpts.Unmarshal(trimmedResult, respPb); err != nil {
			decodeErrCount++
			logger.WithFields(logrus.Fields{
				"pb_message":    "StreamedMmfResponse",
				"http_response": string(trimmedResult),
				"error":         err,
			}).Errorf("cannot unmarshal http response body back into protobuf")
			continue
		}

		// om-core passes every MMF response through, and so does this client.
		logger.Trace("Forwarding StreamedMmfResponse protobuf")
		select {
		case respChan <- respPb:
		case <-streamCtx.Done():
			return fmt.Errorf("InvokeMatchmakingFunctions stopped reading results: %w", context.Cause(streamCtx))
		}
	}

	// If the caller's context ended (for example the director's per-cycle MMF
	// deadline) while blocked reading, report that rather than the resulting read
	// error, so the caller sees its own cancel cause.
	if ctx.Err() != nil {
		return fmt.Errorf("InvokeMatchmakingFunctions stopped reading results: %w", context.Cause(ctx))
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		logger.Errorf("Error reading stream, closing: %v", err)
		return fmt.Errorf("error reading InvokeMatchmakingFunctions stream: %w", err)
	}
	if streamErr != nil {
		return streamErr
	}
	if decodeErrCount > 0 {
		return fmt.Errorf("could not decode %d InvokeMatchmakingFunctions stream message(s)", decodeErrCount)
	}
	return nil
}

// Do makes an HTTP request with the given method at url+path with a byte
// buffer of the JSON payload. A trailing "/" on url is ignored. On Cloud Run
// (K_REVISION set), it attaches a cached ID token for the audience url and, if
// om-core answers 401 Unauthenticated (for example because the token just
// expired), retries once with a freshly minted token. A 403 is returned as-is,
// because it means the service account lacks the Cloud Run Invoker role, which
// a new token will not fix.
func (rc *RestfulOMGrpcClient) Do(ctx context.Context, method string, url string, path string, reqBuf []byte) (*http.Response, error) {
	url = strings.TrimSuffix(url, "/")
	targetURL := url + path

	reqLogger := rc.Log.WithFields(logrus.Fields{
		"url":    targetURL,
		"method": method,
	})

	sendOnce := func(tok *oauth2.Token) (*http.Response, error) {
		req, err := http.NewRequestWithContext(
			ctx,
			method,
			targetURL,
			bytes.NewReader(reqBuf),
		)
		if err != nil {
			reqLogger.WithFields(logrus.Fields{
				"error": err,
			}).Error("cannot create http request with context")
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if tok != nil {
			req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		}

		reqLogger.Trace("Header set, request created")

		if rc.Log.IsLevelEnabled(logrus.TraceLevel) {
			headerFields := make(logrus.Fields, len(req.Header))
			for key, value := range req.Header {
				if strings.EqualFold(key, "Authorization") {
					headerFields[key] = "REDACTED"
				} else {
					headerFields[key] = value
				}
			}
			reqLogger.WithFields(headerFields).Trace("Prepared Headers")
		}

		resp, err := rc.Client.Do(req)
		if err != nil {
			reqLogger.WithFields(logrus.Fields{
				"error": err,
			}).Error("cannot execute http request")
			return nil, err
		}
		return resp, nil
	}

	if rc.Cfg.GetString("K_REVISION") == "" {
		return sendOnce(nil)
	}

	tok, entry, err := rc.tokenCache.lookup(ctx, url)
	if err != nil {
		return nil, err
	}

	resp, err := sendOnce(tok)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}

	if resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	refreshedToken, err := rc.tokenCache.refresh(ctx, url, entry)
	if err != nil {
		return nil, err
	}

	return sendOnce(refreshedToken)
}

// Post makes an HTTP POST request at url+path with a byte buffer of the JSON
// payload. See Do for Cloud Run authentication and 401 refresh behavior.
func (rc *RestfulOMGrpcClient) Post(ctx context.Context, url string, path string, reqBuf []byte) (*http.Response, error) {
	return rc.Do(ctx, http.MethodPost, url, path, reqBuf)
}

// readAllBody is a simple helper function to make sure an HTTP body is completely read and closed.
func readAllBody(resp http.Response, logger *logrus.Entry) ([]byte, error) {
	body, err := io.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		logger.Errorf("cannot read bytes from http response body")
		return nil, err
	}

	return body, err
}

// ValidateConnection checks that the RestfulOMGrpcClient can reach and authenticate
// to the Open Match server by sending an empty ActivateTicketsRequest to /tickets:activate.
func (rc *RestfulOMGrpcClient) ValidateConnection(ctx context.Context, url ...string) error {
	// Bound the check so startup cannot hang on an unreachable om-core.
	ctx, cancel := context.WithTimeout(ctx, rc.requestTimeout())
	defer cancel()

	reqPb, err := protojson.Marshal(&pb.ActivateTicketsRequest{})
	if err != nil {
		return err
	}

	targetURL := rc.Cfg.GetString("OM_CORE_ADDR")
	if len(url) > 0 && url[0] != "" {
		targetURL = url[0]
	}

	resp, err := rc.Post(ctx, targetURL, "/tickets:activate", reqPb)
	if err != nil {
		return err
	}
	if resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadRequest {
		return nil
	}
	return fmt.Errorf("ValidateConnection failed with HTTP status %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
}
