// Copyright 2016 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	mrand "math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"

	"github.com/tikv/pd/pkg/errs"
	"github.com/tikv/pd/pkg/id"
	"github.com/tikv/pd/pkg/mock/mockid"
	"github.com/tikv/pd/pkg/utils/keyutil"
)

func TestNeedMerge(t *testing.T) {
	re := require.New(t)
	mererSize, mergeKeys := int64(20), int64(200000)
	testdata := []struct {
		size   int64
		keys   int64
		expect bool
	}{{
		size:   20,
		keys:   200000,
		expect: true,
	}, {
		size:   20 - 1,
		keys:   200000 - 1,
		expect: true,
	}, {
		size:   20,
		keys:   200000 - 1,
		expect: true,
	}, {
		size:   20,
		keys:   200000 + 1,
		expect: false,
	}, {
		size:   20 + 1,
		keys:   200000 + 1,
		expect: false,
	}}
	for _, v := range testdata {
		r := RegionInfo{
			approximateSize: v.size,
			approximateKeys: v.keys,
		}
		re.Equal(v.expect, r.NeedMerge(mererSize, mergeKeys))
	}
}

func TestSortedEqual(t *testing.T) {
	re := require.New(t)
	testCases := []struct {
		idsA    []int
		idsB    []int
		isEqual bool
	}{
		{
			[]int{},
			[]int{},
			true,
		},
		{
			[]int{},
			[]int{1, 2},
			false,
		},
		{
			[]int{1, 2},
			[]int{1, 2},
			true,
		},
		{
			[]int{1, 2},
			[]int{2, 1},
			true,
		},
		{
			[]int{1, 2},
			[]int{1, 2, 3},
			false,
		},
		{
			[]int{1, 2, 3},
			[]int{2, 3, 1},
			true,
		},
		{
			[]int{1, 3},
			[]int{1, 2},
			false,
		},
	}
	meta := &metapb.Region{
		Id: 100,
		Peers: []*metapb.Peer{
			{
				Id:      1,
				StoreId: 10,
			},
			{
				Id:      3,
				StoreId: 30,
			},
			{
				Id:      2,
				StoreId: 20,
				Role:    metapb.PeerRole_Learner,
			},
			{
				Id:      4,
				StoreId: 40,
				Role:    metapb.PeerRole_IncomingVoter,
			},
		},
	}
	pickPeers := func(ids []int) []*metapb.Peer {
		peers := make([]*metapb.Peer, 0, len(ids))
		for _, i := range ids {
			peers = append(peers, meta.Peers[i])
		}
		return peers
	}
	pickPeerStats := func(ids []int) []*pdpb.PeerStats {
		peers := make([]*pdpb.PeerStats, 0, len(ids))
		for _, i := range ids {
			peers = append(peers, &pdpb.PeerStats{Peer: meta.Peers[i]})
		}
		return peers
	}
	// test NewRegionInfo
	for _, testCase := range testCases {
		regionA := NewRegionInfo(&metapb.Region{Id: 100, Peers: pickPeers(testCase.idsA)}, nil)
		regionB := NewRegionInfo(&metapb.Region{Id: 100, Peers: pickPeers(testCase.idsB)}, nil)
		re.Equal(testCase.isEqual, SortedPeersEqual(regionA.GetVoters(), regionB.GetVoters()))
		re.Equal(testCase.isEqual, SortedPeersEqual(regionA.GetVoters(), regionB.GetVoters()))
	}

	flowRoundDivisor := 3
	// test RegionFromHeartbeat
	for _, testCase := range testCases {
		regionA := RegionFromHeartbeat(&pdpb.RegionHeartbeatRequest{
			Region:       &metapb.Region{Id: 100, Peers: pickPeers(testCase.idsA)},
			DownPeers:    pickPeerStats(testCase.idsA),
			PendingPeers: pickPeers(testCase.idsA),
		}, flowRoundDivisor)
		regionB := RegionFromHeartbeat(&pdpb.RegionHeartbeatRequest{
			Region:       &metapb.Region{Id: 100, Peers: pickPeers(testCase.idsB)},
			DownPeers:    pickPeerStats(testCase.idsB),
			PendingPeers: pickPeers(testCase.idsB),
		}, flowRoundDivisor)
		re.Equal(testCase.isEqual, SortedPeersEqual(regionA.GetVoters(), regionB.GetVoters()))
		re.Equal(testCase.isEqual, SortedPeersEqual(regionA.GetVoters(), regionB.GetVoters()))
		re.Equal(testCase.isEqual, SortedPeersEqual(regionA.GetPendingPeers(), regionB.GetPendingPeers()))
		re.Equal(testCase.isEqual, SortedPeersStatsEqual(regionA.GetDownPeers(), regionB.GetDownPeers()))
	}

	// test Clone
	region := NewRegionInfo(meta, meta.Peers[0])
	for _, testCase := range testCases {
		downPeersA := pickPeerStats(testCase.idsA)
		downPeersB := pickPeerStats(testCase.idsB)
		pendingPeersA := pickPeers(testCase.idsA)
		pendingPeersB := pickPeers(testCase.idsB)

		regionA := region.Clone(WithDownPeers(downPeersA), WithPendingPeers(pendingPeersA))
		regionB := region.Clone(WithDownPeers(downPeersB), WithPendingPeers(pendingPeersB))
		re.Equal(testCase.isEqual, SortedPeersStatsEqual(regionA.GetDownPeers(), regionB.GetDownPeers()))
		re.Equal(testCase.isEqual, SortedPeersEqual(regionA.GetPendingPeers(), regionB.GetPendingPeers()))
	}
}

func TestInherit(t *testing.T) {
	re := require.New(t)
	// size in MB
	// case for approximateSize
	testCases := []struct {
		originExists bool
		originSize   uint64
		size         uint64
		expect       uint64
	}{
		{false, 0, 0, 1},
		{false, 0, 2, 2},
		{true, 0, 2, 2},
		{true, 1, 2, 2},
		{true, 2, 0, 2},
	}
	for _, testCase := range testCases {
		var origin *RegionInfo
		if testCase.originExists {
			origin = NewRegionInfo(&metapb.Region{Id: 100}, nil)
			origin.approximateSize = int64(testCase.originSize)
		}
		r := NewRegionInfo(&metapb.Region{Id: 100}, nil)
		r.approximateSize = int64(testCase.size)
		r.Inherit(origin, false)
		re.Equal(int64(testCase.expect), r.approximateSize)
	}

	// bucket
	data := []struct {
		originBuckets *metapb.Buckets
		buckets       *metapb.Buckets
	}{
		{nil, nil},
		{nil, &metapb.Buckets{RegionId: 100, Version: 2}},
		{&metapb.Buckets{RegionId: 100, Version: 2}, &metapb.Buckets{RegionId: 100, Version: 3}},
		{&metapb.Buckets{RegionId: 100, Version: 2}, nil},
	}
	for _, d := range data {
		origin := NewRegionInfo(&metapb.Region{Id: 100}, nil, SetBuckets(d.originBuckets))
		r := NewRegionInfo(&metapb.Region{Id: 100}, nil)
		r.Inherit(origin, true)
		re.Equal(d.originBuckets, r.GetBuckets())
		// region will not inherit bucket keys.
		if origin.GetBuckets() != nil {
			newRegion := NewRegionInfo(&metapb.Region{Id: 100}, nil)
			newRegion.Inherit(origin, false)
			re.NotEqual(d.originBuckets, newRegion.GetBuckets())
		}
	}
}

