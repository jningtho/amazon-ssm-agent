// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package service is a wrapper for the SSM Message Delivery Service
package service

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/log"
	"github.com/aws/amazon-ssm-agent/agent/platform"
	"github.com/aws/amazon-ssm-agent/agent/sdkutil"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssmmds"
	"github.com/twinj/uuid"
)

// FailureType is used for failure types.
type FailureType string

const (
	// InternalHandlerException signifies an error while running a plugin.
	InternalHandlerException FailureType = "InternalHandlerException"

	// NoHandlerExists signifies that there is no plugin for a given name.
	NoHandlerExists FailureType = "NoHandlerExists"

	// QuickResponseThreshold is the threshold time - any api response that comes before this (time in seconds) is treated as fast response
	QuickResponseThreshold = 10
)

// Service is an interface to the MDS service.
type Service interface {
	GetMessages(log log.T, instanceID string) (messages *ssmmds.GetMessagesOutput, err error)
	AcknowledgeMessage(log log.T, messageID string) error
	SendReply(log log.T, messageID string, payload string) error
	FailMessage(log log.T, messageID string, failureType FailureType) error
	DeleteMessage(log log.T, messageID string) error
	Stop()
}

// sdkService is an service wrapper that delegates to the ssm sdk.
type sdkService struct {
	sdk         *ssmmds.SSMMDS
	tr          *http.Transport
	lastRequest *request.Request
	m           sync.Mutex
}

var clientBasedErrorMessages, serverBasedErrorMessages []string

// NewService creates a new MDS service instance.
func NewService(region string, endpoint string, creds *credentials.Credentials, connectionTimeout time.Duration) Service {

	config := sdkutil.AwsConfig()

	if region != "" {
		config.Region = &region
	}

	if endpoint != "" {
		config.Endpoint = &endpoint
	} else {
		if region, err := platform.Region(); err == nil {
			if defaultEndpoint := appconfig.GetDefaultEndPoint(region, "ec2messages"); defaultEndpoint != "" {
				config.Endpoint = &defaultEndpoint
			}
		}
	}

	if creds != nil {
		config.Credentials = creds
	}

	// capture Transport so we can use it to cancel requests
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout:   connectionTimeout,
			KeepAlive: 0,
		}).Dial,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	config.HTTPClient = &http.Client{Transport: tr, Timeout: connectionTimeout}

	msgSvc := ssmmds.New(session.New(config))

	//adding server based expected error messages
	serverBasedErrorMessages = make([]string, 2)
	serverBasedErrorMessages = append(serverBasedErrorMessages, "use of closed network connection")
	serverBasedErrorMessages = append(serverBasedErrorMessages, "connection reset by peer")

	//adding client based expected error messages
	clientBasedErrorMessages = make([]string, 1)
	clientBasedErrorMessages = append(clientBasedErrorMessages, "Client.Timeout exceeded while awaiting headers")

	return &sdkService{sdk: msgSvc, tr: tr}
}

// GetMessages calls the GetMessages MDS API.
func (mds *sdkService) GetMessages(log log.T, instanceID string) (messages *ssmmds.GetMessagesOutput, err error) {
	uuid.SwitchFormat(uuid.CleanHyphen)
	uid := uuid.NewV4().String()
	params := &ssmmds.GetMessagesInput{
		Destination:                aws.String(instanceID), // Required
		MessagesRequestId:          aws.String(uid),        // Required
		VisibilityTimeoutInSeconds: aws.Int64(10),
	}
	log.Debug("Calling GetMessages with params", params)
	requestTime := time.Now()
	req, messages := mds.sdk.GetMessagesRequest(params)
	if requestErr := mds.sendRequest(req); requestErr != nil {
		log.Debug(requestErr)
		if isErrorUnexpected(log, requestErr, requestTime, time.Now()) {
			//GetMessages api responded with unexpected errors - we must return this as error
			err = fmt.Errorf("GetMessages Error: %v", requestErr)
			log.Debug(err)
		}
	} else {
		log.Debug("GetMessages Response", messages)
	}
	return
}

