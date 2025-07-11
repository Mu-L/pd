// Copyright 2022 TiKV Project Authors.
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

package keyspace

import (
	"bytes"
	"context"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/kvproto/pkg/keyspacepb"
	"github.com/pingcap/log"

	"github.com/tikv/pd/pkg/errs"
	"github.com/tikv/pd/pkg/id"
	"github.com/tikv/pd/pkg/keyspace/constant"
	"github.com/tikv/pd/pkg/schedule/core"
	"github.com/tikv/pd/pkg/schedule/labeler"
	"github.com/tikv/pd/pkg/slice"
	"github.com/tikv/pd/pkg/storage/endpoint"
	"github.com/tikv/pd/pkg/storage/kv"
	"github.com/tikv/pd/pkg/utils/etcdutil"
	"github.com/tikv/pd/pkg/utils/keypath"
	"github.com/tikv/pd/pkg/utils/logutil"
	"github.com/tikv/pd/pkg/utils/syncutil"
	"github.com/tikv/pd/pkg/versioninfo/kerneltype"
)

const (
	// AllocStep set idAllocator's step when write persistent window boundary.
	// Use a lower value for denser idAllocation in the event of frequent pd leader change.
	AllocStep = uint64(100)
	// regionLabelIDPrefix is used to prefix the keyspace region label.
	regionLabelIDPrefix = "keyspaces/"
	// regionLabelKey is the key for keyspace id in keyspace region label.
	regionLabelKey = "id"
	// UserKindKey is the key for user kind in keyspace config.
	UserKindKey = "user_kind"
	// TSOKeyspaceGroupIDKey is the key for tso keyspace group id in keyspace config.
	// Note: Config[TSOKeyspaceGroupIDKey] is only used to judge whether there is keyspace group id.
	// It will not update the keyspace group id when merging or splitting.
	TSOKeyspaceGroupIDKey = "tso_keyspace_group_id"
	// GCManagementType is the key for gc_management_type in keyspace config.
	// If `gc_management_type` is `unified`, it means the current keyspace requires a tidb without 'keyspace-name'
	// configured to run a unified GC worker to calculate a unified GC state.
	// If `gc_management_type` is `keyspace_level` it means the current keyspace can calculate GC states by its own.
	GCManagementType = "gc_management_type"
	// KeyspaceLevelGC is a type of gc_management_type used to indicate that this keyspace independently manages its own
	// GC states
	KeyspaceLevelGC = "keyspace_level"
	// UnifiedGC is a type of gc_management_type used to indicate that the GC states of this keyspace is managed
	// in a unified way (managed by the NullKeyspace).
	UnifiedGC = "unified"
)

// Config is the interface for keyspace config.
type Config interface {
	GetPreAlloc() []string
	ToWaitRegionSplit() bool
	GetWaitRegionSplitTimeout() time.Duration
	GetCheckRegionSplitInterval() time.Duration
}

// Manager manages keyspace related data.
// It validates requests and provides concurrency control.
type Manager struct {
	// ctx is the context of the manager, to be used in transaction.
	ctx context.Context
	// metaLock guards keyspace meta.
	metaLock *syncutil.LockGroup
	// idAllocator allocates keyspace id.
	idAllocator id.Allocator
	// store is the storage for keyspace related information.
	store endpoint.KeyspaceStorage
	// rc is the raft cluster of the server.
	cluster core.ClusterInformer
	// config is the configurations of the manager.
	config Config
	// kgm is the keyspace group manager of the server.
	kgm *GroupManager
	// nextPatrolStartID is the next start id of keyspace assignment patrol.
	nextPatrolStartID uint32
}

// CreateKeyspaceRequest represents necessary arguments to create a keyspace.
type CreateKeyspaceRequest struct {
	// Name of the keyspace to be created.
	// Using an existing name will result in error.
	Name   string
	Config map[string]string
	// CreateTime is the timestamp used to record creation time.
	CreateTime int64
}

// CreateKeyspaceByIDRequest represents necessary arguments to create a keyspace.
type CreateKeyspaceByIDRequest struct {
	// ID of the keyspace to be created.
	// Using an existing ID will result in error.
	ID *uint32
	// Name of the keyspace to be created.
	// Using an existing name will result in error.
	Name   string
	Config map[string]string
	// CreateTime is the timestamp used to record creation time.
	CreateTime int64
}

