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

package sink

import (
	"context"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/pingcap/check"
	"github.com/pingcap/errors"
	"github.com/pingcap/ticdc/cdc/model"
	"github.com/pingcap/ticdc/pkg/notify"
	"github.com/pingcap/ticdc/pkg/util/testleak"
	"golang.org/x/sync/errgroup"
)

func (s MySQLSinkSuite) TestMysqlSinkWorker(c *check.C) {
	defer testleak.AfterTest(c)()
	testCases := []struct {
		txns                     []*model.SingleTableTxn
		expectedOutputRows       [][]*model.RowChangedEvent
		exportedOutputReplicaIDs []uint64
		maxTxnRow                int
	}{
		{
			txns:      []*model.SingleTableTxn{},
			maxTxnRow: 4,
		}, {
			txns: []*model.SingleTableTxn{
				{
					CommitTs:  1,
					Rows:      []*model.RowChangedEvent{{CommitTs: 1}},
					ReplicaID: 1,
				},
			},
			expectedOutputRows:       [][]*model.RowChangedEvent{{{CommitTs: 1}}},
			exportedOutputReplicaIDs: []uint64{1},
			maxTxnRow:                2,
		}, {
			txns: []*model.SingleTableTxn{
				{
					CommitTs:  1,
					Rows:      []*model.RowChangedEvent{{CommitTs: 1}, {CommitTs: 1}, {CommitTs: 1}},
					ReplicaID: 1,
				},
			},
			expectedOutputRows: [][]*model.RowChangedEvent{
				{{CommitTs: 1}, {CommitTs: 1}, {CommitTs: 1}},
			},
			exportedOutputReplicaIDs: []uint64{1},
			maxTxnRow:                2,
		}, {
			txns: []*model.SingleTableTxn{
				{
					CommitTs:  1,
					Rows:      []*model.RowChangedEvent{{CommitTs: 1}, {CommitTs: 1}},
					ReplicaID: 1,
				},
				{
					CommitTs:  2,
					Rows:      []*model.RowChangedEvent{{CommitTs: 2}},
					ReplicaID: 1,
				},
				{
					CommitTs:  3,
					Rows:      []*model.RowChangedEvent{{CommitTs: 3}, {CommitTs: 3}},
					ReplicaID: 1,
				},
			},
			expectedOutputRows: [][]*model.RowChangedEvent{
				{{CommitTs: 1}, {CommitTs: 1}, {CommitTs: 2}},
				{{CommitTs: 3}, {CommitTs: 3}},
			},
			exportedOutputReplicaIDs: []uint64{1, 1},
			maxTxnRow:                4,
		}, {
			txns: []*model.SingleTableTxn{
				{
					CommitTs:  1,
					Rows:      []*model.RowChangedEvent{{CommitTs: 1}},
					ReplicaID: 1,
				},
				{
					CommitTs:  2,
					Rows:      []*model.RowChangedEvent{{CommitTs: 2}},
					ReplicaID: 2,
				},
				{
					CommitTs:  3,
					Rows:      []*model.RowChangedEvent{{CommitTs: 3}},
					ReplicaID: 3,
				},
			},
			expectedOutputRows: [][]*model.RowChangedEvent{
				{{CommitTs: 1}},
				{{CommitTs: 2}},
				{{CommitTs: 3}},
			},
			exportedOutputReplicaIDs: []uint64{1, 2, 3},
			maxTxnRow:                4,
		}, {
			txns: []*model.SingleTableTxn{
				{
					CommitTs:  1,
					Rows:      []*model.RowChangedEvent{{CommitTs: 1}},
					ReplicaID: 1,
				},
				{
					CommitTs:  2,
					Rows:      []*model.RowChangedEvent{{CommitTs: 2}, {CommitTs: 2}, {CommitTs: 2}},
					ReplicaID: 1,
				},
				{
					CommitTs:  3,
					Rows:      []*model.RowChangedEvent{{CommitTs: 3}},
					ReplicaID: 1,
				},
				{
					CommitTs:  4,
					Rows:      []*model.RowChangedEvent{{CommitTs: 4}},
					ReplicaID: 1,
				},
			},
			expectedOutputRows: [][]*model.RowChangedEvent{
				{{CommitTs: 1}},
				{{CommitTs: 2}, {CommitTs: 2}, {CommitTs: 2}},
				{{CommitTs: 3}, {CommitTs: 4}},
			},
			exportedOutputReplicaIDs: []uint64{1, 1, 1},
			maxTxnRow:                2,
		},
	}
	ctx := context.Background()

	notifier := new(notify.Notifier)
	for i, tc := range testCases {
		cctx, cancel := context.WithCancel(ctx)
		var outputRows [][]*model.RowChangedEvent
		var outputReplicaIDs []uint64
		receiver, err := notifier.NewReceiver(-1)
		c.Assert(err, check.IsNil)
		w := newMySQLSinkWorker(tc.maxTxnRow, 1,
			bucketSizeCounter.WithLabelValues("capture", "changefeed", "1"),
			receiver,
			func(ctx context.Context, events []*model.RowChangedEvent, replicaID uint64, bucket int) error {
				outputRows = append(outputRows, events)
				outputReplicaIDs = append(outputReplicaIDs, replicaID)
				return nil
			})
		errg, cctx := errgroup.WithContext(cctx)
		errg.Go(func() error {
			return w.run(cctx)
		})
		for _, txn := range tc.txns {
			w.appendTxn(cctx, txn)
		}
		var wg sync.WaitGroup
		w.appendFinishTxn(&wg)
		// ensure all txns are fetched from txn channel in sink worker
		time.Sleep(time.Millisecond * 100)
		notifier.Notify()
		wg.Wait()
		cancel()
		c.Assert(errors.Cause(errg.Wait()), check.Equals, context.Canceled)
		c.Assert(outputRows, check.DeepEquals, tc.expectedOutputRows,
			check.Commentf("case %v, %s, %s", i, spew.Sdump(outputRows), spew.Sdump(tc.expectedOutputRows)))
		c.Assert(outputReplicaIDs, check.DeepEquals, tc.exportedOutputReplicaIDs,
			check.Commentf("case %v, %s, %s", i, spew.Sdump(outputReplicaIDs), spew.Sdump(tc.exportedOutputReplicaIDs)))
	}
}

