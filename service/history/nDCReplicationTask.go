// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	"time"

	"github.com/pborman/uuid"
	commonpb "go.temporal.io/temporal-proto/common"
	eventpb "go.temporal.io/temporal-proto/event"
	"go.temporal.io/temporal-proto/serviceerror"

	eventgenpb "github.com/temporalio/temporal/.gen/proto/event"
	"github.com/temporalio/temporal/.gen/proto/historyservice"
	"github.com/temporalio/temporal/common"
	"github.com/temporalio/temporal/common/cluster"
	"github.com/temporalio/temporal/common/log"
	"github.com/temporalio/temporal/common/log/tag"
	"github.com/temporalio/temporal/common/persistence"
	"github.com/temporalio/temporal/common/persistence/serialization"
)

type (
	nDCReplicationTask interface {
		getNamespaceID() string
		getExecution() *commonpb.WorkflowExecution
		getWorkflowID() string
		getRunID() string
		getEventTime() time.Time
		getFirstEvent() *eventpb.HistoryEvent
		getLastEvent() *eventpb.HistoryEvent
		getVersion() int64
		getSourceCluster() string
		getEvents() []*eventpb.HistoryEvent
		getNewEvents() []*eventpb.HistoryEvent
		getLogger() log.Logger
		getVersionHistory() *persistence.VersionHistory
		isWorkflowReset() bool

		splitTask(taskStartTime time.Time) (nDCReplicationTask, nDCReplicationTask, error)
	}

	nDCReplicationTaskImpl struct {
		sourceCluster  string
		namespaceID    string
		execution      *commonpb.WorkflowExecution
		version        int64
		firstEvent     *eventpb.HistoryEvent
		lastEvent      *eventpb.HistoryEvent
		eventTime      time.Time
		events         []*eventpb.HistoryEvent
		newEvents      []*eventpb.HistoryEvent
		versionHistory *persistence.VersionHistory

		startTime time.Time
		logger    log.Logger
	}
)

var (
	// ErrInvalidNamespaceID is returned if namespace ID is invalid
	ErrInvalidNamespaceID = serviceerror.NewInvalidArgument("invalid namespace ID")
	// ErrInvalidExecution is returned if execution is invalid
	ErrInvalidExecution = serviceerror.NewInvalidArgument("invalid execution")
	// ErrInvalidRunID is returned if run ID is invalid
	ErrInvalidRunID = serviceerror.NewInvalidArgument("invalid run ID")
	// ErrEventIDMismatch is returned if event ID mis-matched
	ErrEventIDMismatch = serviceerror.NewInvalidArgument("event ID mismatch")
	// ErrEventVersionMismatch is returned if event version mis-matched
	ErrEventVersionMismatch = serviceerror.NewInvalidArgument("event version mismatch")
	// ErrNoNewRunHistory is returned if there is no new run history
	ErrNoNewRunHistory = serviceerror.NewInvalidArgument("no new run history events")
	// ErrLastEventIsNotContinueAsNew is returned if the last event is not continue as new
	ErrLastEventIsNotContinueAsNew = serviceerror.NewInvalidArgument("last event is not continue as new")
)

