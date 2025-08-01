// Copyright 2019 TiKV Project Authors.
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

package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/kvproto/pkg/metapb"

	"github.com/tikv/pd/pkg/core"
	sc "github.com/tikv/pd/pkg/schedule/config"
	"github.com/tikv/pd/pkg/schedule/types"
	"github.com/tikv/pd/pkg/slice"
	"github.com/tikv/pd/pkg/utils/testutil"
	"github.com/tikv/pd/pkg/versioninfo"
	pdTests "github.com/tikv/pd/tests"
	ctl "github.com/tikv/pd/tools/pd-ctl/pdctl"
	"github.com/tikv/pd/tools/pd-ctl/tests"
)

type schedulerTestSuite struct {
	suite.Suite
	env *pdTests.SchedulingTestEnvironment
}

func TestSchedulerTestSuite(t *testing.T) {
	suite.Run(t, new(schedulerTestSuite))
}

func (suite *schedulerTestSuite) SetupSuite() {
	re := suite.Require()
	re.NoError(failpoint.Enable("github.com/tikv/pd/server/cluster/skipStoreConfigSync", `return(true)`))
	suite.env = pdTests.NewSchedulingTestEnvironment(suite.T())
}

func (suite *schedulerTestSuite) SetupTest() {
	re := suite.Require()
	suite.env.RunFunc(func(cluster *pdTests.TestCluster) {
		stores := []*metapb.Store{
			{
				Id:            1,
				State:         metapb.StoreState_Up,
				LastHeartbeat: time.Now().UnixNano(),
			},
			{
				Id:            2,
				State:         metapb.StoreState_Up,
				LastHeartbeat: time.Now().UnixNano(),
			},
			{
				Id:            3,
				State:         metapb.StoreState_Up,
				LastHeartbeat: time.Now().UnixNano(),
			},
			{
				Id:            4,
				State:         metapb.StoreState_Up,
				LastHeartbeat: time.Now().UnixNano(),
			},
		}
		for _, store := range stores {
			pdTests.MustPutStore(re, cluster, store)
		}

		// note: because pdqsort is an unstable sort algorithm, set ApproximateSize for this region.
		pdTests.MustPutRegion(re, cluster, 1, 1, []byte("a"), []byte("b"), core.SetApproximateSize(10))
	})
}

func (suite *schedulerTestSuite) TearDownSuite() {
	re := suite.Require()
	suite.env.Cleanup()
	re.NoError(failpoint.Disable("github.com/tikv/pd/server/cluster/skipStoreConfigSync"))
}

func (suite *schedulerTestSuite) TearDownTest() {
	re := suite.Require()
	suite.env.Reset(re)
}

func (suite *schedulerTestSuite) checkDefaultSchedulers(cmd *cobra.Command, pdAddr string) {
	re := suite.Require()
	expected := make(map[string]bool)
	for _, scheduler := range types.DefaultSchedulers {
		expected[scheduler.String()] = true
	}
	checkSchedulerCommand(re, cmd, pdAddr, nil, expected)
}

func (suite *schedulerTestSuite) TestScheduler() {
	suite.env.RunTest(suite.checkScheduler)
}

