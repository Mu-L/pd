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

package schedulers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/unrolled/render"
	"go.uber.org/zap"

	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/log"

	"github.com/tikv/pd/pkg/core"
	"github.com/tikv/pd/pkg/core/constant"
	"github.com/tikv/pd/pkg/errs"
	sche "github.com/tikv/pd/pkg/schedule/core"
	"github.com/tikv/pd/pkg/schedule/filter"
	"github.com/tikv/pd/pkg/schedule/operator"
	"github.com/tikv/pd/pkg/schedule/placement"
	"github.com/tikv/pd/pkg/schedule/plan"
	"github.com/tikv/pd/pkg/schedule/types"
	"github.com/tikv/pd/pkg/utils/apiutil"
	"github.com/tikv/pd/pkg/utils/keyutil"
	"github.com/tikv/pd/pkg/utils/syncutil"
)

var (
	defaultJobTimeout = 30 * time.Minute
	reserveDuration   = 7 * 24 * time.Hour
)

type balanceRangeSchedulerHandler struct {
	rd     *render.Render
	config *balanceRangeSchedulerConfig
}

func newBalanceRangeHandler(conf *balanceRangeSchedulerConfig) http.Handler {
	handler := &balanceRangeSchedulerHandler{
		config: conf,
		rd:     render.New(render.Options{IndentJSON: true}),
	}
	router := mux.NewRouter()
	router.HandleFunc("/config", handler.updateConfig).Methods(http.MethodPost)
	router.HandleFunc("/list", handler.listConfig).Methods(http.MethodGet)
	router.HandleFunc("/job", handler.addJob).Methods(http.MethodPut)
	router.HandleFunc("/job", handler.deleteJob).Methods(http.MethodDelete)
	return router
}

func (handler *balanceRangeSchedulerHandler) updateConfig(w http.ResponseWriter, _ *http.Request) {
	handler.rd.JSON(w, http.StatusBadRequest, "update config is not supported")
}

func (handler *balanceRangeSchedulerHandler) listConfig(w http.ResponseWriter, _ *http.Request) {
	handler.config.Lock()
	defer handler.config.Unlock()
	conf := handler.config.cloneLocked()
	if err := handler.rd.JSON(w, http.StatusOK, conf); err != nil {
		log.Error("failed to marshal balance key range scheduler config", errs.ZapError(err))
	}
}

