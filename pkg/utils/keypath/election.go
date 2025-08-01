// Copyright 2024 TiKV Project Authors.
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

package keypath

import (
	"fmt"

	"github.com/tikv/pd/pkg/mcs/utils/constant"
)

// ElectionPath returns the election path.
func ElectionPath(p *MsParam) string {
	if p == nil || p.ServiceName == "" {
		return fmt.Sprintf(leaderPathFormat, ClusterID())
	}
	if p.ServiceName == constant.TSOServiceName {
		if p.GroupID == 0 {
			return fmt.Sprintf(msTsoDefaultPrimaryPathFormat, ClusterID())
		}
		return fmt.Sprintf(msTsoKespacePrimaryPathFormat, ClusterID(), p.GroupID)
	}
	return fmt.Sprintf(msPrimaryPathFormat, ClusterID(), p.ServiceName)
}

// ExpectedPrimaryPath returns the expected_primary path.
func ExpectedPrimaryPath(p *MsParam) string {
	if p.ServiceName == constant.TSOServiceName {
		if p.GroupID == 0 {
			return fmt.Sprintf(msTsoDefaultExpectedPrimaryPathFormat, ClusterID())
		}
		return fmt.Sprintf(msTsoKespaceExpectedPrimaryPathFormat, ClusterID(), p.GroupID)
	}
	return fmt.Sprintf(msExpectedPrimaryPathFormat, ClusterID(), p.ServiceName)
}

// MemberLeaderPriorityPath returns the member leader priority path.
func MemberLeaderPriorityPath(id uint64) string {
	return fmt.Sprintf(memberLeaderPriorityPathFormat, ClusterID(), id)
}
