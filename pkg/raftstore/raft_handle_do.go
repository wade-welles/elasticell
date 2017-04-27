// Copyright 2016 DeepFabric, Inc.
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

package raftstore

import (
	"bytes"
	"fmt"
	"sync/atomic"

	"time"

	"github.com/Workiva/go-datastructures/queue"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/deepfabric/elasticell/pkg/log"
	"github.com/deepfabric/elasticell/pkg/pb/metapb"
	"github.com/deepfabric/elasticell/pkg/pb/mraft"
	"github.com/deepfabric/elasticell/pkg/pb/pdpb"
	"github.com/deepfabric/elasticell/pkg/pb/raftcmdpb"
	"github.com/deepfabric/elasticell/pkg/util"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

type tempRaftContext struct {
	raftState  mraft.RaftLocalState
	applyState mraft.RaftApplyState
	lastTerm   uint64
	snapCell   *metapb.Cell
}

type applySnapResult struct {
	prevCell metapb.Cell
	cell     metapb.Cell
}

type readIndexQueue struct {
	cellID   uint64
	reads    *queue.RingBuffer
	readyCnt int32
}

func (q *readIndexQueue) push(cmd *cmd) error {
	return q.reads.Put(cmd)
}

func (q *readIndexQueue) pop() *cmd {
	v, err := q.reads.Get()
	if err != nil {
		log.Fatalf("raftstore[cell-%d]: handle read index failed, errors:\n %+v",
			q.cellID,
			err)
	}

	return v.(*cmd)
}

func (q *readIndexQueue) incrReadyCnt() int32 {
	return atomic.AddInt32(&q.readyCnt, 1)
}

func (q *readIndexQueue) decrReadyCnt() int32 {
	return atomic.AddInt32(&q.readyCnt, -1)
}

func (q *readIndexQueue) resetReadyCnt() {
	atomic.StoreInt32(&q.readyCnt, 0)
}

func (q *readIndexQueue) getReadyCnt() int32 {
	return atomic.LoadInt32(&q.readyCnt)
}

// ====================== raft ready handle methods
func (ps *peerStorage) doAppendSnapshot(ctx *tempRaftContext, snap raftpb.Snapshot) error {
	log.Infof("raftstore[cell-%d]: begin to apply snapshot", ps.getCell().ID)

	snapData := &mraft.RaftSnapshotData{}
	util.MustUnmarshal(snapData, snap.Data)

	if snapData.Cell.ID != ps.getCell().ID {
		return fmt.Errorf("raftstore[cell-%d]: cell not match, snapCell=<%d> currCell=<%d>",
			ps.getCell().ID,
			snapData.Cell.ID,
			ps.getCell().ID)
	}

	if ps.isInitialized() {
		err := ps.clearMeta()
		if err != nil {
			log.Errorf("raftstore[cell-%d]: clear meta failed, errors:\n %+v",
				ps.getCell().ID,
				err)
			return err
		}
	}

	err := ps.updatePeerState(ps.getCell(), mraft.Applying)
	if err != nil {
		log.Errorf("raftstore[cell-%d]: write peer state failed, errors:\n %+v",
			ps.getCell().ID,
			err)
		return err
	}

	lastIndex := snap.Metadata.Index
	lastTerm := snap.Metadata.Term

	ctx.raftState.LastIndex = lastIndex
	ctx.applyState.AppliedIndex = lastIndex
	ctx.lastTerm = lastTerm

	// The snapshot only contains log which index > applied index, so
	// here the truncate state's (index, term) is in snapshot metadata.
	ctx.applyState.TruncatedState.Index = lastIndex
	ctx.applyState.TruncatedState.Term = lastTerm

	log.Infof("raftstore[cell-%d]: apply snapshot ok, state=<%s>",
		ps.getCell().ID,
		ctx.applyState.String())

	c := snapData.Cell
	ctx.snapCell = &c

	return nil
}

// doAppendEntries the given entries to the raft log using previous last index or self.last_index.
// Return the new last index for later update. After we commit in engine, we can set last_index
// to the return one.
func (ps *peerStorage) doAppendEntries(ctx *tempRaftContext, entries []raftpb.Entry) error {
	c := len(entries)

	log.Debugf("raftstore[cell-%d]: append entries, count=<%d>",
		ps.getCell().ID,
		c)

	if c == 0 {
		return nil
	}

	prevLastIndex := ctx.raftState.LastIndex
	lastIndex := entries[c-1].Index
	lastTerm := entries[c-1].Term

	for _, e := range entries {
		d := util.MustMarshal(&e)
		err := ps.store.getMetaEngine().Set(getRaftLogKey(ps.getCell().ID, e.Index), d)
		if err != nil {
			log.Errorf("raftstore[cell-%d]: append entry failure, entry=<%s> errors:\n %+v",
				ps.getCell().ID,
				e.String(),
				err)
			return err
		}
	}

	// Delete any previously appended log entries which never committed.
	for index := lastIndex + 1; index < prevLastIndex+1; index++ {
		err := ps.store.getMetaEngine().Delete(getRaftLogKey(ps.getCell().ID, index))
		if err != nil {
			log.Errorf("raftstore[cell-%d]: delete any previously appended log entries failure, index=<%d> errors:\n %+v",
				ps.getCell().ID,
				index,
				err)
			return err
		}
	}

	ctx.raftState.LastIndex = lastIndex
	ctx.lastTerm = lastTerm

	return nil
}

func (pr *PeerReplicate) doSaveRaftState(ctx *tempRaftContext) error {
	data, _ := ctx.raftState.Marshal()
	err := pr.store.getMetaEngine().Set(getRaftStateKey(pr.ps.getCell().ID), data)
	if err != nil {
		log.Errorf("raftstore[cell-%d]: save temp raft state failure, errors:\n %+v",
			pr.ps.getCell().ID,
			err)
	}

	return err
}

func (pr *PeerReplicate) doSaveApplyState(ctx *tempRaftContext) error {
	err := pr.store.getMetaEngine().Set(getApplyStateKey(pr.ps.getCell().ID), util.MustMarshal(&ctx.applyState))
	if err != nil {
		log.Errorf("raftstore[cell-%d]: save temp apply state failure, errors:\n %+v",
			pr.ps.getCell().ID,
			err)
	}

	return err
}

func (pr *PeerReplicate) doApplySnap(ctx *tempRaftContext) *applySnapResult {
	pr.ps.raftState = ctx.raftState
	pr.ps.setApplyState(&ctx.applyState)
	pr.ps.lastTerm = ctx.lastTerm

	// If we apply snapshot ok, we should update some infos like applied index too.
	if ctx.snapCell == nil {
		return nil
	}

	// cleanup data before apply snap job
	if pr.ps.isInitialized() {
		// TODO: why??
		err := pr.ps.clearExtraData(pr.ps.getCell())
		if err != nil {
			// No need panic here, when applying snapshot, the deletion will be tried
			// again. But if the region range changes, like [a, c) -> [a, b) and [b, c),
			// [b, c) will be kept in rocksdb until a covered snapshot is applied or
			// store is restarted.
			log.Errorf("raftstore[cell-%d]: cleanup data failed, may leave some dirty data, errors:\n %+v",
				pr.cellID,
				err)
			return nil
		}
	}

	pr.startApplyingSnapJob()

	prevCell := pr.ps.getCell()
	pr.ps.setCell(*ctx.snapCell)

	return &applySnapResult{
		prevCell: prevCell,
		cell:     pr.ps.getCell(),
	}
}

func (pr *PeerReplicate) applyCommittedEntries(rd *raft.Ready) bool {
	if !pr.ps.isApplyingSnap() {
		if len(rd.CommittedEntries) > 0 {
			err := pr.startApplyCommittedEntriesJob(pr.ps.getCell().ID, pr.getCurrentTerm(), rd.CommittedEntries)
			if err != nil {
				log.Fatalf("raftstore[cell-%d]: add apply committed entries job failed, errors:\n %+v",
					pr.ps.getCell().ID,
					err)
			}

			return true
		}
	}

	return false
}

func (pr *PeerReplicate) doPropose(meta *proposalMeta, isConfChange bool, cmd *cmd) error {
	delegate := pr.store.delegates.get(pr.cellID)
	if delegate == nil {
		cmd.respCellNotFound(pr.cellID, meta.term)
		return nil
	}

	if delegate.cell.ID != pr.cellID {
		log.Fatal("bug: delegate id not match")
	}

	if isConfChange {
		c := delegate.getPendingChangePeerCMD()
		if nil != delegate.getPendingChangePeerCMD() {
			delegate.notifyStaleCMD(c)
		}
		delegate.setPedingChangePeerCMD(meta.term, cmd)
	} else {
		delegate.appendPendingCmd(meta.term, cmd)
	}

	return nil
}

func (pr *PeerReplicate) doSplitCheck(epoch metapb.CellEpoch, startKey, endKey []byte) error {
	var size uint64
	var splitKey []byte

	err := pr.store.getDataEngine().Scan(startKey, endKey, func(key, value []byte) (bool, error) {
		size += uint64(len(value))

		if len(splitKey) == 0 && size > pr.store.cfg.CellSplitSize {
			splitKey = key
		}

		if size > pr.store.cfg.CellMaxSize {
			return false, nil
		}

		return true, nil
	})

	if err != nil {
		log.Errorf("raftstore-split[cell-%d]: failed to scan split key, errors:\n %+v",
			pr.cellID,
			err)
		return err
	}

	if size < pr.store.cfg.CellMaxSize {
		log.Debugf("raftstore-split[cell-%d]: no need to split, size=<%d> max=<%d>",
			pr.cellID,
			size,
			pr.store.cfg.CellMaxSize)
		return nil
	}

	pr.store.notify(&splitCheckResult{
		cellID:   pr.cellID,
		splitKey: splitKey,
		epoch:    epoch,
	})

	return nil
}

func (pr *PeerReplicate) doAskSplit(cell metapb.Cell, peer metapb.Peer, splitKey []byte) error {
	req := &pdpb.AskSplitReq{
		Cell: cell,
	}

	rsp, err := pr.store.pdClient.AskSplit(context.TODO(), req)
	if err != nil {
		log.Debugf("raftstore-split[cell-%d]: ask split to pd failed, error:\n %+v",
			pr.cellID,
			err)
		return err
	}

	splitReq := new(raftcmdpb.SplitRequest)
	splitReq.NewCellID = rsp.NewCellID
	splitReq.SplitKey = splitKey
	splitReq.NewPeerIDs = rsp.NewPeerIDs

	adminReq := new(raftcmdpb.AdminRequest)
	adminReq.Type = raftcmdpb.Split
	adminReq.Body = util.MustMarshal(splitReq)

	pr.store.sendAdminRequest(cell, peer, adminReq)
	return nil
}

func (pr *PeerReplicate) doPostApply(result *asyncApplyResult) {
	if pr.ps.isApplyingSnap() {
		log.Fatalf("raftstore[cell-%d]: should not applying snapshot, when do post apply.",
			pr.cellID)
	}

	log.Debugf("raftstore[cell-%d]: async apply committied entries finished", pr.cellID)

	pr.ps.setApplyState(&result.applyState)
	pr.ps.setAppliedIndexTerm(result.appliedIndexTerm)
	pr.writtenBytes += result.metrics.writtenBytes
	pr.writtenKeys += result.metrics.writtenKeys

	if result.hasSplitExecResult() {
		pr.deleteKeysHint = result.metrics.deleteKeysHint
		pr.sizeDiffHint = result.metrics.sizeDiffHint
	} else {
		pr.deleteKeysHint += result.metrics.deleteKeysHint
		pr.sizeDiffHint += result.metrics.sizeDiffHint
	}

	readyCnt := int(pr.pendingReads.getReadyCnt())
	if readyCnt > 0 && pr.readyToHandleRead() {
		for index := 0; index < readyCnt; index++ {
			pr.doExecReadCmd(pr.pendingReads.pop())
		}

		pr.pendingReads.resetReadyCnt()
	}
}

func (s *Store) doPostApplyResult(result *asyncApplyResult) {
	switch result.result.adminType {
	case raftcmdpb.ChangePeer:
		s.doApplyConfChange(result.cellID, result.result.changePeer)
	case raftcmdpb.Split:
		s.doApplySplit(result.cellID, result.result.splitResult)
	}
}

func (s *Store) doApplyConfChange(cellID uint64, cp *changePeer) {
	pr := s.replicatesMap.get(cellID)
	if nil == pr {
		log.Fatalf("raftstore-apply[cell-%d]: missing cell",
			cellID)
	}

	pr.rn.ApplyConfChange(cp.confChange)
	if cp.confChange.NodeID == 0 {
		// Apply failed, skip.
		return
	}

	pr.ps.setCell(cp.cell)

	if pr.isLeader() {
		// Notify pd immediately.
		log.Infof("raftstore-apply[cell-%d]: notify pd with change peer, cell=<%+v>",
			cellID,
			cp.cell)
		pr.handleHeartbeat()
	}

	switch cp.confChange.Type {
	case raftpb.ConfChangeAddNode:
		// Add this peer to cache.
		pr.peerHeartbeatsMap.put(cp.peer.ID, time.Now())
		s.peerCache.put(cp.peer.ID, cp.peer)
	case raftpb.ConfChangeRemoveNode:
		// Remove this peer from cache.
		pr.peerHeartbeatsMap.delete(cp.peer.ID)
		s.peerCache.delete(cp.peer.ID)

		// We only care remove itself now.
		if cp.peer.StoreID == pr.store.GetID() {
			if cp.peer.ID == pr.peer.ID {
				s.destroyPeer(cellID, cp.peer, false)
			} else {
				log.Fatalf("raftstore-apply[cell-%d]: trying to remove unknown peer, peer=<%+v>",
					cellID,
					cp.peer)
			}
		}
	}
}

func (s *Store) doApplySplit(cellID uint64, result *splitResult) {
	pr := s.replicatesMap.get(cellID)
	if nil == pr {
		log.Fatalf("raftstore-apply[cell-%d]: missing cell",
			cellID)
	}

	left := result.left
	right := result.right

	pr.ps.setCell(left)

	// add new cell peers to cache
	for _, p := range right.Peers {
		s.peerCache.put(p.ID, *p)
	}

	newCellID := right.ID
	newPR := s.replicatesMap.get(newCellID)
	if nil != newPR {
		for _, p := range right.Peers {
			s.peerCache.put(p.ID, *p)
		}

		// If the store received a raft msg with the new region raft group
		// before splitting, it will creates a uninitialized peer.
		// We can remove this uninitialized peer directly.
		if newPR.ps.isInitialized() {
			log.Fatalf("raftstore-apply[cell-%d]: duplicated cell for split, newCellID=<%d>",
				cellID,
				newCellID)
		}
	}

	newPR, err := createPeerReplicate(s, &right)
	if err != nil {
		// peer information is already written into db, can't recover.
		// there is probably a bug.
		log.Fatalf("raftstore-apply[cell-%d]: create new split cell failed, newCell=<%d> errors:\n %+v",
			cellID,
			right,
			err)
	}

	// If this peer is the leader of the cell before split, it's intuitional for
	// it to become the leader of new split cell.
	// The ticks are accelerated here, so that the peer for the new split cell
	// comes to campaign earlier than the other follower peers. And then it's more
	// likely for this peer to become the leader of the new split cell.
	// If the other follower peers applies logs too slowly, they may fail to vote the
	// `MsgRequestVote` from this peer on its campaign.
	// In this worst case scenario, the new split raft group will not be available
	// since there is no leader established during one election timeout after the split.
	if pr.isLeader() && len(right.Peers) > 1 {
		// TODO: accelerate tick for first election timeout, it will most become leader
	}

	if pr.isLeader() {
		log.Infof("raftstore-apply[cell-%d]: notify pd with split, left=<%+v> right=<%+v>",
			cellID,
			left,
			right)

		pr.handleHeartbeat()
		newPR.handleHeartbeat()

		err := s.startReportSpltJob(left, right)
		log.Errorf("raftstore-apply[cell-%d]: add report split job failed, errors:\n %+v",
			cellID,
			err)
	}

	log.Infof("raftstore-apply[cell-%d]: insert new cell, left=<%+v> right <%+v>",
		cellID,
		left,
		right)
	s.keyRanges.Update(left)
	s.keyRanges.Update(right)

	newPR.sizeDiffHint = s.cfg.CellCheckSizeDiff
	newPR.startRegistrationJob()
	s.replicatesMap.put(newPR.cellID, newPR)
}

func (pr *PeerReplicate) doApplyReads(rd *raft.Ready) {
	if pr.readyToHandleRead() {
		for _, state := range rd.ReadStates {
			cmd := pr.pendingReads.pop()

			if bytes.Compare(state.RequestCtx, cmd.getUUID()) != 0 {
				log.Fatalf("raftstore[cell-%d]: apply read failed, uuid not match",
					pr.cellID)
			}

			pr.doExecReadCmd(cmd)
		}
	} else {
		for _ = range rd.ReadStates {
			pr.pendingReads.incrReadyCnt()
		}
	}

	// Note that only after handle read_states can we identify what requests are
	// actually stale.
	if rd.SoftState != nil {
		if rd.SoftState.RaftState != raft.StateLeader {
			n := int(pr.pendingReads.getReadyCnt())
			if n > 0 {
				// all uncommitted reads will be dropped silently in raft.
				for index := 0; index < n; index++ {
					cmd := pr.pendingReads.pop()
					resp := errorStaleCMDResp(cmd.getUUID(), pr.getCurrentTerm())

					log.Infof("raftstore[cell-%d]: cmd is stale, skip. cmd=<%+v>",
						pr.cellID,
						cmd)
					cmd.resp(resp)
				}
			}
		}
	}
}

func (pr *PeerReplicate) updateKeyRange(result *applySnapResult) {
	log.Infof("raftstore[cell-%d]: snapshot is applied, cell=<%+v>",
		pr.cellID,
		result.cell)

	if len(result.prevCell.Peers) > 0 {
		log.Infof("raftstore[cell-%d]: cell changed after apply snapshot, from=<%+v> to=<%+v>",
			pr.cellID,
			result.prevCell,
			result.cell)
		// we have already initialized the peer, so it must exist in cell_ranges.
		if !pr.store.keyRanges.Remove(result.prevCell) {
			log.Fatalf("raftstore[cell-%d]: cell not exist, cell=<%+v>",
				pr.cellID,
				result.prevCell)
		}
	}

	pr.store.keyRanges.Update(result.cell)
}

func (pr *PeerReplicate) readyToHandleRead() bool {
	// If applied_index_term isn't equal to current term, there may be some values that are not
	// applied by this leader yet but the old leader.
	return pr.ps.getAppliedIndexTerm() == pr.getCurrentTerm()
}

// ======================raft storage interface method
func (ps *peerStorage) InitialState() (raftpb.HardState, raftpb.ConfState, error) {
	hardState := ps.raftState.HardState
	confState := raftpb.ConfState{}

	if hardState.Commit == 0 &&
		hardState.Term == 0 &&
		hardState.Vote == 0 {
		if ps.isInitialized() {
			log.Fatalf("raftstore[cell-%d]: cell is initialized but local state has empty hard state, hardState=<%v>",
				ps.getCell().ID,
				hardState)
		}

		return hardState, confState, nil
	}

	for _, p := range ps.getCell().Peers {
		confState.Nodes = append(confState.Nodes, p.ID)
	}

	return hardState, confState, nil
}

func (ps *peerStorage) Entries(low, high, maxSize uint64) ([]raftpb.Entry, error) {
	err := ps.checkRange(low, high)
	if err != nil {
		return nil, err
	}

	var ents []raftpb.Entry
	if low == high {
		return ents, nil
	}

	var totalSize uint64
	nextIndex := low
	exceededMaxSize := false

	startKey := getRaftLogKey(ps.getCell().ID, low)

	if low+1 == high {
		// If election happens in inactive cells, they will just try
		// to fetch one empty log.
		v, err := ps.store.getMetaEngine().Get(startKey)
		if err != nil {
			return nil, errors.Wrap(err, "")
		}

		if nil == v {
			return nil, raft.ErrUnavailable
		}

		e, err := ps.unmarshal(v, low)
		if err != nil {
			return nil, err
		}

		ents = append(ents, *e)
		return ents, nil
	}

	endKey := getRaftLogKey(ps.getCell().ID, high)
	err = ps.store.getMetaEngine().Scan(startKey, endKey, func(data, value []byte) (bool, error) {
		e, err := ps.unmarshal(data, nextIndex)
		if err != nil {
			return false, err
		}

		nextIndex++
		totalSize += uint64(len(data))

		exceededMaxSize = totalSize > maxSize
		if !exceededMaxSize || len(ents) == 0 {
			ents = append(ents, *e)
		}

		return !exceededMaxSize, nil
	})

	if err != nil {
		return nil, err
	}

	// If we get the correct number of entries the total size exceeds max_size, returns.
	if len(ents) == int(high-low) || exceededMaxSize {
		return ents, nil
	}

	return nil, raft.ErrUnavailable
}

func (ps *peerStorage) Term(idx uint64) (uint64, error) {
	if idx == ps.getTruncatedIndex() {
		return ps.getTruncatedTerm(), nil
	}

	err := ps.checkRange(idx, idx+1)
	if err != nil {
		return 0, err
	}

	lastIdx, err := ps.LastIndex()
	if err != nil {
		return 0, err
	}

	if ps.getTruncatedTerm() == ps.lastTerm || idx == lastIdx {
		return ps.lastTerm, nil
	}

	key := getRaftLogKey(ps.getCell().ID, idx)
	v, err := ps.store.getMetaEngine().Get(key)
	if err != nil {
		return 0, err
	}

	if v == nil {
		return 0, raft.ErrUnavailable
	}

	e, err := ps.unmarshal(v, idx)
	if err != nil {
		return 0, err
	}

	return e.Term, nil
}

func (ps *peerStorage) LastIndex() (uint64, error) {
	return atomic.LoadUint64(&ps.raftState.LastIndex), nil
}

func (ps *peerStorage) FirstIndex() (uint64, error) {
	return ps.getTruncatedIndex() + 1, nil
}

func (ps *peerStorage) Snapshot() (raftpb.Snapshot, error) {
	if ps.isGeneratingSnap() {
		return raftpb.Snapshot{}, raft.ErrSnapshotTemporarilyUnavailable
	}

	if ps.isGenSnapJobComplete() {
		result := ps.applySnapJob.GetResult()
		// snapshot failure, we will continue try do snapshot
		if nil == result {
			log.Warnf("raftstore[cell-%d]: snapshot generating failed, triedCnt=<%d>",
				ps.getCell().ID,
				ps.snapTriedCnt)
			ps.snapTriedCnt++
		} else {
			snap := result.(*raftpb.Snapshot)
			ps.snapTriedCnt = 0
			if ps.validateSnap(snap) {
				ps.resetGenSnapJob()
				return *snap, nil
			}
		}
	}

	if ps.snapTriedCnt >= maxSnapTryCnt {
		cnt := ps.snapTriedCnt
		ps.resetGenSnapJob()
		return raftpb.Snapshot{}, fmt.Errorf("raftstore[cell-%d]: failed to get snapshot after %d times",
			ps.getCell().ID,
			cnt)
	}

	log.Infof("raftstore[cell-%d]: start snapshot", ps.getCell().ID)
	ps.snapTriedCnt++

	job, err := ps.store.addSnapJob(ps.doGenerateSnapshotJob)
	if err != nil {
		log.Fatalf("raftstore[cell-%d]: add generate job failed, errors:\n %+v",
			ps.getCell().ID,
			err)
	}
	ps.genSnapJob = job
	return raftpb.Snapshot{}, raft.ErrSnapshotTemporarilyUnavailable
}
