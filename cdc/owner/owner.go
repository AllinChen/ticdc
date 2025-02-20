// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package owner

import (
	"context"
	"fmt"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	"github.com/pingcap/ticdc/cdc/model"
	cdcContext "github.com/pingcap/ticdc/pkg/context"
	cerror "github.com/pingcap/ticdc/pkg/errors"
	"github.com/pingcap/ticdc/pkg/orchestrator"
	"github.com/pingcap/ticdc/pkg/txnutil/gc"
	"github.com/pingcap/ticdc/pkg/version"
	pd "github.com/tikv/pd/client"
	"go.uber.org/zap"
)

type ownerJobType int

// All OwnerJob types
const (
	ownerJobTypeRebalance ownerJobType = iota
	ownerJobTypeManualSchedule
	ownerJobTypeAdminJob
	ownerJobTypeDebugInfo
	ownerJobTypeQuery
)

type ownerJob struct {
	tp           ownerJobType
	changefeedID model.ChangeFeedID

	// for ManualSchedule only
	targetCaptureID model.CaptureID
	// for ManualSchedule only
	tableID model.TableID

	// for Admin Job only
	adminJob *model.AdminJob

	// for debug info only
	debugInfoWriter io.Writer

	// for status provider
	query *ownerQuery

	done chan struct{}
}

// Owner manages many changefeeds
// All public functions are THREAD-SAFE, except for Tick, Tick is only used for etcd worker
type Owner struct {
	changefeeds map[model.ChangeFeedID]*changefeed
	captures    map[model.CaptureID]*model.CaptureInfo

	gcManager gc.Manager

	ownerJobQueueMu sync.Mutex
	ownerJobQueue   []*ownerJob

	lastTickTime time.Time

	closed int32

	newChangefeed func(id model.ChangeFeedID, gcManager gc.Manager) *changefeed
}

// NewOwner creates a new Owner
func NewOwner(pdClient pd.Client) *Owner {
	return &Owner{
		changefeeds:   make(map[model.ChangeFeedID]*changefeed),
		gcManager:     gc.NewManager(pdClient),
		lastTickTime:  time.Now(),
		newChangefeed: newChangefeed,
	}
}

// NewOwner4Test creates a new Owner for test
func NewOwner4Test(
	newDDLPuller func(ctx cdcContext.Context, startTs uint64) (DDLPuller, error),
	newSink func(ctx cdcContext.Context) (AsyncSink, error),
	pdClient pd.Client,
) *Owner {
	o := NewOwner(pdClient)
	o.newChangefeed = func(id model.ChangeFeedID, gcManager gc.Manager) *changefeed {
		return newChangefeed4Test(id, gcManager, newDDLPuller, newSink)
	}
	return o
}