// NewKeyspaceManager creates a Manager of keyspace related data.
func NewKeyspaceManager(
	ctx context.Context,
	store endpoint.KeyspaceStorage,
	cluster core.ClusterInformer,
	idAllocator id.Allocator,
	config Config,
	kgm *GroupManager,
) *Manager {
	return &Manager{
		ctx: ctx,
		// Remove the lock of the given key from the lock group when unlock to
		// keep minimal working set, which is suited for low qps, non-time-critical
		// and non-consecutive large key space scenarios. One of scenarios for
		// last use case is keyspace group split loads non-consecutive keyspace meta
		// in batches and lock all loaded keyspace meta within a batch at the same time.
		metaLock:          syncutil.NewLockGroup(syncutil.WithRemoveEntryOnUnlock(true)),
		idAllocator:       idAllocator,
		store:             store,
		cluster:           cluster,
		config:            config,
		kgm:               kgm,
		nextPatrolStartID: constant.StartKeyspaceID,
	}
}

// Bootstrap saves default keyspace info.
func (manager *Manager) Bootstrap() error {
	bootstrapKeyspaceID := GetBootstrapKeyspaceID()
	bootstrapKeyspaceName := GetBootstrapKeyspaceName()
	err := manager.initReserveKeyspace(bootstrapKeyspaceID, bootstrapKeyspaceName)
	if err != nil {
		return err
	}
	// Initialize pre-alloc keyspace.
	preAlloc := manager.config.GetPreAlloc()
	for _, keyspaceName := range preAlloc {
		go func() {
			config, err := manager.kgm.GetKeyspaceConfigByKind(endpoint.Basic)
			if err != nil {
				log.Error("[keyspace] failed to get keyspace config for pre-alloc keyspace", zap.String("keyspaceName", keyspaceName), zap.Error(err))
				return
			}
			req := &CreateKeyspaceRequest{
				Name:       keyspaceName,
				CreateTime: time.Now().Unix(),
				Config:     config,
			}
			keyspace, err := manager.CreateKeyspace(req)
			// Ignore the keyspaceExists error for the same reason as saving default keyspace.
			if err != nil && err != errs.ErrKeyspaceExists {
				log.Error("[keyspace] failed to create pre-alloc keyspace", zap.String("keyspaceName", keyspaceName), zap.Error(err))
				return
			}
			if err := manager.kgm.UpdateKeyspaceForGroup(endpoint.Basic, config[TSOKeyspaceGroupIDKey], keyspace.GetId(), opAdd); err != nil {
				log.Error("[keyspace] failed to update pre-alloc keyspace for group", zap.String("keyspaceName", keyspaceName), zap.Error(err))
				return
			}
		}()
	}
	return nil
}

func (manager *Manager) initReserveKeyspace(id uint32, name string) error {
	// Split Keyspace Region for default/system keyspace.
	if err := manager.splitKeyspaceRegion(id, false); err != nil {
		return err
	}
	now := time.Now().Unix()
	meta := &keyspacepb.KeyspaceMeta{
		Id:             id,
		Name:           name,
		State:          keyspacepb.KeyspaceState_ENABLED,
		CreatedAt:      now,
		StateChangedAt: now,
	}

	config, err := manager.kgm.GetKeyspaceConfigByKind(endpoint.Basic)
	if err != nil {
		return err
	}
	// It is needed to set for system keyspace in next-gen.
	if id == constant.SystemKeyspaceID {
		config[GCManagementType] = KeyspaceLevelGC
	}
	meta.Config = config
	err = manager.saveNewKeyspace(meta)
	// It's possible that default/system keyspace already exists in the storage (e.g. PD restart/recover),
	// so we ignore the keyspaceExists error.
	if err != nil && err != errs.ErrKeyspaceExists {
		return err
	}
	return manager.kgm.UpdateKeyspaceForGroup(endpoint.Basic, config[TSOKeyspaceGroupIDKey], meta.GetId(), opAdd)
}

// UpdateConfig update keyspace manager's config.
func (manager *Manager) UpdateConfig(cfg Config) {
	manager.config = cfg
}

