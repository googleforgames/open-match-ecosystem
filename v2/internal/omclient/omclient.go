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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	pb "github.com/googleforgames/open-match2/v2/pkg/pb"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var MMFsComplete = errors.New("Open Match HTTP client detected that the stream of matches from OM was complete and cancelled the context")

type RestfulOMGrpcClient struct {
	Client      *http.Client
	Log         *logrus.Logger
	Cfg         *viper.Viper
	tokenSource oauth2.TokenSource
}

// CreateTicket Example
// Your platform services layer should take the matchmaking request, add
// attributes your platform services have authority over (ex: MMR, ELO,
// inventory, etc), then call Open Match core on the client's behalf to make a
// matchmaking ticket. In this sense, Open Match sees your platform services
// layer as a 'proxy' for the player's game client.
func (rc *RestfulOMGrpcClient) CreateTicket(ctx context.Context, ticket *pb.Ticket) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := rc.Log.WithFields(logrus.Fields{
		"component": "open_match_client",
		"operation": "proxy_CreateTicket",
	})

	// Update metrics
	//createdTicketCounter.Add(ctx, 1)

	// Put the ticket into open match CreateTicket request protobuf message.
	reqPb := &pb.CreateTicketRequest{Ticket: ticket}
	resPb := &pb.CreateTicketResponse{}
	var err error
	var buf []byte

	// Marshal request into json
	buf, err = protojson.Marshal(reqPb)
	if err != nil {
		// Mark as a permanent error so the backoff library doesn't retry this (invalid input)
		err := backoff.Permanent(err)
		logger.WithFields(logrus.Fields{
			"pb_message": "CreateTicketsRequest",
		}).Errorf("cannot marshal proto to json")
		return "", err
	}

	// HTTP version of the gRPC CreateTicket() call
	resp, err := rc.Post(ctx, logger, rc.Cfg.GetString("OM_CORE_ADDR"), "/tickets", buf)

	// Include headers in structured logging when using trace log level [slow]
	headerFields := logrus.Fields{}
	if logrus.IsLevelEnabled(logrus.TraceLevel) {
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
		//TODO: switch resp.Header?
		resp.Body.Close() // make sure the body is closed.
		return "", err
	}
	if err != nil { // HTTP library error
		// ERRO[59639] CreateTicket failed: Post "http://localhost:8080/tickets": dial tcp [::1]:8080: connect: cannot assign requested address
		logger.WithFields(logrus.Fields{
			"CreateTicketsRequest": reqPb,
		}).Errorf("CreateTicket failed: %v", err)
		// Shouldn't have a response but if somehow there is one, make sure it is closed.
		if resp != nil {
			resp.Body.Close()
		}
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
	if resPb == nil {
		// Mark as a permanent error so the backoff library doesn't retry this REST call
		err := backoff.Permanent(errors.New("CreateTicket returned empty result"))
		logger.Error(err)
		return "", err
	}

	// Successful ticket creation
	logger.Tracef("CreateTicket %v complete", resPb.TicketId)
	return resPb.TicketId, err
}

