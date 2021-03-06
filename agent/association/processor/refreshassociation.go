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

// Package processor refreshes association immediately
package processor

import (
	"fmt"
	"os"
	"time"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/association/cache"
	"github.com/aws/amazon-ssm-agent/agent/association/model"
	"github.com/aws/amazon-ssm-agent/agent/association/schedulemanager"
	"github.com/aws/amazon-ssm-agent/agent/association/schedulemanager/signal"
	"github.com/aws/amazon-ssm-agent/agent/association/service"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/fileutil"
	"github.com/aws/amazon-ssm-agent/agent/jsonutil"
	"github.com/aws/amazon-ssm-agent/agent/log"
	"github.com/aws/amazon-ssm-agent/agent/platform"
	"github.com/aws/amazon-ssm-agent/agent/plugins/pluginutil"
	"github.com/aws/amazon-ssm-agent/agent/times"

	"path/filepath"

	"github.com/aws/aws-sdk-go/aws"
)

// ProcessRefreshAssociation executes one set of commands and returns their output.
func (p *Processor) ProcessRefreshAssociation(log log.T, pluginRes *contracts.PluginResult, orchestrationDir string, apply bool) {
	var associationIds []string
	jsonutil.Remarshal(pluginRes.Output, &associationIds)
	var out contracts.PluginOutput
	defaultPluginConfig := pluginutil.DefaultPluginConfig()
	//this is the current plugin run, trigger refresh
	if apply {
		out = p.refreshAssociation(log, associationIds, orchestrationDir, pluginRes.OutputS3BucketName, pluginRes.OutputS3KeyPrefix)

		// Upload output to S3
		uploadOutputToS3BucketErrors := p.defaultPlugin.UploadOutputToS3Bucket(log, pluginRes.PluginID, orchestrationDir, pluginRes.OutputS3BucketName, pluginRes.OutputS3KeyPrefix, false, "", out.Stdout, out.Stderr)
		if len(uploadOutputToS3BucketErrors) > 0 {
			log.Errorf("Unable to upload the logs: %s", uploadOutputToS3BucketErrors)
		}

		// Return Json indented response
		responseContent, _ := jsonutil.Marshal(out)
		log.Debug("Returning response:\n", jsonutil.Indent(responseContent))

		// Create output file paths
		stdoutFilePath := filepath.Join(orchestrationDir, defaultPluginConfig.StdoutFileName)
		stderrFilePath := filepath.Join(orchestrationDir, defaultPluginConfig.StderrFileName)
		log.Debugf("stdout file %v, stderr file %v", stdoutFilePath, stderrFilePath)

		// create orchestration dir if needed
		if err := fileutil.MakeDirs(orchestrationDir); err != nil {
			out.AppendError(log, "Failed to create orchestrationDir directory for log files")
		}
		if _, err := fileutil.WriteIntoFileWithPermissions(stdoutFilePath, out.Stdout, os.FileMode(int(appconfig.ReadWriteAccess))); err != nil {
			log.Error(err)
		}

		if _, err := fileutil.WriteIntoFileWithPermissions(stderrFilePath, out.Stderr, os.FileMode(int(appconfig.ReadWriteAccess))); err != nil {
			log.Error(err)
		}
	} else {
		//operation is already done, fill success result as default
		out.MarkAsSucceeded()
		// if user provided empty list or "" in the document, we will run all the associations now
		applyAll := len(associationIds) == 0 || (len(associationIds) == 1 && associationIds[0] == "")
		if applyAll {
			out.AppendInfo(log, "All associations have been requested to execute immediately")
		} else {
			out.AppendInfof(log, "Associations %v have been requested to execute immediately", associationIds)
		}
	}

	pluginRes.Code = out.ExitCode
	pluginRes.Status = contracts.ResultStatusSuccess
	pluginRes.Output = out.String()
	pluginRes.StandardOutput = pluginutil.StringPrefix(out.Stdout, defaultPluginConfig.MaxStdoutLength, defaultPluginConfig.OutputTruncatedSuffix)
	pluginRes.StandardError = pluginutil.StringPrefix(out.Stderr, defaultPluginConfig.MaxStderrLength, defaultPluginConfig.OutputTruncatedSuffix)
}