// CreateKeyspace create a keyspace meta with given config and save it to storage.
func (manager *Manager) CreateKeyspace(request *CreateKeyspaceRequest) (*keyspacepb.KeyspaceMeta, error) {
	// Validate purposed name's legality.
	if err := validateName(request.Name); err != nil {
		return nil, err
	}
	// Allocate new keyspaceID.
	newID, err := manager.allocID()
	if err != nil {
		return nil, err
	}
	userKind := endpoint.StringUserKind(request.Config[UserKindKey])
	config, err := manager.kgm.GetKeyspaceConfigByKind(userKind)
	if err != nil {
		return nil, err
	}
	if len(config) != 0 {
		if request.Config == nil {
			request.Config = config
		} else {
			request.Config[TSOKeyspaceGroupIDKey] = config[TSOKeyspaceGroupIDKey]
			request.Config[UserKindKey] = config[UserKindKey]
		}
	}
	// Set default value of GCManagementType to KeyspaceLevelGC for NextGen
	if kerneltype.IsNextGen() {
		if v, ok := request.Config[GCManagementType]; !ok || len(v) == 0 {
			request.Config[GCManagementType] = KeyspaceLevelGC
		}
	}
	// Create a disabled keyspace meta for tikv-server to get the config on keyspace split.
	keyspace := &keyspacepb.KeyspaceMeta{
		Id:             newID,
		Name:           request.Name,
		State:          keyspacepb.KeyspaceState_DISABLED,
		CreatedAt:      request.CreateTime,
		StateChangedAt: request.CreateTime,
		Config:         request.Config,
	}
	err = manager.saveNewKeyspace(keyspace)
	if err != nil {
		log.Warn("[keyspace] failed to save keyspace before split",
			zap.Uint32("keyspace-id", keyspace.GetId()),
			zap.String("name", keyspace.GetName()),
			zap.Error(err),
		)
		return nil, err
	}
	// Split keyspace region.
	err = manager.splitKeyspaceRegion(newID, manager.config.ToWaitRegionSplit())
	if err != nil {
		err2 := manager.store.RunInTxn(manager.ctx, func(txn kv.Txn) error {
			idPath := keypath.KeyspaceIDPath(request.Name)
			metaPath := keypath.KeyspaceMetaPath(newID)
			e := txn.Remove(idPath)
			if e != nil {
				return e
			}
			return txn.Remove(metaPath)
		})
		if err2 != nil {
			log.Warn("[keyspace] failed to remove pre-created keyspace after split failed",
				zap.Uint32("keyspace-id", keyspace.GetId()),
				zap.String("name", keyspace.GetName()),
				zap.Error(err2),
			)
		}
		return nil, err
	}
	// enable the keyspace metadata after split.
	keyspace.State = keyspacepb.KeyspaceState_ENABLED
	_, err = manager.UpdateKeyspaceStateByID(newID, keyspacepb.KeyspaceState_ENABLED, request.CreateTime)
	if err != nil {
		log.Warn("[keyspace] failed to create keyspace",
			zap.Uint32("keyspace-id", keyspace.GetId()),
			zap.String("name", keyspace.GetName()),
			zap.Error(err),
		)
		return nil, err
	}
	if err := manager.kgm.UpdateKeyspaceForGroup(userKind, config[TSOKeyspaceGroupIDKey], keyspace.GetId(), opAdd); err != nil {
		return nil, err
	}
	log.Info("[keyspace] keyspace created",
		zap.Uint32("keyspace-id", keyspace.GetId()),
		zap.String("name", keyspace.GetName()),
	)
	return keyspace, nil
}

// CreateKeyspaceByID create a keyspace meta with given config and save it to storage.
func (manager *Manager) CreateKeyspaceByID(request *CreateKeyspaceByIDRequest) (*keyspacepb.KeyspaceMeta, error) {
	if request.ID == nil {
		return nil, errors.New("keyspace id is empty")
	}
	id := *request.ID
	name := request.Name
	if len(name) == 0 {
		return nil, errors.New("keyspace name is empty")
	}
	// Validate purposed name's legality.
	if err := validateName(name); err != nil {
		return nil, err
	}
	userKind := endpoint.StringUserKind(request.Config[UserKindKey])
	config, err := manager.kgm.GetKeyspaceConfigByKind(userKind)
	if err != nil {
		return nil, err
	}
	if len(config) != 0 {
		if request.Config == nil {
			request.Config = config
		} else {
			request.Config[TSOKeyspaceGroupIDKey] = config[TSOKeyspaceGroupIDKey]
			request.Config[UserKindKey] = config[UserKindKey]
		}
	}
	// Create a disabled keyspace meta for tikv-server to get the config on keyspace split.
	keyspace := &keyspacepb.KeyspaceMeta{
		Id:             id,
		Name:           name,
		State:          keyspacepb.KeyspaceState_DISABLED,
		CreatedAt:      request.CreateTime,
		StateChangedAt: request.CreateTime,
		Config:         request.Config,
	}
	err = manager.saveNewKeyspace(keyspace)
	if err != nil {
		log.Warn("[keyspace] failed to save keyspace before split",
			zap.Uint32("keyspace-id", keyspace.GetId()),
			zap.String("keyspace-name", keyspace.GetName()),
			zap.Error(err),
		)
		return nil, err
	}
	// Split keyspace region.
	err = manager.splitKeyspaceRegion(id, manager.config.ToWaitRegionSplit())
	if err != nil {
		err2 := manager.store.RunInTxn(manager.ctx, func(txn kv.Txn) error {
			metaPath := keypath.KeyspaceMetaPath(id)
			return txn.Remove(metaPath)
		})
		if err2 != nil {
			log.Warn("[keyspace] failed to remove pre-created keyspace after split failed",
				zap.Uint32("keyspace-id", keyspace.GetId()),
				zap.String("keyspace-name", keyspace.GetName()),
				zap.Error(err2),
			)
		}
		return nil, err
	}
	// enable the keyspace metadata after split.
	keyspace.State = keyspacepb.KeyspaceState_ENABLED
	_, err = manager.UpdateKeyspaceStateByID(id, keyspacepb.KeyspaceState_ENABLED, request.CreateTime)
	if err != nil {
		log.Warn("[keyspace] failed to create keyspace",
			zap.Uint32("keyspace-id", keyspace.GetId()),
			zap.String("keyspace-name", keyspace.GetName()),
			zap.Error(err),
		)
		return nil, err
	}
	if err := manager.kgm.UpdateKeyspaceForGroup(userKind, config[TSOKeyspaceGroupIDKey], keyspace.GetId(), opAdd); err != nil {
		return nil, err
	}
	log.Info("[keyspace] keyspace created",
		zap.Uint32("keyspace-id", keyspace.GetId()),
		zap.String("keyspace-name", keyspace.GetName()),
	)
	return keyspace, nil
}

