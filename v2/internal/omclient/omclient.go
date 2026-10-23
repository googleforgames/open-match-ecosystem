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
	"os"
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

var MMFsComplete = errors.New("Open Match HTTP client detected that the stream of matches from OM was complete and cancelled the context")

// ActivationResult reports the activation outcome for a single ticket ID.
type ActivationResult struct {
	TicketId string
	Err      error
}

type tokenSourceFactory func(ctx context.Context, audience string) (oauth2.TokenSource, error)

func defaultTokenSourceFactory(ctx context.Context, audience string) (oauth2.TokenSource, error) {
	return idtoken.NewTokenSource(ctx, audience)
}

type cachedAudienceToken struct {
	source oauth2.TokenSource
	token  *oauth2.Token
}

type oidcTokenCache struct {
	mu      sync.Mutex
	factory tokenSourceFactory
	entries map[string]*cachedAudienceToken
}

func newOIDCTokenCache(factory tokenSourceFactory) *oidcTokenCache {
	if factory == nil {
		factory = defaultTokenSourceFactory
	}
	return &oidcTokenCache{
		factory: factory,
		entries: make(map[string]*cachedAudienceToken),
	}
}

func (c *oidcTokenCache) get(audience string) (*oauth2.Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = make(map[string]*cachedAudienceToken)
	}
	if c.factory == nil {
		c.factory = defaultTokenSourceFactory
	}

	if entry, ok := c.entries[audience]; ok && entry != nil {
		if entry.token != nil && entry.token.Valid() {
			return entry.token, nil
		}
		if entry.source != nil {
			tok, err := entry.source.Token()
			if err == nil && tok != nil && tok.Valid() {
				entry.token = tok
				return tok, nil
			}
		}
		delete(c.entries, audience)
	}

	return c.mintLocked(audience)
}

func (c *oidcTokenCache) refresh(audience string, rejectedAccessToken string) (*oauth2.Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = make(map[string]*cachedAudienceToken)
	}
	if c.factory == nil {
		c.factory = defaultTokenSourceFactory
	}

	if entry, ok := c.entries[audience]; ok && entry != nil {
		if entry.token != nil && entry.token.Valid() && entry.token.AccessToken != rejectedAccessToken {
			return entry.token, nil
		}
		delete(c.entries, audience)
	}

	return c.mintLocked(audience)
}

func (c *oidcTokenCache) mintLocked(audience string) (*oauth2.Token, error) {
	src, err := c.factory(context.Background(), audience)
	if err != nil {
		return nil, fmt.Errorf("Cloud Run service account authentication requires token source, but couldn't get one. idtoken.NewTokenSource: %w", err)
	}
	tok, err := src.Token()
	if err != nil {
		return nil, fmt.Errorf("Cloud Run service account authentication requires a token, but couldn't get one. TokenSource.Token: %w", err)
	}
	c.entries[audience] = &cachedAudienceToken{
		source: src,
		token:  tok,
	}
	return tok, nil
}