// ActivateTickets takes a channel of ticket ids for tickets awaiting activation,
// and breaks them into batches of the size defined in
// rc.cfg.GetInt("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL") (see
// open-match.dev/core/internal/config for limits on how many actions you can
// request in a single API call, this needs to use the same value).
func (rc *RestfulOMGrpcClient) ActivateTickets(ctx context.Context, ticketIdsToActivate chan string) {
	logger := rc.Log.WithFields(logrus.Fields{
		"component": "open_match_client",
		"operation": "proxy_ActivateTickets",
		"caller": func() string {
			// Quick in-line function to grab information the calling function
			// put into the context and propogate it to the logs
			ts, ok := ctx.Value("activationType").(string)
			if !ok {
				rc.Log.Error("unable to get caller type from context")
				return "undefined"
			}
			return ts
		}(),
	})

	// batching asynchronous goroutine. Processes incoming ticket ids into a
	// batch of max size OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL, or until the
	// deadline is reached, whichever comes first, then starts a new batch as long
	// as the incoming channel has not been closed.
	batchChan := make(chan []string)
	go func() {
		bLogger := logger.WithFields(logrus.Fields{
			"suboperation": "create_batches",
		})

		// As long as the channel is not closed, keep reading batches from it.
		numBatches := 0
		senderActive := true
		var tid string
		for senderActive {

			// Wait no longer than deadline for a batch to be created and sent for processing
			deadline := time.NewTimer(250 * time.Millisecond)
			activationBatch := make([]string, 0)
			batchComplete := false

			// Loop through incoming ticket ids, until batch limit is reached or deadline is exceeded
			for !batchComplete {

				select {
				case tid, senderActive = <-ticketIdsToActivate:
					activationBatch = append(activationBatch, tid)
					if len(activationBatch) == rc.Cfg.GetInt("OM_CORE_MAX_UPDATES_PER_ACTIVATION_CALL") {
						batchComplete = true
					}
				case <-deadline.C:
					batchComplete = true
				}
			}

			// Send the batch to be sent in a single call to om-core
			numBatches++
			if len(activationBatch) > 0 {
				bLogger.Tracef("queueing batch of %v ticket ids to send for activation", len(activationBatch))
				batchChan <- activationBatch
			}

		}

		// All done with creating batches.
		bLogger.Tracef("processed %v batches before the sender stopped sending ticket ids", numBatches)
		close(batchChan)
	}()

	// Loop that sends all batches made by the above goroutine to om-core
	var activationWg sync.WaitGroup
	for batch := range batchChan {

		// Kick off activation in a goroutine, so if there are multiple batches
		// all calls are made concurrently.
		go func(thisBatch []string) {

			// Track this goroutine
			activationWg.Add(1)
			defer activationWg.Done()

			// Generate request protobuffer to send to activate tickets endpoint
			reqPb := &pb.ActivateTicketsRequest{TicketIds: thisBatch}
			var err error

			// Marshal request into json
			req, err := protojson.Marshal(reqPb)
			if err != nil {
				logger.WithFields(logrus.Fields{
					"pb_message": "ActivateTicketsRequest",
				}).Errorf("cannot marshal protobuf to json")
			}

			// HTTP version of the gRPC ActivateTickets() call that we want to retry with exponential backoff and jitter
			activateTicketCall := func() error {
				logger.Tracef("ActivateTicket call with %v tickets: %v...", len(thisBatch), thisBatch[0])
				resp, err := rc.Post(ctx, logger, rc.Cfg.GetString("OM_CORE_ADDR"), "/tickets:activate", req)
				defer func(resp *http.Response) {
					if resp != nil {
						resp.Body.Close()
					}
				}(resp)
				// TODO: In reality, the error has details fields, telling us which ticket couldn't
				// be activated, but we're not processing those or passing them on yet, we just act as
				// though all activations succeeded or all activations failed.
				if err != nil {
					logger.Errorf("ActivateTickets attempt failed: %v", err)
					return err
				} else if resp != nil && resp.StatusCode != http.StatusOK { // HTTP error code
					err = fmt.Errorf("%s (%d)", http.StatusText(resp.StatusCode), resp.StatusCode)
					logger.Errorf("ActivateTickets attempt failed: %v", err)
					return err
				}
				return nil
			}

			// Try to call ActivateTickets() with retries for up to 5 seconds.
			err = backoff.RetryNotify(
				activateTicketCall,
				backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(5*time.Second)),
				func(err error, bo time.Duration) {
					logger.Errorf("ActivateTicket temporary failure (backoff for %v): %v", err, bo)
				},
			)
			if err != nil {
				logger.Errorf("ActivateTickets failed: %v", err)
			}
			logger.Trace("ActivateTickets complete")
		}(batch)
	}

	// wait for all outstanding asynchronous calls to om-core's ActivateTickets() endpoint to complete
	activationWg.Wait()
}

func (rc *RestfulOMGrpcClient) InvokeMatchmakingFunctions(ctx context.Context, reqPb *pb.MmfRequest, respChan chan *pb.StreamedMmfResponse) {
	// Make sure the output channel always gets closed when this function is finished.
	defer close(respChan)

	logger := rc.Log.WithFields(logrus.Fields{
		"component": "open_match_client",
		"operation": "proxy_InvokeMatchmakingFunctions",
		"url":       rc.Cfg.GetString("OM_CORE_ADDR") + "/matches:fetch",
	})

	// Have to cancel the context to tell the om-core server we're done reading from the stream.
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(MMFsComplete)

	// Marshal protobuf request to JSON for grpc-gateway HTTP call
	req, err := protojson.Marshal(reqPb)
	if err != nil {
		logger.WithFields(logrus.Fields{
			"pb_message": "MmfRequest",
		}).Errorf("cannot marshal protobuf to json")
	} else {
		logger.Trace("Marshalled MmfRequest protobuf message to JSON")
	}

	// HTTP version of gRPC InvokeMatchmakingFunctions() call
	logger.Trace("Calling InvokeMatchmakingFunctions()")
	resp, err := rc.Post(ctx, logger, rc.Cfg.GetString("OM_CORE_ADDR"), "/matches:fetch", req)
	if err != nil {
		logger.Errorf("InvokeMatchmakingFunction call failed: %v", err)
	} else if resp != nil && resp.StatusCode != http.StatusOK {
		logger.Errorf("InvokeMatchmakingFunction call failed: %v", resp.StatusCode)
	} else {
		logger.Tracef("InvokeMatchmakingFunction call successful: %v", resp.StatusCode)

		// Process the result stream
		for {
			var resPb *pb.StreamedMmfResponse
			// Retrieve JSON-formatted protobuf from response body
			var msg string
			_, err := fmt.Fscanf(resp.Body, "%s\n", &msg)

			// Validate response before processing
			if err == io.EOF {
				break // End of stream; done!
			}
			if err != nil {
				logger.Errorf("Error reading stream, closing: %v", err)
				break
			}

			// Easy to miss: grpc-gateway returns your result inside a
			// overarching JSON enclosure under the key 'result', so
			// the actual text we need to pass to protojson.Unmarshal
			// needs to omit the top level JSON object. Rather than
			// marshal the text to json (which is slow), we just use
			// string trimming functions.
			trimmedMsg := strings.TrimSuffix(strings.TrimPrefix(msg, `{"result":`), "}")
			logger.WithFields(logrus.Fields{
				"pb_message": "StreamedMmfResponse",
			}).Tracef("HTTP response JSON body version of StreamedMmfResponse received")

			// Unmarshal json back into protobuf
			resPb = &pb.StreamedMmfResponse{}
			err = protojson.Unmarshal([]byte(trimmedMsg), resPb)
			if err != nil {
				logger.WithFields(logrus.Fields{
					"pb_message":    "StreamedMmfResponse",
					"http_response": trimmedMsg,
					"error":         err,
				}).Errorf("cannot unmarshal http response body back into protobuf")
				continue
			} else {
				logger.Trace("Successfully unmarshalled HTTP response JSON body back into StreamedMmfResponse protobuf message")
			}

			if resPb == nil {
				logger.Trace("StreamedMmfResponse protobuf was nil!")
				continue // Loop again to get the next streamed response
			}

			// Send back the streamed responses as we get them
			logger.Trace("StreamedMmfResponse protobuf exists")
			respChan <- resPb

		}

		// Close response body
		resp.Body.Close()
	}
}