func TestRegionRoundingFlow(t *testing.T) {
	re := require.New(t)
	testCases := []struct {
		flow   uint64
		digit  int
		expect uint64
	}{
		{10, 0, 10},
		{13, 1, 10},
		{11807, 3, 12000},
		{252623, 4, 250000},
		{258623, 4, 260000},
		{258623, 64, 0},
		{252623, math.MaxInt64, 0},
		{252623, math.MinInt64, 252623},
	}
	for _, testCase := range testCases {
		r := NewRegionInfo(&metapb.Region{Id: 100}, nil, WithFlowRoundByDigit(testCase.digit))
		r.readBytes = testCase.flow
		r.writtenBytes = testCase.flow
		re.Equal(testCase.expect, r.GetRoundBytesRead())
	}
}

func TestRegionWriteRate(t *testing.T) {
	re := require.New(t)
	testCases := []struct {
		bytes           uint64
		keys            uint64
		interval        uint64
		expectBytesRate float64
		expectKeysRate  float64
	}{
		{0, 0, 0, 0, 0},
		{10, 3, 0, 0, 0},
		{0, 0, 1, 0, 0},
		{10, 3, 1, 0, 0},
		{0, 0, 5, 0, 0},
		{10, 3, 5, 2, 0.6},
		{0, 0, 500, 0, 0},
		{10, 3, 500, 0, 0},
	}
	for _, testCase := range testCases {
		r := NewRegionInfo(&metapb.Region{Id: 100}, nil, SetWrittenBytes(testCase.bytes), SetWrittenKeys(testCase.keys), SetReportInterval(0, testCase.interval))
		bytesRate, keysRate := r.GetWriteRate()
		re.Equal(testCase.expectBytesRate, bytesRate)
		re.Equal(testCase.expectKeysRate, keysRate)
	}
}

func TestNeedSync(t *testing.T) {
	re := require.New(t)
	RegionGuide := GenerateRegionGuideFunc(false)
	meta := &metapb.Region{
		Id:          1000,
		StartKey:    []byte("a"),
		EndKey:      []byte("z"),
		RegionEpoch: &metapb.RegionEpoch{ConfVer: 100, Version: 100},
		Peers: []*metapb.Peer{
			{Id: 11, StoreId: 1, Role: metapb.PeerRole_Voter},
			{Id: 12, StoreId: 1, Role: metapb.PeerRole_Voter},
			{Id: 13, StoreId: 1, Role: metapb.PeerRole_Voter},
		},
	}
	region := NewRegionInfo(meta, meta.Peers[0])

	testCases := []struct {
		optionsA []RegionCreateOption
		optionsB []RegionCreateOption
		needSync bool
	}{
		{
			optionsB: []RegionCreateOption{WithLeader(nil)},
			needSync: true,
		},
		{
			optionsB: []RegionCreateOption{WithLeader(meta.Peers[1])},
			needSync: true,
		},
		{
			optionsB: []RegionCreateOption{WithPendingPeers(meta.Peers[1:2])},
			needSync: true,
		},
		{
			optionsB: []RegionCreateOption{WithDownPeers([]*pdpb.PeerStats{{Peer: meta.Peers[1], DownSeconds: 600}})},
			needSync: true,
		},
		{
			optionsA: []RegionCreateOption{SetWrittenBytes(200), WithFlowRoundByDigit(2)},
			optionsB: []RegionCreateOption{SetWrittenBytes(300), WithFlowRoundByDigit(2)},
			needSync: true,
		},
		{
			optionsA: []RegionCreateOption{SetWrittenBytes(250), WithFlowRoundByDigit(2)},
			optionsB: []RegionCreateOption{SetWrittenBytes(349), WithFlowRoundByDigit(2)},
			needSync: false,
		},
		{
			optionsA: []RegionCreateOption{SetWrittenBytes(200), WithFlowRoundByDigit(4)},
			optionsB: []RegionCreateOption{SetWrittenBytes(300), WithFlowRoundByDigit(4)},
			needSync: false,
		},
		{
			optionsA: []RegionCreateOption{SetWrittenBytes(100000), WithFlowRoundByDigit(4)},
			optionsB: []RegionCreateOption{SetWrittenBytes(200), WithFlowRoundByDigit(2)},
			needSync: true,
		},
		{
			optionsA: []RegionCreateOption{SetWrittenBytes(100000), WithFlowRoundByDigit(127)},
			optionsB: []RegionCreateOption{SetWrittenBytes(0), WithFlowRoundByDigit(2)},
			needSync: false,
		},
		{
			optionsA: []RegionCreateOption{SetWrittenBytes(0), WithFlowRoundByDigit(2)},
			optionsB: []RegionCreateOption{SetWrittenBytes(100000), WithFlowRoundByDigit(127)},
			needSync: true,
		},
	}

	for _, testCase := range testCases {
		regionA := region.Clone(testCase.optionsA...)
		regionB := region.Clone(testCase.optionsB...)
		_, _, needSync, _ := RegionGuide(ContextTODO(), regionA, regionB)
		re.Equal(testCase.needSync, needSync)
	}
}

func TestRegionMap(t *testing.T) {
	re := require.New(t)
	rm := make(map[uint64]*regionItem)
	checkMap(re, rm)
	rm[1] = &regionItem{RegionInfo: regionInfo(1)}
	checkMap(re, rm, 1)

	rm[2] = &regionItem{RegionInfo: regionInfo(2)}
	rm[3] = &regionItem{RegionInfo: regionInfo(3)}
	checkMap(re, rm, 1, 2, 3)

	rm[3] = &regionItem{RegionInfo: regionInfo(3)}
	delete(rm, 4)
	checkMap(re, rm, 1, 2, 3)

	delete(rm, 3)
	delete(rm, 1)
	checkMap(re, rm, 2)

	rm[3] = &regionItem{RegionInfo: regionInfo(3)}
	checkMap(re, rm, 2, 3)
}

func regionInfo(id uint64) *RegionInfo {
	return &RegionInfo{
		meta: &metapb.Region{
			Id: id,
		},
		approximateSize: int64(id),
		approximateKeys: int64(id),
	}
}

func checkMap(re *require.Assertions, rm map[uint64]*regionItem, ids ...uint64) {
	// Check Get.
	for _, id := range ids {
		re.Equal(id, rm[id].GetID())
	}
	// Check Len.
	re.Len(ids, len(rm))
	// Check id set.
	expect := make(map[uint64]struct{})
	for _, id := range ids {
		expect[id] = struct{}{}
	}
	set1 := make(map[uint64]struct{})
	for _, r := range rm {
		set1[r.GetID()] = struct{}{}
	}
	re.Equal(expect, set1)
}