func (suite *schedulerTestSuite) checkScheduler(cluster *pdTests.TestCluster) {
	re := suite.Require()
	pdAddr := cluster.GetConfig().GetClientURL()
	cmd := ctl.GetRootCmd()
	leaderServer := cluster.GetLeaderServer()

	mustUsage := func(args []string) {
		output, err := tests.ExecuteCommand(cmd, args...)
		re.NoError(err)
		re.Contains(string(output), "Usage")
	}

	checkSchedulerConfigCommand := func(expectedConfig map[string]any, schedulerName string) {
		testutil.Eventually(re, func() bool {
			configInfo := make(map[string]any)
			tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", schedulerName}, &configInfo)
			return reflect.DeepEqual(expectedConfig["store-id-ranges"], configInfo["store-id-ranges"])
		})
	}

	// scheduler delete command
	args := []string{"-u", pdAddr, "scheduler", "remove", "balance-region-scheduler"}
	expected := map[string]bool{
		"balance-leader-scheduler":     true,
		"balance-hot-region-scheduler": true,
		"evict-slow-store-scheduler":   true,
	}
	checkSchedulerCommand(re, cmd, pdAddr, args, expected)

	// avoid the influence of the scheduler order
	schedulers := []string{"evict-leader-scheduler", "grant-leader-scheduler", "evict-leader-scheduler", "grant-leader-scheduler"}

	checkStorePause := func(changedStores []uint64, schedulerName string) {
		stores := leaderServer.GetStores()
		for _, store := range stores {
			storeInfo := cluster.GetLeaderServer().GetRaftCluster().GetStore(store.GetId())
			status, isStorePaused := func() (string, bool) {
				switch schedulerName {
				case "evict-leader-scheduler":
					return "paused", !storeInfo.AllowLeaderTransferIn()
				case "grant-leader-scheduler":
					return "paused", !storeInfo.AllowLeaderTransferOut()
				default:
					re.Failf("unknown scheduler %s", schedulerName)
					return "", false
				}
			}()
			if slice.AnyOf(changedStores, func(i int) bool {
				return store.GetId() == changedStores[i]
			}) {
				re.Truef(isStorePaused,
					"store %d should be %s with %s", store.GetId(), status, schedulerName)
			} else {
				re.Falsef(isStorePaused,
					"store %d should not be %s with %s", store.GetId(), status, schedulerName)
			}
			if sche := cluster.GetSchedulingPrimaryServer(); sche != nil {
				switch schedulerName {
				case "evict-leader-scheduler":
					re.Equal(isStorePaused, !sche.GetCluster().GetStore(store.GetId()).AllowLeaderTransferIn())
				case "grant-leader-scheduler":
					re.Equal(isStorePaused, !sche.GetCluster().GetStore(store.GetId()).AllowLeaderTransferOut())
				default:
					re.Failf("unknown scheduler %s", schedulerName)
				}
			}
		}
	}

	for idx := range schedulers {
		checkStorePause([]uint64{}, schedulers[idx])

		// will fail because the scheduler is not existed
		args = []string{"-u", pdAddr, "scheduler", "config", schedulers[idx], "add-store", "3"}
		output := tests.MustExec(re, cmd, args, nil)
		re.Contains(output, fmt.Sprintf("Unable to update config: scheduler %s does not exist.", schedulers[idx]))

		// scheduler add command
		args = []string{"-u", pdAddr, "scheduler", "add", schedulers[idx], "2"}
		expected = map[string]bool{
			"balance-leader-scheduler":     true,
			"balance-hot-region-scheduler": true,
			schedulers[idx]:                true,
			"evict-slow-store-scheduler":   true,
		}
		checkSchedulerCommand(re, cmd, pdAddr, args, expected)

		// scheduler config show command
		expectedConfig := make(map[string]any)
		expectedConfig["store-id-ranges"] = map[string]any{"2": []any{map[string]any{"end-key": "", "start-key": ""}}}
		checkSchedulerConfigCommand(expectedConfig, schedulers[idx])
		checkStorePause([]uint64{2}, schedulers[idx])

		// scheduler config update command
		args = []string{"-u", pdAddr, "scheduler", "config", schedulers[idx], "add-store", "3"}
		expected = map[string]bool{
			"balance-leader-scheduler":     true,
			"balance-hot-region-scheduler": true,
			schedulers[idx]:                true,
			"evict-slow-store-scheduler":   true,
		}

		// check update success
		checkSchedulerCommand(re, cmd, pdAddr, args, expected)
		expectedConfig["store-id-ranges"] = map[string]any{"2": []any{map[string]any{"end-key": "", "start-key": ""}}, "3": []any{map[string]any{"end-key": "", "start-key": ""}}}
		checkSchedulerConfigCommand(expectedConfig, schedulers[idx])
		checkStorePause([]uint64{2, 3}, schedulers[idx])

		// scheduler delete command
		args = []string{"-u", pdAddr, "scheduler", "remove", schedulers[idx]}
		expected = map[string]bool{
			"balance-leader-scheduler":     true,
			"balance-hot-region-scheduler": true,
			"evict-slow-store-scheduler":   true,
		}
		checkSchedulerCommand(re, cmd, pdAddr, args, expected)
		checkStorePause([]uint64{}, schedulers[idx])

		// scheduler add command
		args = []string{"-u", pdAddr, "scheduler", "add", schedulers[idx], "2"}
		expected = map[string]bool{
			"balance-leader-scheduler":     true,
			"balance-hot-region-scheduler": true,
			schedulers[idx]:                true,
			"evict-slow-store-scheduler":   true,
		}
		checkSchedulerCommand(re, cmd, pdAddr, args, expected)
		checkStorePause([]uint64{2}, schedulers[idx])

		// scheduler add command twice
		args = []string{"-u", pdAddr, "scheduler", "add", schedulers[idx], "4"}
		expected = map[string]bool{
			"balance-leader-scheduler":     true,
			"balance-hot-region-scheduler": true,
			schedulers[idx]:                true,
			"evict-slow-store-scheduler":   true,
		}
		checkSchedulerCommand(re, cmd, pdAddr, args, expected)

		// check add success
		expectedConfig["store-id-ranges"] = map[string]any{"2": []any{map[string]any{"end-key": "", "start-key": ""}}, "4": []any{map[string]any{"end-key": "", "start-key": ""}}}
		checkSchedulerConfigCommand(expectedConfig, schedulers[idx])
		checkStorePause([]uint64{2, 4}, schedulers[idx])

		// scheduler remove command [old]
		args = []string{"-u", pdAddr, "scheduler", "remove", schedulers[idx] + "-4"}
		expected = map[string]bool{
			"balance-leader-scheduler":     true,
			"balance-hot-region-scheduler": true,
			schedulers[idx]:                true,
			"evict-slow-store-scheduler":   true,
		}
		checkSchedulerCommand(re, cmd, pdAddr, args, expected)

		// check remove success
		expectedConfig["store-id-ranges"] = map[string]any{"2": []any{map[string]any{"end-key": "", "start-key": ""}}}
		checkSchedulerConfigCommand(expectedConfig, schedulers[idx])
		checkStorePause([]uint64{2}, schedulers[idx])

		// scheduler remove command, when remove the last store, it should remove whole scheduler
		args = []string{"-u", pdAddr, "scheduler", "remove", schedulers[idx] + "-2"}
		expected = map[string]bool{
			"balance-leader-scheduler":     true,
			"balance-hot-region-scheduler": true,
			"evict-slow-store-scheduler":   true,
		}
		checkSchedulerCommand(re, cmd, pdAddr, args, expected)
		checkStorePause([]uint64{}, schedulers[idx])
	}

	// test remove and add scheduler
	echo := tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "balance-region-scheduler"}, nil)
	re.Contains(echo, "Success!")
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", "balance-region-scheduler"}, nil)
	re.Contains(echo, "Success!")
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", "balance-region-scheduler"}, nil)
	re.NotContains(echo, "Success!")
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "balance-region-scheduler"}, nil)
	re.Contains(echo, "Success!")
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "evict-leader-scheduler", "1"}, nil)
	re.Equal("Success! The scheduler is created.\n", echo)
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "evict-leader-scheduler", "2"}, nil)
	re.Equal("Success! The scheduler has been applied to the store.\n", echo)
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", "evict-leader-scheduler-1"}, nil)
	re.Contains(echo, "Success!")
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", "evict-leader-scheduler-2"}, nil)
	re.Contains(echo, "Success!")
	testutil.Eventually(re, func() bool { // wait for removed scheduler to be synced to scheduling server.
		echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "evict-leader-scheduler"}, nil)
		return strings.Contains(echo, "[404] scheduler not found")
	})
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", "evict-leader-scheduler-1"}, nil)
	re.Contains(echo, "Unable to update config: scheduler evict-leader-scheduler does not exist.")

	// test remove and add
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", "balance-hot-region-scheduler"}, nil)
	re.Contains(echo, "Success")
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "balance-hot-region-scheduler"}, nil)
	re.Contains(echo, "Success")

	// test show scheduler with paused and disabled status.
	checkSchedulerWithStatusCommand := func(status string, expected []string) {
		testutil.Eventually(re, func() bool {
			var schedulers []string
			tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "show", "--status", status}, &schedulers)
			return reflect.DeepEqual(expected, schedulers)
		})
	}

	// test scatter range scheduler
	for _, name := range []string{
		"test", "test#", "?test",
		/* TODO: to handle case like "tes&t", we need to modify the server's JSON render to unescape the HTML characters */
	} {
		echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "scatter-range-scheduler", "--format=raw", "a", "b", name}, nil)
		re.Contains(echo, "Success!")
		schedulerName := fmt.Sprintf("scatter-range-scheduler-%s", name)
		// test show scheduler
		testutil.Eventually(re, func() bool {
			echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "show"}, nil)
			return strings.Contains(echo, schedulerName)
		})
		// test remove scheduler
		echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", schedulerName}, nil)
		re.Contains(echo, "Success!")
		testutil.Eventually(re, func() bool {
			echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "show"}, nil)
			return !strings.Contains(echo, schedulerName)
		})
	}

	mustUsage([]string{"-u", pdAddr, "scheduler", "pause", "balance-leader-scheduler"})
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "pause", "balance-leader-scheduler", "60"}, nil)
	re.Contains(echo, "Success!")
	checkSchedulerWithStatusCommand("paused", []string{
		"balance-leader-scheduler",
	})
	result := make(map[string]any)
	testutil.Eventually(re, func() bool {
		mightExec(re, cmd, []string{"-u", pdAddr, "scheduler", "describe", "balance-leader-scheduler"}, &result)
		return len(result) != 0 && result["status"] == "paused" && result["summary"] == ""
	}, testutil.WithWaitFor(30*time.Second))

	mustUsage([]string{"-u", pdAddr, "scheduler", "resume", "balance-leader-scheduler", "60"})
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "resume", "balance-leader-scheduler"}, nil)
	re.Contains(echo, "Success!")
	checkSchedulerWithStatusCommand("paused", []string{})

	// set label scheduler to disabled manually.
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "label-scheduler"}, nil)
	re.Contains(echo, "Success!")
	cfg := leaderServer.GetServer().GetScheduleConfig()
	origin := cfg.Schedulers
	cfg.Schedulers = sc.SchedulerConfigs{{Type: "label", Disable: true}}
	err := leaderServer.GetServer().SetScheduleConfig(*cfg)
	re.NoError(err)
	checkSchedulerWithStatusCommand("disabled", []string{"label-scheduler"})
	// reset Schedulers in ScheduleConfig
	cfg.Schedulers = origin
	err = leaderServer.GetServer().SetScheduleConfig(*cfg)
	re.NoError(err)
	checkSchedulerWithStatusCommand("disabled", []string{})
}