func (manager *Manager) saveNewKeyspace(keyspace *keyspacepb.KeyspaceMeta) error {
	manager.metaLock.Lock(keyspace.Id)
	defer manager.metaLock.Unlock(keyspace.Id)

	return manager.store.RunInTxn(manager.ctx, func(txn kv.Txn) error {
		// Save keyspace ID.
		// Check if keyspace with that name already exists.
		nameExists, _, err := manager.store.LoadKeyspaceID(txn, keyspace.Name)
		if err != nil {
			return err
		}
		if nameExists {
			return errs.ErrKeyspaceExists
		}
		err = manager.store.SaveKeyspaceID(txn, keyspace.Id, keyspace.Name)
		if err != nil {
			return err
		}
		// Save keyspace meta.
		// Check if keyspace with that id already exists.
		loadedMeta, err := manager.store.LoadKeyspaceMeta(txn, keyspace.Id)
		if err != nil {
			return err
		}
		if loadedMeta != nil {
			return errs.ErrKeyspaceExists
		}
		return manager.store.SaveKeyspaceMeta(txn, keyspace)
	})
}

// splitKeyspaceRegion add keyspace's boundaries to region label. The corresponding
// region will then be split by Coordinator's patrolRegion.
func (manager *Manager) splitKeyspaceRegion(id uint32, waitRegionSplit bool) (err error) {
	failpoint.Inject("skipSplitRegion", func() {
		failpoint.Return(nil)
	})

	start := time.Now()
	keyspaceRule := MakeLabelRule(id)
	cl, ok := manager.cluster.(interface{ GetRegionLabeler() *labeler.RegionLabeler })
	if !ok {
		return errors.New("cluster does not support region label")
	}
	err = cl.GetRegionLabeler().SetLabelRule(keyspaceRule)
	if err != nil {
		log.Warn("[keyspace] failed to add region label for keyspace",
			zap.Uint32("keyspace-id", id),
			zap.Error(err),
		)
		return err
	}
	defer func() {
		if err != nil {
			if err := cl.GetRegionLabeler().DeleteLabelRule(keyspaceRule.ID); err != nil {
				log.Warn("[keyspace] failed to delete region label for keyspace",
					zap.Uint32("keyspace-id", id),
					zap.Error(err),
				)
			}
		}
	}()

	if waitRegionSplit {
		ranges := keyspaceRule.Data.([]*labeler.KeyRangeRule)
		if len(ranges) < 2 {
			log.Warn("[keyspace] failed to split keyspace region with insufficient range", logutil.ZapRedactString("label-rule", keyspaceRule.String()))
			return errs.ErrRegionSplitFailed
		}
		rawLeftBound, rawRightBound := ranges[0].StartKey, ranges[0].EndKey
		txnLeftBound, txnRightBound := ranges[1].StartKey, ranges[1].EndKey

		ticker := time.NewTicker(manager.config.GetCheckRegionSplitInterval())
		timer := time.NewTimer(manager.config.GetWaitRegionSplitTimeout())
		defer func() {
			ticker.Stop()
			timer.Stop()
		}()
		for {
			select {
			case <-ticker.C:
				c := manager.cluster.GetBasicCluster()
				region := c.GetRegionByKey(rawLeftBound)
				if region == nil || !bytes.Equal(region.GetStartKey(), rawLeftBound) {
					continue
				}
				region = c.GetRegionByKey(rawRightBound)
				if region == nil || !bytes.Equal(region.GetStartKey(), rawRightBound) {
					continue
				}
				region = c.GetRegionByKey(txnLeftBound)
				if region == nil || !bytes.Equal(region.GetStartKey(), txnLeftBound) {
					continue
				}
				region = c.GetRegionByKey(txnRightBound)
				if region == nil || !bytes.Equal(region.GetStartKey(), txnRightBound) {
					continue
				}
				// Note: we reset the ticker here to support updating configuration dynamically.
				ticker.Reset(manager.config.GetCheckRegionSplitInterval())
			case <-timer.C:
				log.Warn("[keyspace] wait region split timeout",
					zap.Uint32("keyspace-id", id),
					zap.Error(err),
				)
				err = errs.ErrRegionSplitTimeout
				return
			}
			log.Info("[keyspace] wait region split successfully", zap.Uint32("keyspace-id", id))
			break
		}
	}

	log.Info("[keyspace] added region label for keyspace",
		zap.Uint32("keyspace-id", id),
		logutil.ZapRedactString("label-rule", keyspaceRule.String()),
		zap.Duration("takes", time.Since(start)),
	)
	return
}