func TestRegionKey(t *testing.T) {
	re := require.New(t)
	testCase := []struct {
		key    string
		expect string
	}{
		{`"t\x80\x00\x00\x00\x00\x00\x00\xff!_r\x80\x00\x00\x00\x00\xff\x02\u007fY\x00\x00\x00\x00\x00\xfa"`,
			`7480000000000000FF215F728000000000FF027F590000000000FA`},
		{"\"\\x80\\x00\\x00\\x00\\x00\\x00\\x00\\xff\\x05\\x00\\x00\\x00\\x00\\x00\\x00\\x00\\xf8\"",
			`80000000000000FF0500000000000000F8`},
	}
	for _, test := range testCase {
		got, err := strconv.Unquote(test.key)
		re.NoError(err)
		s := fmt.Sprintln(RegionToHexMeta(&metapb.Region{StartKey: []byte(got)}))
		re.Contains(s, test.expect)

		// start key changed
		origin := NewRegionInfo(&metapb.Region{EndKey: []byte(got)}, nil)
		region := NewRegionInfo(&metapb.Region{StartKey: []byte(got), EndKey: []byte(got)}, nil)
		s = DiffRegionKeyInfo(origin, region)
		re.Regexp(".*StartKey Changed.*", s)
		re.Contains(s, test.expect)

		// end key changed
		origin = NewRegionInfo(&metapb.Region{StartKey: []byte(got)}, nil)
		region = NewRegionInfo(&metapb.Region{StartKey: []byte(got), EndKey: []byte(got)}, nil)
		s = DiffRegionKeyInfo(origin, region)
		re.Regexp(".*EndKey Changed.*", s)
		re.Contains(s, test.expect)
	}
}

func TestSetRegionConcurrence(t *testing.T) {
	re := require.New(t)
	re.NoError(failpoint.Enable("github.com/tikv/pd/pkg/core/UpdateSubTree", `return()`))
	regions := NewRegionsInfo()
	region := NewTestRegionInfo(1, 1, []byte("a"), []byte("b"))
	go func() {
		_, err := regions.AtomicCheckAndPutRegion(ContextTODO(), region)
		re.NoError(err)
	}()
	_, err := regions.AtomicCheckAndPutRegion(ContextTODO(), region)
	re.NoError(err)
	re.NoError(failpoint.Disable("github.com/tikv/pd/pkg/core/UpdateSubTree"))
}

func TestSetRegion(t *testing.T) {
	re := require.New(t)
	regions := NewRegionsInfo()
	for i := range 100 {
		peer1 := &metapb.Peer{StoreId: uint64(i%5 + 1), Id: uint64(i*5 + 1)}
		peer2 := &metapb.Peer{StoreId: uint64((i+1)%5 + 1), Id: uint64(i*5 + 2)}
		peer3 := &metapb.Peer{StoreId: uint64((i+2)%5 + 1), Id: uint64(i*5 + 3)}
		if i%3 == 0 {
			peer2.IsWitness = true
		}
		region := NewRegionInfo(&metapb.Region{
			Id:       uint64(i + 1),
			Peers:    []*metapb.Peer{peer1, peer2, peer3},
			StartKey: []byte(fmt.Sprintf("%20d", i*10)),
			EndKey:   []byte(fmt.Sprintf("%20d", (i+1)*10)),
		}, peer1)
		origin, overlaps, rangeChanged := regions.SetRegion(region)
		regions.UpdateSubTree(region, origin, overlaps, rangeChanged)
	}

	peer1 := &metapb.Peer{StoreId: uint64(4), Id: uint64(101)}
	peer2 := &metapb.Peer{StoreId: uint64(5), Id: uint64(102), Role: metapb.PeerRole_Learner}
	peer3 := &metapb.Peer{StoreId: uint64(1), Id: uint64(103)}
	region := NewRegionInfo(&metapb.Region{
		Id:       uint64(21),
		Peers:    []*metapb.Peer{peer1, peer2, peer3},
		StartKey: []byte(fmt.Sprintf("%20d", 184)),
		EndKey:   []byte(fmt.Sprintf("%20d", 211)),
	}, peer1)
	region.pendingPeers = append(region.pendingPeers, peer3)
	origin, overlaps, rangeChanged := regions.SetRegion(region)
	regions.UpdateSubTree(region, origin, overlaps, rangeChanged)
	checkRegions(re, regions)
	re.Equal(97, regions.tree.length())
	re.Len(regions.GetRegions(), 97)

	origin, overlaps, rangeChanged = regions.SetRegion(region)
	regions.UpdateSubTree(region, origin, overlaps, rangeChanged)
	peer1 = &metapb.Peer{StoreId: uint64(2), Id: uint64(101)}
	peer2 = &metapb.Peer{StoreId: uint64(3), Id: uint64(102), Role: metapb.PeerRole_Learner}
	peer3 = &metapb.Peer{StoreId: uint64(1), Id: uint64(103)}
	region = NewRegionInfo(&metapb.Region{
		Id:       uint64(21),
		Peers:    []*metapb.Peer{peer1, peer2, peer3},
		StartKey: []byte(fmt.Sprintf("%20d", 184)),
		EndKey:   []byte(fmt.Sprintf("%20d", 212)),
	}, peer1)
	region.pendingPeers = append(region.pendingPeers, peer3)
	origin, overlaps, rangeChanged = regions.SetRegion(region)
	regions.UpdateSubTree(region, origin, overlaps, rangeChanged)
	checkRegions(re, regions)
	re.Equal(97, regions.tree.length())
	re.Len(regions.GetRegions(), 97)

	// Test remove overlaps.
	region = region.Clone(WithStartKey([]byte(fmt.Sprintf("%20d", 175))), WithNewRegionID(201))
	re.NotNil(regions.GetRegion(21))
	re.NotNil(regions.GetRegion(18))
	origin, overlaps, rangeChanged = regions.SetRegion(region)
	regions.UpdateSubTree(region, origin, overlaps, rangeChanged)
	checkRegions(re, regions)
	re.Equal(96, regions.tree.length())
	re.Len(regions.GetRegions(), 96)
	re.NotNil(regions.GetRegion(201))
	re.Nil(regions.GetRegion(21))
	re.Nil(regions.GetRegion(18))

	// Test update keys and size of region.
	region = region.Clone(
		SetApproximateKeys(20),
		SetApproximateSize(30),
		SetWrittenBytes(40),
		SetWrittenKeys(10),
		SetReportInterval(0, 5))
	origin, overlaps, rangeChanged = regions.SetRegion(region)
	regions.UpdateSubTree(region, origin, overlaps, rangeChanged)
	checkRegions(re, regions)
	re.Equal(96, regions.tree.length())
	re.Len(regions.GetRegions(), 96)
	re.NotNil(regions.GetRegion(201))
	re.Equal(int64(30), regions.tree.TotalSize())
	bytesRate, keysRate := regions.tree.TotalWriteRate()
	re.Equal(float64(8), bytesRate)
	re.Equal(float64(2), keysRate)
}