// Post makes an HTTP request at the given url+path, and a byte buffer of the
// data to POST.  It attempts to transparently handle TLS if the target server
// requires it.
func (rc *RestfulOMGrpcClient) Post(ctx context.Context, logger *logrus.Entry, url string, path string, reqBuf []byte) (*http.Response, error) {
	// Add the url as a structured logging field when debug logging is enabled
	postLogger := logger.WithFields(logrus.Fields{
		"url": url + path,
	})

	// Set up our request parameters
	req, err := http.NewRequestWithContext(
		ctx,                     // Context
		http.MethodPost,         // HTTP verb
		url+path,                // RESTful OM2 path
		bytes.NewReader(reqBuf), // JSON-marshalled protobuf request message
	)
	if err != nil {
		postLogger.Errorf("cannot create http request with context")
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	postLogger.Trace("Header set, request created")

	if os.Getenv("K_REVISION") != "" { // Running on Cloud Run, get GCP SA token
		if rc.tokenSource == nil {
			// Create a TokenSource if none exists.
			rc.tokenSource, err = idtoken.NewTokenSource(ctx, url)
			if err != nil {
				err = fmt.Errorf("Cloud Run service account authentication requires token source, but couldn't get one. idtoken.NewTokenSource: %v", err)
				return nil, err
			}
		}

		// Retrieve an identity token. Will reuse tokens until refresh needed.
		token, err := rc.tokenSource.Token()
		if err != nil {
			err = fmt.Errorf("Cloud Run service account authentication requires a token, but couldn't get one. TokenSource.Token: %v", err)
			return nil, err
		}
		token.SetAuthHeader(req)
	}

	// Log all request headers if using trace logging (slow)
	if logrus.IsLevelEnabled(logrus.TraceLevel) {
		headerFields := logrus.Fields{}
		for key, value := range req.Header {
			headerFields[key] = value
		}
		postLogger.WithFields(headerFields).Debug("Prepared Headers")
	}

	// Send request
	resp, err := rc.Client.Do(req)
	if err != nil {
		postLogger.WithFields(logrus.Fields{
			"error": err,
		}).Error("cannot execute http request")
		return nil, err
	}
	return resp, err
}

// readAllBody is a simple helper function to make sure an HTTP body is completely read and closed.
func readAllBody(resp http.Response, logger *logrus.Entry) ([]byte, error) {
	// Get results
	body, err := io.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		logger.Errorf("cannot read bytes from http response body")
		return nil, err
	}

	return body, err
}

// ValidateConnection can be used to check that the RestfulOMGrpcClient can
// send requests to the provide Open Match URL
func (rc *RestfulOMGrpcClient) ValidateConnection(ctx context.Context, url string) error {

	// Make a dummy ticket that will immediately expire and matches no filters.
	buf, err := protojson.Marshal(&pb.CreateTicketRequest{
		Ticket: &pb.Ticket{ExpirationTime: timestamppb.Now()},
	})
	// Try to create the dummy ticket.
	resp, err := rc.Post(ctx,
		rc.Log.WithFields(logrus.Fields{
			"operation": "proxy_ValidateConnection",
		}),
		url, "/", buf)
	if resp != nil {
		resp.Body.Close()
	}
	return err
}

// syncMapDump can be uncommented and called to see the contents of a sync.Map
// when debugging.
//func syncMapDump(sm *sync.Map) map[string]interface{} {
//	out := map[string]interface{}{}
//	sm.Range(func(key, value interface{}) bool {
//		out[fmt.Sprint(key)] = value
//		return true
//	})
//	return out
//}