// LoadKeyspace returns the keyspace specified by name.
// It returns error if loading or unmarshalling met error or if keyspace does not exist.
func (manager *Manager) LoadKeyspace(name string) (*keyspacepb.KeyspaceMeta, error) {
	var meta *keyspacepb.KeyspaceMeta
	err := manager.store.RunInTxn(manager.ctx, func(txn kv.Txn) error {
		loaded, id, err := manager.store.LoadKeyspaceID(txn, name)
		if err != nil {
			return err
		}
		if !loaded {
			return errs.ErrKeyspaceNotFound
		}
		meta, err = manager.store.LoadKeyspaceMeta(txn, id)
		if err != nil {
			return err
		}
		if meta == nil {
			return errs.ErrKeyspaceNotFound
		}
		return nil
	})
	return meta, err
}

// LoadKeyspaceByID returns the keyspace specified by id.
// It returns error if loading or unmarshalling met error or if keyspace does not exist.
func (manager *Manager) LoadKeyspaceByID(spaceID uint32) (*keyspacepb.KeyspaceMeta, error) {
	var (
		meta *keyspacepb.KeyspaceMeta
		err  error
	)
	err = manager.store.RunInTxn(manager.ctx, func(txn kv.Txn) error {
		meta, err = manager.store.LoadKeyspaceMeta(txn, spaceID)
		if err != nil {
			return err
		}
		if meta == nil {
			return errs.ErrKeyspaceNotFound
		}
		return nil
	})
	return meta, err
}

// Mutation represents a single operation to be applied on keyspace config.
type Mutation struct {
	Op    OpType
	Key   string
	Value string
}

// OpType defines the type of keyspace config operation.
type OpType int

const (
	// OpPut denotes a put operation onto the given config.
	// If target key exists, it will put a new value,
	// otherwise, it creates a new config entry.
	OpPut OpType = iota + 1 // Operation type starts at 1.
	// OpDel denotes a deletion operation onto the given config.
	// Note: OpDel is idempotent, deleting a non-existing key
	// will not result in error.
	OpDel
)