// Tick implements the Reactor interface
func (o *Owner) Tick(stdCtx context.Context, rawState orchestrator.ReactorState) (nextState orchestrator.ReactorState, err error) {
	failpoint.Inject("owner-run-with-error", func() {
		failpoint.Return(nil, errors.New("owner run with injected error"))
	})
	failpoint.Inject("sleep-in-owner-tick", nil)
	ctx := stdCtx.(cdcContext.Context)
	state := rawState.(*orchestrator.GlobalReactorState)
	o.captures = state.Captures
	o.updateMetrics(state)

	// handleJobs() should be called before clusterVersionConsistent(), because
	// when there are different versions of cdc nodes in the cluster,
	// the admin job may not be processed all the time. And http api relies on
	// admin job, which will cause all http api unavailable.
	o.handleJobs()

	if !o.clusterVersionConsistent(state.Captures) {
		// sleep one second to avoid printing too much log
		time.Sleep(1 * time.Second)
		return state, nil
	}
	// Owner should update GC safepoint before initializing changefeed, so
	// changefeed can remove its "ticdc-creating" service GC safepoint during
	// initializing.
	//
	// See more gc doc.
	if err = o.updateGCSafepoint(stdCtx, state); err != nil {
		return nil, errors.Trace(err)
	}

	for changefeedID, changefeedState := range state.Changefeeds {
		if changefeedState.Info == nil {
			o.cleanUpChangefeed(changefeedState)
			if cfReactor, ok := o.changefeeds[changefeedID]; ok {
				cfReactor.isRemoved = true
			}
			continue
		}
		ctx = cdcContext.WithChangefeedVars(ctx, &cdcContext.ChangefeedVars{
			ID:   changefeedID,
			Info: changefeedState.Info,
		})
		cfReactor, exist := o.changefeeds[changefeedID]
		if !exist {
			cfReactor = o.newChangefeed(changefeedID, o.gcManager)
			o.changefeeds[changefeedID] = cfReactor
		}
		cfReactor.Tick(ctx, changefeedState, state.Captures)
	}
	if len(o.changefeeds) != len(state.Changefeeds) {
		for changefeedID, cfReactor := range o.changefeeds {
			if _, exist := state.Changefeeds[changefeedID]; exist {
				continue
			}
			cfReactor.Close(stdCtx)
			delete(o.changefeeds, changefeedID)
		}
	}
	if atomic.LoadInt32(&o.closed) != 0 {
		for _, cfReactor := range o.changefeeds {
			cfReactor.Close(stdCtx)
		}
		return state, cerror.ErrReactorFinished.GenWithStackByArgs()
	}
	return state, nil
}

// EnqueueJob enqueues an admin job into an internal queue, and the Owner will handle the job in the next tick
func (o *Owner) EnqueueJob(adminJob model.AdminJob) {
	o.pushOwnerJob(&ownerJob{
		tp:           ownerJobTypeAdminJob,
		adminJob:     &adminJob,
		changefeedID: adminJob.CfID,
		done:         make(chan struct{}),
	})
}

// TriggerRebalance triggers a rebalance for the specified changefeed
func (o *Owner) TriggerRebalance(cfID model.ChangeFeedID) {
	o.pushOwnerJob(&ownerJob{
		tp:           ownerJobTypeRebalance,
		changefeedID: cfID,
		done:         make(chan struct{}),
	})
}

// ManualSchedule moves a table from a capture to another capture
func (o *Owner) ManualSchedule(cfID model.ChangeFeedID, toCapture model.CaptureID, tableID model.TableID) {
	o.pushOwnerJob(&ownerJob{
		tp:              ownerJobTypeManualSchedule,
		changefeedID:    cfID,
		targetCaptureID: toCapture,
		tableID:         tableID,
		done:            make(chan struct{}),
	})
}

// WriteDebugInfo writes debug info into the specified http writer
func (o *Owner) WriteDebugInfo(w io.Writer) {
	timeout := time.Second * 3
	done := make(chan struct{})
	o.pushOwnerJob(&ownerJob{
		tp:              ownerJobTypeDebugInfo,
		debugInfoWriter: w,
		done:            done,
	})
	// wait the debug info printed
	select {
	case <-done:
	case <-time.After(timeout):
		fmt.Fprintf(w, "failed to print debug info for owner\n")
	}
}

// AsyncStop stops the owner asynchronously
func (o *Owner) AsyncStop() {
	atomic.StoreInt32(&o.closed, 1)
}

func (o *Owner) cleanUpChangefeed(state *orchestrator.ChangefeedReactorState) {
	state.PatchInfo(func(info *model.ChangeFeedInfo) (*model.ChangeFeedInfo, bool, error) {
		return nil, info != nil, nil
	})
	state.PatchStatus(func(status *model.ChangeFeedStatus) (*model.ChangeFeedStatus, bool, error) {
		return nil, status != nil, nil
	})
	for captureID := range state.TaskStatuses {
		state.PatchTaskStatus(captureID, func(status *model.TaskStatus) (*model.TaskStatus, bool, error) {
			return nil, status != nil, nil
		})
	}
	for captureID := range state.TaskPositions {
		state.PatchTaskPosition(captureID, func(position *model.TaskPosition) (*model.TaskPosition, bool, error) {
			return nil, position != nil, nil
		})
	}
	for captureID := range state.Workloads {
		state.PatchTaskWorkload(captureID, func(workload model.TaskWorkload) (model.TaskWorkload, bool, error) {
			return nil, workload != nil, nil
		})
	}
}