// refreshAssociation executes one the command and returns their output.
func (p *Processor) refreshAssociation(log log.T, associationIds []string, orchestrationDirectory string, outputS3BucketName string, outputS3KeyPrefix string) (out contracts.PluginOutput) {
	var err error
	var instanceID string
	associations := []*model.InstanceAssociation{}

	if instanceID, err = platform.InstanceID(); err != nil {
		out.MarkAsFailed(log, fmt.Errorf("failed to load instance ID, %v", err))
		return
	}

	// Get associations
	if associations, err = p.assocSvc.ListInstanceAssociations(log, instanceID); err != nil {
		out.MarkAsFailed(log, fmt.Errorf("failed to list instance associations, %v", err))
		return
	}

	// evict the invalid cache first
	for _, assoc := range associations {
		cache.ValidateCache(assoc)
	}

	// if user provided empty list or "" in the document, we will run all the associations now
	applyAll := len(associationIds) == 0 || (len(associationIds) == 1 && associationIds[0] == "")

	// Default is success
	out.MarkAsSucceeded()

	// read from cache or load association details from service
	for _, assoc := range associations {
		if err = p.assocSvc.LoadAssociationDetail(log, assoc); err != nil {
			err = fmt.Errorf("Encountered error while loading association %v contents, %v",
				*assoc.Association.AssociationId,
				err)
			assoc.Errors = append(assoc.Errors, err)
			p.assocSvc.UpdateInstanceAssociationStatus(
				log,
				*assoc.Association.AssociationId,
				*assoc.Association.Name,
				*assoc.Association.InstanceId,
				contracts.AssociationStatusFailed,
				contracts.AssociationErrorCodeListAssociationError,
				times.ToIso8601UTC(time.Now()),
				err.Error(),
				service.NoOutputUrl)
			out.MarkAsFailed(log, err)
			continue
		}

		// validate association expression, fail association if expression cannot be passed
		// Note: we do not want to fail runcommand with out.MarkAsFailed
		if !assoc.IsRunOnceAssociation() {
			if err := assoc.ParseExpression(log); err != nil {
				message := fmt.Sprintf("Encountered error while parsing expression for association %v", *assoc.Association.AssociationId)
				log.Errorf("%v, %v", message, err)
				assoc.Errors = append(assoc.Errors, err)
				p.assocSvc.UpdateInstanceAssociationStatus(
					log,
					*assoc.Association.AssociationId,
					*assoc.Association.Name,
					*assoc.Association.InstanceId,
					contracts.AssociationStatusFailed,
					contracts.AssociationErrorCodeInvalidExpression,
					times.ToIso8601UTC(time.Now()),
					message,
					service.NoOutputUrl)
				out.MarkAsFailed(log, err)
				continue
			}
		}

		if applyAll || isAssociationQualifiedToRunNow(associationIds, assoc) {
			// If association is already InProgress, we don't want to run it again
			if assoc.Association.DetailedStatus == nil ||
				(*assoc.Association.DetailedStatus != contracts.AssociationStatusInProgress && *assoc.Association.DetailedStatus != contracts.AssociationStatusPending) {
				// Updates status to pending, which is the indicator for schedulemanager for immediate execution
				p.assocSvc.UpdateInstanceAssociationStatus(
					log,
					*assoc.Association.AssociationId,
					*assoc.Association.Name,
					*assoc.Association.InstanceId,
					contracts.AssociationStatusPending,
					contracts.AssociationErrorCodeNoError,
					times.ToIso8601UTC(time.Now()),
					contracts.AssociationPendingMessage,
					service.NoOutputUrl)
				assoc.Association.DetailedStatus = aws.String(contracts.AssociationStatusPending)
			}
		}
	}

	schedulemanager.Refresh(log, associations)

	if applyAll {
		out.AppendInfo(log, "All associations have been requested to execute immediately")
	} else {
		out.AppendInfof(log, "Associations %v have been requested to execute immediately", associationIds)
	}

	signal.ExecuteAssociation(log)

	return
}

// doesAssociationQualifiedToRunNow finds out if association is qualified to run now
func isAssociationQualifiedToRunNow(AssociationIds []string, assoc *model.InstanceAssociation) bool {
	for _, id := range AssociationIds {
		if *assoc.Association.AssociationId == id {
			return true
		}
	}

	return false
}