func newNDCReplicationTask(
	clusterMetadata cluster.Metadata,
	historySerializer persistence.PayloadSerializer,
	taskStartTime time.Time,
	logger log.Logger,
	request *historyservice.ReplicateEventsV2Request,
) (*nDCReplicationTaskImpl, error) {

	events, newEvents, err := validateReplicateEventsRequest(
		historySerializer,
		request,
	)
	if err != nil {
		return nil, err
	}

	namespaceID := request.GetNamespaceId()
	execution := request.WorkflowExecution
	versionHistory := &eventgenpb.VersionHistory{
		BranchToken: nil,
		Items:       request.VersionHistoryItems,
	}

	firstEvent := events[0]
	lastEvent := events[len(events)-1]
	version := firstEvent.GetVersion()

	sourceCluster := clusterMetadata.ClusterNameForFailoverVersion(version)

	eventTime := int64(0)
	for _, event := range events {
		if event.GetTimestamp() > eventTime {
			eventTime = event.GetTimestamp()
		}
	}
	for _, event := range newEvents {
		if event.GetTimestamp() > eventTime {
			eventTime = event.GetTimestamp()
		}
	}

	logger = logger.WithTags(
		tag.WorkflowID(execution.GetWorkflowId()),
		tag.WorkflowRunID(execution.GetRunId()),
		tag.SourceCluster(sourceCluster),
		tag.IncomingVersion(version),
		tag.WorkflowFirstEventID(firstEvent.GetEventId()),
		tag.WorkflowNextEventID(lastEvent.GetEventId()+1),
	)

	return &nDCReplicationTaskImpl{
		sourceCluster:  sourceCluster,
		namespaceID:    namespaceID,
		execution:      execution,
		version:        version,
		firstEvent:     firstEvent,
		lastEvent:      lastEvent,
		eventTime:      time.Unix(0, eventTime),
		events:         events,
		newEvents:      newEvents,
		versionHistory: persistence.NewVersionHistoryFromProto(versionHistory),

		startTime: taskStartTime,
		logger:    logger,
	}, nil
}

func (t *nDCReplicationTaskImpl) getNamespaceID() string {
	return t.namespaceID
}

func (t *nDCReplicationTaskImpl) getExecution() *commonpb.WorkflowExecution {
	return t.execution
}

func (t *nDCReplicationTaskImpl) getWorkflowID() string {
	return t.execution.GetWorkflowId()
}

func (t *nDCReplicationTaskImpl) getRunID() string {
	return t.execution.GetRunId()
}

func (t *nDCReplicationTaskImpl) getEventTime() time.Time {
	return t.eventTime
}

func (t *nDCReplicationTaskImpl) getFirstEvent() *eventpb.HistoryEvent {
	return t.firstEvent
}

func (t *nDCReplicationTaskImpl) getLastEvent() *eventpb.HistoryEvent {
	return t.lastEvent
}

func (t *nDCReplicationTaskImpl) getVersion() int64 {
	return t.version
}

func (t *nDCReplicationTaskImpl) getSourceCluster() string {
	return t.sourceCluster
}

func (t *nDCReplicationTaskImpl) getEvents() []*eventpb.HistoryEvent {
	return t.events
}

func (t *nDCReplicationTaskImpl) getNewEvents() []*eventpb.HistoryEvent {
	return t.newEvents
}

func (t *nDCReplicationTaskImpl) getLogger() log.Logger {
	return t.logger
}

func (t *nDCReplicationTaskImpl) getVersionHistory() *persistence.VersionHistory {
	return t.versionHistory
}

func (t *nDCReplicationTaskImpl) isWorkflowReset() bool {
	switch t.getFirstEvent().GetEventType() {
	case eventpb.EventType_DecisionTaskFailed:
		decisionTaskFailedEvent := t.getFirstEvent()
		attr := decisionTaskFailedEvent.GetDecisionTaskFailedEventAttributes()
		baseRunID := attr.GetBaseRunId()
		baseEventVersion := attr.GetForkEventVersion()
		newRunID := attr.GetNewRunId()

		return len(baseRunID) > 0 && baseEventVersion != 0 && len(newRunID) > 0

	default:
		return false
	}
}