func (suite *schedulerTestSuite) TestSchedulerConfig() {
	suite.env.RunTest(suite.checkSchedulerConfig)
}

func (suite *schedulerTestSuite) checkSchedulerConfig(cluster *pdTests.TestCluster) {
	re := suite.Require()
	pdAddr := cluster.GetConfig().GetClientURL()
	cmd := ctl.GetRootCmd()

	suite.checkDefaultSchedulers(cmd, pdAddr)

	// test evict-slow-store && evict-slow-trend schedulers config
	evictSlownessSchedulers := []string{"evict-slow-store-scheduler", "evict-slow-trend-scheduler"}
	for _, schedulerName := range evictSlownessSchedulers {
		echo := tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", schedulerName}, nil)
		if strings.Contains(echo, "Success!") {
			re.Contains(echo, "Success!")
		} else {
			re.Contains(echo, "scheduler existed")
		}
		testutil.Eventually(re, func() bool {
			echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "show"}, nil)
			return strings.Contains(echo, schedulerName)
		})
		echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", schedulerName, "set", "recovery-duration", "100"}, nil)
		re.Contains(echo, "Success! Config updated.")
		conf := make(map[string]any)
		testutil.Eventually(re, func() bool {
			tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", schedulerName, "show"}, &conf)
			return conf["recovery-duration"] == 100.
		})
		echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", schedulerName}, nil)
		re.Contains(echo, "Success!")
		testutil.Eventually(re, func() bool {
			echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "show"}, nil)
			return !strings.Contains(echo, schedulerName)
		})
	}
	// test shuffle region config
	checkSchedulerCommand(re, cmd, pdAddr, []string{"-u", pdAddr, "scheduler", "add", "shuffle-region-scheduler"}, map[string]bool{
		"balance-region-scheduler":     true,
		"balance-leader-scheduler":     true,
		"balance-hot-region-scheduler": true,
		"shuffle-region-scheduler":     true,
	})
	var roles []string
	tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "shuffle-region-scheduler", "show-roles"}, &roles)
	re.Equal([]string{"leader", "follower", "learner"}, roles)
	echo := tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "shuffle-region-scheduler", "set-roles", "learner"}, nil)
	re.Contains(echo, "Success!")
	testutil.Eventually(re, func() bool {
		tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "shuffle-region-scheduler", "show-roles"}, &roles)
		return reflect.DeepEqual([]string{"learner"}, roles)
	})
	tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "shuffle-region-scheduler"}, &roles)
	re.Equal([]string{"learner"}, roles)

	checkSchedulerCommand(re, cmd, pdAddr, []string{"-u", pdAddr, "scheduler", "remove", "shuffle-region-scheduler"}, map[string]bool{
		"balance-region-scheduler":     true,
		"balance-leader-scheduler":     true,
		"balance-hot-region-scheduler": true,
	})

	// test shuffle hot region scheduler
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "shuffle-hot-region-scheduler"}, nil)
	re.Contains(echo, "Success!")
	testutil.Eventually(re, func() bool {
		echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "show"}, nil)
		return strings.Contains(echo, "shuffle-hot-region-scheduler")
	})
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "shuffle-hot-region-scheduler", "set", "limit", "127"}, nil)
	re.Contains(echo, "Success!")
	conf := make(map[string]any)
	testutil.Eventually(re, func() bool {
		tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "shuffle-hot-region-scheduler", "show"}, &conf)
		return conf["limit"] == 127.
	})
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", "shuffle-hot-region-scheduler"}, nil)
	re.Contains(echo, "Success!")
	testutil.Eventually(re, func() bool {
		echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "show"}, nil)
		return !strings.Contains(echo, "shuffle-hot-region-scheduler")
	})

	// test evict leader scheduler
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "evict-leader-scheduler", "1"}, nil)
	re.Contains(echo, "Success!")
	testutil.Eventually(re, func() bool {
		echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "show"}, nil)
		return strings.Contains(echo, "evict-leader-scheduler")
	})
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "evict-leader-scheduler", "set", "batch", "5"}, nil)
	re.Contains(echo, "Success!")
	conf = make(map[string]any)
	testutil.Eventually(re, func() bool {
		tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "evict-leader-scheduler"}, &conf)
		return conf["batch"] == 5.
	})
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", "evict-leader-scheduler-1"}, nil)
	re.Contains(echo, "Success!")
	testutil.Eventually(re, func() bool {
		echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "show"}, nil)
		return !strings.Contains(echo, "evict-leader-scheduler")
	})

	// test balance key range scheduler
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "balance-range-scheduler"}, nil)
	re.NotContains(echo, "Success!")
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "balance-range-scheduler", "--format=raw", "tiflash", "learner-scatter", "test", "a", "b"}, nil)
	re.Contains(echo, "Success!")
	var rangeConf []map[string]any
	var jobConf map[string]any
	testutil.Eventually(re, func() bool {
		mightExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-range-scheduler"}, &rangeConf)
		if len(rangeConf) == 0 {
			return false
		}
		jobConf = rangeConf[0]
		return jobConf["rule"] == "learner-scatter" && jobConf["engine"] == "tiflash" && jobConf["alias"] == "test"
	})
	re.Equal(float64(30*time.Minute.Nanoseconds()), jobConf["timeout"])
	re.Equal("running", jobConf["status"])
	ranges := jobConf["ranges"].([]any)[0].(map[string]any)
	re.Equal(core.HexRegionKeyStr([]byte("a")), ranges["start-key"])
	re.Equal(core.HexRegionKeyStr([]byte("b")), ranges["end-key"])

	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "balance-range-scheduler", "--format=raw", "tiflash", "learner-scatter", "learner", "a", "b"}, nil)
	re.Contains(echo, "Success!")
	testutil.Eventually(re, func() bool {
		mightExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-range-scheduler"}, &rangeConf)
		return len(rangeConf) == 2
	})
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", "balance-range-scheduler"}, nil)
	re.Contains(echo, "Success!")

	// test balance leader config
	conf = make(map[string]any)
	conf1 := make(map[string]any)
	tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-leader-scheduler", "show"}, &conf)
	re.Equal(4., conf["batch"])
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-leader-scheduler", "set", "batch", "3"}, nil)
	re.Contains(echo, "Success!")
	testutil.Eventually(re, func() bool {
		tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-leader-scheduler"}, &conf1)
		return conf1["batch"] == 3.
	})
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "balance-leader-scheduler"}, nil)
	re.NotContains(echo, "Success!")
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", "balance-leader-scheduler"}, nil)
	re.Contains(echo, "Success!")
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", "balance-leader-scheduler"}, nil)
	re.Contains(echo, "404")
	re.Contains(echo, "PD:scheduler:ErrSchedulerNotFound]scheduler not found")
	// The scheduling service need time to sync from PD.
	testutil.Eventually(re, func() bool {
		echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-leader-scheduler"}, nil)
		return strings.Contains(echo, "404") && strings.Contains(echo, "scheduler not found")
	})
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "balance-leader-scheduler"}, nil)
	re.Contains(echo, "Success!")

	// test evict slow store scheduler
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "evict-slow-store-scheduler"}, nil)
	re.Contains(echo, "Success!")
	conf = make(map[string]any)
	conf1 = make(map[string]any)
	testutil.Eventually(re, func() bool {
		tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "evict-slow-store-scheduler", "show"}, &conf)
		return conf["batch"] == 3.
	})
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "evict-slow-store-scheduler", "set", "batch", "10"}, nil)
	re.Contains(echo, "Success!")
	testutil.Eventually(re, func() bool {
		tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "evict-slow-store-scheduler"}, &conf1)
		return conf1["batch"] == 10.
	})
}