func (o *Owner) updateMetrics(state *orchestrator.GlobalReactorState) {
	// Keep the value of prometheus expression `rate(counter)` = 1
	// Please also change alert rule in ticdc.rules.yml when change the expression value.
	now := time.Now()
	ownershipCounter.Add(float64(now.Sub(o.lastTickTime)) / float64(time.Second))
	o.lastTickTime = now

	ownerMaintainTableNumGauge.Reset()
	changefeedStatusGauge.Reset()
	for changefeedID, changefeedState := range state.Changefeeds {
		for captureID, captureInfo := range state.Captures {
			taskStatus, exist := changefeedState.TaskStatuses[captureID]
			if !exist {
				continue
			}
			ownerMaintainTableNumGauge.WithLabelValues(changefeedID, captureInfo.AdvertiseAddr, maintainTableTypeTotal).Set(float64(len(taskStatus.Tables)))
			ownerMaintainTableNumGauge.WithLabelValues(changefeedID, captureInfo.AdvertiseAddr, maintainTableTypeWip).Set(float64(len(taskStatus.Operation)))
			if changefeedState.Info != nil {
				changefeedStatusGauge.WithLabelValues(changefeedID).Set(float64(changefeedState.Info.State.ToInt()))
			}
		}
	}
}

func (o *Owner) clusterVersionConsistent(captures map[model.CaptureID]*model.CaptureInfo) bool {
	myVersion := version.ReleaseVersion
	for _, capture := range captures {
		if myVersion != capture.Version {
			log.Warn("the capture version is different with the owner", zap.Reflect("capture", capture), zap.String("my-version", myVersion))
			return false
		}
	}
	return true
}

func (o *Owner) handleJobs() {
	jobs := o.takeOwnerJobs()
	for _, job := range jobs {
		changefeedID := job.changefeedID
		cfReactor, exist := o.changefeeds[changefeedID]
		if !exist && job.tp != ownerJobTypeQuery {
			log.Warn("changefeed not found when handle a job", zap.Reflect("job", job))
			continue
		}
		switch job.tp {
		case ownerJobTypeAdminJob:
			cfReactor.feedStateManager.PushAdminJob(job.adminJob)
		case ownerJobTypeManualSchedule:
			cfReactor.scheduler.MoveTable(job.tableID, job.targetCaptureID)
		case ownerJobTypeRebalance:
			cfReactor.scheduler.Rebalance()
		case ownerJobTypeQuery:
			o.handleQueries(job.query)
		case ownerJobTypeDebugInfo:
			// TODO: implement this function
		}
		close(job.done)
	}
}

