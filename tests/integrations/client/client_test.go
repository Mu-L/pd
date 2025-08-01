// Copyright 2018 TiKV Project Authors.
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

package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/go-units"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/goleak"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/kvproto/pkg/meta_storagepb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"

	pd "github.com/tikv/pd/client"
	"github.com/tikv/pd/client/clients/gc"
	"github.com/tikv/pd/client/clients/router"
	"github.com/tikv/pd/client/constants"
	pdHttp "github.com/tikv/pd/client/http"
	"github.com/tikv/pd/client/opt"
	"github.com/tikv/pd/client/pkg/caller"
	cb "github.com/tikv/pd/client/pkg/circuitbreaker"
	"github.com/tikv/pd/client/pkg/retry"
	sd "github.com/tikv/pd/client/servicediscovery"
	"github.com/tikv/pd/pkg/core"
	"github.com/tikv/pd/pkg/keyspace"
	"github.com/tikv/pd/pkg/mock/mockid"
	"github.com/tikv/pd/pkg/storage/endpoint"
	"github.com/tikv/pd/pkg/utils/assertutil"
	"github.com/tikv/pd/pkg/utils/keypath"
	"github.com/tikv/pd/pkg/utils/keyutil"
	"github.com/tikv/pd/pkg/utils/testutil"
	"github.com/tikv/pd/pkg/utils/tsoutil"
	"github.com/tikv/pd/pkg/utils/typeutil"
	"github.com/tikv/pd/server"
	"github.com/tikv/pd/server/config"
	"github.com/tikv/pd/tests"
	"github.com/tikv/pd/tests/integrations/mcs/utils"
)

const (
	tsoRequestConcurrencyNumber = 5
	tsoRequestRound             = 30
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, testutil.LeakOptions...)
}

func TestClientLeaderChange(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestCluster(ctx, 3)
	re.NoError(err)
	defer cluster.Destroy()

	endpoints := runServer(re, cluster)
	endpointsWithWrongURL := append([]string{}, endpoints...)
	// inject wrong http scheme
	for i := range endpointsWithWrongURL {
		endpointsWithWrongURL[i] = "https://" + strings.TrimPrefix(endpointsWithWrongURL[i], "http://")
	}
	cli := setupCli(ctx, re, endpointsWithWrongURL)
	defer cli.Close()
	innerCli, ok := cli.(interface{ GetServiceDiscovery() sd.ServiceDiscovery })
	re.True(ok)

	var ts1, ts2 uint64
	testutil.Eventually(re, func() bool {
		p1, l1, err := cli.GetTS(context.TODO())
		if err == nil {
			ts1 = tsoutil.ComposeTS(p1, l1)
			return true
		}
		t.Log(err)
		return false
	})
	re.True(cluster.CheckTSOUnique(ts1))

	leader := cluster.GetLeader()
	waitLeader(re, innerCli.GetServiceDiscovery(), cluster.GetServer(leader))

	err = cluster.GetServer(leader).Stop()
	re.NoError(err)
	leader = cluster.WaitLeader()
	re.NotEmpty(leader)

	waitLeader(re, innerCli.GetServiceDiscovery(), cluster.GetServer(leader))

	// Check TS won't fall back after leader changed.
	testutil.Eventually(re, func() bool {
		p2, l2, err := cli.GetTS(context.TODO())
		if err == nil {
			ts2 = tsoutil.ComposeTS(p2, l2)
			return true
		}
		t.Log(err)
		return false
	})
	re.True(cluster.CheckTSOUnique(ts2))
	re.Less(ts1, ts2)

	// Check URL list.
	cli.Close()
	urls := innerCli.GetServiceDiscovery().GetServiceURLs()
	sort.Strings(urls)
	sort.Strings(endpoints)
	re.Equal(endpoints, urls)
}

