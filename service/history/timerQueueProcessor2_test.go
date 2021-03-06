// Copyright (c) 2017 Uber Technologies, Inc.
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
	"errors"
	"os"
	"testing"
	"time"

	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/mocks"
	"github.com/uber/cadence/common/persistence"

	log "github.com/Sirupsen/logrus"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"github.com/uber-common/bark"
	"github.com/uber-go/tally"
	workflow "github.com/uber/cadence/.gen/go/shared"
)

type (
	timerQueueProcessor2Suite struct {
		suite.Suite
		mockShardManager *mocks.ShardManager
		shardClosedCh    chan int
		logger           bark.Logger

		mockHistoryEngine  *historyEngineImpl
		mockMatchingClient *mocks.MatchingClient
		mockMetadataMgr    *mocks.MetadataManager
		mockVisibilityMgr  *mocks.VisibilityManager
		mockExecutionMgr   *mocks.ExecutionManager
		mockHistoryMgr     *mocks.HistoryManager
		mockShard          ShardContext
	}
)

func TestTimerQueueProcessor2Suite(t *testing.T) {
	s := new(timerQueueProcessor2Suite)
	suite.Run(t, s)
}

func (s *timerQueueProcessor2Suite) SetupSuite() {
	if testing.Verbose() {
		log.SetOutput(os.Stdout)
	}

	log2 := log.New()
	log2.Level = log.DebugLevel
	s.logger = bark.NewLoggerFromLogrus(log2)

}

func (s *timerQueueProcessor2Suite) SetupTest() {
	shardID := 0
	s.mockMatchingClient = &mocks.MatchingClient{}
	s.mockExecutionMgr = &mocks.ExecutionManager{}
	s.mockShardManager = &mocks.ShardManager{}
	s.mockHistoryMgr = &mocks.HistoryManager{}
	s.mockVisibilityMgr = &mocks.VisibilityManager{}
	s.shardClosedCh = make(chan int, 100)

	s.mockShard = &shardContextImpl{
		shardInfo:                 &persistence.ShardInfo{ShardID: shardID, RangeID: 1, TransferAckLevel: 0},
		transferSequenceNumber:    1,
		executionManager:          s.mockExecutionMgr,
		shardManager:              s.mockShardManager,
		historyMgr:                s.mockHistoryMgr,
		rangeSize:                 defaultRangeSize,
		maxTransferSequenceNumber: 100000,
		closeCh:                   s.shardClosedCh,
		logger:                    s.logger,
		metricsClient:             metrics.NewClient(tally.NoopScope, metrics.History),
	}

	historyCache := newHistoryCache(historyCacheMaxSize, s.mockShard, s.logger)
	domainCache := cache.NewDomainCache(s.mockMetadataMgr, s.logger)
	txProcessor := newTransferQueueProcessor(s.mockShard, s.mockVisibilityMgr, s.mockMatchingClient, &mocks.HistoryClient{}, historyCache, domainCache)
	h := &historyEngineImpl{
		shard:              s.mockShard,
		historyMgr:         s.mockHistoryMgr,
		executionManager:   s.mockExecutionMgr,
		txProcessor:        txProcessor,
		historyCache:       historyCache,
		logger:             s.logger,
		tokenSerializer:    common.NewJSONTaskTokenSerializer(),
		hSerializerFactory: persistence.NewHistorySerializerFactory(),
	}
	h.timerProcessor = newTimerQueueProcessor(s.mockShard, h, s.mockExecutionMgr, s.logger)
	s.mockHistoryEngine = h
}

func (s *timerQueueProcessor2Suite) TearDownTest() {
	s.mockShardManager.AssertExpectations(s.T())
	s.mockMatchingClient.AssertExpectations(s.T())
	s.mockExecutionMgr.AssertExpectations(s.T())
	s.mockHistoryMgr.AssertExpectations(s.T())
	s.mockVisibilityMgr.AssertExpectations(s.T())
}

func (s *timerQueueProcessor2Suite) TestTimerUpdateTimesOut() {
	domainID := "5bb49df8-71bc-4c63-b57f-05f2a508e7b5"
	we := workflow.WorkflowExecution{WorkflowId: common.StringPtr("timer-update-timesout-test"),
		RunId: common.StringPtr("0d00698f-08e1-4d36-a3e2-3bf109f5d2d6")}

	taskList := "user-timer-update-times-out"

	builder := newMutableStateBuilder(s.logger)
	builder.AddWorkflowExecutionStartedEvent(domainID, we, &workflow.StartWorkflowExecutionRequest{
		WorkflowType:                   &workflow.WorkflowType{Name: common.StringPtr("wType")},
		TaskList:                       common.TaskListPtr(workflow.TaskList{Name: common.StringPtr(taskList)}),
		TaskStartToCloseTimeoutSeconds: common.Int32Ptr(1),
	})

	decisionScheduledEvent, _ := addDecisionTaskScheduledEvent(builder)
	addDecisionTaskStartedEvent(builder, decisionScheduledEvent.GetEventId(), taskList, uuid.New())

	waitCh := make(chan struct{})

	mockTS := &mockTimeSource{currTime: time.Now()}

	taskID := int64(100)
	timerTask := &persistence.TimerTaskInfo{WorkflowID: "wid", RunID: "rid", TaskID: taskID,
		TaskType: persistence.TaskTypeDecisionTimeout, TimeoutType: int(workflow.TimeoutType_START_TO_CLOSE),
		VisibilityTimestamp: mockTS.Now(),
		EventID:             decisionScheduledEvent.GetEventId()}
	timerIndexResponse := &persistence.GetTimerIndexTasksResponse{Timers: []*persistence.TimerTaskInfo{timerTask}}

	s.mockExecutionMgr.On("GetTimerIndexTasks", mock.Anything).Return(timerIndexResponse, nil).Once()

	for i := 0; i < 2; i++ {
		ms := createMutableState(builder)
		wfResponse := &persistence.GetWorkflowExecutionResponse{State: ms}
		s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(wfResponse, nil).Once()
	}

	s.mockExecutionMgr.On("GetTimerIndexTasks", mock.Anything).Return(
		&persistence.GetTimerIndexTasksResponse{Timers: []*persistence.TimerTaskInfo{}}, nil)

	s.mockExecutionMgr.On("CompleteTimerTask", mock.Anything).Return(nil).Once()

	s.mockHistoryMgr.On("AppendHistoryEvents", mock.Anything).Return(nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(errors.New("FAILED")).Once()
	s.mockShardManager.On("UpdateShard", mock.Anything).Return(nil)

	s.mockHistoryMgr.On("AppendHistoryEvents", mock.Anything).Return(nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(nil).Run(func(arguments mock.Arguments) {
		// Done.
		waitCh <- struct{}{}
	}).Once()

	// Start timer Processor.
	processor := newTimerQueueProcessor(s.mockShard, s.mockHistoryEngine, s.mockExecutionMgr, s.logger).(*timerQueueProcessorImpl)
	processor.Start()

	processor.NotifyNewTimer([]persistence.Task{&persistence.DecisionTimeoutTask{
		VisibilityTimestamp: timerTask.VisibilityTimestamp,
		EventID:             timerTask.EventID,
	}})

	<-waitCh
	processor.Stop()
}