func TestShouldRemoveFromSubTree(t *testing.T) {
	re := require.New(t)
	peer1 := &metapb.Peer{StoreId: uint64(1), Id: uint64(1)}
	peer2 := &metapb.Peer{StoreId: uint64(2), Id: uint64(2)}
	peer3 := &metapb.Peer{StoreId: uint64(3), Id: uint64(3)}
	peer4 := &metapb.Peer{StoreId: uint64(3), Id: uint64(3)}
	region := NewRegionInfo(&metapb.Region{
		Id:       uint64(1),
		Peers:    []*metapb.Peer{peer1, peer2, peer4},
		StartKey: []byte(fmt.Sprintf("%20d", 10)),
		EndKey:   []byte(fmt.Sprintf("%20d", 20)),
	}, peer1)

	origin := NewRegionInfo(&metapb.Region{
		Id:       uint64(1),
		Peers:    []*metapb.Peer{peer1, peer2, peer3},
		StartKey: []byte(fmt.Sprintf("%20d", 10)),
		EndKey:   []byte(fmt.Sprintf("%20d", 20)),
	}, peer1)
	re.True(region.peersEqualTo(origin))

	region.leader = peer2
	re.False(region.peersEqualTo(origin))

	region.leader = peer1
	region.pendingPeers = append(region.pendingPeers, peer4)
	re.False(region.peersEqualTo(origin))

	region.pendingPeers = nil
	region.learners = append(region.learners, peer2)
	re.False(region.peersEqualTo(origin))

	origin.learners = append(origin.learners, peer2, peer3)
	region.learners = append(region.learners, peer4)
	re.True(region.peersEqualTo(origin))

	region.voters[2].StoreId = 4
	re.False(region.peersEqualTo(origin))
}

func checkRegions(re *require.Assertions, regions *RegionsInfo) {
	leaderMap := make(map[uint64]uint64)
	followerMap := make(map[uint64]uint64)
	learnerMap := make(map[uint64]uint64)
	witnessMap := make(map[uint64]uint64)
	pendingPeerMap := make(map[uint64]uint64)
	for _, item := range regions.GetRegions() {
		leaderMap[item.leader.StoreId]++
		for _, follower := range item.GetFollowers() {
			followerMap[follower.StoreId]++
		}
		for _, learner := range item.GetLearners() {
			learnerMap[learner.StoreId]++
		}
		for _, witness := range item.GetWitnesses() {
			witnessMap[witness.StoreId]++
		}
		for _, pendingPeer := range item.GetPendingPeers() {
			pendingPeerMap[pendingPeer.StoreId]++
		}
	}
	for key, value := range regions.leaders {
		re.Equal(int(leaderMap[key]), value.length())
	}
	for key, value := range regions.followers {
		re.Equal(int(followerMap[key]), value.length())
	}
	for key, value := range regions.learners {
		re.Equal(int(learnerMap[key]), value.length())
	}
	for key, value := range regions.witnesses {
		re.Equal(int(witnessMap[key]), value.length())
	}
	for key, value := range regions.pendingPeers {
		re.Equal(int(pendingPeerMap[key]), value.length())
	}
}

func BenchmarkUpdateBuckets(b *testing.B) {
	region := NewTestRegionInfo(1, 1, []byte{}, []byte{})
	b.ResetTimer()
	for i := range b.N {
		buckets := &metapb.Buckets{RegionId: 0, Version: uint64(i)}
		region.UpdateBuckets(buckets, region.GetBuckets())
	}
	if region.GetBuckets().GetVersion() != uint64(b.N-1) {
		b.Fatal("update buckets failed")
	}
}

func BenchmarkRandomRegion(b *testing.B) {
	for _, size := range []int{10, 100, 1000, 10000, 100000, 1000000, 10000000} {
		regions := NewRegionsInfo()
		for i := range size {
			peer := &metapb.Peer{StoreId: 1, Id: uint64(i + 1)}
			region := NewRegionInfo(&metapb.Region{
				Id:       uint64(i + 1),
				Peers:    []*metapb.Peer{peer},
				StartKey: []byte(fmt.Sprintf("%20d", i)),
				EndKey:   []byte(fmt.Sprintf("%20d", i+1)),
			}, peer)
			origin, overlaps, rangeChanged := regions.SetRegion(region)
			regions.UpdateSubTree(region, origin, overlaps, rangeChanged)
		}
		b.Run(fmt.Sprintf("random region whole range with size %d", size), func(b *testing.B) {
			b.ResetTimer()
			for range b.N {
				regions.randLeaderRegion(1, nil)
			}
		})
		b.Run(fmt.Sprintf("random regions whole range with size %d", size), func(b *testing.B) {
			b.ResetTimer()
			for range b.N {
				regions.RandLeaderRegions(1, nil)
			}
		})
		ranges := []keyutil.KeyRange{
			keyutil.NewKeyRange(fmt.Sprintf("%20d", size/4), fmt.Sprintf("%20d", size*3/4)),
		}
		b.Run(fmt.Sprintf("random region single range with size %d", size), func(b *testing.B) {
			b.ResetTimer()
			for range b.N {
				regions.randLeaderRegion(1, ranges)
			}
		})
		b.Run(fmt.Sprintf("random regions single range with size %d", size), func(b *testing.B) {
			b.ResetTimer()
			for range b.N {
				regions.RandLeaderRegions(1, ranges)
			}
		})
		ranges = []keyutil.KeyRange{
			keyutil.NewKeyRange(fmt.Sprintf("%20d", 0), fmt.Sprintf("%20d", size/4)),
			keyutil.NewKeyRange(fmt.Sprintf("%20d", size/4), fmt.Sprintf("%20d", size/2)),
			keyutil.NewKeyRange(fmt.Sprintf("%20d", size/2), fmt.Sprintf("%20d", size*3/4)),
			keyutil.NewKeyRange(fmt.Sprintf("%20d", size*3/4), fmt.Sprintf("%20d", size)),
		}
		b.Run(fmt.Sprintf("random region multiple ranges with size %d", size), func(b *testing.B) {
			b.ResetTimer()
			for range b.N {
				regions.randLeaderRegion(1, ranges)
			}
		})
		b.Run(fmt.Sprintf("random regions multiple ranges with size %d", size), func(b *testing.B) {
			b.ResetTimer()
			for range b.N {
				regions.RandLeaderRegions(1, ranges)
			}
		})
	}
}

