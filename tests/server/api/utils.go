// Copyright 2025 TiKV Project Authors.
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

package api

import (
	"fmt"

	"github.com/stretchr/testify/require"

	"github.com/tikv/pd/pkg/utils/testutil"
	"github.com/tikv/pd/tests"
)

func pauseAllCheckers(re *require.Assertions, cluster *tests.TestCluster) {
	checkerNames := []string{"learner", "replica", "rule", "split", "merge", "joint-state"}
	addr := cluster.GetLeaderServer().GetAddr()
	for _, checkerName := range checkerNames {
		resp := make(map[string]any)
		url := fmt.Sprintf("%s/pd/api/v1/checker/%s", addr, checkerName)
		err := testutil.CheckPostJSON(tests.TestDialClient, url, []byte(`{"delay":1000}`), testutil.StatusOK(re))
		re.NoError(err)
		err = testutil.ReadGetJSON(re, tests.TestDialClient, url, &resp)
		re.NoError(err)
		re.True(resp["paused"].(bool))
	}
}