// UpdateKeyspaceConfig changes target keyspace's config in the order specified in mutations.
// It returns error if saving failed, operation not allowed, or if keyspace not exists.
func (manager *Manager) UpdateKeyspaceConfig(name string, mutations []*Mutation) (*keyspacepb.KeyspaceMeta, error) {
	var meta *keyspacepb.KeyspaceMeta
	oldConfig := make(map[string]string)
	err := manager.store.RunInTxn(manager.ctx, func(txn kv.Txn) error {
		// First get KeyspaceID from Name.
		loaded, id, err := manager.store.LoadKeyspaceID(txn, name)
		if err != nil {
			return err
		}
		if !loaded {
			return errs.ErrKeyspaceNotFound
		}
		manager.metaLock.Lock(id)
		defer manager.metaLock.Unlock(id)
		// Load keyspace by id.
		meta, err = manager.store.LoadKeyspaceMeta(txn, id)
		if err != nil {
			return err
		}
		if meta == nil {
			return errs.ErrKeyspaceNotFound
		}
		// Only keyspace with state listed in allowChangeConfig are allowed to change their config.
		if !slice.Contains(allowChangeConfig, meta.GetState()) {
			return errors.Errorf("cannot change config for keyspace with state %s", meta.GetState().String())
		}
		// Initialize meta's config map if it's nil.
		if meta.GetConfig() == nil {
			meta.Config = map[string]string{}
		}
		for k, v := range meta.GetConfig() {
			oldConfig[k] = v
		}
		// Update keyspace config according to mutations.
		for _, mutation := range mutations {
			switch mutation.Op {
			case OpPut:
				meta.Config[mutation.Key] = mutation.Value
			case OpDel:
				delete(meta.Config, mutation.Key)
			default:
				return errs.ErrIllegalOperation
			}
		}
		newConfig := meta.GetConfig()
		oldUserKind := endpoint.StringUserKind(oldConfig[UserKindKey])
		newUserKind := endpoint.StringUserKind(newConfig[UserKindKey])
		oldID := oldConfig[TSOKeyspaceGroupIDKey]
		newID := newConfig[TSOKeyspaceGroupIDKey]
		needUpdate := oldUserKind != newUserKind || oldID != newID
		if needUpdate {
			if err := manager.kgm.UpdateKeyspaceGroup(oldID, newID, oldUserKind, newUserKind, meta.GetId()); err != nil {
				return err
			}
		}
		// Save the updated keyspace meta.
		if err := manager.store.SaveKeyspaceMeta(txn, meta); err != nil {
			if needUpdate {
				if err := manager.kgm.UpdateKeyspaceGroup(newID, oldID, newUserKind, oldUserKind, meta.GetId()); err != nil {
					log.Error("failed to revert keyspace group", zap.Error(err))
				}
			}
			return err
		}
		return nil
	})
	if err != nil {
		log.Warn("[keyspace] failed to update keyspace config",
			zap.Uint32("keyspace-id", meta.GetId()),
			zap.String("name", meta.GetName()),
			zap.Error(err),
		)
		return nil, err
	}
	log.Info("[keyspace] keyspace config updated",
		zap.Uint32("keyspace-id", meta.GetId()),
		zap.String("name", meta.GetName()),
		zap.Any("new-config", meta.GetConfig()),
	)
	return meta, nil
}

// UpdateKeyspaceState updates target keyspace to the given state if it's not already in that state.
// It returns error if saving failed, operation not allowed, or if keyspace not exists.
func (manager *Manager) UpdateKeyspaceState(name string, newState keyspacepb.KeyspaceState, now int64) (*keyspacepb.KeyspaceMeta, error) {
	if isProtectedKeyspaceName(name) {
		err := newModifyProtectedKeyspaceError()
		log.Warn("[keyspace] failed to update keyspace config", errs.ZapError(err))
		return nil, err
	}
	var meta *keyspacepb.KeyspaceMeta
	err := manager.store.RunInTxn(manager.ctx, func(txn kv.Txn) error {
		// First get KeyspaceID from Name.
		loaded, id, err := manager.store.LoadKeyspaceID(txn, name)
		if err != nil {
			return err
		}
		if !loaded {
			return errs.ErrKeyspaceNotFound
		}
		manager.metaLock.Lock(id)
		defer manager.metaLock.Unlock(id)
		// Load keyspace by id.
		meta, err = manager.store.LoadKeyspaceMeta(txn, id)
		if err != nil {
			return err
		}
		if meta == nil {
			return errs.ErrKeyspaceNotFound
		}
		// Update keyspace meta.
		if err = updateKeyspaceState(meta, newState, now); err != nil {
			return err
		}
		return manager.store.SaveKeyspaceMeta(txn, meta)
	})
	if err != nil {
		log.Warn("[keyspace] failed to update keyspace config",
			zap.Uint32("keyspace-id", meta.GetId()),
			zap.String("name", meta.GetName()),
			zap.Error(err),
		)
		return nil, err
	}
	log.Info("[keyspace] keyspace state updated",
		zap.Uint32("id", meta.GetId()),
		zap.String("keyspace-id", meta.GetName()),
		zap.String("new-state", newState.String()),
	)
	return meta, nil
}