func TestLeaderTransferAndMoveCluster(t *testing.T) {
	re := require.New(t)
	re.NoError(failpoint.Enable("github.com/tikv/pd/pkg/member/skipCampaignLeaderCheck", "return(true)"))
	defer func() {
		re.NoError(failpoint.Disable("github.com/tikv/pd/pkg/member/skipCampaignLeaderCheck"))
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestCluster(ctx, 3)
	re.NoError(err)
	defer cluster.Destroy()

	endpoints := runServer(re, cluster)
	cli := setupCli(ctx, re, endpoints)
	defer cli.Close()

	var lastTS uint64
	testutil.Eventually(re, func() bool {
		physical, logical, err := cli.GetTS(context.TODO())
		if err == nil {
			lastTS = tsoutil.ComposeTS(physical, logical)
			return true
		}
		t.Log(err)
		return false
	})
	re.True(cluster.CheckTSOUnique(lastTS))

	// Start a goroutine the make sure TS won't fall back.
	quit := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-quit:
				return
			default:
			}

			physical, logical, err := cli.GetTS(context.TODO())
			if err == nil {
				ts := tsoutil.ComposeTS(physical, logical)
				re.True(cluster.CheckTSOUnique(ts))
				re.Less(lastTS, ts)
				lastTS = ts
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// Transfer leader.
	for range 3 {
		oldLeaderName := cluster.WaitLeader()
		err := cluster.GetServer(oldLeaderName).ResignLeader()
		re.NoError(err)
		newLeaderName := cluster.WaitLeader()
		re.NotEqual(oldLeaderName, newLeaderName)
	}

	// ABC->ABCDEF
	oldServers := cluster.GetServers()
	oldLeaderName := cluster.WaitLeader()
	for range 3 {
		time.Sleep(5 * time.Second)
		newPD, err := cluster.Join(ctx)
		re.NoError(err)
		re.NoError(newPD.Run())
		oldLeaderName = cluster.WaitLeader()
	}

	// ABCDEF->DEF
	oldNames := make([]string, 0)
	for _, s := range oldServers {
		oldNames = append(oldNames, s.GetServer().GetMemberInfo().GetName())
		err = s.Stop()
		re.NoError(err)
	}
	newLeaderName := cluster.WaitLeader()
	re.NotEqual(oldLeaderName, newLeaderName)
	re.NotContains(oldNames, newLeaderName)

	close(quit)
	wg.Wait()
}

func TestGetTSAfterTransferLeader(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestCluster(ctx, 2)
	re.NoError(err)
	defer cluster.Destroy()
	endpoints := runServer(re, cluster)
	leader := cluster.WaitLeader()
	re.NotEmpty(leader)

	cli := setupCli(ctx, re, endpoints, opt.WithCustomTimeoutOption(10*time.Second))
	defer cli.Close()

	var leaderSwitched atomic.Bool
	cli.GetServiceDiscovery().AddLeaderSwitchedCallback(func(string) error {
		leaderSwitched.Store(true)
		return nil
	})
	err = cluster.GetServer(leader).ResignLeader()
	re.NoError(err)
	newLeader := cluster.WaitLeader()
	re.NotEmpty(newLeader)
	re.NotEqual(leader, newLeader)
	leader = cluster.WaitLeader()
	re.NotEmpty(leader)
	err = cli.GetServiceDiscovery().CheckMemberChanged()
	re.NoError(err)

	testutil.Eventually(re, leaderSwitched.Load)
	// The leader stream must be updated after the leader switch is sensed by the client.
	_, _, err = cli.GetTS(context.TODO())
	re.NoError(err)
}

func TestTSOFollowerProxy(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestCluster(ctx, 3)
	re.NoError(err)
	defer cluster.Destroy()

	endpoints := runServer(re, cluster)
	cli1 := setupCli(ctx, re, endpoints)
	defer cli1.Close()
	cli2 := setupCli(ctx, re, endpoints)
	defer cli2.Close()
	re.NoError(failpoint.Enable("github.com/tikv/pd/client/clients/tso/speedUpTsoDispatcherUpdateInterval", "return(true)"))
	err = cli2.UpdateOption(opt.EnableTSOFollowerProxy, true)
	re.NoError(err)

	var wg sync.WaitGroup
	wg.Add(tsoRequestConcurrencyNumber)
	for range tsoRequestConcurrencyNumber {
		go func() {
			defer wg.Done()
			var lastTS uint64
			for range tsoRequestRound {
				physical, logical, err := cli2.GetTS(context.Background())
				re.NoError(err)
				ts := tsoutil.ComposeTS(physical, logical)
				re.Less(lastTS, ts)
				lastTS = ts
				// After requesting with the follower proxy, request with the leader directly.
				physical, logical, err = cli1.GetTS(context.Background())
				re.NoError(err)
				ts = tsoutil.ComposeTS(physical, logical)
				re.Less(lastTS, ts)
				lastTS = ts
			}
		}()
	}
	wg.Wait()

	followerServer := cluster.GetServer(cluster.GetFollower())
	re.NoError(followerServer.Stop())
	ch := make(chan struct{})
	re.NoError(failpoint.EnableCall("github.com/tikv/pd/server/delayStartServer", func() {
		// Server is not in `Running` state, so the follower proxy should return
		// error while create stream.
		ch <- struct{}{}
	}))
	wg.Add(1)
	go func() {
		defer wg.Done()
		re.NoError(followerServer.Run())
	}()
	re.Eventually(func() bool {
		_, _, err := cli2.GetTS(context.Background())
		if err == nil {
			return false
		}
		return strings.Contains(err.Error(), "server not started")
	}, 3*time.Second, 10*time.Millisecond)
	<-ch
	re.NoError(failpoint.Disable("github.com/tikv/pd/server/delayStartServer"))
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/clients/tso/speedUpTsoDispatcherUpdateInterval"))
	wg.Wait()

	// Disable the follower proxy and check if the stream is updated.
	err = cli2.UpdateOption(opt.EnableTSOFollowerProxy, false)
	re.NoError(err)

	wg.Add(tsoRequestConcurrencyNumber)
	for range tsoRequestConcurrencyNumber {
		go func() {
			defer wg.Done()
			var lastTS uint64
			for range tsoRequestRound {
				physical, logical, err := cli2.GetTS(context.Background())
				if err != nil {
					// It can only be the context canceled error caused by the stale stream cleanup.
					re.ErrorContains(err, "context canceled")
					continue
				}
				re.NoError(err)
				ts := tsoutil.ComposeTS(physical, logical)
				re.Less(lastTS, ts)
				lastTS = ts
				// After requesting with the follower proxy, request with the leader directly.
				physical, logical, err = cli1.GetTS(context.Background())
				re.NoError(err)
				ts = tsoutil.ComposeTS(physical, logical)
				re.Less(lastTS, ts)
				lastTS = ts
			}
			// Ensure at least one request is successful.
			re.NotEmpty(lastTS)
		}()
	}
	wg.Wait()
}

func TestTSOFollowerProxyWithTSOService(t *testing.T) {
	re := require.New(t)
	re.NoError(failpoint.Enable("github.com/tikv/pd/client/servicediscovery/fastUpdateServiceMode", `return(true)`))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestClusterWithKeyspaceGroup(ctx, 1)
	re.NoError(err)
	defer cluster.Destroy()
	err = cluster.RunInitialServers()
	re.NoError(err)
	leaderName := cluster.WaitLeader()
	pdLeaderServer := cluster.GetServer(leaderName)
	re.NoError(pdLeaderServer.BootstrapCluster())
	backendEndpoints := pdLeaderServer.GetAddr()
	tsoCluster, err := tests.NewTestTSOCluster(ctx, 2, backendEndpoints)
	re.NoError(err)
	defer tsoCluster.Destroy()
	time.Sleep(100 * time.Millisecond)
	cli := utils.SetupClientWithKeyspaceID(ctx, re, constants.DefaultKeyspaceID, strings.Split(backendEndpoints, ","))
	re.NotNil(cli)
	defer cli.Close()
	// TSO service does not support the follower proxy, so enabling it should fail.
	err = cli.UpdateOption(opt.EnableTSOFollowerProxy, true)
	re.Error(err)
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/servicediscovery/fastUpdateServiceMode"))
}

// TestUnavailableTimeAfterLeaderIsReady is used to test https://github.com/tikv/pd/issues/5207
func TestUnavailableTimeAfterLeaderIsReady(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestCluster(ctx, 3)
	re.NoError(err)
	defer cluster.Destroy()

	endpoints := runServer(re, cluster)
	cli := setupCli(ctx, re, endpoints)
	defer cli.Close()

	var wg sync.WaitGroup
	var maxUnavailableTime, leaderReadyTime time.Time
	getTsoFunc := func() {
		defer wg.Done()
		var lastTS uint64
		for range tsoRequestRound {
			var physical, logical int64
			var ts uint64
			physical, logical, err = cli.GetTS(context.Background())
			ts = tsoutil.ComposeTS(physical, logical)
			if err != nil {
				maxUnavailableTime = time.Now()
				continue
			}
			re.NoError(err)
			re.Less(lastTS, ts)
			lastTS = ts
		}
	}

	// test resign pd leader or stop pd leader
	wg.Add(1 + 1)
	go getTsoFunc()
	go func() {
		defer wg.Done()
		leader := cluster.GetLeaderServer()
		err = leader.Stop()
		re.NoError(err)
		re.NotEmpty(cluster.WaitLeader())
		leaderReadyTime = time.Now()
		err = tests.RunServers([]*tests.TestServer{leader})
		re.NoError(err)
	}()
	wg.Wait()
	re.Less(maxUnavailableTime.UnixMilli(), leaderReadyTime.Add(1*time.Second).UnixMilli())

	// test kill pd leader pod or network of leader is unreachable
	wg.Add(1 + 1)
	maxUnavailableTime, leaderReadyTime = time.Time{}, time.Time{}
	go getTsoFunc()
	go func() {
		defer wg.Done()
		leader := cluster.GetLeaderServer()
		re.NoError(failpoint.Enable("github.com/tikv/pd/client/clients/tso/unreachableNetwork", "return(true)"))
		err = leader.Stop()
		re.NoError(err)
		re.NotEmpty(cluster.WaitLeader())
		re.NoError(failpoint.Disable("github.com/tikv/pd/client/clients/tso/unreachableNetwork"))
		leaderReadyTime = time.Now()
	}()
	wg.Wait()
	re.Less(maxUnavailableTime.UnixMilli(), leaderReadyTime.Add(1*time.Second).UnixMilli())
}

func TestCustomTimeout(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestCluster(ctx, 1)
	re.NoError(err)
	defer cluster.Destroy()

	endpoints := runServer(re, cluster)
	cli := setupCli(ctx, re, endpoints, opt.WithCustomTimeoutOption(time.Second))
	defer cli.Close()

	start := time.Now()
	re.NoError(failpoint.Enable("github.com/tikv/pd/server/customTimeout", "return(true)"))
	_, err = cli.GetAllStores(context.TODO())
	re.NoError(failpoint.Disable("github.com/tikv/pd/server/customTimeout"))
	re.Error(err)
	re.GreaterOrEqual(time.Since(start), time.Second)
	re.Less(time.Since(start), 2*time.Second)
}

type followerForwardAndHandleTestSuite struct {
	suite.Suite
	ctx   context.Context
	clean context.CancelFunc

	cluster   *tests.TestCluster
	endpoints []string
	regionID  uint64
}

func TestFollowerForwardAndHandleTestSuite(t *testing.T) {
	suite.Run(t, new(followerForwardAndHandleTestSuite))
}

func (suite *followerForwardAndHandleTestSuite) SetupSuite() {
	re := suite.Require()
	suite.ctx, suite.clean = context.WithCancel(context.Background())
	sd.MemberHealthCheckInterval = 100 * time.Millisecond
	cluster, err := tests.NewTestCluster(suite.ctx, 3)
	re.NoError(err)
	suite.cluster = cluster
	suite.endpoints = runServer(re, cluster)
	re.NotEmpty(cluster.WaitLeader())
	leader := cluster.GetLeaderServer()
	grpcPDClient := testutil.MustNewGrpcClient(re, leader.GetAddr())
	suite.regionID = regionIDAllocator.alloc()
	testutil.Eventually(re, func() bool {
		regionHeartbeat, err := grpcPDClient.RegionHeartbeat(suite.ctx)
		re.NoError(err)
		region := &metapb.Region{
			Id: suite.regionID,
			RegionEpoch: &metapb.RegionEpoch{
				ConfVer: 1,
				Version: 1,
			},
			Peers: peers,
		}
		req := &pdpb.RegionHeartbeatRequest{
			Header: newHeader(),
			Region: region,
			Leader: peers[0],
		}
		err = regionHeartbeat.Send(req)
		re.NoError(err)
		_, err = regionHeartbeat.Recv()
		return err == nil
	})
}

func (suite *followerForwardAndHandleTestSuite) TearDownSuite() {
	suite.cluster.Destroy()
	suite.clean()
}

func (suite *followerForwardAndHandleTestSuite) TestGetRegionByFollowerForwarding() {
	re := suite.Require()
	ctx, cancel := context.WithCancel(suite.ctx)
	defer cancel()

	cli := setupCli(ctx, re, suite.endpoints, opt.WithForwardingOption(true))
	defer cli.Close()
	re.NoError(failpoint.Enable("github.com/tikv/pd/client/servicediscovery/unreachableNetwork1", "return(true)"))
	time.Sleep(200 * time.Millisecond)
	r, err := cli.GetRegion(context.Background(), []byte("a"))
	re.NoError(err)
	re.NotNil(r)

	re.NoError(failpoint.Disable("github.com/tikv/pd/client/servicediscovery/unreachableNetwork1"))
	time.Sleep(200 * time.Millisecond)
	r, err = cli.GetRegion(context.Background(), []byte("a"))
	re.NoError(err)
	re.NotNil(r)
}

// case 1: unreachable -> normal
func (suite *followerForwardAndHandleTestSuite) TestGetTsoByFollowerForwarding1() {
	re := suite.Require()
	ctx, cancel := context.WithCancel(suite.ctx)
	defer cancel()
	cli := setupCli(ctx, re, suite.endpoints, opt.WithForwardingOption(true))
	defer cli.Close()

	re.NoError(failpoint.Enable("github.com/tikv/pd/client/clients/tso/unreachableNetwork", "return(true)"))
	var lastTS uint64
	testutil.Eventually(re, func() bool {
		physical, logical, err := cli.GetTS(context.TODO())
		if err == nil {
			lastTS = tsoutil.ComposeTS(physical, logical)
			return true
		}
		suite.T().Log(err)
		return false
	})

	lastTS = checkTS(re, cli, lastTS)
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/clients/tso/unreachableNetwork"))
	time.Sleep(2 * time.Second)
	checkTS(re, cli, lastTS)

	re.NoError(failpoint.Enable("github.com/tikv/pd/client/responseNil", "return(true)"))
	regions, err := cli.BatchScanRegions(ctx, []router.KeyRange{{StartKey: []byte(""), EndKey: []byte("")}}, 100)
	re.NoError(err)
	re.Empty(regions)
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/responseNil"))
	regions, err = cli.BatchScanRegions(ctx, []router.KeyRange{{StartKey: []byte(""), EndKey: []byte("")}}, 100)
	re.NoError(err)
	re.Len(regions, 1)
}

// case 2: unreachable -> leader transfer -> normal
func (suite *followerForwardAndHandleTestSuite) TestGetTsoByFollowerForwarding2() {
	re := suite.Require()
	ctx, cancel := context.WithCancel(suite.ctx)
	defer cancel()
	cli := setupCli(ctx, re, suite.endpoints, opt.WithForwardingOption(true))
	defer cli.Close()

	re.NoError(failpoint.Enable("github.com/tikv/pd/client/clients/tso/unreachableNetwork", "return(true)"))
	var lastTS uint64
	testutil.Eventually(re, func() bool {
		physical, logical, err := cli.GetTS(context.TODO())
		if err == nil {
			lastTS = tsoutil.ComposeTS(physical, logical)
			return true
		}
		suite.T().Log(err)
		return false
	})

	lastTS = checkTS(re, cli, lastTS)
	re.NoError(suite.cluster.GetLeaderServer().ResignLeader())
	re.NotEmpty(suite.cluster.WaitLeader())
	lastTS = checkTS(re, cli, lastTS)

	re.NoError(failpoint.Disable("github.com/tikv/pd/client/clients/tso/unreachableNetwork"))
	time.Sleep(5 * time.Second)
	checkTS(re, cli, lastTS)
}

// case 3: network partition between client and follower A -> transfer leader to follower A -> normal
func (suite *followerForwardAndHandleTestSuite) TestGetTsoAndRegionByFollowerForwarding() {
	re := suite.Require()
	ctx, cancel := context.WithCancel(suite.ctx)
	defer cancel()

	cluster := suite.cluster
	leader := cluster.GetLeaderServer()

	follower := cluster.GetServer(cluster.GetFollower())
	re.NoError(failpoint.Enable("github.com/tikv/pd/client/pkg/utils/grpcutil/unreachableNetwork2", fmt.Sprintf("return(\"%s\")", follower.GetAddr())))

	cli := setupCli(ctx, re, suite.endpoints, opt.WithForwardingOption(true))
	defer cli.Close()
	var lastTS uint64
	testutil.Eventually(re, func() bool {
		physical, logical, err := cli.GetTS(context.TODO())
		if err == nil {
			lastTS = tsoutil.ComposeTS(physical, logical)
			return true
		}
		suite.T().Log(err)
		return false
	})
	lastTS = checkTS(re, cli, lastTS)
	r, err := cli.GetRegion(context.Background(), []byte("a"))
	re.NoError(err)
	re.NotNil(r)
	err = leader.GetServer().GetMember().ResignEtcdLeader(leader.GetServer().Context(),
		leader.GetServer().Name(), follower.GetServer().Name())
	re.NoError(err)
	re.NotEmpty(cluster.WaitLeader())
	testutil.Eventually(re, func() bool {
		physical, logical, err := cli.GetTS(context.TODO())
		if err == nil {
			lastTS = tsoutil.ComposeTS(physical, logical)
			return true
		}
		suite.T().Log(err)
		return false
	})
	lastTS = checkTS(re, cli, lastTS)
	testutil.Eventually(re, func() bool {
		r, err = cli.GetRegion(context.Background(), []byte("a"))
		if err == nil && r != nil {
			return true
		}
		return false
	})

	re.NoError(failpoint.Disable("github.com/tikv/pd/client/pkg/utils/grpcutil/unreachableNetwork2"))
	testutil.Eventually(re, func() bool {
		physical, logical, err := cli.GetTS(context.TODO())
		if err == nil {
			lastTS = tsoutil.ComposeTS(physical, logical)
			return true
		}
		suite.T().Log(err)
		return false
	})
	lastTS = checkTS(re, cli, lastTS)
	testutil.Eventually(re, func() bool {
		r, err = cli.GetRegion(context.Background(), []byte("a"))
		if err == nil && r != nil {
			return true
		}
		return false
	})
}

func (suite *followerForwardAndHandleTestSuite) TestGetRegionFromLeaderWhenNetworkErr() {
	re := suite.Require()
	ctx, cancel := context.WithCancel(suite.ctx)
	defer cancel()

	cluster := suite.cluster
	re.NotEmpty(cluster.WaitLeader())
	leader := cluster.GetLeaderServer()

	follower := cluster.GetServer(cluster.GetFollower())
	re.NoError(failpoint.Enable("github.com/tikv/pd/client/pkg/utils/grpcutil/unreachableNetwork2", fmt.Sprintf("return(\"%s\")", follower.GetAddr())))

	cli := setupCli(ctx, re, suite.endpoints)
	defer cli.Close()

	err := cluster.GetLeaderServer().GetServer().GetMember().ResignEtcdLeader(ctx, leader.GetServer().Name(), follower.GetServer().Name())
	re.NoError(err)
	re.NotEmpty(cluster.WaitLeader())

	// here is just for trigger the leader change.
	_, err = cli.GetRegion(context.Background(), []byte("a"))
	re.Error(err)

	testutil.Eventually(re, func() bool {
		return cli.GetLeaderURL() == follower.GetAddr()
	})
	r, err := cli.GetRegion(context.Background(), []byte("a"))
	re.Error(err)
	re.Nil(r)

	re.NoError(failpoint.Disable("github.com/tikv/pd/client/pkg/utils/grpcutil/unreachableNetwork2"))
	err = cli.GetServiceDiscovery().CheckMemberChanged()
	re.NoError(err)
	testutil.Eventually(re, func() bool {
		r, err = cli.GetRegion(context.Background(), []byte("a"))
		if err == nil && r != nil {
			return true
		}
		return false
	})
}

func (suite *followerForwardAndHandleTestSuite) TestGetRegionFromFollower() {
	re := suite.Require()
	ctx, cancel := context.WithCancel(suite.ctx)
	defer cancel()

	cluster := suite.cluster
	cli := setupCli(ctx, re, suite.endpoints)
	defer cli.Close()
	err := cli.UpdateOption(opt.EnableFollowerHandle, true)
	re.NoError(err)
	re.NotEmpty(cluster.WaitLeader())
	leader := cluster.GetLeaderServer()
	testutil.Eventually(re, func() bool {
		ret := true
		for _, s := range cluster.GetServers() {
			if s.IsLeader() {
				continue
			}
			if !s.GetServer().DirectlyGetRaftCluster().GetRegionSyncer().IsRunning() {
				ret = false
			}
		}
		return ret
	})
	// follower have no region
	cnt := 0
	for range 100 {
		resp, err := cli.GetRegion(ctx, []byte("a"), opt.WithAllowFollowerHandle())
		if err == nil && resp != nil {
			cnt++
		}
		re.Equal(resp.Meta.Id, suite.regionID)
	}
	re.Equal(100, cnt)

	// because we can't check whether this request is processed by followers from response,
	// we can disable forward and make network problem for leader.
	re.NoError(failpoint.Enable("github.com/tikv/pd/client/servicediscovery/unreachableNetwork1", fmt.Sprintf("return(\"%s\")", leader.GetAddr())))
	time.Sleep(150 * time.Millisecond)
	cnt = 0
	for range 100 {
		resp, err := cli.GetRegion(ctx, []byte("a"), opt.WithAllowFollowerHandle())
		if err == nil && resp != nil {
			cnt++
		}
		re.Equal(resp.Meta.Id, suite.regionID)
	}
	re.Equal(100, cnt)
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/servicediscovery/unreachableNetwork1"))

	// make network problem for follower.
	follower := cluster.GetServer(cluster.GetFollower())
	re.NoError(failpoint.Enable("github.com/tikv/pd/client/servicediscovery/unreachableNetwork1", fmt.Sprintf("return(\"%s\")", follower.GetAddr())))
	time.Sleep(100 * time.Millisecond)
	cnt = 0
	for range 100 {
		resp, err := cli.GetRegion(ctx, []byte("a"), opt.WithAllowFollowerHandle())
		if err == nil && resp != nil {
			cnt++
		}
		re.Equal(resp.Meta.Id, suite.regionID)
	}
	re.Equal(100, cnt)
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/servicediscovery/unreachableNetwork1"))

	// follower client failed will retry by leader service client.
	re.NoError(failpoint.Enable("github.com/tikv/pd/server/followerHandleError", "return(true)"))
	cnt = 0
	for range 100 {
		resp, err := cli.GetRegion(ctx, []byte("a"), opt.WithAllowFollowerHandle())
		if err == nil && resp != nil {
			cnt++
		}
		re.Equal(resp.Meta.Id, suite.regionID)
	}
	re.Equal(100, cnt)
	re.NoError(failpoint.Disable("github.com/tikv/pd/server/followerHandleError"))

	// test after being healthy
	re.NoError(failpoint.Enable("github.com/tikv/pd/client/servicediscovery/unreachableNetwork1", fmt.Sprintf("return(\"%s\")", leader.GetAddr())))
	re.NoError(failpoint.Enable("github.com/tikv/pd/client/servicediscovery/fastCheckAvailable", "return(true)"))
	time.Sleep(100 * time.Millisecond)
	cnt = 0
	for range 100 {
		resp, err := cli.GetRegion(ctx, []byte("a"), opt.WithAllowFollowerHandle())
		if err == nil && resp != nil {
			cnt++
		}
		re.Equal(resp.Meta.Id, suite.regionID)
	}
	re.Equal(100, cnt)
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/servicediscovery/unreachableNetwork1"))
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/servicediscovery/fastCheckAvailable"))
}

func (suite *followerForwardAndHandleTestSuite) TestGetTSFuture() {
	re := suite.Require()
	ctx, cancel := context.WithCancel(suite.ctx)
	defer cancel()

	re.NoError(failpoint.Enable("github.com/tikv/pd/client/clients/tso/shortDispatcherChannel", "return(true)"))

	cli := setupCli(ctx, re, suite.endpoints)

	ctxs := make([]context.Context, 20)
	cancels := make([]context.CancelFunc, 20)
	for i := range 20 {
		ctxs[i], cancels[i] = context.WithCancel(ctx)
	}
	start := time.Now()
	wg1 := sync.WaitGroup{}
	wg2 := sync.WaitGroup{}
	wg3 := sync.WaitGroup{}
	wg1.Add(1)
	go func() {
		<-time.After(time.Second)
		for i := range 20 {
			cancels[i]()
		}
		wg1.Done()
	}()
	wg2.Add(1)
	go func() {
		cli.Close()
		wg2.Done()
	}()
	wg3.Add(1)
	go func() {
		for i := range 20 {
			cli.GetTSAsync(ctxs[i])
		}
		wg3.Done()
	}()
	wg1.Wait()
	wg2.Wait()
	wg3.Wait()
	re.Less(time.Since(start), time.Second*2)
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/clients/tso/shortDispatcherChannel"))
}

func checkTS(re *require.Assertions, cli pd.Client, lastTS uint64) uint64 {
	for range tsoRequestRound {
		physical, logical, err := cli.GetTS(context.TODO())
		if err == nil {
			ts := tsoutil.ComposeTS(physical, logical)
			re.Less(lastTS, ts)
			lastTS = ts
		}
		time.Sleep(time.Millisecond)
	}
	return lastTS
}

func runServer(re *require.Assertions, cluster *tests.TestCluster) []string {
	err := cluster.RunInitialServers()
	re.NoError(err)
	re.NotEmpty(cluster.WaitLeader())
	leaderServer := cluster.GetLeaderServer()
	re.NoError(leaderServer.BootstrapCluster())

	testServers := cluster.GetServers()
	endpoints := make([]string, 0, len(testServers))
	for _, s := range testServers {
		endpoints = append(endpoints, s.GetConfig().AdvertiseClientUrls)
	}
	return endpoints
}

func setupCli(ctx context.Context, re *require.Assertions, endpoints []string, opts ...opt.ClientOption) pd.Client {
	cli, err := pd.NewClientWithContext(ctx, caller.TestComponent,
		endpoints, pd.SecurityOption{}, opts...)
	re.NoError(err)
	return cli
}

func waitLeader(re *require.Assertions, cli sd.ServiceDiscovery, leader *tests.TestServer) {
	testutil.Eventually(re, func() bool {
		cli.ScheduleCheckMemberChanged()
		return cli.GetServingURL() == leader.GetConfig().ClientUrls && leader.GetAddr() == cli.GetServingURL()
	})
}

func TestConfigTTLAfterTransferLeader(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestCluster(ctx, 3)
	re.NoError(err)
	defer cluster.Destroy()
	err = cluster.RunInitialServers()
	re.NoError(err)
	leader := cluster.GetServer(cluster.WaitLeader())
	re.NoError(leader.BootstrapCluster())
	addr := fmt.Sprintf("%s/pd/api/v1/config?ttlSecond=5", leader.GetAddr())
	postData, err := json.Marshal(map[string]any{
		"schedule.max-snapshot-count":             999,
		"schedule.enable-location-replacement":    false,
		"schedule.max-merge-region-size":          999,
		"schedule.max-merge-region-keys":          999,
		"schedule.scheduler-max-waiting-operator": 999,
		"schedule.leader-schedule-limit":          999,
		"schedule.region-schedule-limit":          999,
		"schedule.hot-region-schedule-limit":      999,
		"schedule.replica-schedule-limit":         999,
		"schedule.merge-schedule-limit":           999,
	})
	re.NoError(err)
	resp, err := leader.GetHTTPClient().Post(addr, "application/json", bytes.NewBuffer(postData))
	resp.Body.Close()
	re.NoError(err)
	time.Sleep(2 * time.Second)
	re.NoError(leader.Destroy())
	time.Sleep(2 * time.Second)
	leader = cluster.GetServer(cluster.WaitLeader())
	re.NotNil(leader)
	options := leader.GetPersistOptions()
	re.NotNil(options)
	re.Equal(uint64(999), options.GetMaxSnapshotCount())
	re.False(options.IsLocationReplacementEnabled())
	re.Equal(uint64(999), options.GetMaxMergeRegionSize())
	re.Equal(uint64(999), options.GetMaxMergeRegionKeys())
	re.Equal(uint64(999), options.GetSchedulerMaxWaitingOperator())
	re.Equal(uint64(999), options.GetLeaderScheduleLimit())
	re.Equal(uint64(999), options.GetRegionScheduleLimit())
	re.Equal(uint64(999), options.GetHotRegionScheduleLimit())
	re.Equal(uint64(999), options.GetReplicaScheduleLimit())
	re.Equal(uint64(999), options.GetMergeScheduleLimit())
}

func TestCloseClient(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestCluster(ctx, 1)
	re.NoError(err)
	defer cluster.Destroy()
	endpoints := runServer(re, cluster)
	cli := setupCli(ctx, re, endpoints)
	ts := cli.GetTSAsync(context.TODO())
	time.Sleep(time.Second)
	cli.Close()
	physical, logical, err := ts.Wait()
	if err == nil {
		re.Positive(physical)
		re.Positive(logical)
	} else {
		re.ErrorIs(err, context.Canceled)
		re.Zero(physical)
		re.Zero(logical)
	}
	ts = cli.GetTSAsync(context.TODO())
	physical, logical, err = ts.Wait()
	re.ErrorIs(err, context.Canceled)
	re.Zero(physical)
	re.Zero(logical)
}

type idAllocator struct {
	allocator *mockid.IDAllocator
}

func (i *idAllocator) alloc() uint64 {
	// This error will be always nil.
	id, _, _ := i.allocator.Alloc(1)
	return id
}

var (
	regionIDAllocator = &idAllocator{allocator: &mockid.IDAllocator{}}
	// Note: IDs below are entirely arbitrary. They are only for checking
	// whether GetRegion/GetStore works.
	// If we alloc ID in client in the future, these IDs must be updated.
	stores = []*metapb.Store{
		{Id: 1,
			Address: "mock://tikv-1:1",
		},
		{Id: 2,
			Address: "mock://tikv-2:2",
		},
		{Id: 3,
			Address: "mock://tikv-3:3",
		},
		{Id: 4,
			Address: "mock://tikv-4:4",
		},
	}

	peers = []*metapb.Peer{
		{Id: regionIDAllocator.alloc(),
			StoreId: stores[0].GetId(),
		},
		{Id: regionIDAllocator.alloc(),
			StoreId: stores[1].GetId(),
		},
		{Id: regionIDAllocator.alloc(),
			StoreId: stores[2].GetId(),
		},
	}
)

type clientTestSuiteImpl struct {
	suite.Suite
	cleanup         testutil.CleanupFunc
	ctx             context.Context
	clean           context.CancelFunc
	srv             *server.Server
	grpcSvr         *server.GrpcServer
	client          pd.Client
	grpcPDClient    pdpb.PDClient
	regionHeartbeat pdpb.PD_RegionHeartbeatClient
	reportBucket    pdpb.PD_ReportBucketsClient
}

func (suite *clientTestSuiteImpl) setup() {
	var err error
	re := suite.Require()
	suite.srv, suite.cleanup, err = tests.NewServer(re, assertutil.CheckerWithNilAssert(re))
	re.NoError(err)
	suite.grpcPDClient = testutil.MustNewGrpcClient(re, suite.srv.GetAddr())
	suite.grpcSvr = &server.GrpcServer{Server: suite.srv}

	tests.MustWaitLeader(re, []*server.Server{suite.srv})
	bootstrapServer(re, newHeader(), suite.grpcPDClient)

	suite.ctx, suite.clean = context.WithCancel(context.Background())
	suite.client = setupCli(suite.ctx, re, suite.srv.GetEndpoints())

	suite.regionHeartbeat, err = suite.grpcPDClient.RegionHeartbeat(suite.ctx)
	re.NoError(err)
	suite.reportBucket, err = suite.grpcPDClient.ReportBuckets(suite.ctx)
	re.NoError(err)
	cluster := suite.srv.GetRaftCluster()
	re.NotNil(cluster)
	now := time.Now().UnixNano()
	for _, store := range stores {
		_, err = suite.grpcSvr.PutStore(context.Background(), &pdpb.PutStoreRequest{
			Header: newHeader(),
			Store: &metapb.Store{
				Id:            store.Id,
				Address:       store.Address,
				LastHeartbeat: now,
			},
		})
		re.NoError(err)

		_, err = suite.grpcSvr.StoreHeartbeat(context.Background(), &pdpb.StoreHeartbeatRequest{
			Header: newHeader(),
			Stats: &pdpb.StoreStats{
				StoreId:   store.GetId(),
				Capacity:  uint64(10 * units.GiB),
				UsedSize:  uint64(9 * units.GiB),
				Available: uint64(1 * units.GiB),
			},
		})
		re.NoError(err)
	}
	cluster.GetOpts().(*config.PersistOptions).SetRegionBucketEnabled(true)
}

func (suite *clientTestSuiteImpl) tearDown() {
	suite.client.Close()
	suite.clean()
	suite.cleanup()
}
func newHeader() *pdpb.RequestHeader {
	return &pdpb.RequestHeader{
		ClusterId: keypath.ClusterID(),
	}
}

func bootstrapServer(re *require.Assertions, header *pdpb.RequestHeader, client pdpb.PDClient) {
	regionID := regionIDAllocator.alloc()
	region := &metapb.Region{
		Id: regionID,
		RegionEpoch: &metapb.RegionEpoch{
			ConfVer: 1,
			Version: 1,
		},
		Peers: peers[:1],
	}
	req := &pdpb.BootstrapRequest{
		Header: header,
		Store:  stores[0],
		Region: region,
	}
	resp, err := client.Bootstrap(context.Background(), req)
	re.NoError(err)
	re.Equal(pdpb.ErrorType_OK, resp.GetHeader().GetError().GetType())
}

type clientStatelessTestSuite struct {
	clientTestSuiteImpl
}

func TestClientStatelessTestSuite(t *testing.T) {
	suite.Run(t, new(clientStatelessTestSuite))
}

func (suite *clientStatelessTestSuite) SetupSuite() {
	suite.setup()
}

func (suite *clientStatelessTestSuite) TearDownSuite() {
	suite.tearDown()
}

func (suite *clientStatelessTestSuite) SetupTest() {
	suite.grpcSvr.DirectlyGetRaftCluster().ResetRegionCache()
}

func (suite *clientStatelessTestSuite) TestScanRegions() {
	re := suite.Require()
	regionLen := 10
	regions := make([]*metapb.Region, 0, regionLen)
	for i := range regionLen {
		regionID := regionIDAllocator.alloc()
		r := &metapb.Region{
			Id: regionID,
			RegionEpoch: &metapb.RegionEpoch{
				ConfVer: 1,
				Version: 1,
			},
			StartKey: []byte{byte(i)},
			EndKey:   []byte{byte(i + 1)},
			Peers:    peers,
		}
		regions = append(regions, r)
		req := &pdpb.RegionHeartbeatRequest{
			Header: newHeader(),
			Region: r,
			Leader: peers[0],
		}
		err := suite.regionHeartbeat.Send(req)
		re.NoError(err)
	}

	// Wait for region heartbeats.
	testutil.Eventually(re, func() bool {
		scanRegions, err := suite.client.BatchScanRegions(context.Background(), []router.KeyRange{{StartKey: []byte{0}, EndKey: nil}}, 10)
		return err == nil && len(scanRegions) == 10
	})

	// Set leader of region3 to nil.
	region3 := core.NewRegionInfo(regions[3], nil)
	err := suite.srv.GetRaftCluster().HandleRegionHeartbeat(region3)
	re.NoError(err)

	// Add down peer for region4.
	region4 := core.NewRegionInfo(regions[4], regions[4].Peers[0], core.WithDownPeers([]*pdpb.PeerStats{{Peer: regions[4].Peers[1]}}))
	err = suite.srv.GetRaftCluster().HandleRegionHeartbeat(region4)
	re.NoError(err)

	// Add pending peers for region5.
	region5 := core.NewRegionInfo(regions[5], regions[5].Peers[0], core.WithPendingPeers([]*metapb.Peer{regions[5].Peers[1], regions[5].Peers[2]}))
	err = suite.srv.GetRaftCluster().HandleRegionHeartbeat(region5)
	re.NoError(err)

	t := suite.T()
	check := func(start, end []byte, limit int, expect []*metapb.Region) {
		scanRegions, err := suite.client.BatchScanRegions(context.Background(), []router.KeyRange{{StartKey: start, EndKey: end}}, limit)
		re.NoError(err)
		re.Len(scanRegions, len(expect))
		t.Log("scanRegions", scanRegions)
		t.Log("expect", expect)
		for i := range expect {
			re.Equal(expect[i], scanRegions[i].Meta)

			if scanRegions[i].Meta.GetId() == region3.GetID() {
				re.Equal(&metapb.Peer{}, scanRegions[i].Leader)
			} else {
				re.Equal(expect[i].Peers[0], scanRegions[i].Leader)
			}

			if scanRegions[i].Meta.GetId() == region4.GetID() {
				re.Equal([]*metapb.Peer{expect[i].Peers[1]}, scanRegions[i].DownPeers)
			}

			if scanRegions[i].Meta.GetId() == region5.GetID() {
				re.Equal([]*metapb.Peer{expect[i].Peers[1], expect[i].Peers[2]}, scanRegions[i].PendingPeers)
			}
		}
	}

	check([]byte{0}, nil, 10, regions)
	check([]byte{1}, nil, 5, regions[1:6])
	check([]byte{100}, nil, 1, nil)
	check([]byte{1}, []byte{6}, 0, regions[1:6])
	check([]byte{1}, []byte{6}, 2, regions[1:3])
}

func (suite *clientStatelessTestSuite) TestGetStore() {
	re := suite.Require()
	cluster := suite.srv.GetRaftCluster()
	re.NotNil(cluster)
	store := stores[0]

	// Get an up store should be OK.
	n, err := suite.client.GetStore(context.Background(), store.GetId())
	re.NoError(err)
	store.LastHeartbeat = n.LastHeartbeat
	re.Equal(store, n)

	actualStores, err := suite.client.GetAllStores(context.Background())
	re.NoError(err)
	re.Len(actualStores, len(stores))
	stores = actualStores

	// Mark the store as offline.
	err = cluster.RemoveStore(store.GetId(), false)
	re.NoError(err)
	offlineStore := typeutil.DeepClone(store, core.StoreFactory)
	offlineStore.State = metapb.StoreState_Offline
	offlineStore.NodeState = metapb.NodeState_Removing

	// Get an offline store should be OK.
	n, err = suite.client.GetStore(context.Background(), store.GetId())
	re.NoError(err)
	re.Equal(offlineStore, n)

	// Should return offline stores.
	contains := false
	stores, err = suite.client.GetAllStores(context.Background())
	re.NoError(err)
	for _, store := range stores {
		if store.GetId() == offlineStore.GetId() {
			contains = true
			re.Equal(offlineStore, store)
		}
	}
	re.True(contains)

	// Mark the store as physically destroyed and offline.
	err = cluster.RemoveStore(store.GetId(), true)
	re.NoError(err)
	physicallyDestroyedStoreID := store.GetId()

	// Get a physically destroyed and offline store
	// It should be Tombstone(become Tombstone automatically) or Offline
	n, err = suite.client.GetStore(context.Background(), physicallyDestroyedStoreID)
	re.NoError(err)
	if n != nil { // store is still offline and physically destroyed
		re.Equal(metapb.NodeState_Removing, n.GetNodeState())
		re.True(n.PhysicallyDestroyed)
	}
	// Should return tombstone stores.
	contains = false
	stores, err = suite.client.GetAllStores(context.Background())
	re.NoError(err)
	for _, store := range stores {
		if store.GetId() == physicallyDestroyedStoreID {
			contains = true
			re.NotEqual(metapb.StoreState_Up, store.GetState())
			re.True(store.PhysicallyDestroyed)
		}
	}
	re.True(contains)

	// Should not return tombstone stores.
	stores, err = suite.client.GetAllStores(context.Background(), opt.WithExcludeTombstone())
	re.NoError(err)
	for _, store := range stores {
		if store.GetId() == physicallyDestroyedStoreID {
			re.Equal(metapb.StoreState_Offline, store.GetState())
			re.True(store.PhysicallyDestroyed)
		}
	}
}

func (suite *clientStatelessTestSuite) TestScatterRegion() {
	re := suite.Require()
	regionID := regionIDAllocator.alloc()
	testutil.Eventually(re, func() bool {
		err := suite.regionHeartbeat.Send(&pdpb.RegionHeartbeatRequest{
			Header: newHeader(),
			Region: &metapb.Region{
				Id: regionID,
				RegionEpoch: &metapb.RegionEpoch{
					ConfVer: 1,
					Version: 1,
				},
				Peers:    peers,
				StartKey: []byte("fff"),
				EndKey:   []byte("ggg"),
			},
			Leader: peers[0],
		})
		if err != nil {
			return false
		}
		scatterResp, err := suite.client.ScatterRegions(context.Background(), []uint64{regionID}, opt.WithGroup("test"), opt.WithRetry(1))
		if err != nil {
			return false
		}
		if scatterResp.FinishedPercentage != uint64(100) {
			re.Contains(scatterResp.FailedRegionsId, regionID)
			return false
		}
		re.Empty(scatterResp.FailedRegionsId)
		resp, err := suite.client.GetOperator(context.Background(), regionID)
		if err != nil {
			return false
		}
		if resp.GetRegionId() != regionID || string(resp.GetDesc()) != "scatter-region" {
			return false
		}
		return resp.GetStatus() == pdpb.OperatorStatus_RUNNING || resp.GetStatus() == pdpb.OperatorStatus_SUCCESS
	})

	// Test interface `ScatterRegion`.
	// TODO: Deprecate interface `ScatterRegion`.
	// create a new region as scatter operation from previous test might be running
	regionID = regionIDAllocator.alloc()
	testutil.Eventually(re, func() bool {
		err := suite.regionHeartbeat.Send(&pdpb.RegionHeartbeatRequest{
			Header: newHeader(),
			Region: &metapb.Region{
				Id: regionID,
				RegionEpoch: &metapb.RegionEpoch{
					ConfVer: 1,
					Version: 1,
				},
				Peers:    peers,
				StartKey: []byte("ggg"),
				EndKey:   []byte("hhh"),
			},
			Leader: peers[0],
		})
		if err != nil {
			return false
		}
		err = suite.client.ScatterRegion(context.Background(), regionID)
		if err != nil {
			return false
		}
		resp, err := suite.client.GetOperator(context.Background(), regionID)
		if err != nil {
			return false
		}
		if resp.GetRegionId() != regionID || string(resp.GetDesc()) != "scatter-region" {
			return false
		}
		return resp.GetStatus() == pdpb.OperatorStatus_RUNNING || resp.GetStatus() == pdpb.OperatorStatus_SUCCESS
	})
}

func TestWatch(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestCluster(ctx, 1)
	re.NoError(err)
	defer cluster.Destroy()
	endpoints := runServer(re, cluster)
	client := setupCli(ctx, re, endpoints)
	defer client.Close()

	key := "test"
	resp, err := client.Get(ctx, []byte(key))
	re.NoError(err)
	rev := resp.GetHeader().GetRevision()
	ch, err := client.Watch(ctx, []byte(key), opt.WithRev(rev))
	re.NoError(err)
	exit := make(chan struct{})
	go func() {
		var events []*meta_storagepb.Event
		for e := range ch {
			events = append(events, e...)
			if len(events) >= 3 {
				break
			}
		}
		re.Equal(meta_storagepb.Event_PUT, events[0].GetType())
		re.Equal("1", string(events[0].GetKv().GetValue()))
		re.Equal(meta_storagepb.Event_PUT, events[1].GetType())
		re.Equal("2", string(events[1].GetKv().GetValue()))
		re.Equal(meta_storagepb.Event_DELETE, events[2].GetType())
		exit <- struct{}{}
	}()

	cli, err := clientv3.NewFromURLs(endpoints)
	re.NoError(err)
	defer cli.Close()
	_, err = cli.Put(context.Background(), key, "1")
	re.NoError(err)
	_, err = cli.Put(context.Background(), key, "2")
	re.NoError(err)
	_, err = cli.Delete(context.Background(), key)
	re.NoError(err)
	<-exit
}

func TestPutGet(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestCluster(ctx, 1)
	re.NoError(err)
	defer cluster.Destroy()
	endpoints := runServer(re, cluster)
	client := setupCli(ctx, re, endpoints)
	defer client.Close()

	key := []byte("test")
	putResp, err := client.Put(context.Background(), key, []byte("1"))
	re.NoError(err)
	re.Empty(putResp.GetPrevKv())
	getResp, err := client.Get(context.Background(), key)
	re.NoError(err)
	re.Equal([]byte("1"), getResp.GetKvs()[0].Value)
	re.NotEqual(0, getResp.GetHeader().GetRevision())
	putResp, err = client.Put(context.Background(), key, []byte("2"), opt.WithPrevKV())
	re.NoError(err)
	re.Equal([]byte("1"), putResp.GetPrevKv().Value)
	getResp, err = client.Get(context.Background(), key)
	re.NoError(err)
	re.Equal([]byte("2"), getResp.GetKvs()[0].Value)
	s := cluster.GetLeaderServer()
	// use etcd client delete the key
	_, err = s.GetEtcdClient().Delete(context.Background(), string(key))
	re.NoError(err)
	getResp, err = client.Get(context.Background(), key)
	re.NoError(err)
	re.Empty(getResp.GetKvs())
}

// TestClientWatchWithRevision is the same as TestClientWatchWithRevision in global config.
func TestClientWatchWithRevision(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestCluster(ctx, 1)
	re.NoError(err)
	defer cluster.Destroy()
	endpoints := runServer(re, cluster)
	client := setupCli(ctx, re, endpoints)
	defer client.Close()
	s := cluster.GetLeaderServer()
	watchPrefix := "watch_test"
	defer func() {
		_, err := s.GetEtcdClient().Delete(context.Background(), watchPrefix+"test")
		re.NoError(err)

		for i := 3; i < 9; i++ {
			_, err := s.GetEtcdClient().Delete(context.Background(), watchPrefix+strconv.Itoa(i))
			re.NoError(err)
		}
	}()
	// Mock get revision by loading
	r, err := s.GetEtcdClient().Put(context.Background(), watchPrefix+"test", "test")
	re.NoError(err)
	res, err := client.Get(context.Background(), []byte(watchPrefix), opt.WithPrefix())
	re.NoError(err)
	re.Len(res.Kvs, 1)
	re.LessOrEqual(r.Header.GetRevision(), res.GetHeader().GetRevision())
	// Mock when start watcher there are existed some keys, will load firstly

	for i := range 6 {
		_, err = s.GetEtcdClient().Put(context.Background(), watchPrefix+strconv.Itoa(i), strconv.Itoa(i))
		re.NoError(err)
	}
	// Start watcher at next revision
	ch, err := client.Watch(context.Background(), []byte(watchPrefix), opt.WithRev(res.GetHeader().GetRevision()), opt.WithPrefix(), opt.WithPrevKV())
	re.NoError(err)
	// Mock delete
	for i := range 3 {
		_, err = s.GetEtcdClient().Delete(context.Background(), watchPrefix+strconv.Itoa(i))
		re.NoError(err)
	}
	// Mock put
	for i := 6; i < 9; i++ {
		_, err = s.GetEtcdClient().Put(context.Background(), watchPrefix+strconv.Itoa(i), strconv.Itoa(i))
		re.NoError(err)
	}
	var watchCount int
	for {
		select {
		case <-time.After(1 * time.Second):
			re.Equal(13, watchCount)
			return
		case res := <-ch:
			for _, r := range res {
				watchCount++
				if r.GetType() == meta_storagepb.Event_DELETE {
					re.Equal(watchPrefix+string(r.PrevKv.Value), string(r.Kv.Key))
				} else {
					re.Equal(watchPrefix+string(r.Kv.Value), string(r.Kv.Key))
				}
			}
		}
	}
}

func (suite *clientStatelessTestSuite) TestMemberUpdateBackOff() {
	re := suite.Require()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := tests.NewTestCluster(ctx, 3)
	re.NoError(err)
	defer cluster.Destroy()

	endpoints := runServer(re, cluster)
	cli := setupCli(ctx, re, endpoints)
	defer cli.Close()
	innerCli, ok := cli.(interface{ GetServiceDiscovery() sd.ServiceDiscovery })
	re.True(ok)

	leader := cluster.GetLeader()
	waitLeader(re, innerCli.GetServiceDiscovery(), cluster.GetServer(leader))
	memberID := cluster.GetServer(leader).GetLeader().GetMemberId()

	re.NoError(failpoint.Enable("github.com/tikv/pd/server/leaderLoopCheckAgain", fmt.Sprintf("return(\"%d\")", memberID)))
	re.NoError(failpoint.Enable("github.com/tikv/pd/server/exitCampaignLeader", fmt.Sprintf("return(\"%d\")", memberID)))
	re.NoError(failpoint.Enable("github.com/tikv/pd/server/timeoutWaitPDLeader", `return(true)`))
	// make sure back off executed.
	re.NoError(failpoint.Enable("github.com/tikv/pd/client/pkg/retry/backOffExecute", `return(true)`))
	leader2 := waitLeaderChange(re, cluster, leader, innerCli.GetServiceDiscovery())
	re.True(retry.TestBackOffExecute())

	re.NotEqual(leader, leader2)

	re.NoError(failpoint.Disable("github.com/tikv/pd/server/leaderLoopCheckAgain"))
	re.NoError(failpoint.Disable("github.com/tikv/pd/server/exitCampaignLeader"))
	re.NoError(failpoint.Disable("github.com/tikv/pd/server/timeoutWaitPDLeader"))
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/pkg/retry/backOffExecute"))
}

func waitLeaderChange(re *require.Assertions, cluster *tests.TestCluster, old string, cli sd.ServiceDiscovery) string {
	var leader string
	testutil.Eventually(re, func() bool {
		cli.ScheduleCheckMemberChanged()
		leader = cluster.GetLeader()
		if leader == old || leader == "" {
			return false
		}
		return true
	})
	return leader
}

func (suite *clientStatelessTestSuite) TestBatchScanRegions() {
	var (
		re        = suite.Require()
		ctx       = context.Background()
		regionLen = 10
		regions   = make([]*metapb.Region, 0, regionLen)
	)

	for i := range regionLen {
		regionID := regionIDAllocator.alloc()
		r := &metapb.Region{
			Id: regionID,
			RegionEpoch: &metapb.RegionEpoch{
				ConfVer: 1,
				Version: 1,
			},
			StartKey: []byte{byte(i)},
			EndKey:   []byte{byte(i + 1)},
			Peers:    peers,
		}
		regions = append(regions, r)
		req := &pdpb.RegionHeartbeatRequest{
			Header: newHeader(),
			Region: r,
			Leader: peers[0],
		}
		err := suite.regionHeartbeat.Send(req)
		re.NoError(err)
	}

	// Wait for region heartbeats.
	testutil.Eventually(re, func() bool {
		scanRegions, err := suite.client.BatchScanRegions(ctx, []router.KeyRange{{StartKey: []byte{0}, EndKey: nil}}, 10)
		return err == nil && len(scanRegions) == 10
	})

	// Set leader of region3 to nil.
	region3 := core.NewRegionInfo(regions[3], nil)
	err := suite.srv.GetRaftCluster().HandleRegionHeartbeat(region3)
	re.NoError(err)

	// Add down peer for region4.
	region4 := core.NewRegionInfo(regions[4], regions[4].Peers[0], core.WithDownPeers([]*pdpb.PeerStats{{Peer: regions[4].Peers[1]}}))
	err = suite.srv.GetRaftCluster().HandleRegionHeartbeat(region4)
	re.NoError(err)

	// Add pending peers for region5.
	region5 := core.NewRegionInfo(regions[5], regions[5].Peers[0], core.WithPendingPeers([]*metapb.Peer{regions[5].Peers[1], regions[5].Peers[2]}))
	err = suite.srv.GetRaftCluster().HandleRegionHeartbeat(region5)
	re.NoError(err)

	// Add buckets for region6.
	region6 := core.NewRegionInfo(regions[6], regions[6].Peers[0], core.SetBuckets(&metapb.Buckets{RegionId: regions[6].Id, Version: 2}))
	err = suite.srv.GetRaftCluster().HandleRegionHeartbeat(region6)
	re.NoError(err)

	t := suite.T()
	var outputMustContainAllKeyRangeOptions []bool
	check := func(ranges []router.KeyRange, limit int, expect []*metapb.Region) {
		for _, bucket := range []bool{false, true} {
			for _, outputMustContainAllKeyRange := range outputMustContainAllKeyRangeOptions {
				var opts []opt.GetRegionOption
				if bucket {
					opts = append(opts, opt.WithBuckets())
				}
				if outputMustContainAllKeyRange {
					opts = append(opts, opt.WithOutputMustContainAllKeyRange())
				}
				scanRegions, err := suite.client.BatchScanRegions(ctx, ranges, limit, opts...)
				re.NoError(err)
				t.Log("scanRegions", scanRegions)
				t.Log("expect", expect)
				re.Len(scanRegions, len(expect))
				for i := range expect {
					re.Equal(expect[i], scanRegions[i].Meta)

					if scanRegions[i].Meta.GetId() == region3.GetID() {
						re.Equal(&metapb.Peer{}, scanRegions[i].Leader)
					} else {
						re.Equal(expect[i].Peers[0], scanRegions[i].Leader)
					}

					if scanRegions[i].Meta.GetId() == region4.GetID() {
						re.Equal([]*metapb.Peer{expect[i].Peers[1]}, scanRegions[i].DownPeers)
					}

					if scanRegions[i].Meta.GetId() == region5.GetID() {
						re.Equal([]*metapb.Peer{expect[i].Peers[1], expect[i].Peers[2]}, scanRegions[i].PendingPeers)
					}

					if scanRegions[i].Meta.GetId() == region6.GetID() {
						if !bucket {
							re.Nil(scanRegions[i].Buckets)
						} else {
							re.Equal(scanRegions[i].Buckets, region6.GetBuckets())
						}
					}
				}
			}
		}
	}

	// valid ranges
	outputMustContainAllKeyRangeOptions = []bool{false, true}
	check([]router.KeyRange{{StartKey: []byte{0}, EndKey: nil}}, 10, regions)
	check([]router.KeyRange{{StartKey: []byte{1}, EndKey: nil}}, 5, regions[1:6])
	check([]router.KeyRange{
		{StartKey: []byte{0}, EndKey: []byte{1}},
		{StartKey: []byte{2}, EndKey: []byte{3}},
		{StartKey: []byte{4}, EndKey: []byte{5}},
		{StartKey: []byte{6}, EndKey: []byte{7}},
		{StartKey: []byte{8}, EndKey: []byte{9}},
	}, 10, []*metapb.Region{regions[0], regions[2], regions[4], regions[6], regions[8]})
	check([]router.KeyRange{
		{StartKey: []byte{0}, EndKey: []byte{1}},
		{StartKey: []byte{2}, EndKey: []byte{3}},
		{StartKey: []byte{4}, EndKey: []byte{5}},
		{StartKey: []byte{6}, EndKey: []byte{7}},
		{StartKey: []byte{8}, EndKey: []byte{9}},
	}, 3, []*metapb.Region{regions[0], regions[2], regions[4]})

	outputMustContainAllKeyRangeOptions = []bool{false}
	check([]router.KeyRange{
		{StartKey: []byte{0}, EndKey: []byte{0, 1}}, // non-continuous ranges in a region
		{StartKey: []byte{0, 2}, EndKey: []byte{0, 3}},
		{StartKey: []byte{0, 3}, EndKey: []byte{0, 4}},
		{StartKey: []byte{0, 5}, EndKey: []byte{0, 6}},
		{StartKey: []byte{0, 7}, EndKey: []byte{3}},
		{StartKey: []byte{4}, EndKey: []byte{5}},
	}, 10, []*metapb.Region{regions[0], regions[1], regions[2], regions[4]})
	outputMustContainAllKeyRangeOptions = []bool{false}
	check([]router.KeyRange{
		{StartKey: []byte{9}, EndKey: []byte{10, 1}},
	}, 10, []*metapb.Region{regions[9]})

	// invalid ranges
	_, err = suite.client.BatchScanRegions(
		ctx,
		[]router.KeyRange{{StartKey: []byte{1}, EndKey: []byte{0}}},
		10,
		opt.WithOutputMustContainAllKeyRange(),
	)
	re.ErrorContains(err, "invalid key range, start key > end key")
	_, err = suite.client.BatchScanRegions(ctx, []router.KeyRange{
		{StartKey: []byte{0}, EndKey: []byte{2}},
		{StartKey: []byte{1}, EndKey: []byte{3}},
	}, 10)
	re.ErrorContains(err, "invalid key range, ranges overlapped")
	_, err = suite.client.BatchScanRegions(
		ctx,
		[]router.KeyRange{{StartKey: []byte{9}, EndKey: []byte{10, 1}}},
		10,
		opt.WithOutputMustContainAllKeyRange(),
	)
	re.ErrorContains(err, "found a hole region in the last")
	req := &pdpb.RegionHeartbeatRequest{
		Header: newHeader(),
		Region: &metapb.Region{
			Id: 100,
			RegionEpoch: &metapb.RegionEpoch{
				ConfVer: 1,
				Version: 1,
			},
			StartKey: []byte{100},
			EndKey:   []byte{101},
			Peers:    peers,
		},
		Leader: peers[0],
	}
	re.NoError(suite.regionHeartbeat.Send(req))

	// Wait for region heartbeats.
	testutil.Eventually(re, func() bool {
		_, err = suite.client.BatchScanRegions(
			ctx,
			[]router.KeyRange{{StartKey: []byte{9}, EndKey: []byte{101}}},
			10,
			opt.WithOutputMustContainAllKeyRange(),
		)
		return err != nil && strings.Contains(err.Error(), "found a hole region between")
	})
}

func TestGetRegionWithBackoff(t *testing.T) {
	re := require.New(t)
	re.NoError(failpoint.Enable("github.com/tikv/pd/server/rateLimit", "return(true)"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cluster, err := tests.NewTestCluster(ctx, 1)
	re.NoError(err)
	defer cluster.Destroy()
	endpoints := runServer(re, cluster)

	// Define the backoff parameters
	base := 100 * time.Millisecond
	max := 500 * time.Millisecond
	total := 3 * time.Second

	// Create a backoff strategy
	bo := retry.InitialBackoffer(base, max, total)
	bo.SetRetryableChecker(needRetry, true)

	// Initialize the client with context and backoff
	client, err := pd.NewClientWithContext(ctx, caller.TestComponent, endpoints, pd.SecurityOption{})
	re.NoError(err)
	defer client.Close()

	// Record the start time
	start := time.Now()

	ctx = retry.WithBackoffer(ctx, bo)
	// Call GetRegion and expect it to handle backoff internally
	_, err = client.GetRegion(ctx, []byte("key"))
	re.Error(err)
	// Calculate the elapsed time
	elapsed := time.Since(start)
	// Verify that some backoff occurred by checking if the elapsed time is greater than the base backoff
	re.Greater(elapsed, total, "Expected some backoff to have occurred")

	re.NoError(failpoint.Disable("github.com/tikv/pd/server/rateLimit"))
	// Call GetRegion again and expect it to succeed
	region, err := client.GetRegion(ctx, []byte("key"))
	re.NoError(err)
	re.Equal(uint64(2), region.Meta.Id) // Adjust this based on expected region
}

func needRetry(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	return st.Code() == codes.ResourceExhausted
}

func TestCircuitBreaker(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cluster, err := tests.NewTestCluster(ctx, 1)
	re.NoError(err)
	defer cluster.Destroy()

	circuitBreakerSettings := cb.Settings{
		ErrorRateThresholdPct: 60,
		MinQPSForOpen:         10,
		ErrorRateWindow:       time.Millisecond,
		CoolDownInterval:      time.Second,
		HalfOpenSuccessCount:  1,
	}

	endpoints := runServer(re, cluster)
	cli := setupCli(ctx, re, endpoints)
	defer cli.Close()

	circuitBreaker := cb.NewCircuitBreaker("region_meta", circuitBreakerSettings)
	ctx = cb.WithCircuitBreaker(ctx, circuitBreaker)
	for range 10 {
		region, err := cli.GetRegion(ctx, []byte("a"))
		re.NoError(err)
		re.NotNil(region)
	}

	re.NoError(failpoint.Enable("github.com/tikv/pd/client/pkg/utils/grpcutil/triggerCircuitBreaker", "return(true)"))

	for range 100 {
		_, err := cli.GetRegion(ctx, []byte("a"))
		re.Error(err)
	}

	_, err = cli.GetRegion(ctx, []byte("a"))
	re.Error(err)
	re.Contains(err.Error(), "circuit breaker is open")
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/pkg/utils/grpcutil/triggerCircuitBreaker"))

	_, err = cli.GetRegion(ctx, []byte("a"))
	re.Error(err)
	re.Contains(err.Error(), "circuit breaker is open")

	// wait cooldown
	time.Sleep(time.Second)

	for range 10 {
		region, err := cli.GetRegion(ctx, []byte("a"))
		re.NoError(err)
		re.NotNil(region)
	}
}

func TestCircuitBreakerOpenAndChangeSettings(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cluster, err := tests.NewTestCluster(ctx, 1)
	re.NoError(err)
	defer cluster.Destroy()

	circuitBreakerSettings := cb.Settings{
		ErrorRateThresholdPct: 60,
		MinQPSForOpen:         10,
		ErrorRateWindow:       time.Millisecond,
		CoolDownInterval:      time.Second,
		HalfOpenSuccessCount:  1,
	}

	endpoints := runServer(re, cluster)
	cli := setupCli(ctx, re, endpoints)
	defer cli.Close()

	circuitBreaker := cb.NewCircuitBreaker("region_meta", circuitBreakerSettings)
	ctx = cb.WithCircuitBreaker(ctx, circuitBreaker)
	for range 10 {
		region, err := cli.GetRegion(ctx, []byte("a"))
		re.NoError(err)
		re.NotNil(region)
	}

	re.NoError(failpoint.Enable("github.com/tikv/pd/client/pkg/utils/grpcutil/triggerCircuitBreaker", "return(true)"))

	for range 100 {
		_, err := cli.GetRegion(ctx, []byte("a"))
		re.Error(err)
	}

	_, err = cli.GetRegion(ctx, []byte("a"))
	re.Error(err)
	re.Contains(err.Error(), "circuit breaker is open")

	circuitBreaker.ChangeSettings(func(config *cb.Settings) {
		*config = cb.AlwaysClosedSettings
	})
	_, err = cli.GetRegion(ctx, []byte("a"))
	re.NoError(err)
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/pkg/utils/grpcutil/triggerCircuitBreaker"))
}

func TestCircuitBreakerHalfOpenAndChangeSettings(t *testing.T) {
	re := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cluster, err := tests.NewTestCluster(ctx, 1)
	re.NoError(err)
	defer cluster.Destroy()

	circuitBreakerSettings := cb.Settings{
		ErrorRateThresholdPct: 60,
		MinQPSForOpen:         10,
		ErrorRateWindow:       time.Millisecond,
		CoolDownInterval:      time.Second,
		HalfOpenSuccessCount:  20,
	}

	endpoints := runServer(re, cluster)

	cli := setupCli(ctx, re, endpoints)
	defer cli.Close()

	circuitBreaker := cb.NewCircuitBreaker("region_meta", circuitBreakerSettings)
	ctx = cb.WithCircuitBreaker(ctx, circuitBreaker)
	for range 10 {
		region, err := cli.GetRegion(ctx, []byte("a"))
		re.NoError(err)
		re.NotNil(region)
	}

	re.NoError(failpoint.Enable("github.com/tikv/pd/client/pkg/utils/grpcutil/triggerCircuitBreaker", "return(true)"))

	for range 100 {
		_, err := cli.GetRegion(ctx, []byte("a"))
		re.Error(err)
	}

	_, err = cli.GetRegion(ctx, []byte("a"))
	re.Error(err)
	re.Contains(err.Error(), "circuit breaker is open")

	fname := testutil.InitTempFileLogger("info")
	defer os.RemoveAll(fname)
	// wait for cooldown
	time.Sleep(time.Second)
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/pkg/utils/grpcutil/triggerCircuitBreaker"))
	// trigger circuit breaker state to be half open
	_, err = cli.GetRegion(ctx, []byte("a"))
	re.NoError(err)
	testutil.Eventually(re, func() bool {
		b, err := os.ReadFile(fname)
		re.NoError(err)
		l := string(b)
		// We need to check the log to see if the circuit breaker is half open
		return strings.Contains(l, "Transitioning to half-open state to test the service")
	})

	// The state is half open
	re.NoError(failpoint.Enable("github.com/tikv/pd/client/pkg/utils/grpcutil/triggerCircuitBreaker", "return(true)"))
	// change settings to always closed
	circuitBreaker.ChangeSettings(func(config *cb.Settings) {
		*config = cb.AlwaysClosedSettings
	})
	// It won't be changed to open state.
	for range 100 {
		_, err := cli.GetRegion(ctx, []byte("a"))
		re.NoError(err)
	}
	re.NoError(failpoint.Disable("github.com/tikv/pd/client/pkg/utils/grpcutil/triggerCircuitBreaker"))
}

type clientStatefulTestSuite struct {
	clientTestSuiteImpl
}

func TestClientStatefulTestSuite(t *testing.T) {
	suite.Run(t, new(clientStatefulTestSuite))
}

func (s *clientStatefulTestSuite) SetupTest() {
	s.setup()
}

func (s *clientStatefulTestSuite) TearDownTest() {
	s.tearDown()
}

func (s *clientStatefulTestSuite) checkGCSafePoint(re *require.Assertions, keyspaceID uint32, expectedGCSafePoint uint64) {
	gcState, err := s.client.GetGCStatesClient(keyspaceID).GetGCState(context.Background())
	re.NoError(err)
	re.Equal(expectedGCSafePoint, gcState.GCSafePoint)
}

func (s *clientStatefulTestSuite) checkTxnSafePoint(re *require.Assertions, keyspaceID uint32, expectedTxnSafePoint uint64) {
	gcState, err := s.client.GetGCStatesClient(keyspaceID).GetGCState(context.Background())
	re.NoError(err)
	re.Equal(expectedTxnSafePoint, gcState.TxnSafePoint)
}

func (*clientStatefulTestSuite) waitForGCBarrierExpiring(re *require.Assertions, b *gc.GCBarrierInfo, maxWaitTime time.Duration) {
	waitStartTime := time.Now()
	for {
		if b.IsExpired() {
			return
		}
		if time.Since(waitStartTime) > maxWaitTime {
			re.Failf("GC barrier not expired in expected time", "barrier: %+v, maxWaitTime: %v", *b, maxWaitTime)
		}
		time.Sleep(time.Millisecond * 50)
	}
}

// checkGCBarrier checks whether the specified GC barrier has the specified barrier TS. This function assumes the
// barrier TS is never 0, and passing 0 means asserting the GC barrier does not exist.
func (s *clientStatefulTestSuite) checkGCBarrier(re *require.Assertions, keyspaceID uint32, barrierID string, expectedBarrierTS uint64) {
	gcState, err := s.client.GetGCStatesClient(keyspaceID).GetGCState(context.Background())
	re.NoError(err)
	found := false
	for _, b := range gcState.GCBarriers {
		if b.BarrierID == barrierID {
			if found {
				re.Failf("duplicated barrier ID found in the GC states", "barrierID: %s, GC state: %+v", barrierID, gcState)
				return
			}
			if expectedBarrierTS != 0 {
				re.Equal(expectedBarrierTS, b.BarrierTS)
			} else {
				re.Failf("expected GC barrier not exist but found", "barrierID: %s, GC state: %+v", barrierID, gcState)
				return
			}
			found = true
		}
	}
	if expectedBarrierTS != 0 && !found {
		re.Failf("GC barrier expected to exist but not found", "barrierID: %s, GC state: %+v", barrierID, gcState)
	}
}

func (s *clientStatefulTestSuite) testUpdateGCSafePointImpl(keyspaceID uint32) {
	re := s.Require()
	client := s.client
	if keyspaceID != constants.NullKeyspaceID {
		// UpdateGCSafePoint works according to the keyspace property set in the client.
		client = utils.SetupClientWithKeyspaceID(context.Background(), re, keyspaceID, s.srv.GetEndpoints())
		defer client.Close()
	}

	s.checkGCSafePoint(re, keyspaceID, 0)
	for _, gcSafePoint := range []uint64{0, 1, 2, 3, 233, 23333, 233333333333, math.MaxUint64} {
		// Now GC safe point is not allowed to be advanced before advancing the txn safe point. Advance txn safe point
		// first.
		_, err := s.client.GetGCInternalController(keyspaceID).AdvanceTxnSafePoint(context.Background(), gcSafePoint)
		re.NoError(err)
		newSafePoint, err := client.UpdateGCSafePoint(context.Background(), gcSafePoint) //nolint:staticcheck
		re.NoError(err)
		re.Equal(gcSafePoint, newSafePoint)
		s.checkGCSafePoint(re, keyspaceID, gcSafePoint)
	}
	// If the new safe point is less than the old one, it should not be updated.
	newSafePoint, err := client.UpdateGCSafePoint(context.Background(), 1) //nolint:staticcheck
	re.Equal(uint64(math.MaxUint64), newSafePoint)
	re.NoError(err)
	s.checkGCSafePoint(re, keyspaceID, math.MaxUint64)
}

func (s *clientStatefulTestSuite) TestUpdateGCSafePoint() {
	s.prepareKeyspacesForGCTest()
	for _, keyspaceID := range []uint32{constants.NullKeyspaceID, 1, 2} {
		s.testUpdateGCSafePointImpl(keyspaceID)
	}
}

func (s *clientStatefulTestSuite) testUpdateServiceGCSafePointImpl(keyspaceID uint32) {
	re := s.Require()

	client := s.client
	if keyspaceID != constants.NullKeyspaceID {
		// UpdateServiceGCSafePoint works according to the keyspace property set in the client.
		client = utils.SetupClientWithKeyspaceID(context.Background(), re, keyspaceID, s.srv.GetEndpoints())
		defer client.Close()
	}

	loadMinServiceGCSafePoint := func() *endpoint.ServiceSafePoint {
		res, _, err := s.srv.GetGCStateManager().CompatibleUpdateServiceGCSafePoint(keyspaceID, "_", 0, 0, time.Now())
		re.NoError(err)
		return res
	}

	// Suppress the unuseful lint warning.
	//nolint:unparam
	loadServiceGCSafePointByServiceID := func(serviceID string) *endpoint.ServiceSafePoint {
		gcStates, err := s.srv.GetGCStateManager().GetGCState(keyspaceID)
		re.NoError(err)
		for _, b := range gcStates.GCBarriers {
			if b.BarrierID == serviceID {
				return b.ToServiceSafePoint(keyspaceID)
			}
		}
		return nil
	}

	// Wrap in a function to avoid writing the nolint directive everywhere.
	updateServiceGCSafePoint := func(serviceID string, ttl int64, safePoint uint64) (uint64, error) {
		return client.UpdateServiceGCSafePoint(context.Background(), serviceID, ttl, safePoint) //nolint:staticcheck
	}

	serviceSafePoints := []struct {
		ServiceID string
		TTL       int64
		SafePoint uint64
	}{
		{"b", 1000, 2},
		{"a", 1000, 1},
		{"c", 1000, 3},
	}
	for _, ssp := range serviceSafePoints {
		min, err := updateServiceGCSafePoint(ssp.ServiceID, 1000, ssp.SafePoint)
		re.NoError(err)
		// An service safepoint of ID "gc_worker" is automatically initialized as 0
		re.Equal(uint64(0), min)
	}

	min, err := updateServiceGCSafePoint("gc_worker", math.MaxInt64, 10)
	re.NoError(err)
	re.Equal(uint64(1), min)

	// Note that as the service safe points became a compatibility layer over the GC barriers and the txn safe point,
	// the (simulated) service safe point of "gc_worker" is no longer able to be advanced over the minimal existing
	// GC barrier.

	min, err = updateServiceGCSafePoint("a", 1000, 4)
	re.NoError(err)
	re.Equal(uint64(1), min)
	min, err = updateServiceGCSafePoint("gc_worker", math.MaxInt64, 10)
	re.NoError(err)
	re.Equal(uint64(2), min)

	min, err = updateServiceGCSafePoint("b", -100, 2)
	re.NoError(err)
	re.Equal(uint64(2), min)
	min, err = updateServiceGCSafePoint("gc_worker", math.MaxInt64, 10)
	re.NoError(err)
	re.Equal(uint64(3), min)

	// Minimum safepoint does not regress
	min, err = updateServiceGCSafePoint("b", 1000, 2)
	re.NoError(err)
	re.Equal(uint64(3), min)

	// Update only the TTL of the service safe point "c"
	oldMinSsp := loadServiceGCSafePointByServiceID("c")
	re.Equal("c", oldMinSsp.ServiceID)
	re.Equal(uint64(3), oldMinSsp.SafePoint)
	min, err = updateServiceGCSafePoint("c", 2000, 3)
	re.NoError(err)
	re.Equal(uint64(3), min)
	minSsp := loadServiceGCSafePointByServiceID("c")
	re.Equal("c", minSsp.ServiceID)
	re.Equal(uint64(3), minSsp.SafePoint)
	s.GreaterOrEqual(minSsp.ExpiredAt-oldMinSsp.ExpiredAt, int64(1000))

	// Shrinking TTL is also allowed
	min, err = updateServiceGCSafePoint("c", 1, 3)
	re.NoError(err)
	re.Equal(uint64(3), min)
	minSsp = loadServiceGCSafePointByServiceID("c")
	re.NoError(err)
	re.Equal("c", minSsp.ServiceID)
	re.Less(minSsp.ExpiredAt, oldMinSsp.ExpiredAt)

	// TTL can be infinite (represented by math.MaxInt64)
	min, err = updateServiceGCSafePoint("c", math.MaxInt64, 3)
	re.NoError(err)
	re.Equal(uint64(3), min)
	minSsp = loadServiceGCSafePointByServiceID("c")
	re.NoError(err)
	re.Equal("c", minSsp.ServiceID)
	re.Equal(minSsp.ExpiredAt, int64(math.MaxInt64))

	// Delete "a" and "c"
	_, err = updateServiceGCSafePoint("c", -1, 3)
	re.NoError(err)
	_, err = updateServiceGCSafePoint("a", -1, 4)
	re.NoError(err)
	// Now the service safe point of gc_worker can be advanced as other service safe points are all deleted.
	min, err = updateServiceGCSafePoint("gc_worker", math.MaxInt64, 10)
	re.NoError(err)
	re.Equal(uint64(10), min)

	// gc_worker cannot be deleted.
	_, err = updateServiceGCSafePoint("gc_worker", -1, 10)
	re.Error(err)

	// Cannot set non-infinity TTL for gc_worker
	_, err = updateServiceGCSafePoint("gc_worker", 10000000, 10)
	re.Error(err)

	// Service safepoint must have a non-empty ID
	_, err = updateServiceGCSafePoint("", 1000, 15)
	re.Error(err)

	// Put some other safepoints to test fixing gc_worker's safepoint when there exists other safepoints.
	_, err = updateServiceGCSafePoint("a", 1000, 11)
	re.NoError(err)
	_, err = updateServiceGCSafePoint("b", 1000, 12)
	re.NoError(err)
	_, err = updateServiceGCSafePoint("c", 1000, 13)
	re.NoError(err)

	// Force set invalid ttl to gc_worker
	gcWorkerKey := keypath.ServiceGCSafePointPath("gc_worker")
	{
		gcWorkerSsp := &endpoint.ServiceSafePoint{
			ServiceID:  "gc_worker",
			ExpiredAt:  -12345,
			SafePoint:  10,
			KeyspaceID: keyspaceID,
		}
		value, err := json.Marshal(gcWorkerSsp)
		re.NoError(err)
		err = s.srv.GetStorage().Save(gcWorkerKey, string(value))
		re.NoError(err)
	}

	minSsp = loadMinServiceGCSafePoint()
	re.NoError(err)
	re.Equal("gc_worker", minSsp.ServiceID)
	re.Equal(uint64(10), minSsp.SafePoint)
	re.Equal(int64(math.MaxInt64), minSsp.ExpiredAt)

	// Advancing txn safe point also affects the gc_worker's service safe point.
	_, err = client.GetGCInternalController(keyspaceID).AdvanceTxnSafePoint(context.Background(), 11)
	re.NoError(err)
	minSsp = loadMinServiceGCSafePoint()
	re.NoError(err)
	re.Equal(uint64(11), minSsp.SafePoint)
}

func (s *clientStatefulTestSuite) TestUpdateServiceGCSafePoint() {
	s.prepareKeyspacesForGCTest()

	for _, keyspaceID := range []uint32{constants.NullKeyspaceID, 1, 2} {
		s.testUpdateServiceGCSafePointImpl(keyspaceID)
	}
}

func (s *clientStatefulTestSuite) prepareKeyspacesForGCTest() {
	re := s.Require()
	ks1, err := s.srv.GetKeyspaceManager().CreateKeyspace(&keyspace.CreateKeyspaceRequest{
		Name:       "ks1",
		Config:     map[string]string{keyspace.GCManagementType: keyspace.KeyspaceLevelGC},
		CreateTime: time.Now().Unix(),
	})
	re.NoError(err)
	re.Equal(uint32(1), ks1.Id)

	ks2, err := s.srv.GetKeyspaceManager().CreateKeyspace(&keyspace.CreateKeyspaceRequest{
		Name:       "ks2",
		Config:     map[string]string{keyspace.GCManagementType: keyspace.KeyspaceLevelGC},
		CreateTime: time.Now().Unix(),
	})
	re.NoError(err)
	re.Equal(uint32(2), ks2.Id)
}

func (s *clientStatefulTestSuite) TestAdvanceTxnSafePointBasic() {
	s.prepareKeyspacesForGCTest()
	re := s.Require()
	ctx := context.Background()

	for _, keyspaceID := range []uint32{constants.NullKeyspaceID, 1, 2} {
		s.checkTxnSafePoint(re, keyspaceID, 0)
		c := s.client.GetGCInternalController(keyspaceID)

		res, err := c.AdvanceTxnSafePoint(ctx, 0)
		re.NoError(err)
		re.Equal(uint64(0), res.OldTxnSafePoint)
		re.Equal(uint64(0), res.Target)
		re.Equal(uint64(0), res.NewTxnSafePoint)
		re.Empty(res.BlockerDescription)
		s.checkTxnSafePoint(re, keyspaceID, 0)

		res, err = c.AdvanceTxnSafePoint(ctx, 10)
		re.NoError(err)
		re.Equal(uint64(0), res.OldTxnSafePoint)
		re.Equal(uint64(10), res.Target)
		re.Equal(uint64(10), res.NewTxnSafePoint)
		re.Empty(res.BlockerDescription)
		s.checkTxnSafePoint(re, keyspaceID, 10)

		// Disallow decreasing.
		_, err = c.AdvanceTxnSafePoint(ctx, 9)
		re.Error(err)
		s.checkTxnSafePoint(re, keyspaceID, 10)
		re.Contains(err.Error(), "ErrDecreasingTxnSafePoint")

		// Allow remaining the same value.
		res, err = c.AdvanceTxnSafePoint(ctx, 10)
		re.NoError(err)
		re.Equal(uint64(10), res.OldTxnSafePoint)
		re.Equal(uint64(10), res.Target)
		re.Equal(uint64(10), res.NewTxnSafePoint)
		re.Empty(res.BlockerDescription)
		s.checkTxnSafePoint(re, keyspaceID, 10)
	}
}

func (s *clientStatefulTestSuite) TestAdvanceGCSafePoint() {
	s.prepareKeyspacesForGCTest()
	re := s.Require()
	ctx := context.Background()

	for _, keyspaceID := range []uint32{constants.NullKeyspaceID, 1, 2} {
		s.checkGCSafePoint(re, keyspaceID, 0)
		c := s.client.GetGCInternalController(keyspaceID)

		res, err := c.AdvanceGCSafePoint(ctx, 0)
		re.NoError(err)
		re.Equal(uint64(0), res.OldGCSafePoint)
		re.Equal(uint64(0), res.Target)
		re.Equal(uint64(0), res.NewGCSafePoint)
		s.checkGCSafePoint(re, keyspaceID, 0)

		_, err = c.AdvanceTxnSafePoint(ctx, 10)
		re.NoError(err)
		s.checkTxnSafePoint(re, keyspaceID, 10)

		// Allows advancing to a value below txn safe point.
		res, err = c.AdvanceGCSafePoint(ctx, 5)
		re.NoError(err)
		re.Equal(uint64(0), res.OldGCSafePoint)
		re.Equal(uint64(5), res.Target)
		re.Equal(uint64(5), res.NewGCSafePoint)
		s.checkGCSafePoint(re, keyspaceID, 5)

		// Disallows going backward.
		res, err = c.AdvanceGCSafePoint(ctx, 4)
		re.Error(err)
		re.Contains(err.Error(), "ErrDecreasingGCSafePoint")
		s.checkGCSafePoint(re, keyspaceID, 5)

		// Disallows exceeding txn safe point.
		res, err = c.AdvanceGCSafePoint(ctx, 11)
		re.Error(err)
		re.Contains(err.Error(), "ErrGCSafePointExceedsTxnSafePoint")
		// Do not chagne the current value in this case.
		s.checkGCSafePoint(re, keyspaceID, 5)

		// Allows advancing exactly to the txn safe point.
		res, err = c.AdvanceGCSafePoint(ctx, 10)
		re.NoError(err)
		re.Equal(uint64(5), res.OldGCSafePoint)
		re.Equal(uint64(10), res.Target)
		re.Equal(uint64(10), res.NewGCSafePoint)
		s.checkGCSafePoint(re, keyspaceID, 10)
	}
}

func (s *clientStatefulTestSuite) TestGCBarriers() {
	s.prepareKeyspacesForGCTest()
	re := s.Require()
	ctx := context.Background()

	for _, keyspaceID := range []uint32{constants.NullKeyspaceID, 1, 2} {
		cli := s.client.GetGCStatesClient(keyspaceID)
		c := s.client.GetGCInternalController(keyspaceID)
		s.checkGCBarrier(re, keyspaceID, "b1", 0)

		b, err := cli.SetGCBarrier(ctx, "b1", 10, math.MaxInt64)
		re.NoError(err)
		re.Equal("b1", b.BarrierID)
		re.Equal(uint64(10), b.BarrierTS)
		re.Equal(int64(math.MaxInt64), int64(b.TTL))
		s.checkGCBarrier(re, keyspaceID, "b1", 10)

		// Allows advancing to a value below the GC barrier.
		res, err := c.AdvanceTxnSafePoint(ctx, 5)
		re.NoError(err)
		re.Equal(uint64(5), res.NewTxnSafePoint)
		re.Empty(res.BlockerDescription)
		s.checkTxnSafePoint(re, keyspaceID, 5)

		// Blocks on the GC barrier when trying to advance over it.
		res, err = c.AdvanceTxnSafePoint(ctx, 11)
		re.NoError(err)
		re.Equal(uint64(5), res.OldTxnSafePoint)
		re.Equal(uint64(11), res.Target)
		re.Equal(uint64(10), res.NewTxnSafePoint)
		re.Contains(res.BlockerDescription, "b1")
		s.checkTxnSafePoint(re, keyspaceID, 10)

		// After deleting the GC barrier, the txn safe point can be resumed to going forward.
		b, err = cli.DeleteGCBarrier(ctx, "b1")
		re.NoError(err)
		re.Equal("b1", b.BarrierID)
		re.Equal(uint64(10), b.BarrierTS)
		re.Equal(int64(math.MaxInt64), int64(b.TTL))
		s.checkGCBarrier(re, keyspaceID, "b1", 0)
		res, err = c.AdvanceTxnSafePoint(ctx, 11)
		re.NoError(err)
		re.Equal(uint64(11), res.NewTxnSafePoint)
		re.Empty(res.BlockerDescription)
		s.checkTxnSafePoint(re, keyspaceID, 11)

		b, err = cli.SetGCBarrier(ctx, "b1", 15, math.MaxInt64)
		re.NoError(err)
		re.Equal("b1", b.BarrierID)
		re.Equal(uint64(15), b.BarrierTS)
		re.Equal(int64(math.MaxInt64), int64(b.TTL))

		// Allows advancing to exactly the same value as the GC barrier, without reporting the blocker.
		res, err = c.AdvanceTxnSafePoint(ctx, 15)
		re.NoError(err)
		re.Equal(uint64(15), res.NewTxnSafePoint)
		re.Empty(res.BlockerDescription)
		s.checkTxnSafePoint(re, keyspaceID, 15)

		// When multiple GC barrier exists, it blocks on the minimum one.
		_, err = cli.SetGCBarrier(ctx, "b1", 22, math.MaxInt64)
		re.NoError(err)
		s.checkGCBarrier(re, keyspaceID, "b1", 22)
		_, err = cli.SetGCBarrier(ctx, "b2", 20, math.MaxInt64)
		re.NoError(err)
		s.checkGCBarrier(re, keyspaceID, "b2", 20)
		res, err = c.AdvanceTxnSafePoint(ctx, 25)
		re.NoError(err)
		re.Equal(uint64(15), res.OldTxnSafePoint)
		re.Equal(uint64(25), res.Target)
		re.Equal(uint64(20), res.NewTxnSafePoint)
		re.Contains(res.BlockerDescription, "b2")
		s.checkTxnSafePoint(re, keyspaceID, 20)

		// Test expiring GC barrier.
		b, err = cli.SetGCBarrier(ctx, "b2", 20, time.Second)
		s.checkGCBarrier(re, keyspaceID, "b2", 20)
		re.NoError(err)
		re.False(b.IsExpired())
		// Considering the rounding-up behaviors, the actual TTL might be slightly greater.
		re.GreaterOrEqual(b.TTL, time.Second)
		re.LessOrEqual(b.TTL, 3*time.Second)
		s.waitForGCBarrierExpiring(re, b, b.TTL)
		// After the returned GCBarrierInfo is expired, the server might be still keeping it due to its rounding
		// behavior. Wait another 1 second.
		time.Sleep(time.Second)

		res, err = c.AdvanceTxnSafePoint(ctx, 25)
		re.NoError(err)
		re.Equal(uint64(20), res.OldTxnSafePoint)
		re.Equal(uint64(25), res.Target)
		re.Equal(uint64(22), res.NewTxnSafePoint)
		re.Contains(res.BlockerDescription, "b1")
		s.checkTxnSafePoint(re, keyspaceID, 22)
		s.checkGCBarrier(re, keyspaceID, "b2", 0)

		// Unable to set GC barrier to earlier ts than txn safe point.
		_, err = cli.SetGCBarrier(ctx, "b2", 21, math.MaxInt64)
		re.Error(err)
		re.Contains(err.Error(), "ErrGCBarrierTSBehindTxnSafePoint")
		s.checkGCBarrier(re, keyspaceID, "b2", 0)

		// Unable to modify an existing GC barrier to earlier ts than txn safe point.
		_, err = cli.SetGCBarrier(ctx, "b1", 21, math.MaxInt64)
		re.Error(err)
		re.Contains(err.Error(), "ErrGCBarrierTSBehindTxnSafePoint")
		// The existing GC barrier remains unchanged.
		s.checkGCBarrier(re, keyspaceID, "b1", 22)
	}
}

func TestDecodeHttpKeyRange(t *testing.T) {
	re := require.New(t)
	input := make(map[string]any)
	startStrs := make([]string, 0)
	endStrs := make([]string, 0)
	for _, kr := range []*pdHttp.KeyRange{
		pdHttp.NewKeyRange([]byte("100"), []byte("200")),
		pdHttp.NewKeyRange([]byte("300"), []byte("400")),
	} {
		startStr, endStr := kr.EscapeAsUTF8Str()
		startStrs = append(startStrs, startStr)
		endStrs = append(endStrs, endStr)
	}
	input["start-key"] = strings.Join(startStrs, ",")
	input["end-key"] = strings.Join(endStrs, ",")
	ret, err := keyutil.DecodeHTTPKeyRanges(input)
	re.NoError(err)
	re.Equal([]string{"100", "200", "300", "400"}, ret)
}