func BenchmarkRandomSetRegion(b *testing.B) {
	regions := NewRegionsInfo()
	var items []*RegionInfo
	for i := range 1000000 {
		peer := &metapb.Peer{StoreId: 1, Id: uint64(i + 1)}
		region := NewRegionInfo(&metapb.Region{
			Id:       uint64(i + 1),
			Peers:    []*metapb.Peer{peer},
			StartKey: []byte(fmt.Sprintf("%20d", i)),
			EndKey:   []byte(fmt.Sprintf("%20d", i+1)),
		}, peer)
		origin, overlaps, rangeChanged := regions.SetRegion(region)
		regions.UpdateSubTree(region, origin, overlaps, rangeChanged)
		items = append(items, region)
	}
	b.ResetTimer()
	for i := range b.N {
		item := items[i%len(items)]
		item.approximateKeys = int64(200000)
		item.approximateSize = int64(20)
		origin, overlaps, rangeChanged := regions.SetRegion(item)
		regions.UpdateSubTree(item, origin, overlaps, rangeChanged)
	}
}

func TestGetRegionSizeByRange(t *testing.T) {
	regions := NewRegionsInfo()
	nums := 100001
	for i := range nums {
		peer := &metapb.Peer{StoreId: 1, Id: uint64(i + 1)}
		endKey := []byte(fmt.Sprintf("%20d", i+1))
		if i == nums-1 {
			endKey = []byte("")
		}
		region := NewRegionInfo(&metapb.Region{
			Id:       uint64(i + 1),
			Peers:    []*metapb.Peer{peer},
			StartKey: []byte(fmt.Sprintf("%20d", i)),
			EndKey:   endKey,
		}, peer, SetApproximateSize(10))
		origin, overlaps, rangeChanged := regions.SetRegion(region)
		regions.UpdateSubTree(region, origin, overlaps, rangeChanged)
	}
	totalSize := regions.GetRegionSizeByRange([]byte(""), []byte(""))
	require.Equal(t, int64(nums*10), totalSize)
	for i := 1; i < 10; i++ {
		verifyNum := nums / i
		endKey := fmt.Sprintf("%20d", verifyNum)
		totalSize := regions.GetRegionSizeByRange([]byte(""), []byte(endKey))
		require.Equal(t, int64(verifyNum*10), totalSize)
	}
}

func BenchmarkRandomSetRegionWithGetRegionSizeByRange(b *testing.B) {
	regions := NewRegionsInfo()
	var items []*RegionInfo
	for i := range 1000000 {
		peer := &metapb.Peer{StoreId: 1, Id: uint64(i + 1)}
		region := NewRegionInfo(&metapb.Region{
			Id:       uint64(i + 1),
			Peers:    []*metapb.Peer{peer},
			StartKey: []byte(fmt.Sprintf("%20d", i)),
			EndKey:   []byte(fmt.Sprintf("%20d", i+1)),
		}, peer, SetApproximateSize(10))
		origin, overlaps, rangeChanged := regions.SetRegion(region)
		regions.UpdateSubTree(region, origin, overlaps, rangeChanged)
		items = append(items, region)
	}
	b.ResetTimer()
	go func() {
		for {
			regions.GetRegionSizeByRange([]byte(""), []byte(""))
			time.Sleep(time.Millisecond)
		}
	}()
	for i := range b.N {
		item := items[i%len(items)]
		item.approximateKeys = int64(200000)
		origin, overlaps, rangeChanged := regions.SetRegion(item)
		regions.UpdateSubTree(item, origin, overlaps, rangeChanged)
	}
}

func BenchmarkRandomSetRegionWithGetRegionSizeByRangeParallel(b *testing.B) {
	regions := NewRegionsInfo()
	var items []*RegionInfo
	for i := range 1000000 {
		peer := &metapb.Peer{StoreId: 1, Id: uint64(i + 1)}
		region := NewRegionInfo(&metapb.Region{
			Id:       uint64(i + 1),
			Peers:    []*metapb.Peer{peer},
			StartKey: []byte(fmt.Sprintf("%20d", i)),
			EndKey:   []byte(fmt.Sprintf("%20d", i+1)),
		}, peer)
		origin, overlaps, rangeChanged := regions.SetRegion(region)
		regions.UpdateSubTree(region, origin, overlaps, rangeChanged)
		items = append(items, region)
	}
	b.ResetTimer()
	go func() {
		for {
			regions.GetRegionSizeByRange([]byte(""), []byte(""))
			time.Sleep(time.Millisecond)
		}
	}()

	b.RunParallel(
		func(pb *testing.PB) {
			for pb.Next() {
				item := items[mrand.Intn(len(items))]
				n := item.Clone(SetApproximateSize(20))
				origin, overlaps, rangeChanged := regions.SetRegion(n)
				regions.UpdateSubTree(item, origin, overlaps, rangeChanged)
			}
		},
	)
}

const (
	peerNum   = 3
	storeNum  = 10
	keyLength = 100
)

func newRegionInfoIDRandom(re *require.Assertions, idAllocator id.Allocator) *RegionInfo {
	var (
		peers  []*metapb.Peer
		leader *metapb.Peer
	)
	// Randomly select a peer as the leader.
	leaderIdx := mrand.Intn(peerNum)
	for i := range peerNum {
		id, _, err := idAllocator.Alloc(1)
		re.NoError(err)
		// Randomly distribute the peers to different stores.
		p := &metapb.Peer{Id: id, StoreId: uint64(mrand.Intn(storeNum) + 1)}
		if i == leaderIdx {
			leader = p
		}
		peers = append(peers, p)
	}
	regionID, _, err := idAllocator.Alloc(1)
	re.NoError(err)
	return NewRegionInfo(
		&metapb.Region{
			Id:       regionID,
			StartKey: randomBytes(keyLength),
			EndKey:   randomBytes(keyLength),
			Peers:    peers,
		},
		leader,
		SetApproximateSize(10),
		SetApproximateKeys(10),
	)
}

func randomBytes(n int) []byte {
	bytes := make([]byte, n)
	_, err := rand.Read(bytes)
	if err != nil {
		panic(err)
	}
	return bytes
}

func BenchmarkAddRegion(b *testing.B) {
	re := require.New(b)
	regions := NewRegionsInfo()
	idAllocator := mockid.NewIDAllocator()
	items := generateRegionItems(re, idAllocator, 10000000)
	b.ResetTimer()
	for i := range b.N {
		origin, overlaps, rangeChanged := regions.SetRegion(items[i])
		regions.UpdateSubTree(items[i], origin, overlaps, rangeChanged)
	}
}

func BenchmarkUpdateSubTreeOrderInsensitive(b *testing.B) {
	re := require.New(b)
	idAllocator := mockid.NewIDAllocator()
	for _, size := range []int{10, 100, 1000, 10000, 100000, 1000000, 10000000} {
		regions := NewRegionsInfo()
		items := generateRegionItems(re, idAllocator, size)
		// Update the subtrees from an empty `*RegionsInfo`.
		b.Run(fmt.Sprintf("from empty with size %d", size), func(b *testing.B) {
			b.ResetTimer()
			for range b.N {
				for idx := range items {
					regions.UpdateSubTreeOrderInsensitive(items[idx])
				}
			}
		})

		// Update the subtrees from a non-empty `*RegionsInfo` with the same regions,
		// which means the regions are completely non-overlapped.
		b.Run(fmt.Sprintf("from non-overlapped regions with size %d", size), func(b *testing.B) {
			b.ResetTimer()
			for range b.N {
				for idx := range items {
					regions.UpdateSubTreeOrderInsensitive(items[idx])
				}
			}
		})

		// Update the subtrees from a non-empty `*RegionsInfo` with different regions,
		// which means the regions are most likely overlapped.
		b.Run(fmt.Sprintf("from overlapped regions with size %d", size), func(b *testing.B) {
			items = generateRegionItems(re, idAllocator, size)
			b.ResetTimer()
			for range b.N {
				for idx := range items {
					regions.UpdateSubTreeOrderInsensitive(items[idx])
				}
			}
		})
	}
}