func (suite *schedulerTestSuite) TestGrantHotRegionScheduler() {
	suite.env.RunTest(suite.checkGrantHotRegionScheduler)
}

func (suite *schedulerTestSuite) checkGrantHotRegionScheduler(cluster *pdTests.TestCluster) {
	re := suite.Require()
	pdAddr := cluster.GetConfig().GetClientURL()
	cmd := ctl.GetRootCmd()

	// case 1: add grant-hot-region-scheduler when balance-hot-region-scheduler is running
	echo := tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "grant-hot-region-scheduler", "1", "1,2,3"}, nil)
	re.Contains(echo, "balance-hot-region-scheduler is running, please remove it first")

	// case 2: add grant-hot-region-scheduler when balance-hot-region-scheduler is paused
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "pause", "balance-hot-region-scheduler", "60"}, nil)
	re.Contains(echo, "Success!")
	suite.checkDefaultSchedulers(cmd, pdAddr)
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "grant-hot-region-scheduler", "1", "1,2,3"}, nil)
	re.Contains(echo, "balance-hot-region-scheduler is running, please remove it first")
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "resume", "balance-hot-region-scheduler"}, nil)
	re.Contains(echo, "Success!")

	// case 3: add grant-hot-region-scheduler when balance-hot-region-scheduler is disabled
	checkSchedulerCommand(re, cmd, pdAddr, []string{"-u", pdAddr, "scheduler", "remove", "balance-hot-region-scheduler"}, map[string]bool{
		"balance-region-scheduler":   true,
		"balance-leader-scheduler":   true,
		"evict-slow-store-scheduler": true,
	})

	checkSchedulerCommand(re, cmd, pdAddr, []string{"-u", pdAddr, "scheduler", "add", "grant-hot-region-scheduler", "1", "2,3"}, map[string]bool{
		"balance-region-scheduler":   true,
		"balance-leader-scheduler":   true,
		"grant-hot-region-scheduler": true,
		"evict-slow-store-scheduler": true,
	})

	// case 4: test grant-hot-region-scheduler config
	var conf3 map[string]any
	expected3 := map[string]any{
		"store-id":        []any{float64(1), float64(2), float64(3)},
		"store-leader-id": float64(1),
	}
	tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "grant-hot-region-scheduler"}, &conf3)
	re.True(compareGrantHotRegionSchedulerConfig(expected3, conf3))

	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "grant-hot-region-scheduler", "set", "2", "1,3"}, nil)
	re.Contains(echo, "Success!")
	expected3["store-leader-id"] = float64(2)
	testutil.Eventually(re, func() bool {
		tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "grant-hot-region-scheduler"}, &conf3)
		return compareGrantHotRegionSchedulerConfig(expected3, conf3)
	})

	checkSchedulerCommand(re, cmd, pdAddr, []string{"-u", pdAddr, "scheduler", "remove", "grant-hot-region-scheduler"}, map[string]bool{
		"balance-region-scheduler":   true,
		"balance-leader-scheduler":   true,
		"evict-slow-store-scheduler": true,
	})

	// use duplicate store id
	checkSchedulerCommand(re, cmd, pdAddr, []string{"-u", pdAddr, "scheduler", "add", "grant-hot-region-scheduler", "3", "1,2,3"}, map[string]bool{
		"balance-region-scheduler":   true,
		"balance-leader-scheduler":   true,
		"grant-hot-region-scheduler": true,
		"evict-slow-store-scheduler": true,
	})
	expected3["store-leader-id"] = float64(3)
	testutil.Eventually(re, func() bool {
		tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "grant-hot-region-scheduler"}, &conf3)
		return compareGrantHotRegionSchedulerConfig(expected3, conf3)
	})

	// case 5: remove grant-hot-region-scheduler
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "balance-hot-region-scheduler"}, nil)
	re.Contains(echo, "grant-hot-region-scheduler is running, please remove it first")

	checkSchedulerCommand(re, cmd, pdAddr, []string{"-u", pdAddr, "scheduler", "remove", "grant-hot-region-scheduler"}, map[string]bool{
		"balance-region-scheduler":   true,
		"balance-leader-scheduler":   true,
		"evict-slow-store-scheduler": true,
	})

	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "balance-hot-region-scheduler"}, nil)
	re.Contains(echo, "Success!")
	suite.checkDefaultSchedulers(cmd, pdAddr)
}