// UpdateKeyspaceStateByID updates target keyspace to the given state if it's not already in that state.
// It returns error if saving failed, operation not allowed, or if keyspace not exists.
func (manager *Manager) UpdateKeyspaceStateByID(id uint32, newState keyspacepb.KeyspaceState, now int64) (*keyspacepb.KeyspaceMeta, error) {
	if isProtectedKeyspaceID(id) {
		err := newModifyProtectedKeyspaceError()
		log.Warn("[keyspace] failed to update keyspace config", errs.ZapError(err))
		return nil, err
	}
	var meta *keyspacepb.KeyspaceMeta
	var err error
	err = manager.store.RunInTxn(manager.ctx, func(txn kv.Txn) error {
		manager.metaLock.Lock(id)
		defer manager.metaLock.Unlock(id)
		// Load keyspace by id.
		meta, err = manager.store.LoadKeyspaceMeta(txn, id)
		if err != nil {
			return err
		}
		if meta == nil {
			return errs.ErrKeyspaceNotFound
		}
		// Update keyspace meta.
		if err = updateKeyspaceState(meta, newState, now); err != nil {
			return err
		}
		return manager.store.SaveKeyspaceMeta(txn, meta)
	})
	if err != nil {
		log.Warn("[keyspace] failed to update keyspace config",
			zap.Uint32("keyspace-id", meta.GetId()),
			zap.String("name", meta.GetName()),
			zap.Error(err),
		)
		return nil, err
	}
	log.Info("[keyspace] keyspace state updated",
		zap.Uint32("keyspace-id", meta.GetId()),
		zap.String("name", meta.GetName()),
		zap.String("new-state", newState.String()),
	)
	return meta, nil
}

// updateKeyspaceState updates keyspace meta and record the update time.
func updateKeyspaceState(meta *keyspacepb.KeyspaceMeta, newState keyspacepb.KeyspaceState, now int64) error {
	// If already in the target state, do nothing and return.
	if meta.GetState() == newState {
		return nil
	}
	// Consult state transition table to check if the operation is legal.
	if !slice.Contains(stateTransitionTable[meta.GetState()], newState) {
		return errors.Errorf("cannot change keyspace state from %s to %s", meta.GetState().String(), newState.String())
	}
	// If the operation is legal, update keyspace state and change time.
	meta.State = newState
	meta.StateChangedAt = now
	return nil
}