func (handler *balanceRangeSchedulerHandler) addJob(w http.ResponseWriter, r *http.Request) {
	var input map[string]any
	if err := apiutil.ReadJSONRespondError(handler.rd, w, r.Body, &input); err != nil {
		return
	}
	job := &balanceRangeSchedulerJob{
		Create:  time.Now(),
		Status:  pending,
		Timeout: defaultJobTimeout,
	}
	job.Engine = input["engine"].(string)
	if job.Engine != core.EngineTiFlash && job.Engine != core.EngineTiKV {
		handler.rd.JSON(w, http.StatusBadRequest, fmt.Sprintf("engine:%s must be tikv or tiflash", input["engine"].(string)))
		return
	}
	job.Rule = core.NewRule(input["rule"].(string))
	if job.Rule != core.LeaderScatter && job.Rule != core.PeerScatter && job.Rule != core.LearnerScatter {
		handler.rd.JSON(w, http.StatusBadRequest, fmt.Sprintf("rule:%s must be leader-scatter, learner-scatter or peer-scatter",
			input["engine"].(string)))
		return
	}

	job.Alias = input["alias"].(string)
	timeoutStr, ok := input["timeout"].(string)
	if ok && len(timeoutStr) > 0 {
		timeout, err := time.ParseDuration(timeoutStr)
		if err != nil {
			handler.rd.JSON(w, http.StatusBadRequest, fmt.Sprintf("timeout:%s is invalid", input["timeout"].(string)))
			return
		}
		job.Timeout = timeout
	}

	keys, err := keyutil.DecodeHTTPKeyRanges(input)
	if err != nil {
		handler.rd.JSON(w, http.StatusBadRequest, err.Error())
		return
	}
	krs, err := getKeyRanges(keys)
	if err != nil {
		handler.rd.JSON(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Info("add balance key range job", zap.String("alias", job.Alias))
	job.Ranges = krs
	if err := handler.config.addJob(job); err != nil {
		handler.rd.JSON(w, http.StatusBadRequest, err.Error())
		return
	}
	handler.rd.JSON(w, http.StatusOK, nil)
}

func (handler *balanceRangeSchedulerHandler) deleteJob(w http.ResponseWriter, r *http.Request) {
	jobStr := r.URL.Query().Get("job-id")
	if jobStr == "" {
		handler.rd.JSON(w, http.StatusBadRequest, "job-id is required")
		return
	}
	jobID, err := strconv.ParseUint(jobStr, 10, 64)
	if err != nil {
		handler.rd.JSON(w, http.StatusBadRequest, "invalid job-id")
		return
	}
	if err := handler.config.deleteJob(jobID); err != nil {
		handler.rd.JSON(w, http.StatusInternalServerError, err)
		return
	}
	handler.rd.JSON(w, http.StatusOK, nil)
}

type balanceRangeSchedulerConfig struct {
	syncutil.RWMutex
	schedulerConfig
	jobs []*balanceRangeSchedulerJob
}

func (conf *balanceRangeSchedulerConfig) addJob(job *balanceRangeSchedulerJob) error {
	conf.Lock()
	defer conf.Unlock()
	for _, c := range conf.jobs {
		if c.isComplete() {
			continue
		}
		if job.Alias == c.Alias {
			return errors.New("job already exists")
		}
	}
	job.Status = pending
	if len(conf.jobs) == 0 {
		job.JobID = 1
	} else {
		job.JobID = conf.jobs[len(conf.jobs)-1].JobID + 1
	}
	return conf.persistLocked(func() {
		conf.jobs = append(conf.jobs, job)
	})
}

func (conf *balanceRangeSchedulerConfig) deleteJob(jobID uint64) error {
	conf.Lock()
	defer conf.Unlock()
	for _, job := range conf.jobs {
		if job.JobID == jobID {
			if job.isComplete() {
				return errs.ErrInvalidArgument.FastGenByArgs(fmt.Sprintf(
					"The job:%d has been completed and cannot be cancelled.", jobID))
			}
			return conf.persistLocked(func() {
				job.Status = cancelled
				now := time.Now()
				if job.Start == nil {
					job.Start = &now
				}
				job.Finish = &now
			})
		}
	}
	return errs.ErrScheduleConfigNotExist.FastGenByArgs(jobID)
}

func (conf *balanceRangeSchedulerConfig) gc() error {
	needGC := false
	gcIdx := 0
	conf.Lock()
	defer conf.Unlock()
	for idx, job := range conf.jobs {
		if job.isComplete() && job.expired(reserveDuration) {
			needGC = true
			gcIdx = idx
		} else {
			// The jobs are sorted by the started time and executed by it.
			// So it can end util the first element doesn't satisfy the condition.
			break
		}
	}
	if !needGC {
		return nil
	}
	return conf.persistLocked(func() {
		conf.jobs = conf.jobs[gcIdx+1:]
	})
}

func (conf *balanceRangeSchedulerConfig) persistLocked(updateFn func()) error {
	originJobs := conf.cloneLocked()
	updateFn()
	if err := conf.save(); err != nil {
		conf.jobs = originJobs
		return err
	}
	return nil
}

// MarshalJSON marshals to json.
func (conf *balanceRangeSchedulerConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(conf.jobs)
}

// UnmarshalJSON unmarshals from json.
func (conf *balanceRangeSchedulerConfig) UnmarshalJSON(data []byte) error {
	jobs := make([]*balanceRangeSchedulerJob, 0)
	if err := json.Unmarshal(data, &jobs); err != nil {
		return err
	}
	conf.jobs = jobs
	return nil
}

func (conf *balanceRangeSchedulerConfig) begin(index int) error {
	conf.Lock()
	defer conf.Unlock()
	job := conf.jobs[index]
	if job.Status != pending {
		return errors.New("the job is not pending")
	}
	return conf.persistLocked(func() {
		now := time.Now()
		job.Start = &now
		job.Status = running
	})
}

func (conf *balanceRangeSchedulerConfig) finish(index int) error {
	conf.Lock()
	defer conf.Unlock()

	job := conf.jobs[index]
	if job.Status != running {
		return errors.New("the job is not running")
	}
	return conf.persistLocked(func() {
		now := time.Now()
		job.Finish = &now
		job.Status = finished
	})
}

func (conf *balanceRangeSchedulerConfig) peek() (int, *balanceRangeSchedulerJob) {
	conf.RLock()
	defer conf.RUnlock()
	for index, job := range conf.jobs {
		if job.isComplete() {
			continue
		}
		return index, job
	}
	return 0, nil
}

func (conf *balanceRangeSchedulerConfig) cloneLocked() []*balanceRangeSchedulerJob {
	jobs := make([]*balanceRangeSchedulerJob, 0, len(conf.jobs))
	for _, job := range conf.jobs {
		ranges := make([]keyutil.KeyRange, len(job.Ranges))
		copy(ranges, job.Ranges)
		jobs = append(jobs, &balanceRangeSchedulerJob{
			Ranges:  ranges,
			Rule:    job.Rule,
			Engine:  job.Engine,
			Timeout: job.Timeout,
			Alias:   job.Alias,
			JobID:   job.JobID,
			Start:   job.Start,
			Status:  job.Status,
			Create:  job.Create,
			Finish:  job.Finish,
		})
	}

	return jobs
}

type balanceRangeSchedulerJob struct {
	JobID   uint64             `json:"job-id"`
	Rule    core.Rule          `json:"rule"`
	Engine  string             `json:"engine"`
	Timeout time.Duration      `json:"timeout"`
	Ranges  []keyutil.KeyRange `json:"ranges"`
	Alias   string             `json:"alias"`
	Start   *time.Time         `json:"start,omitempty"`
	Finish  *time.Time         `json:"finish,omitempty"`
	Create  time.Time          `json:"create"`
	Status  JobStatus          `json:"status"`
}

func (job *balanceRangeSchedulerJob) expired(dur time.Duration) bool {
	if job == nil {
		return true
	}
	if job.Finish == nil {
		return false
	}
	now := time.Now()
	return now.Sub(*job.Finish) > dur
}

func (job *balanceRangeSchedulerJob) isComplete() bool {
	return job.Status == finished || job.Status == cancelled
}

// EncodeConfig serializes the config.
func (s *balanceRangeScheduler) EncodeConfig() ([]byte, error) {
	s.conf.RLock()
	defer s.conf.RUnlock()
	return EncodeConfig(s.conf.jobs)
}

// ReloadConfig reloads the config.
func (s *balanceRangeScheduler) ReloadConfig() error {
	s.conf.Lock()
	defer s.conf.Unlock()

	jobs := make([]*balanceRangeSchedulerJob, 0, len(s.conf.jobs))
	if err := s.conf.load(&jobs); err != nil {
		return err
	}
	s.conf.jobs = jobs
	return nil
}

type balanceRangeScheduler struct {
	*BaseScheduler
	conf          *balanceRangeSchedulerConfig
	handler       http.Handler
	filters       []filter.Filter
	filterCounter *filter.Counter
}

// ServeHTTP implements the http.Handler interface.
func (s *balanceRangeScheduler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// IsScheduleAllowed checks if the scheduler is allowed to schedule new operators.
func (s *balanceRangeScheduler) IsScheduleAllowed(cluster sche.SchedulerCluster) bool {
	allowed := s.OpController.OperatorCount(operator.OpRange) < cluster.GetSchedulerConfig().GetRegionScheduleLimit()
	if !allowed {
		operator.IncOperatorLimitCounter(s.GetType(), operator.OpRange)
	}
	if err := s.conf.gc(); err != nil {
		log.Error("balance range jobs gc failed", errs.ZapError(err))
		return false
	}
	index, job := s.conf.peek()
	if job != nil {
		if job.Status == pending {
			if err := s.conf.begin(index); err != nil {
				return false
			}
			km := cluster.GetKeyRangeManager()
			km.Append(job.Ranges)
		}
		// todo: add other conditions such as the diff of the score between the source and target store.
		if time.Since(*job.Start) > job.Timeout {
			if err := s.conf.finish(index); err != nil {
				return false
			}
			km := cluster.GetKeyRangeManager()
			km.Delete(job.Ranges)
			balanceRangeExpiredCounter.Inc()
		}
	}
	return allowed
}

// BalanceRangeCreateOption is used to create a scheduler with an option.
type BalanceRangeCreateOption func(s *balanceRangeScheduler)

// newBalanceRangeScheduler creates a scheduler that tends to keep given peer rule on
// special store balanced.
func newBalanceRangeScheduler(opController *operator.Controller, conf *balanceRangeSchedulerConfig, options ...BalanceRangeCreateOption) Scheduler {
	s := &balanceRangeScheduler{
		BaseScheduler: NewBaseScheduler(opController, types.BalanceRangeScheduler, conf),
		conf:          conf,
		handler:       newBalanceRangeHandler(conf),
	}
	for _, option := range options {
		option(s)
	}

	s.filters = []filter.Filter{
		&filter.StoreStateFilter{ActionScope: s.GetName(), TransferLeader: true, MoveRegion: true, OperatorLevel: constant.Medium},
		filter.NewSpecialUseFilter(s.GetName()),
	}
	s.filterCounter = filter.NewCounter(s.GetName())
	return s
}

// Schedule schedules the balance key range operator.
func (s *balanceRangeScheduler) Schedule(cluster sche.SchedulerCluster, _ bool) ([]*operator.Operator, []plan.Plan) {
	balanceRangeCounter.Inc()
	_, job := s.conf.peek()
	if job == nil {
		balanceRangeNoJobCounter.Inc()
		return nil, nil
	}
	defer s.filterCounter.Flush()

	opInfluence := s.OpController.GetOpInfluence(cluster.GetBasicCluster(), operator.WithRangeOption(job.Ranges))
	// todo: don't prepare every times, the prepare information can be reused.
	plan, err := s.prepare(cluster, opInfluence, job)
	if err != nil {
		log.Error("failed to prepare balance key range scheduler", errs.ZapError(err))
		return nil, nil
	}

	downFilter := filter.NewRegionDownFilter()
	replicaFilter := filter.NewRegionReplicatedFilter(cluster)
	snapshotFilter := filter.NewSnapshotSendFilter(cluster.GetStores(), constant.Medium)
	pendingFilter := filter.NewRegionPendingFilter()
	baseRegionFilters := []filter.RegionFilter{downFilter, replicaFilter, snapshotFilter, pendingFilter}

	for sourceIndex, sourceStore := range plan.stores {
		plan.source = sourceStore
		plan.sourceScore = plan.score(plan.source.GetID())
		if plan.sourceScore < plan.averageScore {
			break
		}
		switch job.Rule {
		case core.LeaderScatter:
			plan.region = filter.SelectOneRegion(cluster.RandLeaderRegions(plan.sourceStoreID(), job.Ranges), nil, baseRegionFilters...)
		case core.LearnerScatter:
			plan.region = filter.SelectOneRegion(cluster.RandLearnerRegions(plan.sourceStoreID(), job.Ranges), nil, baseRegionFilters...)
		case core.PeerScatter:
			plan.region = filter.SelectOneRegion(cluster.RandFollowerRegions(plan.sourceStoreID(), job.Ranges), nil, baseRegionFilters...)
			if plan.region == nil {
				plan.region = filter.SelectOneRegion(cluster.RandLeaderRegions(plan.sourceStoreID(), job.Ranges), nil, baseRegionFilters...)
			}
			if plan.region == nil {
				plan.region = filter.SelectOneRegion(cluster.RandLearnerRegions(plan.sourceStoreID(), job.Ranges), nil, baseRegionFilters...)
			}
		}
		if plan.region == nil {
			balanceRangeNoRegionCounter.Inc()
			continue
		}
		log.Debug("select region", zap.String("scheduler", s.GetName()), zap.Uint64("region-id", plan.region.GetID()))
		// Skip hot regions.
		if cluster.IsRegionHot(plan.region) {
			log.Debug("region is hot", zap.String("scheduler", s.GetName()), zap.Uint64("region-id", plan.region.GetID()))
			balanceRangeHotCounter.Inc()
			continue
		}
		// Check region leader
		if plan.region.GetLeader() == nil {
			log.Warn("region has no leader", zap.String("scheduler", s.GetName()), zap.Uint64("region-id", plan.region.GetID()))
			balanceRangeNoLeaderCounter.Inc()
			continue
		}
		plan.fit = replicaFilter.(*filter.RegionReplicatedFilter).GetFit()
		if op := s.transferPeer(plan, plan.stores[sourceIndex+1:]); op != nil {
			op.Counters = append(op.Counters, balanceRangeNewOperatorCounter)
			return []*operator.Operator{op}, nil
		}
	}
	return nil, nil
}

// transferPeer selects the best store to create a new peer to replace the old peer.
func (s *balanceRangeScheduler) transferPeer(plan *balanceRangeSchedulerPlan, dstStores []*core.StoreInfo) *operator.Operator {
	excludeTargets := plan.region.GetStoreIDs()
	if plan.job.Rule == core.LeaderScatter {
		excludeTargets = make(map[uint64]struct{})
		excludeTargets[plan.region.GetLeader().GetStoreId()] = struct{}{}
	}
	conf := plan.GetSchedulerConfig()
	filters := s.filters
	filters = append(filters,
		filter.NewExcludedFilter(s.GetName(), nil, excludeTargets),
		filter.NewPlacementSafeguard(s.GetName(), conf, plan.GetBasicCluster(), plan.GetRuleManager(), plan.region, plan.source, plan.fit),
	)

	candidates := filter.NewCandidates(s.R, dstStores).FilterTarget(conf, nil, s.filterCounter, filters...)
	for i := range candidates.Stores {
		plan.target = candidates.Stores[len(candidates.Stores)-i-1]
		plan.targetScore = plan.score(plan.target.GetID())
		if plan.targetScore > plan.averageScore {
			break
		}
		regionID := plan.region.GetID()
		sourceID := plan.source.GetID()
		targetID := plan.target.GetID()
		if !plan.shouldBalance(s.GetName()) {
			continue
		}
		log.Debug("candidate store", zap.Uint64("region-id", regionID), zap.Uint64("source-store", sourceID), zap.Uint64("target-store", targetID))

		oldPeer := plan.region.GetStorePeer(sourceID)
		exist := false
		if plan.job.Rule == core.LeaderScatter {
			peers := plan.region.GetPeers()
			for _, peer := range peers {
				if peer.GetStoreId() == targetID {
					exist = true
					break
				}
			}
		}
		var op *operator.Operator
		var err error
		if exist {
			op, err = operator.CreateTransferLeaderOperator(s.GetName(), plan, plan.region, plan.targetStoreID(), []uint64{}, operator.OpRange)
		} else {
			newPeer := &metapb.Peer{StoreId: plan.target.GetID(), Role: oldPeer.Role}
			if plan.region.GetLeader().GetStoreId() == sourceID {
				op, err = operator.CreateReplaceLeaderPeerOperator(s.GetName(), plan, plan.region, operator.OpRange, oldPeer.GetStoreId(), newPeer, newPeer)
			} else {
				op, err = operator.CreateMovePeerOperator(s.GetName(), plan, plan.region, operator.OpRange, oldPeer.GetStoreId(), newPeer)
			}
		}

		if err != nil {
			balanceRangeCreateOpFailCounter.Inc()
			return nil
		}
		sourceLabel := strconv.FormatUint(sourceID, 10)
		targetLabel := strconv.FormatUint(targetID, 10)
		op.FinishedCounters = append(op.FinishedCounters,
			balanceDirectionCounter.WithLabelValues(s.GetName(), sourceLabel, targetLabel),
		)
		op.SetAdditionalInfo("sourceScore", strconv.FormatInt(plan.sourceScore, 10))
		op.SetAdditionalInfo("targetScore", strconv.FormatInt(plan.targetScore, 10))
		op.SetAdditionalInfo("tolerate", strconv.FormatInt(plan.tolerate, 10))
		return op
	}
	balanceRangeNoReplacementCounter.Inc()
	return nil
}

// balanceRangeSchedulerPlan is used to record the plan of balance key range scheduler.
type balanceRangeSchedulerPlan struct {
	sche.SchedulerCluster
	// stores is sorted by score desc
	stores []*core.StoreInfo
	// scoreMap records the storeID -> score
	scoreMap     map[uint64]int64
	source       *core.StoreInfo
	sourceScore  int64
	target       *core.StoreInfo
	targetScore  int64
	region       *core.RegionInfo
	fit          *placement.RegionFit
	averageScore int64
	job          *balanceRangeSchedulerJob
	opInfluence  operator.OpInfluence
	tolerate     int64
}

func (s *balanceRangeScheduler) prepare(cluster sche.SchedulerCluster, opInfluence operator.OpInfluence, job *balanceRangeSchedulerJob) (*balanceRangeSchedulerPlan, error) {
	filters := s.filters
	switch job.Engine {
	case core.EngineTiKV:
		filters = append(filters, filter.NewEngineFilter(string(types.BalanceRangeScheduler), filter.NotSpecialEngines))
	case core.EngineTiFlash:
		filters = append(filters, filter.NewEngineFilter(string(types.BalanceRangeScheduler), filter.SpecialEngines))
	default:
		return nil, errs.ErrGetSourceStore.FastGenByArgs(job.Engine)
	}
	sources := filter.SelectSourceStores(cluster.GetStores(), filters, cluster.GetSchedulerConfig(), nil, nil)
	if len(sources) <= 1 {
		return nil, errs.ErrStoresNotEnough.FastGenByArgs("no store to select")
	}

	// storeID <--> score mapping
	scoreMap := make(map[uint64]int64, len(sources))
	totalScore := int64(0)
	for _, source := range sources {
		count := 0
		for _, kr := range job.Ranges {
			switch job.Rule {
			case core.LeaderScatter:
				count += cluster.GetStoreLeaderCountByRange(source.GetID(), kr.StartKey, kr.EndKey)
			case core.PeerScatter:
				count += cluster.GetStorePeerCountByRange(source.GetID(), kr.StartKey, kr.EndKey)
			case core.LearnerScatter:
				count += cluster.GetStoreLearnerCountByRange(source.GetID(), kr.StartKey, kr.EndKey)
			}
		}
		scoreMap[source.GetID()] = int64(count)
		totalScore += int64(count)
	}

	sort.Slice(sources, func(i, j int) bool {
		rule := job.Rule
		iop := opInfluence.GetStoreInfluence(sources[i].GetID()).GetStoreInfluenceByRole(rule)
		jop := opInfluence.GetStoreInfluence(sources[j].GetID()).GetStoreInfluenceByRole(rule)
		iScore := scoreMap[sources[i].GetID()]
		jScore := scoreMap[sources[j].GetID()]
		return iScore+iop > jScore+jop
	})

	averageScore := int64(0)
	averageScore = totalScore / int64(len(sources))

	tolerantSizeRatio := int64(float64(totalScore) * adjustRatio)
	if tolerantSizeRatio < 1 {
		tolerantSizeRatio = 1
	}
	return &balanceRangeSchedulerPlan{
		SchedulerCluster: cluster,
		stores:           sources,
		scoreMap:         scoreMap,
		source:           nil,
		target:           nil,
		region:           nil,
		averageScore:     averageScore,
		job:              job,
		opInfluence:      opInfluence,
		tolerate:         tolerantSizeRatio,
	}, nil
}

func (p *balanceRangeSchedulerPlan) sourceStoreID() uint64 {
	return p.source.GetID()
}

func (p *balanceRangeSchedulerPlan) targetStoreID() uint64 {
	return p.target.GetID()
}

func (p *balanceRangeSchedulerPlan) score(storeID uint64) int64 {
	return p.scoreMap[storeID]
}

func (p *balanceRangeSchedulerPlan) shouldBalance(scheduler string) bool {
	sourceInfluence := p.opInfluence.GetStoreInfluence(p.sourceStoreID())
	sourceInf := sourceInfluence.GetStoreInfluenceByRole(p.job.Rule)
	// Sometimes, there are many remove-peer operators in the source store, we don't want to pick this store as source.
	if sourceInf < 0 {
		sourceInf = -sourceInf
	}
	// to avoid schedule too much, if A's core greater than B and C a little
	// we want that A should be moved out one region not two
	sourceScore := p.sourceScore - sourceInf - p.tolerate

	targetInfluence := p.opInfluence.GetStoreInfluence(p.targetStoreID())
	targetInf := targetInfluence.GetStoreInfluenceByRole(p.job.Rule)
	// Sometimes, there are many add-peer operators in the target store, we don't want to pick this store as target.
	if targetInf < 0 {
		targetInf = -targetInf
	}
	targetScore := p.targetScore + targetInf + p.tolerate

	// the source score must be greater than the target score
	shouldBalance := sourceScore >= targetScore
	if !shouldBalance && log.GetLevel() <= zap.DebugLevel {
		log.Debug("skip balance",
			zap.String("scheduler", scheduler),
			zap.Uint64("region-id", p.region.GetID()),
			zap.Uint64("source-store", p.sourceStoreID()),
			zap.Uint64("target-store", p.targetStoreID()),
			zap.Int64("origin-source-score", p.sourceScore),
			zap.Int64("origin-target-score", p.targetScore),
			zap.Int64("influence-source-score", sourceScore),
			zap.Int64("influence-target-score", targetScore),
			zap.Int64("average-region-score", p.averageScore),
			zap.Int64("tolerate", p.tolerate),
		)
	}
	return shouldBalance
}

// JobStatus is the status of the job.
type JobStatus int

const (
	pending JobStatus = iota
	running
	finished
	cancelled
)

func (s *JobStatus) String() string {
	switch *s {
	case pending:
		return "pending"
	case running:
		return "running"
	case finished:
		return "finished"
	case cancelled:
		return "cancelled"
	}
	return "unknown"
}

// MarshalJSON marshals to json.
func (s *JobStatus) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// UnmarshalJSON unmarshals from json.
func (s *JobStatus) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case `"running"`:
		*s = running
	case `"finished"`:
		*s = finished
	case `"cancelled"`:
		*s = cancelled
	case `"pending"`:
		*s = pending
	}
	return nil
}