type RestfulOMGrpcClient struct {
	Client     *http.Client
	Log        *logrus.Logger
	Cfg        *viper.Viper
	tokenCache *oidcTokenCache
	cacheMu    sync.Mutex
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

func (rc *RestfulOMGrpcClient) ensureTokenCache() *oidcTokenCache {
	rc.cacheMu.Lock()
	defer rc.cacheMu.Unlock()
	if rc.tokenCache == nil {
		rc.tokenCache = newOIDCTokenCache(defaultTokenSourceFactory)
	}
	return rc.tokenCache
}

func (rc *RestfulOMGrpcClient) logger() *logrus.Logger {
	if rc != nil && rc.Log != nil {
		return rc.Log
	}
	return logrus.StandardLogger()
}

func (rc *RestfulOMGrpcClient) httpClient() *http.Client {
	if rc != nil && rc.Client != nil {
		return rc.Client
	}
	return http.DefaultClient
}

func (rc *RestfulOMGrpcClient) kRevision() string {
	if rc != nil && rc.Cfg != nil {
		if rev := rc.Cfg.GetString("K_REVISION"); rev != "" {
			return rev
		}
	}
	return os.Getenv("K_REVISION")
}

// CreateTicket Example
// Your platform services layer should take the matchmaking request, add
// attributes your platform services have authority over (ex: MMR, ELO,
// inventory, etc), then call Open Match core on the client's behalf to make a
// matchmaking ticket. In this sense, Open Match sees your platform services
// layer as a 'proxy' for the player's game client.
func (rc *RestfulOMGrpcClient) CreateTicket(ctx context.Context, ticket *pb.Ticket) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	logger := rc.logger().WithFields(logrus.Fields{
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

	// Include headers in structured logging when using trace log level [slow]
	headerFields := logrus.Fields{}
	if resp != nil && logrus.IsLevelEnabled(logrus.TraceLevel) {
		for key, value := range resp.Header {
			headerFields[key] = value
		}
	}
	logger = logger.WithFields(headerFields)

	// Check for failures
	if resp != nil && resp.StatusCode != http.StatusOK { // HTTP error code
		err := fmt.Errorf("%s (%d)", http.StatusText(resp.StatusCode), resp.StatusCode)
		logger.WithFields(logrus.Fields{
			"CreateTicketsRequest": reqPb,
		}).Errorf("CreateTicket failed: %v", err)
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return "", err
	}
	if err != nil { // HTTP library error
		logger.WithFields(logrus.Fields{
			"CreateTicketsRequest": reqPb,
		}).Errorf("CreateTicket failed: %v", err)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return "", err
	}
	if resp == nil {
		err := errors.New("CreateTicket received nil HTTP response")
		logger.Error(err)
		return "", err
	}

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
// reports per-ticket outcomes to resultsChan when non-nil.
func (rc *RestfulOMGrpcClient) ActivateTicketsWithResults(ctx context.Context, ticketIdsToActivate <-chan string, resultsChan chan<- ActivationResult) {
	logger := rc.logger().WithFields(logrus.Fields{
		"component": "open_match_client",
		"operation": "proxy_ActivateTickets",
		"caller": func() string {
			ts, ok := ctx.Value("activationType").(string)
			if !ok {
				rc.logger().Error("unable to get caller type from context")
				return "undefined"
			}
			return ts
		}(),
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

		maxBatchSize := 500
		if rc.Cfg != nil && rc.Cfg.GetInt("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL") > 0 {
			maxBatchSize = rc.Cfg.GetInt("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL")
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

	var activationWg sync.WaitGroup
	for batch := range batchChan {
		activationWg.Add(1)
		go func(thisBatch []string) {
			defer activationWg.Done()

			pendingBatch := deduplicateTicketIDs(slices.Clone(thisBatch))
			if len(pendingBatch) == 0 {
				return
			}

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

				logger.Tracef("ActivateTicket call with %v tickets: %v...", len(pendingBatch), pendingBatch[0])
				resp, err := rc.Post(ctx, rc.Cfg.GetString("OM_CORE_ADDR"), "/tickets:activate", reqPb)
				var respBody []byte
				if resp != nil && resp.Body != nil {
					respBody, _ = io.ReadAll(resp.Body)
					_ = resp.Body.Close()
				}
				if err != nil {
					logger.Errorf("ActivateTickets attempt failed: %v", err)
					return err
				}
				if resp == nil {
					err := errors.New("ActivateTickets received nil HTTP response")
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
					permErr := backoff.Permanent(httpErr)
					logger.Errorf("ActivateTickets permanent failure: %v", httpErr)
					for _, id := range pendingBatch {
						sendResult(id, permErr)
					}
					pendingBatch = nil
					return permErr

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

				default:
					logger.Errorf("ActivateTickets attempt failed: %v", httpErr)
					return httpErr
				}
			}

			bOff := backoff.WithContext(
				backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(5*time.Second)),
				ctx,
			)
			err := backoff.RetryNotify(
				activateTicketCall,
				bOff,
				func(err error, bo time.Duration) {
					logger.Errorf("ActivateTicket temporary failure (backoff for %v): %v", err, bo)
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

type streamedErrorPayload struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type streamedGatewayEnvelope struct {
	Result json.RawMessage       `json:"result"`
	Error  *streamedErrorPayload `json:"error"`
}

func (rc *RestfulOMGrpcClient) InvokeMatchmakingFunctions(ctx context.Context, req *pb.MmfRequest, respChan chan *pb.StreamedMmfResponse) {
	// Make sure the output channel always gets closed when this function is finished.
	defer close(respChan)

	logger := rc.logger().WithFields(logrus.Fields{
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
		logger.WithFields(logrus.Fields{
			"pb_message": "MmfRequest",
		}).Errorf("cannot marshal protobuf to json")
		return
	}
	logger.Trace("Marshalled MmfRequest protobuf message to JSON")

	// HTTP version of gRPC InvokeMatchmakingFunctions() call
	logger.Trace("Calling InvokeMatchmakingFunctions()")
	resp, err := rc.Post(streamCtx, rc.Cfg.GetString("OM_CORE_ADDR"), "/matches:fetch", reqPb)
	if err != nil {
		logger.Errorf("InvokeMatchmakingFunction call failed: %v", err)
		if resp != nil && resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		return
	}
	if resp == nil || resp.Body == nil {
		logger.Error("InvokeMatchmakingFunction call failed: nil response")
		return
	}
	if resp.StatusCode != http.StatusOK {
		logger.Errorf("InvokeMatchmakingFunction call failed: %v", resp.StatusCode)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return
	}
	defer resp.Body.Close()

	logger.Tracef("InvokeMatchmakingFunction call successful: %v", resp.StatusCode)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	unmarshalOpts := protojson.UnmarshalOptions{DiscardUnknown: true}

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		var envelope streamedGatewayEnvelope
		if err := json.Unmarshal(line, &envelope); err != nil {
			logger.WithFields(logrus.Fields{
				"pb_message":    "StreamedMmfResponse",
				"http_response": string(line),
				"error":         err,
			}).Errorf("cannot unmarshal http response envelope from json")
			continue
		}

		if envelope.Error != nil {
			logger.Errorf("InvokeMatchmakingFunctions mid-stream RPC error (code=%d): %s", envelope.Error.Code, envelope.Error.Message)
			continue
		}

		trimmedResult := bytes.TrimSpace(envelope.Result)
		if len(trimmedResult) == 0 || string(trimmedResult) == "null" {
			continue
		}

		respPb := &pb.StreamedMmfResponse{}
		if err := unmarshalOpts.Unmarshal(trimmedResult, respPb); err != nil {
			logger.WithFields(logrus.Fields{
				"pb_message":    "StreamedMmfResponse",
				"http_response": string(trimmedResult),
				"error":         err,
			}).Errorf("cannot unmarshal http response body back into protobuf")
			continue
		}

		if respPb.GetMatch() == nil {
			logger.Trace("StreamedMmfResponse protobuf was empty!")
			continue
		}

		logger.Trace("StreamedMmfResponse protobuf exists and is not empty")
		select {
		case respChan <- respPb:
		case <-streamCtx.Done():
			return
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		logger.Errorf("Error reading stream, closing: %v", err)
	}
}

// Post makes an HTTP request at the given url+path with a byte buffer of the
// JSON payload to POST. When running on Cloud Run (K_REVISION non-empty), it
// injects a cached OIDC identity token for the base server url audience and
// performs a single retry with a refreshed token on HTTP 401 or 403.
func (rc *RestfulOMGrpcClient) Post(ctx context.Context, url string, path string, reqBuf []byte) (*http.Response, error) {
	postLogger := rc.logger().WithFields(logrus.Fields{
		"url": url + path,
	})

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		url+path,
		bytes.NewReader(reqBuf),
	)
	if err != nil {
		postLogger.Errorf("cannot create http request with context")
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	postLogger.Trace("Header set, request created")

	cloudRun := rc.kRevision() != ""
	var usedAccessToken string
	if cloudRun {
		token, err := rc.ensureTokenCache().get(url)
		if err != nil {
			return nil, err
		}
		usedAccessToken = token.AccessToken
		req.Header.Set("Authorization", "Bearer "+usedAccessToken)
	}

	if logrus.IsLevelEnabled(logrus.TraceLevel) {
		headerFields := logrus.Fields{}
		for key, value := range req.Header {
			headerFields[key] = value
		}
		postLogger.WithFields(headerFields).Debug("Prepared Headers")
	}

	client := rc.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		postLogger.WithFields(logrus.Fields{
			"error": err,
		}).Error("cannot execute http request")
		return nil, err
	}

	if resp != nil && cloudRun && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		if resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}

		refreshedToken, err := rc.ensureTokenCache().refresh(url, usedAccessToken)
		if err != nil {
			return nil, err
		}

		retryReq, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			url+path,
			bytes.NewReader(reqBuf),
		)
		if err != nil {
			postLogger.Errorf("cannot create retry http request with context")
			return nil, err
		}
		retryReq.Header.Set("Content-Type", "application/json")
		retryReq.Header.Set("Authorization", "Bearer "+refreshedToken.AccessToken)

		resp, err = client.Do(retryReq)
		if err != nil {
			postLogger.WithFields(logrus.Fields{
				"error": err,
			}).Error("cannot execute retry http request")
			return nil, err
		}
	}

	return resp, nil
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
	reqPb, err := protojson.Marshal(&pb.ActivateTicketsRequest{})
	if err != nil {
		return err
	}

	targetURL := ""
	if len(url) > 0 && url[0] != "" {
		targetURL = url[0]
	} else if rc.Cfg != nil {
		targetURL = rc.Cfg.GetString("OM_CORE_ADDR")
	}

	resp, err := rc.Post(ctx, targetURL, "/tickets:activate", reqPb)
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("ValidateConnection received nil HTTP response")
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadRequest {
		return nil
	}
	return fmt.Errorf("ValidateConnection failed with HTTP status %d (%s)", resp.StatusCode, http.StatusText(resp.StatusCode))
}