// LoadRangeKeyspace load up to limit keyspaces starting from keyspace with startID.
func (manager *Manager) LoadRangeKeyspace(startID uint32, limit int) ([]*keyspacepb.KeyspaceMeta, error) {
	// Load Start should fall within acceptable ID range.
	if startID > constant.MaxValidKeyspaceID {
		return nil, errors.Errorf("startID of the scan %d exceeds spaceID Max %d", startID, constant.MaxValidKeyspaceID)
	}
	var (
		keyspaces []*keyspacepb.KeyspaceMeta
		err       error
	)
	err = manager.store.RunInTxn(manager.ctx, func(txn kv.Txn) error {
		keyspaces, err = manager.store.LoadRangeKeyspace(txn, startID, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return keyspaces, nil
}

// allocID allocate a new keyspace id.
func (manager *Manager) allocID() (uint32, error) {
	id64, _, err := manager.idAllocator.Alloc(1)
	if err != nil {
		return 0, err
	}
	id32 := uint32(id64)
	if err = validateID(id32); err != nil {
		return 0, err
	}
	return id32, nil
}

// PatrolKeyspaceAssignment is used to patrol all keyspaces and assign them to the keyspace groups.
func (manager *Manager) PatrolKeyspaceAssignment(startKeyspaceID, endKeyspaceID uint32) error {
	if startKeyspaceID > manager.nextPatrolStartID {
		manager.nextPatrolStartID = startKeyspaceID
	}
	if endKeyspaceID != 0 && endKeyspaceID < manager.nextPatrolStartID {
		log.Info("[keyspace] end keyspace id is smaller than the next patrol start id, skip patrol",
			zap.Uint32("end-keyspace-id", endKeyspaceID),
			zap.Uint32("next-patrol-start-id", manager.nextPatrolStartID))
		return nil
	}
	var (
		// Some statistics info.
		start                  = time.Now()
		patrolledKeyspaceCount uint64
		assignedKeyspaceCount  uint64
		// The current start ID of the patrol, used for logging.
		currentStartID = manager.nextPatrolStartID
		// The next start ID of the patrol, used for the next patrol.
		nextStartID  = currentStartID
		moreToPatrol = true
		err          error
	)
	defer func() {
		log.Debug("[keyspace] patrol keyspace assignment finished",
			zap.Duration("cost", time.Since(start)),
			zap.Uint64("patrolled-keyspace-count", patrolledKeyspaceCount),
			zap.Uint64("assigned-keyspace-count", assignedKeyspaceCount),
			zap.Int("batch-size", etcdutil.MaxEtcdTxnOps),
			zap.Uint32("start-keyspace-id", startKeyspaceID),
			zap.Uint32("end-keyspace-id", endKeyspaceID),
			zap.Uint32("current-start-id", currentStartID),
			zap.Uint32("next-start-id", nextStartID),
		)
	}()
	for moreToPatrol {
		var defaultKeyspaceGroup *endpoint.KeyspaceGroup
		err = manager.store.RunInTxn(manager.ctx, func(txn kv.Txn) error {
			var err error
			defaultKeyspaceGroup, err = manager.kgm.store.LoadKeyspaceGroup(txn, constant.DefaultKeyspaceGroupID)
			if err != nil {
				return err
			}
			if defaultKeyspaceGroup == nil {
				return errors.Errorf("default keyspace group %d not found", constant.DefaultKeyspaceGroupID)
			}
			if defaultKeyspaceGroup.IsSplitting() {
				return errs.ErrKeyspaceGroupInSplit.FastGenByArgs(constant.DefaultKeyspaceGroupID)
			}
			if defaultKeyspaceGroup.IsMerging() {
				return errs.ErrKeyspaceGroupInMerging.FastGenByArgs(constant.DefaultKeyspaceGroupID)
			}
			keyspaces, err := manager.store.LoadRangeKeyspace(txn, manager.nextPatrolStartID, etcdutil.MaxEtcdTxnOps)
			if err != nil {
				return err
			}
			keyspaceNum := len(keyspaces)
			// If there are more than one keyspace, update the current and next start IDs.
			if keyspaceNum > 0 {
				currentStartID = keyspaces[0].GetId()
				nextStartID = keyspaces[keyspaceNum-1].GetId() + 1
			}
			// If there are less than ` etcdutil.MaxEtcdTxnOps` keyspaces or the next start ID reaches the end,
			// there is no need to patrol again.
			moreToPatrol = keyspaceNum == etcdutil.MaxEtcdTxnOps
			var (
				assigned            = false
				keyspaceIDsToUnlock = make([]uint32, 0, keyspaceNum)
			)
			defer func() {
				for _, id := range keyspaceIDsToUnlock {
					manager.metaLock.Unlock(id)
				}
			}()
			for _, ks := range keyspaces {
				if ks == nil {
					continue
				}
				if endKeyspaceID != 0 && ks.Id > endKeyspaceID {
					moreToPatrol = false
					break
				}
				patrolledKeyspaceCount++
				manager.metaLock.Lock(ks.Id)
				if ks.Config == nil {
					ks.Config = make(map[string]string, 1)
				} else if _, ok := ks.Config[TSOKeyspaceGroupIDKey]; ok {
					// If the keyspace already has a group ID, skip it.
					manager.metaLock.Unlock(ks.Id)
					continue
				}
				// Unlock the keyspace meta lock after the whole txn.
				keyspaceIDsToUnlock = append(keyspaceIDsToUnlock, ks.Id)
				// If the keyspace doesn't have a group ID, assign it to the default keyspace group.
				if !slice.Contains(defaultKeyspaceGroup.Keyspaces, ks.Id) {
					defaultKeyspaceGroup.Keyspaces = append(defaultKeyspaceGroup.Keyspaces, ks.Id)
					// Only save the keyspace group meta if any keyspace is assigned to it.
					assigned = true
				}
				ks.Config[TSOKeyspaceGroupIDKey] = strconv.FormatUint(uint64(constant.DefaultKeyspaceGroupID), 10)
				err = manager.store.SaveKeyspaceMeta(txn, ks)
				if err != nil {
					log.Error("[keyspace] failed to save keyspace meta during patrol",
						zap.Int("batch-size", etcdutil.MaxEtcdTxnOps),
						zap.Uint32("start-keyspace-id", startKeyspaceID),
						zap.Uint32("end-keyspace-id", endKeyspaceID),
						zap.Uint32("current-start-id", currentStartID),
						zap.Uint32("next-start-id", nextStartID),
						zap.Uint32("keyspace-id", ks.Id), zap.Error(err))
					return err
				}
				assignedKeyspaceCount++
			}
			if assigned {
				err = manager.kgm.store.SaveKeyspaceGroup(txn, defaultKeyspaceGroup)
				if err != nil {
					log.Error("[keyspace] failed to save default keyspace group meta during patrol",
						zap.Int("batch-size", etcdutil.MaxEtcdTxnOps),
						zap.Uint32("start-keyspace-id", startKeyspaceID),
						zap.Uint32("end-keyspace-id", endKeyspaceID),
						zap.Uint32("current-start-id", currentStartID),
						zap.Uint32("next-start-id", nextStartID), zap.Error(err))
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		manager.kgm.Lock()
		manager.kgm.groups[endpoint.StringUserKind(defaultKeyspaceGroup.UserKind)].Put(defaultKeyspaceGroup)
		manager.kgm.Unlock()
		// If all keyspaces in the current batch are assigned, update the next start ID.
		manager.nextPatrolStartID = nextStartID
	}
	return nil
}