func (suite *schedulerTestSuite) TestHotRegionSchedulerConfig() {
	suite.env.RunTest(suite.checkHotRegionSchedulerConfig)
}

func (suite *schedulerTestSuite) checkHotRegionSchedulerConfig(cluster *pdTests.TestCluster) {
	re := suite.Require()
	pdAddr := cluster.GetConfig().GetClientURL()
	cmd := ctl.GetRootCmd()

	// test hot region config
	expected1 := map[string]any{
		"min-hot-byte-rate":       float64(100),
		"min-hot-key-rate":        float64(10),
		"min-hot-query-rate":      float64(10),
		"src-tolerance-ratio":     1.05,
		"dst-tolerance-ratio":     1.05,
		"read-priorities":         []any{"byte", "key"},
		"write-leader-priorities": []any{"key", "byte"},
		"write-peer-priorities":   []any{"byte", "key"},
		"strict-picking-store":    "true",
		"rank-formula-version":    "v2",
		"split-thresholds":        0.2,
		"history-sample-duration": "5m0s",
		"history-sample-interval": "30s",
	}
	checkHotSchedulerConfig := func(expect map[string]any) {
		testutil.Eventually(re, func() bool {
			var conf1 map[string]any
			tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler"}, &conf1)
			return reflect.DeepEqual(expect, conf1)
		})
	}
	var conf map[string]any
	tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "list"}, &conf)
	re.Equal(expected1, conf)
	tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "show"}, &conf)
	re.Equal(expected1, conf)
	echo := tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "src-tolerance-ratio", "1.02"}, nil)
	re.Contains(echo, "Success!")
	expected1["src-tolerance-ratio"] = 1.02
	checkHotSchedulerConfig(expected1)
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "disabled", "true"}, nil)
	re.Contains(echo, "Failed!")

	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "read-priorities", "byte,key"}, nil)
	re.Contains(echo, "Success!")
	expected1["read-priorities"] = []any{"byte", "key"}
	checkHotSchedulerConfig(expected1)

	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "read-priorities", "key"}, nil)
	re.Contains(echo, "Failed!")
	checkHotSchedulerConfig(expected1)
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "read-priorities", "key,byte"}, nil)
	re.Contains(echo, "Success!")
	expected1["read-priorities"] = []any{"key", "byte"}
	checkHotSchedulerConfig(expected1)
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "read-priorities", "foo,bar"}, nil)
	re.Contains(echo, "Failed!")
	checkHotSchedulerConfig(expected1)
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "read-priorities", ""}, nil)
	re.Contains(echo, "Failed!")
	checkHotSchedulerConfig(expected1)
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "read-priorities", "key,key"}, nil)
	re.Contains(echo, "Failed!")
	checkHotSchedulerConfig(expected1)
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "read-priorities", "byte,byte"}, nil)
	re.Contains(echo, "Failed!")
	checkHotSchedulerConfig(expected1)
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "read-priorities", "key,key,byte"}, nil)
	re.Contains(echo, "Failed!")
	checkHotSchedulerConfig(expected1)

	// write-priorities is divided into write-leader-priorities and write-peer-priorities
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "write-priorities", "key,byte"}, nil)
	re.Contains(echo, "Failed!")
	re.Contains(echo, "Config item is not found.")
	checkHotSchedulerConfig(expected1)

	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "rank-formula-version", "v0"}, nil)
	re.Contains(echo, "Failed!")
	checkHotSchedulerConfig(expected1)
	expected1["rank-formula-version"] = "v2"
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "rank-formula-version", "v2"}, nil)
	re.Contains(echo, "Success!")
	checkHotSchedulerConfig(expected1)
	expected1["rank-formula-version"] = "v1"
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "rank-formula-version", "v1"}, nil)
	re.Contains(echo, "Success!")
	checkHotSchedulerConfig(expected1)

	expected1["forbid-rw-type"] = "read"
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "forbid-rw-type", "read"}, nil)
	re.Contains(echo, "Success!")
	checkHotSchedulerConfig(expected1)

	expected1["history-sample-duration"] = "1m0s"
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "history-sample-duration", "1m"}, nil)
	re.Contains(echo, "Success!")
	checkHotSchedulerConfig(expected1)

	expected1["history-sample-interval"] = "1s"
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "history-sample-interval", "1s"}, nil)
	re.Contains(echo, "Success!")
	checkHotSchedulerConfig(expected1)

	expected1["history-sample-duration"] = "0s"
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "history-sample-duration", "0s"}, nil)
	re.Contains(echo, "Success!")
	checkHotSchedulerConfig(expected1)

	expected1["history-sample-interval"] = "0s"
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "history-sample-interval", "0s"}, nil)
	re.Contains(echo, "Success!")
	checkHotSchedulerConfig(expected1)
}