func (t *nDCReplicationTaskImpl) splitTask(
	taskStartTime time.Time,
) (nDCReplicationTask, nDCReplicationTask, error) {

	if len(t.newEvents) == 0 {
		return nil, nil, ErrNoNewRunHistory
	}
	newHistoryEvents := t.newEvents

	if t.getLastEvent().GetEventType() != eventpb.EventType_WorkflowExecutionContinuedAsNew ||
		t.getLastEvent().GetWorkflowExecutionContinuedAsNewEventAttributes() == nil {
		return nil, nil, ErrLastEventIsNotContinueAsNew
	}
	newRunID := t.getLastEvent().GetWorkflowExecutionContinuedAsNewEventAttributes().GetNewExecutionRunId()

	newFirstEvent := newHistoryEvents[0]
	newLastEvent := newHistoryEvents[len(newHistoryEvents)-1]

	newEventTime := int64(0)
	for _, event := range newHistoryEvents {
		if event.GetTimestamp() > newEventTime {
			newEventTime = event.GetTimestamp()
		}
	}

	newVersionHistory := persistence.NewVersionHistoryFromProto(&eventgenpb.VersionHistory{
		BranchToken: nil,
		Items: []*eventgenpb.VersionHistoryItem{{
			EventId: newLastEvent.GetEventId(),
			Version: newLastEvent.GetVersion(),
		}},
	})

	logger := t.logger.WithTags(
		tag.WorkflowID(t.getExecution().GetWorkflowId()),
		tag.WorkflowRunID(newRunID),
		tag.SourceCluster(t.sourceCluster),
		tag.IncomingVersion(t.version),
		tag.WorkflowFirstEventID(newFirstEvent.GetEventId()),
		tag.WorkflowNextEventID(newLastEvent.GetEventId()+1),
	)

	newRunTask := &nDCReplicationTaskImpl{
		sourceCluster: t.sourceCluster,
		namespaceID:   t.namespaceID,
		execution: &commonpb.WorkflowExecution{
			WorkflowId: t.execution.WorkflowId,
			RunId:      newRunID,
		},
		version:        t.version,
		firstEvent:     newFirstEvent,
		lastEvent:      newLastEvent,
		eventTime:      time.Unix(0, newEventTime),
		events:         newHistoryEvents,
		newEvents:      []*eventpb.HistoryEvent{},
		versionHistory: newVersionHistory,

		startTime: taskStartTime,
		logger:    logger,
	}
	t.newEvents = nil

	return t, newRunTask, nil
}

func validateReplicateEventsRequest(
	historySerializer persistence.PayloadSerializer,
	request *historyservice.ReplicateEventsV2Request,
) ([]*eventpb.HistoryEvent, []*eventpb.HistoryEvent, error) {

	// TODO add validation on version history

	if valid := validateUUID(request.GetNamespaceId()); !valid {
		return nil, nil, ErrInvalidNamespaceID
	}
	if request.WorkflowExecution == nil {
		return nil, nil, ErrInvalidExecution
	}
	if valid := validateUUID(request.WorkflowExecution.GetRunId()); !valid {
		return nil, nil, ErrInvalidRunID
	}

	events, err := deserializeBlob(historySerializer, request.Events)
	if err != nil {
		return nil, nil, err
	}
	if len(events) == 0 {
		return nil, nil, ErrEmptyHistoryRawEventBatch
	}

	version, err := validateEvents(events)
	if err != nil {
		return nil, nil, err
	}

	if request.NewRunEvents == nil {
		return events, nil, nil
	}

	newRunEvents, err := deserializeBlob(historySerializer, request.NewRunEvents)
	if err != nil {
		return nil, nil, err
	}

	newRunVersion, err := validateEvents(newRunEvents)
	if err != nil {
		return nil, nil, err
	}
	if version != newRunVersion {
		return nil, nil, ErrEventVersionMismatch
	}
	return events, newRunEvents, nil
}

func validateUUID(input string) bool {
	if uuid.Parse(input) == nil {
		return false
	}
	return true
}

func validateEvents(events []*eventpb.HistoryEvent) (int64, error) {

	firstEvent := events[0]
	firstEventID := firstEvent.GetEventId()
	version := firstEvent.GetVersion()

	for index, event := range events {
		if event.GetEventId() != firstEventID+int64(index) {
			return 0, ErrEventIDMismatch
		}
		if event.GetVersion() != version {
			return 0, ErrEventVersionMismatch
		}
	}
	return version, nil
}

func deserializeBlob(
	historySerializer persistence.PayloadSerializer,
	blob *commonpb.DataBlob,
) ([]*eventpb.HistoryEvent, error) {

	if blob == nil {
		return nil, nil
	}

	events, err := historySerializer.DeserializeBatchEvents(&serialization.DataBlob{
		Encoding: common.EncodingTypeProto3,
		Data:     blob.Data,
	})

	return events, err
}