func generateRegionItems(re *require.Assertions, idAllocator *mockid.IDAllocator, size int) []*RegionInfo {
	items := make([]*RegionInfo, size)
	for i := range size {
		items[i] = newRegionInfoIDRandom(re, idAllocator)
	}
	return items
}

func BenchmarkRegionFromHeartbeat(b *testing.B) {
	peers := make([]*metapb.Peer, 0, 3)
	for i := uint64(1); i <= 3; i++ {
		peers = append(peers, &metapb.Peer{
			Id:      i,
			StoreId: i,
		})
	}
	regionReq := &pdpb.RegionHeartbeatRequest{
		Region: &metapb.Region{
			Id:       1,
			Peers:    peers,
			StartKey: []byte{byte(2)},
			EndKey:   []byte{byte(3)},
			RegionEpoch: &metapb.RegionEpoch{
				ConfVer: 2,
				Version: 1,
			},
		},
		Leader:          peers[0],
		Term:            5,
		ApproximateSize: 10,
		PendingPeers:    []*metapb.Peer{peers[1]},
		DownPeers:       []*pdpb.PeerStats{{Peer: peers[2], DownSeconds: 100}},
	}
	flowRoundDivisor := 3
	b.ResetTimer()
	for range b.N {
		RegionFromHeartbeat(regionReq, flowRoundDivisor)
	}
}

func TestUpdateRegionEquivalence(t *testing.T) {
	re := require.New(t)
	regionsOld := NewRegionsInfo()
	regionsNew := NewRegionsInfo()
	storeNums := 5
	items := generateTestRegions(1000, storeNums)

	updateRegion := func(item *RegionInfo) {
		// old way
		ctx := ContextTODO()
		_, err := regionsOld.AtomicCheckAndPutRegion(ctx, item)
		re.NoError(err)
		// new way
		newItem := item.Clone()
		ctx = ContextTODO()
		_, err = regionsNew.CheckAndPutRootTree(ctx, newItem)
		re.NoError(err)
		regionsNew.CheckAndPutSubTree(newItem)
	}
	checksEquivalence := func() {
		re.Equal(regionsOld.GetRegionCount([]byte(""), []byte("")), regionsNew.GetRegionCount([]byte(""), []byte("")))
		re.Equal(regionsOld.GetRegionSizeByRange([]byte(""), []byte("")), regionsNew.GetRegionSizeByRange([]byte(""), []byte("")))
		checkRegions(re, regionsOld)
		checkRegions(re, regionsNew)

		for _, r := range regionsOld.GetRegions() {
			re.Equalf(int32(2), r.GetRef(), "inconsistent region %d", r.GetID())
		}
		for _, r := range regionsNew.GetRegions() {
			re.Equalf(int32(2), r.GetRef(), "inconsistent region %d", r.GetID())
		}

		for i := 1; i <= storeNums; i++ {
			re.Equal(regionsOld.GetStoreRegionCount(uint64(i)), regionsNew.GetStoreRegionCount(uint64(i)))
			re.Equal(regionsOld.GetStoreLeaderCount(uint64(i)), regionsNew.GetStoreLeaderCount(uint64(i)))
			re.Equal(regionsOld.GetStorePendingPeerCount(uint64(i)), regionsNew.GetStorePendingPeerCount(uint64(i)))
			re.Equal(regionsOld.GetStoreLearnerRegionSize(uint64(i)), regionsNew.GetStoreLearnerRegionSize(uint64(i)))
			re.Equal(regionsOld.GetStoreRegionSize(uint64(i)), regionsNew.GetStoreRegionSize(uint64(i)))
			re.Equal(regionsOld.GetStoreLeaderRegionSize(uint64(i)), regionsNew.GetStoreLeaderRegionSize(uint64(i)))
			re.Equal(regionsOld.GetStoreFollowerRegionSize(uint64(i)), regionsNew.GetStoreFollowerRegionSize(uint64(i)))
		}
	}

	// Add a region.
	for _, item := range items {
		updateRegion(item)
	}
	checksEquivalence()

	// Merge regions.
	itemA, itemB := items[10], items[11]
	itemMergedAB := itemA.Clone(WithEndKey(itemB.GetEndKey()), WithIncVersion())
	updateRegion(itemMergedAB)
	checksEquivalence()

	// Split
	itemA = itemA.Clone(WithIncVersion(), WithIncVersion())
	itemB = itemB.Clone(WithIncVersion(), WithIncVersion())
	updateRegion(itemA)
	updateRegion(itemB)
	checksEquivalence()
}

func generateTestRegions(count int, storeNum int) []*RegionInfo {
	var items []*RegionInfo
	for i := range count {
		peer1 := &metapb.Peer{StoreId: uint64(i%storeNum + 1), Id: uint64(i*storeNum + 1)}
		peer2 := &metapb.Peer{StoreId: uint64((i+1)%storeNum + 1), Id: uint64(i*storeNum + 2)}
		peer3 := &metapb.Peer{StoreId: uint64((i+2)%storeNum + 1), Id: uint64(i*storeNum + 3)}
		if i%3 == 0 {
			peer2.IsWitness = true
		}
		region := NewRegionInfo(&metapb.Region{
			Id:          uint64(i + 1),
			Peers:       []*metapb.Peer{peer1, peer2, peer3},
			StartKey:    []byte(fmt.Sprintf("%20d", i*10)),
			EndKey:      []byte(fmt.Sprintf("%20d", (i+1)*10)),
			RegionEpoch: &metapb.RegionEpoch{ConfVer: 100, Version: 100},
		},
			peer1,
			SetApproximateKeys(10),
			SetApproximateSize(10))
		items = append(items, region)
	}
	return items
}