func (suite *schedulerTestSuite) TestSchedulerDiagnostic() {
	suite.env.RunTest(suite.checkSchedulerDiagnostic)
}

func (suite *schedulerTestSuite) checkSchedulerDiagnostic(cluster *pdTests.TestCluster) {
	re := suite.Require()
	pdAddr := cluster.GetConfig().GetClientURL()
	cmd := ctl.GetRootCmd()

	checkSchedulerDescribeCommand := func(schedulerName, expectedStatus, expectedSummary string) {
		result := make(map[string]any)
		testutil.Eventually(re, func() bool {
			mightExec(re, cmd, []string{"-u", pdAddr, "scheduler", "describe", schedulerName}, &result)
			return len(result) != 0 && expectedStatus == result["status"] && expectedSummary == result["summary"]
		}, testutil.WithTickInterval(50*time.Millisecond))
	}

	echo := tests.MustExec(re, cmd, []string{"-u", pdAddr, "config", "set", "enable-diagnostic", "true"}, nil)
	re.Contains(echo, "Success!")
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "config", "set", "region-schedule-limit", "0"}, nil)
	re.Contains(echo, "Success!")
	checkSchedulerDescribeCommand("balance-region-scheduler", "pending", "balance-region-scheduler reach limit")

	// scheduler delete command
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "remove", "balance-leader-scheduler"}, nil)
	re.Contains(echo, "Success!")
	checkSchedulerDescribeCommand("balance-leader-scheduler", "disabled", "")

	// scheduler add command
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "add", "balance-leader-scheduler"}, nil)
	re.Contains(echo, "Success!")
	checkSchedulerDescribeCommand("balance-leader-scheduler", "normal", "")

	// scheduler pause command
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "pause", "balance-leader-scheduler", "60"}, nil)
	re.Contains(echo, "Success!")
	checkSchedulerDescribeCommand("balance-leader-scheduler", "paused", "")

	// scheduler resume command
	echo = tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "resume", "balance-leader-scheduler"}, nil)
	re.Contains(echo, "Success!")
	checkSchedulerDescribeCommand("balance-leader-scheduler", "normal", "")
}