// isErrorUnexpected processes GetMessages errors and determines if its unexpected error
func isErrorUnexpected(log log.T, err error, requestTime, responseTime time.Time) bool {
	//determine the time it took for the api to respond
	timeDiff := responseTime.Sub(requestTime).Seconds()
	//check if response isn't coming too quick & if error is unexpected
	if timeDiff < QuickResponseThreshold {
		//response was too quick - this is unexpected
		return true
	}

	//response wasn't too quick
	//checking if the class of errors are expected
	if isServerBasedError(err.Error()) {
		log.Debugf("server terminated connection after %v seconds - this is expected in long polling api calls.", timeDiff)
		return false
	} else if isClientBasedError(err.Error()) {
		log.Debugf("client terminated connection after %v seconds - this is expected in long polling api calls.", timeDiff)
		return false
	} else {
		//errors are truly unexpected
		return true
	}
}

// isServerBasedError returns true if and only if the error is server related
func isServerBasedError(message string) bool {
	for _, m := range serverBasedErrorMessages {
		if strings.Contains(message, m) {
			return true
		}
	}
	return false
}

// isClientBasedError returns true if and only if the error is client related
func isClientBasedError(message string) bool {
	for _, m := range clientBasedErrorMessages {
		if strings.Contains(message, m) {
			return true
		}
	}
	return false
}

// AcknowledgeMessage calls AcknowledgeMessage MDS API.
func (mds *sdkService) AcknowledgeMessage(log log.T, messageID string) (err error) {
	params := &ssmmds.AcknowledgeMessageInput{
		MessageId: aws.String(messageID), // Required
	}
	log.Debug("Calling AcknowledgeMessage with params", params)
	req, resp := mds.sdk.AcknowledgeMessageRequest(params)
	if err = mds.sendRequest(req); err != nil {
		err = fmt.Errorf("AcknowledgeMessage Error: %v", err)
		log.Debug(err)
	} else {
		log.Debug("AcknowledgeMessage Response", resp)
	}
	return
}

// SendReply calls the SendReply MDS API.
func (mds *sdkService) SendReply(log log.T, messageID string, payload string) (err error) {
	uuid.SwitchFormat(uuid.CleanHyphen)
	replyID := uuid.NewV4().String()
	params := &ssmmds.SendReplyInput{
		MessageId: aws.String(messageID), // Required
		Payload:   aws.String(payload),   // Required
		ReplyId:   aws.String(replyID),   // Required
	}
	log.Debug("Calling SendReply with params", params)
	req, resp := mds.sdk.SendReplyRequest(params)
	if err = mds.sendRequest(req); err != nil {
		err = fmt.Errorf("SendReply Error: %v", err)
		log.Debug(err)
	} else {
		log.Info("SendReply Response", resp)
	}
	return
}

// FailMessage calls the FailMessage MDS API.
func (mds *sdkService) FailMessage(log log.T, messageID string, failureType FailureType) (err error) {
	params := &ssmmds.FailMessageInput{
		FailureType: aws.String(string(failureType)), // Required
		MessageId:   aws.String(messageID),           // Required
	}
	log.Debug("Calling FailMessage with params", params)
	req, resp := mds.sdk.FailMessageRequest(params)
	if err = mds.sendRequest(req); err != nil {
		err = fmt.Errorf("FailMessage Error: %v", err)
		log.Debug(err)
	} else {
		log.Debug("FailMessage Response", resp)
	}
	return
}

// DeleteMessage calls the DeleteMessage MDS API.
func (mds *sdkService) DeleteMessage(log log.T, messageID string) (err error) {
	params := &ssmmds.DeleteMessageInput{
		MessageId: aws.String(messageID), // Required
	}
	log.Debug("Calling DeleteMessage with params", params)
	req, resp := mds.sdk.DeleteMessageRequest(params)
	if err = mds.sendRequest(req); err != nil {
		err = fmt.Errorf("DeleteMessage Error: %v", err)
		log.Debug(err)
	} else {
		log.Debug("DeleteMessage Response", resp)
	}
	return
}

// Stop stops this service so that any blocked calls wake up.
func (mds *sdkService) Stop() {
	mds.m.Lock()
	defer mds.m.Unlock()
	if mds.lastRequest != nil {
		// cancel the underlying http request to wake up the last call
		mds.tr.CancelRequest(mds.lastRequest.HTTPRequest)
	}
}

// sendRequest wraps req.Send() so that it can keep track of the executing request
func (mds *sdkService) sendRequest(req *request.Request) error {
	mds.storeRequest(req)
	defer mds.clearRequest()
	return req.Send()
}

func (mds *sdkService) storeRequest(req *request.Request) {
	mds.m.Lock()
	defer mds.m.Unlock()
	mds.lastRequest = req
}

func (mds *sdkService) clearRequest() {
	mds.storeRequest(nil)
}