func (o *Owner) handleQueries(query *ownerQuery) {
	switch query.tp {
	case ownerQueryAllChangeFeedStatuses:
		ret := map[model.ChangeFeedID]*model.ChangeFeedStatus{}
		for cfID, cfReactor := range o.changefeeds {
			ret[cfID] = &model.ChangeFeedStatus{}
			if cfReactor.state == nil {
				continue
			}
			if cfReactor.state.Status == nil {
				continue
			}
			ret[cfID].ResolvedTs = cfReactor.state.Status.ResolvedTs
			ret[cfID].CheckpointTs = cfReactor.state.Status.CheckpointTs
			ret[cfID].AdminJobType = cfReactor.state.Status.AdminJobType
		}
		query.data = ret
	case ownerQueryAllChangeFeedInfo:
		ret := map[model.ChangeFeedID]*model.ChangeFeedInfo{}
		for cfID, cfReactor := range o.changefeeds {
			if cfReactor.state == nil {
				continue
			}
			if cfReactor.state.Info == nil {
				ret[cfID] = &model.ChangeFeedInfo{}
				continue
			}
			var err error
			ret[cfID], err = cfReactor.state.Info.Clone()
			if err != nil {
				query.err = errors.Trace(err)
				return
			}
		}
		query.data = ret
	case ownerQueryAllTaskStatuses:
		cfReactor, ok := o.changefeeds[query.changeFeedID]
		if !ok {
			query.err = cerror.ErrChangeFeedNotExists.GenWithStackByArgs(query.changeFeedID)
			return
		}
		if cfReactor.state == nil {
			query.err = cerror.ErrChangeFeedNotExists.GenWithStackByArgs(query.changeFeedID)
			return
		}
		ret := map[model.CaptureID]*model.TaskStatus{}
		for captureID, taskStatus := range cfReactor.state.TaskStatuses {
			ret[captureID] = taskStatus.Clone()
		}
		query.data = ret
	case ownerQueryTaskPositions:
		cfReactor, ok := o.changefeeds[query.changeFeedID]
		if !ok {
			query.err = cerror.ErrChangeFeedNotExists.GenWithStackByArgs(query.changeFeedID)
			return
		}
		if cfReactor.state == nil {
			query.err = cerror.ErrChangeFeedNotExists.GenWithStackByArgs(query.changeFeedID)
			return
		}
		ret := map[model.CaptureID]*model.TaskPosition{}
		for captureID, taskPosition := range cfReactor.state.TaskPositions {
			ret[captureID] = taskPosition.Clone()
		}
		query.data = ret
	case ownerQueryProcessors:
		var ret []*model.ProcInfoSnap
		for cfID, cfReactor := range o.changefeeds {
			if cfReactor.state == nil {
				continue
			}
			for captureID := range cfReactor.state.TaskStatuses {
				ret = append(ret, &model.ProcInfoSnap{
					CfID:      cfID,
					CaptureID: captureID,
				})
			}
		}
		query.data = ret
	case ownerQueryCaptures:
		var ret []*model.CaptureInfo
		for _, captureInfo := range o.captures {
			ret = append(ret, &model.CaptureInfo{
				ID:            captureInfo.ID,
				AdvertiseAddr: captureInfo.AdvertiseAddr,
				Version:       captureInfo.Version,
			})
		}
		query.data = ret
	}
}

func (o *Owner) takeOwnerJobs() []*ownerJob {
	o.ownerJobQueueMu.Lock()
	defer o.ownerJobQueueMu.Unlock()

	jobs := o.ownerJobQueue
	o.ownerJobQueue = nil
	return jobs
}

func (o *Owner) pushOwnerJob(job *ownerJob) {
	o.ownerJobQueueMu.Lock()
	defer o.ownerJobQueueMu.Unlock()
	o.ownerJobQueue = append(o.ownerJobQueue, job)
}

func (o *Owner) updateGCSafepoint(
	ctx context.Context, state *orchestrator.GlobalReactorState,
) error {
	forceUpdate := false
	minCheckpointTs := uint64(math.MaxUint64)
	for changefeedID, changefeefState := range state.Changefeeds {
		if changefeefState.Info == nil {
			continue
		}
		switch changefeefState.Info.State {
		case model.StateNormal, model.StateStopped, model.StateError:
		default:
			continue
		}
		checkpointTs := changefeefState.Info.GetCheckpointTs(changefeefState.Status)
		if minCheckpointTs > checkpointTs {
			minCheckpointTs = checkpointTs
		}
		// Force update when adding a new changefeed.
		_, exist := o.changefeeds[changefeedID]
		if !exist {
			forceUpdate = true
		}
	}
	// When the changefeed starts up, CDC will do a snapshot read at
	// (checkpointTs - 1) from TiKV, so (checkpointTs - 1) should be an upper
	// bound for the GC safepoint.
	gcSafepointUpperBound := minCheckpointTs - 1
	err := o.gcManager.TryUpdateGCSafePoint(ctx, gcSafepointUpperBound, forceUpdate)
	return errors.Trace(err)
}

// StatusProvider returns a StatusProvider
func (o *Owner) StatusProvider() StatusProvider {
	return &ownerStatusProvider{owner: o}
}