func TestUpdateRegionEventualConsistency(t *testing.T) {
	re := require.New(t)
	regionsOld := NewRegionsInfo()
	regionsNew := NewRegionsInfo()
	i := 1
	storeNum := 5
	peer1 := &metapb.Peer{StoreId: uint64(i%storeNum + 1), Id: uint64(i*storeNum + 1)}
	peer2 := &metapb.Peer{StoreId: uint64((i+1)%storeNum + 1), Id: uint64(i*storeNum + 2)}
	peer3 := &metapb.Peer{StoreId: uint64((i+2)%storeNum + 1), Id: uint64(i*storeNum + 3)}
	item := NewRegionInfo(&metapb.Region{
		Id:          uint64(i + 1),
		Peers:       []*metapb.Peer{peer1, peer2, peer3},
		StartKey:    []byte(fmt.Sprintf("%20d", i*10)),
		EndKey:      []byte(fmt.Sprintf("%20d", (i+1)*10)),
		RegionEpoch: &metapb.RegionEpoch{ConfVer: 100, Version: 100},
	},
		peer1,
		SetApproximateKeys(10),
		SetApproximateSize(10),
	)
	regionItemA := item
	regionPendingItemA := regionItemA.Clone(WithPendingPeers([]*metapb.Peer{peer3}))

	regionItemB := regionItemA.Clone()
	regionPendingItemB := regionItemB.Clone(WithPendingPeers([]*metapb.Peer{peer3}))
	regionGuide := GenerateRegionGuideFunc(true)

	// Old way
	{
		ctx := ContextTODO()
		_, err := regionsOld.AtomicCheckAndPutRegion(ctx, regionPendingItemA)
		re.NoError(err)
		re.Equal(int32(2), regionPendingItemA.GetRef())
		// check new item
		saveKV, saveCache, needSync, _ := regionGuide(ctx, regionItemA, regionPendingItemA)
		re.True(needSync)
		re.True(saveCache)
		re.False(saveKV)
		// update cache
		_, err = regionsOld.AtomicCheckAndPutRegion(ctx, regionItemA)
		re.NoError(err)
		re.Equal(int32(2), regionItemA.GetRef())
	}

	// New way
	{
		// root tree part in order, and updated in order, updated regionPendingItemB first, then regionItemB
		ctx := ContextTODO()
		_, err := regionsNew.CheckAndPutRootTree(ctx, regionPendingItemB)
		re.NoError(err)
		re.Equal(int32(1), regionPendingItemB.GetRef())
		ctx = ContextTODO()
		_, err = regionsNew.CheckAndPutRootTree(ctx, regionItemB)
		re.NoError(err)
		re.Equal(int32(1), regionItemB.GetRef())
		re.Equal(int32(0), regionPendingItemB.GetRef())

		// subtree part missing order, updated regionItemB first, then regionPendingItemB
		regionsNew.CheckAndPutSubTree(regionItemB)
		re.Equal(int32(2), regionItemB.GetRef())
		re.Equal(int32(0), regionPendingItemB.GetRef())
		regionsNew.UpdateSubTreeOrderInsensitive(regionPendingItemB)
		re.Equal(int32(1), regionItemB.GetRef())
		re.Equal(int32(1), regionPendingItemB.GetRef())

		// heartbeat again, no need updates root tree
		saveKV, saveCache, needSync, _ := regionGuide(ctx, regionItemB, regionItemB)
		re.False(needSync)
		re.False(saveCache)
		re.False(saveKV)

		// but need update sub tree again
		item := regionsNew.GetRegion(regionItemB.GetID())
		re.Equal(int32(1), item.GetRef())
		regionsNew.CheckAndPutSubTree(item)
		re.Equal(int32(2), item.GetRef())
	}
}

func TestCheckAndPutSubTree(t *testing.T) {
	re := require.New(t)
	regions := NewRegionsInfo()
	region := NewTestRegionInfo(1, 1, []byte("a"), []byte("b"))
	regions.CheckAndPutSubTree(region)
	// should failed to put because the root tree is missing
	re.Equal(0, regions.tree.length())
}

func TestCntRefAfterResetRegionCache(t *testing.T) {
	re := require.New(t)
	regions := NewRegionsInfo()
	// Put the region first.
	region := NewTestRegionInfo(1, 1, []byte("a"), []byte("b"))
	regions.CheckAndPutRegion(region)
	re.Equal(int32(2), region.GetRef())
	regions.ResetRegionCache()
	// Put the region after reset.
	region = NewTestRegionInfo(1, 1, []byte("a"), []byte("b"))
	re.Zero(region.GetRef())
	regions.CheckAndPutRegion(region)
	re.Equal(int32(2), region.GetRef())
}

func TestScanRegion(t *testing.T) {
	var (
		re                   = require.New(t)
		tree                 = newRegionTree()
		needContainAllRanges = true
		regions              []*RegionInfo
		err                  error
	)
	scanError := func(startKey, endKey []byte, limit int) {
		regions, err = scanRegion(tree, &keyutil.KeyRange{StartKey: startKey, EndKey: endKey}, limit, needContainAllRanges)
		re.Error(err)
	}
	scanNoError := func(startKey, endKey []byte, limit int) []*RegionInfo {
		regions, err = scanRegion(tree, &keyutil.KeyRange{StartKey: startKey, EndKey: endKey}, limit, needContainAllRanges)
		re.NoError(err)
		return regions
	}
	// region1
	// [a, b)
	updateNewItem(tree, NewTestRegionInfo(1, 1, []byte("a"), []byte("b")))
	re.Len(scanNoError([]byte("a"), []byte("b"), 0), 1)
	scanError([]byte("a"), []byte("c"), 0)
	re.Len(scanNoError([]byte("a"), []byte("c"), 1), 1)

	// region1 | region2
	// [a, b)  | [b, c)
	updateNewItem(tree, NewTestRegionInfo(2, 1, []byte("b"), []byte("c")))
	re.Len(scanNoError([]byte("a"), []byte("c"), 0), 2)
	re.Len(scanNoError([]byte("a"), []byte("c"), 1), 1)

	// region1 | region2 | region3
	// [a, b)  | [b, c)  | [d, f)
	updateNewItem(tree, NewTestRegionInfo(3, 1, []byte("d"), []byte("f")))
	scanError([]byte("a"), []byte("e"), 0)
	scanError([]byte("c"), []byte("e"), 0)

	// region1 | region2 | region3 | region4
	// [a, b)  | [b, c)  | [d, f)  | [f, i)
	updateNewItem(tree, NewTestRegionInfo(4, 1, []byte("f"), []byte("i")))
	scanError([]byte("c"), []byte("g"), 0)
	re.Len(scanNoError([]byte("g"), []byte("h"), 0), 1)
	re.Equal(uint64(4), regions[0].GetID())
	// test error type
	scanError([]byte(string('a'-1)), []byte("g"), 0)
	re.True(errs.ErrRegionNotAdjacent.Equal(err))

	// region1 | region2 | region3 | region4 | region5 | region6
	// [a, b)  | [b, c)  | [d, f)  | [f, i)  | [j, k)  | [l, +∞)]
	updateNewItem(tree, NewTestRegionInfo(6, 1, []byte("l"), nil))
	// test boundless
	re.Len(scanNoError([]byte("m"), nil, 0), 1)

	// ********** needContainAllRanges = false **********
	// Tests that previously reported errors will no longer report errors.
	needContainAllRanges = false
	re.Len(scanNoError([]byte("a"), []byte("e"), 0), 3)
	re.Len(scanNoError([]byte("c"), []byte("e"), 0), 1)
}