func (suite *schedulerTestSuite) TestEvictLeaderScheduler() {
	suite.env.RunTest(suite.checkEvictLeaderScheduler)
}

func (suite *schedulerTestSuite) checkEvictLeaderScheduler(cluster *pdTests.TestCluster) {
	re := suite.Require()
	pdAddr := cluster.GetConfig().GetClientURL()
	cmd := ctl.GetRootCmd()

	output, err := tests.ExecuteCommand(cmd, []string{"-u", pdAddr, "scheduler", "add", "evict-leader-scheduler", "2"}...)
	re.NoError(err)
	re.Contains(string(output), "Success!")
	output, err = tests.ExecuteCommand(cmd, []string{"-u", pdAddr, "scheduler", "add", "evict-leader-scheduler", "1"}...)
	re.NoError(err)
	re.Contains(string(output), "Success!")
	output, err = tests.ExecuteCommand(cmd, []string{"-u", pdAddr, "scheduler", "remove", "evict-leader-scheduler"}...)
	re.NoError(err)
	re.Contains(string(output), "Success!")
	output, err = tests.ExecuteCommand(cmd, []string{"-u", pdAddr, "scheduler", "add", "evict-leader-scheduler", "1"}...)
	re.NoError(err)
	re.Contains(string(output), "Success!")
	testutil.Eventually(re, func() bool {
		output, err = tests.ExecuteCommand(cmd, []string{"-u", pdAddr, "scheduler", "remove", "evict-leader-scheduler-1"}...)
		return err == nil && strings.Contains(string(output), "Success!")
	})
	testutil.Eventually(re, func() bool {
		output, err = tests.ExecuteCommand(cmd, []string{"-u", pdAddr, "scheduler", "show"}...)
		return err == nil && !strings.Contains(string(output), "evict-leader-scheduler")
	})
}

