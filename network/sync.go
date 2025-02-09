package network

import (
	"fmt"
	"time"

	"github.com/MixinNetwork/mixin/common"
	"github.com/MixinNetwork/mixin/config"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/mixin/logger"
)

func (me *Peer) cacheReadSnapshotsForNodeRound(nodeId crypto.Hash, number uint64, final bool) ([]*common.SnapshotWithTopologicalOrder, error) {
	key := []byte(fmt.Sprintf("SFNR%s:%d", nodeId.String(), number))
	data := me.storeCache.GetBig(nil, key)
	if len(data) == 0 {
		ss, err := me.handle.ReadSnapshotsForNodeRound(nodeId, number)
		if err != nil || len(ss) == 0 {
			return nil, err
		}
		if final {
			me.storeCache.SetBig(key, common.MsgpackMarshalPanic(ss))
		}
		return ss, nil
	}
	var ss []*common.SnapshotWithTopologicalOrder
	err := common.MsgpackUnmarshal(data, &ss)
	return ss, err
}

func (me *Peer) cacheReadSnapshotsSinceTopology(offset, limit uint64) ([]*common.SnapshotWithTopologicalOrder, error) {
	key := []byte(fmt.Sprintf("SSTME%d-%d", offset, limit))
	data := me.storeCache.GetBig(nil, key)
	if len(data) == 0 {
		ss, err := me.handle.ReadSnapshotsSinceTopology(offset, limit)
		if err != nil {
			return nil, err
		}
		if uint64(len(ss)) == limit {
			me.storeCache.SetBig(key, common.MsgpackMarshalPanic(ss))
		}
		return ss, nil
	}
	var ss []*common.SnapshotWithTopologicalOrder
	err := common.MsgpackUnmarshal(data, &ss)
	return ss, err
}

func (me *Peer) compareRoundGraphAndGetTopologicalOffset(p *Peer, local, remote []*SyncPoint) (uint64, error) {
	remoteFilter := make(map[crypto.Hash]*SyncPoint)
	for _, p := range remote {
		remoteFilter[p.NodeId] = p
	}

	var future bool
	var offset uint64

	for _, l := range local {
		r := remoteFilter[l.NodeId]
		if r == nil {
			future = true
			break
		}
		if l.Number >= r.Number+config.SnapshotReferenceThreshold/2 {
			future = true
			break
		}
		if l.Number < config.SnapshotReferenceThreshold && l.Number > r.Number {
			future = true
			break
		}
	}
	if !future {
		return 0, nil
	}

	for _, l := range local {
		r := remoteFilter[l.NodeId]
		if r != nil && r.Number > l.Number {
			continue
		}
		number := uint64(0)
		if r != nil && r.Number > 0 {
			number = r.Number + 2 // because the node may be stale or removed, and with cache
		}
		logger.Verbosef("network.sync compareRoundGraphAndGetTopologicalOffset %s try %s:%d\n", p.IdForNetwork, l.NodeId, number)

		ss, err := me.cacheReadSnapshotsForNodeRound(l.NodeId, number, number <= l.Number)
		if err != nil {
			return offset, err
		}
		if len(ss) == 0 {
			logger.Verbosef("network.sync compareRoundGraphAndGetTopologicalOffset %s local round empty %s:%d:%d\n", p.IdForNetwork, l.NodeId, number, l.Number)
			continue
		}
		topo := ss[0].TopologicalOrder
		if offset == 0 || topo < offset {
			offset = topo
		}
	}
	return offset, nil
}

func (me *Peer) syncToNeighborSince(graph map[crypto.Hash]*SyncPoint, p *Peer, offset uint64) (uint64, error) {
	logger.Verbosef("network.sync syncToNeighborSince %s %d\n", p.IdForNetwork, offset)
	limit := 200
	snapshots, err := me.cacheReadSnapshotsSinceTopology(offset, uint64(limit))
	if err != nil {
		return offset, err
	}
	for _, s := range snapshots {
		var remoteRound uint64
		if r := graph[s.NodeId]; r != nil {
			remoteRound = r.Number
		}
		if s.RoundNumber < remoteRound {
			offset = s.TopologicalOrder
			continue
		}
		if s.RoundNumber >= remoteRound+config.SnapshotReferenceThreshold*2 {
			return offset, fmt.Errorf("FUTURE %s %d %d", s.NodeId, s.RoundNumber, remoteRound)
		}
		err := me.SendSnapshotFinalizationMessage(p.IdForNetwork, &s.Snapshot)
		if err != nil {
			return offset, err
		}
		offset = s.TopologicalOrder
	}
	time.Sleep(100 * time.Millisecond)
	if len(snapshots) < limit {
		return offset, fmt.Errorf("EOF")
	}
	return offset, nil
}

func (me *Peer) syncHeadRoundToRemote(local, remote map[crypto.Hash]*SyncPoint, p *Peer, nodeId crypto.Hash) {
	var localFinal, remoteFinal uint64
	if r := remote[nodeId]; r != nil {
		remoteFinal = r.Number
	}
	if l := local[nodeId]; l != nil {
		localFinal = l.Number
	}
	logger.Verbosef("network.sync syncHeadRoundToRemote %s %s:%d\n", p.IdForNetwork, nodeId, remoteFinal)
	for i := remoteFinal; i <= remoteFinal+config.SnapshotReferenceThreshold+2; i++ {
		ss, _ := me.cacheReadSnapshotsForNodeRound(nodeId, i, i <= localFinal)
		for _, s := range ss {
			me.SendSnapshotFinalizationMessage(p.IdForNetwork, &s.Snapshot)
		}
	}
}

func (me *Peer) syncToNeighborLoop(p *Peer) {
	for !p.closing {
		graph, offset := me.getSyncPointOffset(p)
		logger.Verbosef("network.sync syncToNeighborLoop getSyncPointOffset %s %d\n", p.IdForNetwork, offset)

		for offset > 0 {
			off, err := me.syncToNeighborSince(graph, p, offset)
			if err != nil {
				logger.Verbosef("network.sync syncToNeighborLoop syncToNeighborSince %s %d DONE with %s", p.IdForNetwork, offset, err)
				break
			}
			offset = off
		}

		if graph != nil {
			points := me.handle.BuildGraph()
			nodes := me.handle.ReadAllNodes()
			local := make(map[crypto.Hash]*SyncPoint)
			for _, n := range points {
				local[n.NodeId] = n
			}
			for _, n := range nodes {
				me.syncHeadRoundToRemote(local, graph, p, n)
			}
		}
	}
}

func (me *Peer) getSyncPointOffset(p *Peer) (map[crypto.Hash]*SyncPoint, uint64) {
	var offset uint64
	var graph map[crypto.Hash]*SyncPoint

	for {
		timer := time.NewTimer(time.Duration(config.SnapshotRoundGap / 3))
		select {
		case g := <-p.sync:
			graph = make(map[crypto.Hash]*SyncPoint)
			for _, r := range g {
				graph[r.NodeId] = r
			}
			off, err := me.compareRoundGraphAndGetTopologicalOffset(p, me.handle.BuildGraph(), g)
			if err != nil {
				logger.Printf("network.sync compareRoundGraphAndGetTopologicalOffset %s error %s\n", p.IdForNetwork, err.Error())
			}
			if off > 0 {
				offset = off
			}
		case <-timer.C:
			return graph, offset
		}
		timer.Stop()
	}
}