func (s MySQLSinkSuite) TestMySQLSinkWorkerExitWithError(c *check.C) {
	defer testleak.AfterTest(c)()
	txns1 := []*model.SingleTableTxn{
		{
			CommitTs: 1,
			Rows:     []*model.RowChangedEvent{{CommitTs: 1}},
		},
		{
			CommitTs: 2,
			Rows:     []*model.RowChangedEvent{{CommitTs: 2}},
		},
		{
			CommitTs: 3,
			Rows:     []*model.RowChangedEvent{{CommitTs: 3}},
		},
		{
			CommitTs: 4,
			Rows:     []*model.RowChangedEvent{{CommitTs: 4}},
		},
	}
	txns2 := []*model.SingleTableTxn{
		{
			CommitTs: 5,
			Rows:     []*model.RowChangedEvent{{CommitTs: 5}},
		},
		{
			CommitTs: 6,
			Rows:     []*model.RowChangedEvent{{CommitTs: 6}},
		},
	}
	maxTxnRow := 1
	ctx := context.Background()

	errExecFailed := errors.New("sink worker exec failed")
	notifier := new(notify.Notifier)
	cctx, cancel := context.WithCancel(ctx)
	receiver, err := notifier.NewReceiver(-1)
	c.Assert(err, check.IsNil)
	w := newMySQLSinkWorker(maxTxnRow, 1, /*bucket*/
		bucketSizeCounter.WithLabelValues("capture", "changefeed", "1"),
		receiver,
		func(ctx context.Context, events []*model.RowChangedEvent, replicaID uint64, bucket int) error {
			return errExecFailed
		})
	errg, cctx := errgroup.WithContext(cctx)
	errg.Go(func() error {
		return w.run(cctx)
	})
	// txn in txns1 will be sent to worker txnCh
	for _, txn := range txns1 {
		w.appendTxn(cctx, txn)
	}

	// simulate notify sink worker to flush existing txns
	var wg sync.WaitGroup
	w.appendFinishTxn(&wg)
	time.Sleep(time.Millisecond * 100)
	// txn in txn2 will be blocked since the worker has exited
	for _, txn := range txns2 {
		w.appendTxn(cctx, txn)
	}
	notifier.Notify()

	// simulate sink shutdown and send closed singal to sink worker
	w.closedCh <- struct{}{}
	w.cleanup()

	// the flush notification wait group should be done
	wg.Wait()

	cancel()
	c.Assert(errg.Wait(), check.Equals, errExecFailed)
}

func (s MySQLSinkSuite) TestMySQLSinkWorkerExitCleanup(c *check.C) {
	defer testleak.AfterTest(c)()
	txns1 := []*model.SingleTableTxn{
		{
			CommitTs: 1,
			Rows:     []*model.RowChangedEvent{{CommitTs: 1}},
		},
		{
			CommitTs: 2,
			Rows:     []*model.RowChangedEvent{{CommitTs: 2}},
		},
	}
	txns2 := []*model.SingleTableTxn{
		{
			CommitTs: 5,
			Rows:     []*model.RowChangedEvent{{CommitTs: 5}},
		},
	}

	maxTxnRow := 1
	ctx := context.Background()

	errExecFailed := errors.New("sink worker exec failed")
	notifier := new(notify.Notifier)
	cctx, cancel := context.WithCancel(ctx)
	receiver, err := notifier.NewReceiver(-1)
	c.Assert(err, check.IsNil)
	w := newMySQLSinkWorker(maxTxnRow, 1, /*bucket*/
		bucketSizeCounter.WithLabelValues("capture", "changefeed", "1"),
		receiver,
		func(ctx context.Context, events []*model.RowChangedEvent, replicaID uint64, bucket int) error {
			return errExecFailed
		})
	errg, cctx := errgroup.WithContext(cctx)
	errg.Go(func() error {
		err := w.run(cctx)
		return err
	})
	for _, txn := range txns1 {
		w.appendTxn(cctx, txn)
	}

	// sleep to let txns flushed by tick
	time.Sleep(time.Millisecond * 100)

	// simulate more txns are sent to txnCh after the sink worker run has exited
	for _, txn := range txns2 {
		w.appendTxn(cctx, txn)
	}
	var wg sync.WaitGroup
	w.appendFinishTxn(&wg)
	notifier.Notify()

	// simulate sink shutdown and send closed singal to sink worker
	w.closedCh <- struct{}{}
	w.cleanup()

	// the flush notification wait group should be done
	wg.Wait()

	cancel()
	c.Assert(errg.Wait(), check.Equals, errExecFailed)
}