func TestScanRegionLimit(t *testing.T) {
	re := require.New(t)
	regions := NewRegionsInfo()
	// Put 18 regions.
	// [a0, a1) [a1, a2) [a2, a3) [a3, a4) [a4, a5) [a5, a6) [a6, a7) [a7, a8) [a8, a9)
	// [b0, b1) [b1, b2) [b2, b3) [b3, b4) [b4, b5) [b5, b6) [b6, b7) [b7, b8) [b8, b9)
	for i := range 9 {
		aStart := fmt.Appendf(nil, "a%d", i)
		aEnd := fmt.Appendf(nil, "a%d", i+1)
		regions.CheckAndPutRegion(NewTestRegionInfo(uint64(i), 1, aStart, aEnd))

		bStart := fmt.Appendf(nil, "b%d", i)
		bEnd := fmt.Appendf(nil, "b%d", i+1)
		regions.CheckAndPutRegion(NewTestRegionInfo(uint64(i+9), 1, bStart, bEnd))
	}

	for limit := 1; limit <= 18; limit++ {
		KeyRanges := keyutil.NewKeyRanges([]keyutil.KeyRange{
			keyutil.NewKeyRange("a", "b"),
			keyutil.NewKeyRange("b0", ""), // ensure the key ranges are not merged
		})
		resp, err := regions.BatchScanRegions(KeyRanges, WithLimit(limit))
		re.NoError(err)
		re.Len(resp, limit)
	}
}

func TestQueryRegions(t *testing.T) {
	re := require.New(t)
	regions := NewRegionsInfo()
	regions.CheckAndPutRegion(NewTestRegionInfo(1, 1, []byte("a"), []byte("b")))
	regions.CheckAndPutRegion(NewTestRegionInfo(2, 1, []byte("b"), []byte("c")))
	regions.CheckAndPutRegion(NewTestRegionInfo(3, 1, []byte("d"), []byte("e")))
	// Query regions by keys.
	keyIDMap, prevKeyIDMap, regionsByID := regions.QueryRegions(
		[][]byte{[]byte("a"), []byte("b"), []byte("c")},
		nil,
		nil,
		false,
	)
	re.Len(keyIDMap, 3)
	re.Empty(prevKeyIDMap)
	re.Equal(uint64(1), keyIDMap[0])
	re.Equal(uint64(2), keyIDMap[1])
	re.Zero(keyIDMap[2]) // The key is not in the region tree, so its ID should be 0.
	re.Len(regionsByID, 2)
	re.Equal(uint64(1), regionsByID[1].GetRegion().GetId())
	re.Equal(uint64(2), regionsByID[2].GetRegion().GetId())
	// Query regions by IDs.
	keyIDMap, prevKeyIDMap, regionsByID = regions.QueryRegions(
		nil,
		nil,
		[]uint64{1, 2, 3},
		false,
	)
	re.Empty(keyIDMap)
	re.Empty(prevKeyIDMap)
	re.Len(regionsByID, 3)
	re.Equal(uint64(1), regionsByID[1].GetRegion().GetId())
	re.Equal(uint64(2), regionsByID[2].GetRegion().GetId())
	re.Equal(uint64(3), regionsByID[3].GetRegion().GetId())
	// Query the region that does not exist.
	keyIDMap, prevKeyIDMap, regionsByID = regions.QueryRegions(
		nil,
		nil,
		[]uint64{4},
		false,
	)
	re.Empty(keyIDMap)
	re.Empty(prevKeyIDMap)
	re.Len(regionsByID, 1)
	re.Nil(regionsByID[4])
	keyIDMap, prevKeyIDMap, regionsByID = regions.QueryRegions(
		[][]byte{[]byte("c")},
		nil,
		nil,
		false,
	)
	re.Len(keyIDMap, 1)
	re.Empty(prevKeyIDMap)
	re.Zero(keyIDMap[0])
	re.Empty(regionsByID)
	keyIDMap, prevKeyIDMap, regionsByID = regions.QueryRegions(
		[][]byte{[]byte("c")},
		nil,
		[]uint64{4},
		false,
	)
	re.Len(keyIDMap, 1)
	re.Empty(prevKeyIDMap)
	re.Zero(keyIDMap[0])
	re.Nil(regionsByID[4])
	// Query regions by keys, previous keys and IDs.
	keyIDMap, prevKeyIDMap, regionsByID = regions.QueryRegions(
		[][]byte{[]byte("b"), []byte("c")},
		[][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), []byte("e"), []byte("f")},
		[]uint64{1, 3},
		false,
	)
	re.Len(keyIDMap, 2)
	re.Len(prevKeyIDMap, 6)
	re.Equal(uint64(2), keyIDMap[0])
	re.Zero(keyIDMap[1])
	re.Zero(prevKeyIDMap[0])
	re.Equal(uint64(1), prevKeyIDMap[1])
	re.Zero(prevKeyIDMap[2])
	re.Zero(prevKeyIDMap[3])
	re.Zero(prevKeyIDMap[4])
	re.Zero(prevKeyIDMap[5])
	re.Len(regionsByID, 3)
	re.Equal(uint64(1), regionsByID[1].GetRegion().GetId())
	re.Equal(uint64(2), regionsByID[2].GetRegion().GetId())
	re.Equal(uint64(3), regionsByID[3].GetRegion().GetId())
}

func TestCodecRule(t *testing.T) {
	re := require.New(t)
	for _, v := range []string{"leader", "peer", "learner", "witness"} {
		rule := NewRule(v)
		if rule != Unknown {
			re.Equal(rule.String(), v)
		}
		body, err := json.Marshal(&rule)
		re.NoError(err)
		re.Equal(strings.Join([]string{"\"", rule.String(), "\""}, ""), string(body))
		var rule2 Rule
		re.NoError(json.Unmarshal(body, &rule2))
		re.Equal(rule.String(), rule2.String())
	}
}

func TestRegionCount(t *testing.T) {
	re := require.New(t)
	regions := NewRegionsInfo()

	regions.CheckAndPutRegion(NewTestRegionInfo(1, 1, []byte("a"), []byte("c")))
	regions.CheckAndPutRegion(NewTestRegionInfo(2, 1, []byte("e"), []byte("g")))
	regions.CheckAndPutRegion(NewTestRegionInfo(3, 1, []byte("g"), []byte("i")))
	for _, key := range [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), []byte("e"), []byte("f"), []byte("g"), []byte("h"), []byte("")} {
		count := regions.GetRegionCount([]byte("a"), key)
		scanCount := len(regions.ScanRegions([]byte("a"), key, 100))
		re.Equal(count, scanCount, "endKey: %s", key)

		count = regions.GetRegionCount([]byte(""), key)
		scanCount = len(regions.ScanRegions([]byte(""), key, 100))
		re.Equal(count, scanCount, "endKey: %s", key)

		count = regions.GetRegionCount(key, []byte("i"))
		scanCount = len(regions.ScanRegions(key, []byte("i"), 100))
		re.Equal(count, scanCount, "startKey: %s", key)

		count = regions.GetRegionCount(key, []byte(""))
		scanCount = len(regions.ScanRegions(key, []byte(""), 100))
		re.Equal(count, scanCount, "startKey: %s", key)
	}
}