func mightExec(re *require.Assertions, cmd *cobra.Command, args []string, v any) {
	output, err := tests.ExecuteCommand(cmd, args...)
	re.NoError(err)
	if v == nil {
		return
	}
	json.Unmarshal(output, v)
}

func checkSchedulerCommand(re *require.Assertions, cmd *cobra.Command, pdAddr string, args []string, expected map[string]bool) {
	if args != nil {
		echo := tests.MustExec(re, cmd, args, nil)
		re.Contains(echo, "Success!")
	}
	testutil.Eventually(re, func() bool {
		var schedulers []string
		tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "show"}, &schedulers)
		if len(schedulers) != len(expected) {
			return false
		}
		for _, scheduler := range schedulers {
			if _, ok := expected[scheduler]; !ok {
				return false
			}
		}
		return true
	})
}

func compareGrantHotRegionSchedulerConfig(expect, actual map[string]any) bool {
	if expect["store-leader-id"] != actual["store-leader-id"] {
		return false
	}
	expectStoreID := expect["store-id"].([]any)
	actualStoreID := actual["store-id"].([]any)
	if len(expectStoreID) != len(actualStoreID) {
		return false
	}
	count := map[float64]any{}
	for _, id := range expectStoreID {
		// check if the store id is duplicated
		if _, ok := count[id.(float64)]; ok {
			return false
		}
		count[id.(float64)] = nil
	}
	for _, id := range actualStoreID {
		if _, ok := count[id.(float64)]; !ok {
			return false
		}
	}
	return true
}

// TestHotSchedulerUpgrade tests the config change after pd upgrade to 5.2.0
// It only tests the config change of hot scheduler with non ms.
// Because it change the cluster version, so need to test in another test.
func TestHotSchedulerUpgrade(t *testing.T) {
	re := require.New(t)
	re.NoError(failpoint.Enable("github.com/tikv/pd/server/cluster/skipStoreConfigSync", `return(true)`))
	defer func() {
		re.NoError(failpoint.Disable("github.com/tikv/pd/server/cluster/skipStoreConfigSync"))
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cluster, err := pdTests.NewTestCluster(ctx, 1)
	re.NoError(err)
	defer cluster.Destroy()
	err = cluster.RunInitialServers()
	re.NoError(err)
	re.NotEmpty(cluster.WaitLeader())
	leaderServer := cluster.GetLeaderServer()
	re.NoError(leaderServer.BootstrapCluster())
	pdAddr := cluster.GetConfig().GetClientURL()
	cmd := ctl.GetRootCmd()
	stores := []*metapb.Store{
		{
			Id:            1,
			State:         metapb.StoreState_Up,
			LastHeartbeat: time.Now().UnixNano(),
		},
		{
			Id:            2,
			State:         metapb.StoreState_Up,
			LastHeartbeat: time.Now().UnixNano(),
		},
		{
			Id:            3,
			State:         metapb.StoreState_Up,
			LastHeartbeat: time.Now().UnixNano(),
		},
		{
			Id:            4,
			State:         metapb.StoreState_Up,
			LastHeartbeat: time.Now().UnixNano(),
		},
	}
	for _, store := range stores {
		pdTests.MustPutStore(re, cluster, store)
	}
	pdTests.MustPutRegion(re, cluster, 1, 1, []byte("a"), []byte("b"), core.SetApproximateSize(10))
	// wait scheduelrs run
	testutil.Eventually(re, func() bool {
		schedulers := []string{}
		tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "show"}, &schedulers)
		return len(schedulers) == len(sc.DefaultSchedulers)
	})
	// check config in version 2.0
	var conf map[string]any
	tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "list"}, &conf)
	re.NotContains(conf["write-leader-priorities"], "query")
	// test compatibility
	re.Equal("2.0.0", leaderServer.GetClusterVersion().String())
	for _, store := range stores {
		version := versioninfo.HotScheduleWithQuery
		store.Version = versioninfo.MinSupportedVersion(version).String()
		store.LastHeartbeat = time.Now().UnixNano()
		pdTests.MustPutStore(re, cluster, store)
	}
	re.Equal("5.2.0", leaderServer.GetClusterVersion().String())
	// after upgrading, we can use query automatically.
	testutil.Eventually(re, func() bool {
		tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "list"}, &conf)
		return slice.Contains(conf["write-leader-priorities"].([]any), "query")
	})
	// cannot set qps as write-peer-priorities
	echo := tests.MustExec(re, cmd, []string{"-u", pdAddr, "scheduler", "config", "balance-hot-region-scheduler", "set", "write-peer-priorities", "query,byte"}, nil)
	re.Contains(echo, "query is not allowed to be set in priorities for write-peer-priorities")
}
