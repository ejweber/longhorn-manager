package scheduler

import (
	"fmt"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/longhorn/longhorn-manager/datastore"
	longhorn "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
	"github.com/longhorn/longhorn-manager/types"
	"github.com/longhorn/longhorn-manager/util"
)

const (
	FailedReplicaMaxRetryCount = 5
)

type ReplicaScheduler struct {
	ds *datastore.DataStore
}

type Disk struct {
	longhorn.DiskSpec
	*longhorn.DiskStatus
	NodeID string
}

type DiskSchedulingInfo struct {
	StorageAvailable           int64
	StorageMaximum             int64
	StorageReserved            int64
	StorageScheduled           int64
	OverProvisioningPercentage int64
	MinimalAvailablePercentage int64
}

func NewReplicaScheduler(ds *datastore.DataStore) *ReplicaScheduler {
	rcScheduler := &ReplicaScheduler{
		ds: ds,
	}
	return rcScheduler
}

// ScheduleReplica will return (nil, nil) for unschedulable replica
func (rcs *ReplicaScheduler) ScheduleReplica(replica *longhorn.Replica, replicas map[string]*longhorn.Replica, volume *longhorn.Volume) (*longhorn.Replica, util.MultiError, error) {
	// only called when replica is starting for the first time
	if replica.Spec.NodeID != "" {
		return nil, nil, fmt.Errorf("BUG: Replica %v has been scheduled to node %v", replica.Name, replica.Spec.NodeID)
	}

	// not to schedule a replica failed and unused before.
	if replica.Spec.HealthyAt == "" && replica.Spec.FailedAt != "" {
		logrus.WithField("replica", replica.Name).Warn("Failed replica is not scheduled")
		return nil, nil, nil
	}

	// get all hosts
	nodesInfo, err := rcs.getNodeInfo()
	if err != nil {
		return nil, nil, err
	}

	nodeCandidates, multiError := rcs.getNodeCandidates(nodesInfo, replica)
	if len(nodeCandidates) == 0 {
		logrus.Errorf("There's no available node for replica %v, size %v", replica.ObjectMeta.Name, replica.Spec.VolumeSize)
		return nil, multiError, nil
	}

	nodeDisksMap := map[string]map[string]struct{}{}
	for _, node := range nodeCandidates {
		disks := map[string]struct{}{}
		for diskName, diskStatus := range node.Status.DiskStatus {
			diskSpec, exists := node.Spec.Disks[diskName]
			if !exists {
				continue
			}
			if !diskSpec.AllowScheduling || diskSpec.EvictionRequested {
				continue
			}
			if types.GetCondition(diskStatus.Conditions, longhorn.DiskConditionTypeSchedulable).Status != longhorn.ConditionStatusTrue {
				continue
			}
			disks[diskStatus.DiskUUID] = struct{}{}
		}
		nodeDisksMap[node.Name] = disks
	}

	diskCandidates, multiError := rcs.getDiskCandidates(nodeCandidates, nodeDisksMap, replicas, volume, true)

	// there's no disk that fit for current replica
	if len(diskCandidates) == 0 {
		logrus.Errorf("There's no available disk for replica %v, size %v", replica.ObjectMeta.Name, replica.Spec.VolumeSize)
		return nil, multiError, nil
	}

	// schedule replica to disk
	rcs.scheduleReplicaToDisk(replica, diskCandidates)

	return replica, nil, nil
}

func (rcs *ReplicaScheduler) getNodeCandidates(nodesInfo map[string]*longhorn.Node, schedulingReplica *longhorn.Replica) (nodeCandidates map[string]*longhorn.Node, multiError util.MultiError) {
	if schedulingReplica.Spec.HardNodeAffinity != "" {
		node, exist := nodesInfo[schedulingReplica.Spec.HardNodeAffinity]
		if !exist {
			return nil, util.NewMultiError(longhorn.ErrorReplicaScheduleHardNodeAffinityNotSatisfied)
		}
		nodesInfo = map[string]*longhorn.Node{}
		nodesInfo[schedulingReplica.Spec.HardNodeAffinity] = node
	}

	if len(nodesInfo) == 0 {
		return nil, util.NewMultiError(longhorn.ErrorReplicaScheduleNodeUnavailable)
	}

	nodeCandidates = map[string]*longhorn.Node{}
	for _, node := range nodesInfo {
		if isReady, _ := rcs.ds.CheckEngineImageReadiness(schedulingReplica.Spec.EngineImage, node.Name); isReady {
			nodeCandidates[node.Name] = node
		}
	}

	if len(nodeCandidates) == 0 {
		return map[string]*longhorn.Node{}, util.NewMultiError(longhorn.ErrorReplicaScheduleEngineImageNotReady)
	}

	return nodeCandidates, nil
}

// getNodesWithEvictingReplicas returns nodes that have replicas being evicted
func getNodesWithEvictingReplicas(replicas map[string]*longhorn.Replica, nodeInfo map[string]*longhorn.Node) map[string]*longhorn.Node {
	nodesWithEvictingReplicas := map[string]*longhorn.Node{}
	for _, r := range replicas {
		if r.Status.EvictionRequested {
			if node, ok := nodeInfo[r.Spec.NodeID]; ok {
				nodesWithEvictingReplicas[r.Spec.NodeID] = node
			}
		}
	}
	return nodesWithEvictingReplicas
}

func (rcs *ReplicaScheduler) getDiskCandidates(nodeInfo map[string]*longhorn.Node, nodeDisksMap map[string]map[string]struct{}, replicas map[string]*longhorn.Replica, volume *longhorn.Volume, requireSchedulingCheck bool) (map[string]*Disk, util.MultiError) {
	multiError := util.NewMultiError()

	nodeSoftAntiAffinity, err := rcs.ds.GetSettingAsBool(types.SettingNameReplicaSoftAntiAffinity)
	if err != nil {
		err = errors.Wrapf(err, "failed to get %v setting", types.SettingNameReplicaSoftAntiAffinity)
		multiError.Append(util.NewMultiError(err.Error()))
		return map[string]*Disk{}, multiError
	}

	if volume.Spec.ReplicaSoftAntiAffinity != longhorn.ReplicaSoftAntiAffinityDefault &&
		volume.Spec.ReplicaSoftAntiAffinity != "" {
		nodeSoftAntiAffinity = volume.Spec.ReplicaSoftAntiAffinity == longhorn.ReplicaSoftAntiAffinityEnabled
	}

	zoneSoftAntiAffinity, err := rcs.ds.GetSettingAsBool(types.SettingNameReplicaZoneSoftAntiAffinity)
	if err != nil {
		err = errors.Wrapf(err, "failed to get %v setting", types.SettingNameReplicaZoneSoftAntiAffinity)
		multiError.Append(util.NewMultiError(err.Error()))
		return map[string]*Disk{}, multiError
	}
	if volume.Spec.ReplicaZoneSoftAntiAffinity != longhorn.ReplicaZoneSoftAntiAffinityDefault &&
		volume.Spec.ReplicaZoneSoftAntiAffinity != "" {
		zoneSoftAntiAffinity = volume.Spec.ReplicaZoneSoftAntiAffinity == longhorn.ReplicaZoneSoftAntiAffinityEnabled
	}

	diskSoftAntiAffinity, err := rcs.ds.GetSettingAsBool(types.SettingNameReplicaDiskSoftAntiAffinity)
	if err != nil {
		err = errors.Wrapf(err, "failed to get %v setting", types.SettingNameReplicaDiskSoftAntiAffinity)
		multiError.Append(util.NewMultiError(err.Error()))
		return map[string]*Disk{}, multiError
	}
	if volume.Spec.ReplicaDiskSoftAntiAffinity != longhorn.ReplicaDiskSoftAntiAffinityDefault &&
		volume.Spec.ReplicaDiskSoftAntiAffinity != "" {
		diskSoftAntiAffinity = volume.Spec.ReplicaDiskSoftAntiAffinity == longhorn.ReplicaDiskSoftAntiAffinityEnabled
	}

	getDiskCandidatesFromNodes := func(nodes map[string]*longhorn.Node) (diskCandidates map[string]*Disk, multiError util.MultiError) {
		diskCandidates = map[string]*Disk{}
		multiError = util.NewMultiError()
		for _, node := range nodes {
			diskCandidatesFromNode, errors := rcs.filterNodeDisksForReplica(node, nodeDisksMap[node.Name], replicas,
				volume, requireSchedulingCheck)
			for k, v := range diskCandidatesFromNode {
				diskCandidates[k] = v
			}
			multiError.Append(errors)
		}
		diskCandidates = filterDisksWithMatchingReplicas(diskCandidates, replicas, diskSoftAntiAffinity)
		return diskCandidates, multiError
	}

	usedNodes, usedZones, onlyEvictingNodes, onlyEvictingZones := getCurrentNodesAndZones(replicas, nodeInfo)

	allowEmptyNodeSelectorVolume, err := rcs.ds.GetSettingAsBool(types.SettingNameAllowEmptyNodeSelectorVolume)
	if err != nil {
		err = errors.Wrapf(err, "failed to get %v setting", types.SettingNameAllowEmptyNodeSelectorVolume)
		multiError.Append(util.NewMultiError(err.Error()))
		return map[string]*Disk{}, multiError
	}

	unusedNodes := map[string]*longhorn.Node{}
	unusedNodesInUnusedZones := map[string]*longhorn.Node{}

	// Per https://github.com/longhorn/longhorn/issues/3076, if a replica is being evicted from one disk on a node, the
	// scheduler must be given the opportunity to schedule it to a different disk on the same node (if it meets other
	// requirements). Track nodes that are evicting all their replicas in case we can reuse one.
	unusedNodesAfterEviction := map[string]*longhorn.Node{}
	unusedNodesInUnusedZonesAfterEviction := map[string]*longhorn.Node{}

	for nodeName, node := range nodeInfo {
		// Filter Nodes. If the Nodes don't match the tags, don't bother marking them as candidates.
		if !types.IsSelectorsInTags(node.Spec.Tags, volume.Spec.NodeSelector, allowEmptyNodeSelectorVolume) {
			continue
		}
		if _, ok := usedNodes[nodeName]; !ok {
			unusedNodes[nodeName] = node
		}
		if onlyEvictingNodes[nodeName] {
			unusedNodesAfterEviction[nodeName] = node
			if onlyEvictingZones[node.Status.Zone] {
				unusedNodesInUnusedZonesAfterEviction[nodeName] = node
			}
		}
		if _, ok := usedZones[node.Status.Zone]; !ok {
			unusedNodesInUnusedZones[nodeName] = node
		}
	}

	// In all cases, we should try to use a disk on an unused node in an unused zone first. Don't bother considering
	// zoneSoftAntiAffinity and nodeSoftAntiAffinity settings if such disks are available.
	diskCandidates, errors := getDiskCandidatesFromNodes(unusedNodesInUnusedZones)
	if len(diskCandidates) > 0 {
		return diskCandidates, nil
	}
	multiError.Append(errors)

	switch {
	case !zoneSoftAntiAffinity && !nodeSoftAntiAffinity:
		fallthrough
	// Same as the above. If we cannot schedule two replicas in the same zone, we cannot schedule them on the same node.
	case !zoneSoftAntiAffinity && nodeSoftAntiAffinity:
		diskCandidates, errors = getDiskCandidatesFromNodes(unusedNodesInUnusedZonesAfterEviction)
		if len(diskCandidates) > 0 {
			return diskCandidates, nil
		}
		multiError.Append(errors)
	case zoneSoftAntiAffinity && !nodeSoftAntiAffinity:
		diskCandidates, errors = getDiskCandidatesFromNodes(unusedNodes)
		if len(diskCandidates) > 0 {
			return diskCandidates, nil
		}
		multiError.Append(errors)
		diskCandidates, errors = getDiskCandidatesFromNodes(unusedNodesAfterEviction)
		if len(diskCandidates) > 0 {
			return diskCandidates, nil
		}
		multiError.Append(errors)
	case zoneSoftAntiAffinity && nodeSoftAntiAffinity:
		diskCandidates, errors = getDiskCandidatesFromNodes(unusedNodes)
		if len(diskCandidates) > 0 {
			return diskCandidates, nil
		}
		multiError.Append(errors)
		diskCandidates, errors = getDiskCandidatesFromNodes(usedNodes)
		if len(diskCandidates) > 0 {
			return diskCandidates, nil
		}
		multiError.Append(errors)
	}
	return map[string]*Disk{}, multiError
}

func (rcs *ReplicaScheduler) filterNodeDisksForReplica(node *longhorn.Node, disks map[string]struct{}, replicas map[string]*longhorn.Replica, volume *longhorn.Volume, requireSchedulingCheck bool) (preferredDisks map[string]*Disk, multiError util.MultiError) {
	multiError = util.NewMultiError()
	preferredDisks = map[string]*Disk{}

	allowEmptyDiskSelectorVolume, err := rcs.ds.GetSettingAsBool(types.SettingNameAllowEmptyDiskSelectorVolume)
	if err != nil {
		err = errors.Wrapf(err, "failed to get %v setting", types.SettingNameAllowEmptyDiskSelectorVolume)
		multiError.Append(util.NewMultiError(err.Error()))
		return preferredDisks, multiError
	}

	if len(disks) == 0 {
		multiError.Append(util.NewMultiError(longhorn.ErrorReplicaScheduleDiskUnavailable))
		return preferredDisks, multiError
	}

	// find disk that fit for current replica
	for diskUUID := range disks {
		var diskName string
		var diskSpec longhorn.DiskSpec
		var diskStatus *longhorn.DiskStatus
		diskFound := false
		for diskName, diskStatus = range node.Status.DiskStatus {
			if diskStatus.DiskUUID != diskUUID {
				continue
			}
			if !requireSchedulingCheck || types.GetCondition(diskStatus.Conditions, longhorn.DiskConditionTypeSchedulable).Status == longhorn.ConditionStatusTrue {
				diskFound = true
				diskSpec = node.Spec.Disks[diskName]
				break
			}
		}
		if !diskFound {
			logrus.Errorf("Cannot find the spec or the status for disk %v when scheduling replica", diskUUID)
			multiError.Append(util.NewMultiError(longhorn.ErrorReplicaScheduleDiskNotFound))
			continue
		}

		if !(volume.Spec.BackendStoreDriver == longhorn.BackendStoreDriverTypeV1 && diskSpec.Type == longhorn.DiskTypeFilesystem) &&
			!(volume.Spec.BackendStoreDriver == longhorn.BackendStoreDriverTypeV2 && diskSpec.Type == longhorn.DiskTypeBlock) {
			logrus.Debugf("Volume %v is not compatible with disk %v", volume.Name, diskName)
			continue
		}

		if requireSchedulingCheck {
			info, err := rcs.GetDiskSchedulingInfo(diskSpec, diskStatus)
			if err != nil {
				logrus.Errorf("Failed to get settings when scheduling replica: %v", err)
				multiError.Append(util.NewMultiError(longhorn.ErrorReplicaScheduleSchedulingSettingsRetrieveFailed))
				return preferredDisks, multiError
			}
			scheduledReplica := diskStatus.ScheduledReplica
			// check other replicas for the same volume has been accounted on current node
			var storageScheduled int64
			for rName, r := range replicas {
				if _, ok := scheduledReplica[rName]; !ok && r.Spec.NodeID != "" && r.Spec.NodeID == node.Name {
					storageScheduled += r.Spec.VolumeSize
				}
			}
			if storageScheduled > 0 {
				info.StorageScheduled += storageScheduled
			}
			if !rcs.IsSchedulableToDisk(volume.Spec.Size, volume.Status.ActualSize, info) {
				multiError.Append(util.NewMultiError(longhorn.ErrorReplicaScheduleInsufficientStorage))
				continue
			}
		}

		// Check if the Disk's Tags are valid.
		if !types.IsSelectorsInTags(diskSpec.Tags, volume.Spec.DiskSelector, allowEmptyDiskSelectorVolume) {
			multiError.Append(util.NewMultiError(longhorn.ErrorReplicaScheduleTagsNotFulfilled))
			continue
		}

		suggestDisk := &Disk{
			DiskSpec:   diskSpec,
			DiskStatus: diskStatus,
			NodeID:     node.Name,
		}
		preferredDisks[diskUUID] = suggestDisk
	}

	return preferredDisks, multiError
}

// filterDisksWithMatchingReplicas filters the input disks map and returns only the disks that have the fewest matching
// replicas. If diskSoftAntiAffinity is false, it only returns disks that have no matching replicas.
func filterDisksWithMatchingReplicas(disks map[string]*Disk, replicas map[string]*longhorn.Replica,
	diskSoftAntiAffinity bool) map[string]*Disk {
	replicasCountPerDisk := map[string]int{}
	for _, r := range replicas {
		replicasCountPerDisk[r.Spec.DiskID]++
	}

	highestReplicaCount := 0
	disksByReplicaCount := map[int]map[string]*Disk{}
	for diskUUID, disk := range disks {
		count := replicasCountPerDisk[diskUUID]
		if disksByReplicaCount[count] == nil {
			disksByReplicaCount[count] = map[string]*Disk{}
		}
		disksByReplicaCount[count][diskUUID] = disk
		if count > highestReplicaCount {
			highestReplicaCount = count
		}
	}

	if len(disksByReplicaCount[0]) > 0 || !diskSoftAntiAffinity {
		return disksByReplicaCount[0]
	}

	for i := 1; i <= highestReplicaCount; i++ {
		if len(disksByReplicaCount[i]) > 0 {
			return disksByReplicaCount[i]
		}
	}

	return map[string]*Disk{}
}

func (rcs *ReplicaScheduler) getNodeInfo() (map[string]*longhorn.Node, error) {
	nodeInfo, err := rcs.ds.ListNodes()
	if err != nil {
		return nil, err
	}
	scheduledNode := map[string]*longhorn.Node{}

	for _, node := range nodeInfo {
		// First check node ready condition
		nodeReadyCondition := types.GetCondition(node.Status.Conditions, longhorn.NodeConditionTypeReady)
		// Get Schedulable condition
		nodeSchedulableCondition :=
			types.GetCondition(node.Status.Conditions,
				longhorn.NodeConditionTypeSchedulable)
		if node != nil && node.DeletionTimestamp == nil &&
			nodeReadyCondition.Status == longhorn.ConditionStatusTrue &&
			nodeSchedulableCondition.Status == longhorn.ConditionStatusTrue &&
			node.Spec.AllowScheduling {
			scheduledNode[node.Name] = node
		}
	}
	return scheduledNode, nil
}

func (rcs *ReplicaScheduler) scheduleReplicaToDisk(replica *longhorn.Replica, diskCandidates map[string]*Disk) {
	disk := rcs.getDiskWithMostUsableStorage(diskCandidates)
	replica.Spec.NodeID = disk.NodeID
	replica.Spec.DiskID = disk.DiskUUID
	replica.Spec.DiskPath = disk.Path
	replica.Spec.DataDirectoryName = replica.Spec.VolumeName + "-" + util.RandomID()

	logrus.WithFields(logrus.Fields{
		"replica":           replica.Name,
		"disk":              replica.Spec.DiskID,
		"diskPath":          replica.Spec.DiskPath,
		"dataDirectoryName": replica.Spec.DataDirectoryName,
	}).Infof("Schedule replica to node %v", replica.Spec.NodeID)
}

func (rcs *ReplicaScheduler) getDiskWithMostUsableStorage(disks map[string]*Disk) *Disk {
	diskWithMostUsableStorage := &Disk{}
	for _, disk := range disks {
		diskWithMostUsableStorage = disk
		break
	}

	for _, disk := range disks {
		diskWithMostStorageSize := diskWithMostUsableStorage.StorageAvailable - diskWithMostUsableStorage.StorageReserved
		diskSize := disk.StorageAvailable - disk.StorageReserved
		if diskWithMostStorageSize > diskSize {
			continue
		}

		diskWithMostUsableStorage = disk
	}

	return diskWithMostUsableStorage
}
func filterActiveReplicas(replicas map[string]*longhorn.Replica) map[string]*longhorn.Replica {
	result := map[string]*longhorn.Replica{}
	for _, r := range replicas {
		if r.Spec.Active {
			result[r.Name] = r
		}
	}
	return result
}

func (rcs *ReplicaScheduler) CheckAndReuseFailedReplica(replicas map[string]*longhorn.Replica, volume *longhorn.Volume, hardNodeAffinity string) (*longhorn.Replica, error) {
	replicas = filterActiveReplicas(replicas)

	allNodesInfo, err := rcs.getNodeInfo()
	if err != nil {
		return nil, err
	}

	availableNodesInfo := map[string]*longhorn.Node{}
	availableNodeDisksMap := map[string]map[string]struct{}{}
	reusableNodeReplicasMap := map[string][]*longhorn.Replica{}
	for _, r := range replicas {
		isReusable, err := rcs.isFailedReplicaReusable(r, volume, allNodesInfo, hardNodeAffinity)
		if err != nil {
			return nil, err
		}
		if !isReusable {
			continue
		}

		disks, exists := availableNodeDisksMap[r.Spec.NodeID]
		if exists {
			disks[r.Spec.DiskID] = struct{}{}
		} else {
			disks = map[string]struct{}{r.Spec.DiskID: {}}
		}
		availableNodesInfo[r.Spec.NodeID] = allNodesInfo[r.Spec.NodeID]
		availableNodeDisksMap[r.Spec.NodeID] = disks

		if _, exists := reusableNodeReplicasMap[r.Spec.NodeID]; exists {
			reusableNodeReplicasMap[r.Spec.NodeID] = append(reusableNodeReplicasMap[r.Spec.NodeID], r)
		} else {
			reusableNodeReplicasMap[r.Spec.NodeID] = []*longhorn.Replica{r}
		}
	}

	diskCandidates, _ := rcs.getDiskCandidates(availableNodesInfo, availableNodeDisksMap, replicas, volume, false)

	var reusedReplica *longhorn.Replica
	for _, suggestDisk := range diskCandidates {
		for _, r := range reusableNodeReplicasMap[suggestDisk.NodeID] {
			if r.Spec.DiskID != suggestDisk.DiskUUID {
				continue
			}
			if reusedReplica == nil {
				reusedReplica = r
				continue
			}
			reusedReplica = GetLatestFailedReplica(reusedReplica, r)
		}
	}
	if reusedReplica == nil {
		logrus.Infof("Cannot find a reusable failed replicas for volume %v", volume.Name)
		return nil, nil
	}

	return reusedReplica, nil
}

// RequireNewReplica is used to check if creating new replica immediately is necessary **after a reusable failed replica is not found**.
// If creating new replica immediately is necessary, returns 0.
// Otherwise, returns the duration that the caller should recheck.
// A new replica needs to be created when:
//  1. the volume is a new volume (volume.Status.Robustness is Empty)
//  2. data locality is required (hardNodeAffinity is not Empty and volume.Status.Robustness is Healthy)
//  3. replica eviction happens (volume.Status.Robustness is Healthy)
//  4. there is no potential reusable replica
//  5. there is potential reusable replica but the replica replenishment wait interval is passed.
func (rcs *ReplicaScheduler) RequireNewReplica(replicas map[string]*longhorn.Replica, volume *longhorn.Volume, hardNodeAffinity string) time.Duration {
	if volume.Status.Robustness != longhorn.VolumeRobustnessDegraded {
		return 0
	}
	if hardNodeAffinity != "" {
		return 0
	}

	hasPotentiallyReusableReplica := false
	for _, r := range replicas {
		if IsPotentiallyReusableReplica(r, hardNodeAffinity) {
			hasPotentiallyReusableReplica = true
			break
		}
	}
	if !hasPotentiallyReusableReplica {
		return 0
	}

	// Otherwise Longhorn will relay the new replica creation then there is a chance to reuse failed replicas later.
	settingValue, err := rcs.ds.GetSettingAsInt(types.SettingNameReplicaReplenishmentWaitInterval)
	if err != nil {
		logrus.Errorf("Failed to get Setting ReplicaReplenishmentWaitInterval, will directly replenish a new replica: %v", err)
		return 0
	}
	waitInterval := time.Duration(settingValue) * time.Second
	lastDegradedAt, err := util.ParseTime(volume.Status.LastDegradedAt)

	if err != nil {
		logrus.Errorf("Failed to get parse volume last degraded timestamp %v, will directly replenish a new replica: %v", volume.Status.LastDegradedAt, err)
		return 0
	}
	now := time.Now()
	if now.After(lastDegradedAt.Add(waitInterval)) {
		return 0
	}

	logrus.Infof("Replica replenishment is delayed until %v", lastDegradedAt.Add(waitInterval))
	// Adding 1 more second to the check back interval to avoid clock skew
	return lastDegradedAt.Add(waitInterval).Sub(now) + time.Second
}

func (rcs *ReplicaScheduler) isFailedReplicaReusable(r *longhorn.Replica, v *longhorn.Volume, nodeInfo map[string]*longhorn.Node, hardNodeAffinity string) (bool, error) {
	if r.Spec.FailedAt == "" {
		return false, nil
	}
	if r.Spec.NodeID == "" || r.Spec.DiskID == "" {
		return false, nil
	}
	if r.Spec.RebuildRetryCount >= FailedReplicaMaxRetryCount {
		return false, nil
	}
	if r.Status.EvictionRequested {
		return false, nil
	}
	if hardNodeAffinity != "" && r.Spec.NodeID != hardNodeAffinity {
		return false, nil
	}
	if isReady, _ := rcs.ds.CheckEngineImageReadiness(r.Spec.EngineImage, r.Spec.NodeID); !isReady {
		return false, nil
	}

	allowEmptyDiskSelectorVolume, err := rcs.ds.GetSettingAsBool(types.SettingNameAllowEmptyDiskSelectorVolume)
	if err != nil {
		return false, errors.Wrapf(err, "failed to get %v setting", types.SettingNameAllowEmptyDiskSelectorVolume)
	}

	node, exists := nodeInfo[r.Spec.NodeID]
	if !exists {
		return false, nil
	}
	diskFound := false
	for diskName, diskStatus := range node.Status.DiskStatus {
		diskSpec, ok := node.Spec.Disks[diskName]
		if !ok {
			continue
		}

		if types.GetCondition(diskStatus.Conditions, longhorn.DiskConditionTypeReady).Status != longhorn.ConditionStatusTrue {
			continue
		}
		if types.GetCondition(diskStatus.Conditions, longhorn.DiskConditionTypeSchedulable).Status != longhorn.ConditionStatusTrue {
			// We want to reuse replica on the disk that is unschedulable due to allocated space being bigger than max allocable space but the disk is not full yet
			if types.GetCondition(diskStatus.Conditions, longhorn.DiskConditionTypeSchedulable).Reason != longhorn.DiskConditionReasonDiskPressure {
				continue
			}
			schedulingInfo, err := rcs.GetDiskSchedulingInfo(diskSpec, diskStatus)
			if err != nil {
				logrus.Warnf("failed to GetDiskSchedulingInfo of disk %v on node %v when checking replica %v is reusable: %v", diskName, node.Name, r.Name, err)
			}
			if !rcs.isDiskNotFull(schedulingInfo) {
				continue
			}
		}
		if diskStatus.DiskUUID == r.Spec.DiskID {
			diskFound = true
			diskSpec, exists := node.Spec.Disks[diskName]
			if !exists {
				return false, nil
			}
			if !diskSpec.AllowScheduling || diskSpec.EvictionRequested {
				return false, nil
			}
			if !types.IsSelectorsInTags(diskSpec.Tags, v.Spec.DiskSelector, allowEmptyDiskSelectorVolume) {
				return false, nil
			}
		}
	}
	if !diskFound {
		return false, nil
	}

	im, err := rcs.ds.GetInstanceManagerByInstance(r)
	if err != nil {
		logrus.Errorf("failed to get instance manager when checking replica %v is reusable: %v", r.Name, err)
		return false, nil
	}
	if im.DeletionTimestamp != nil || im.Status.CurrentState != longhorn.InstanceManagerStateRunning {
		return false, nil
	}

	return true, nil
}

// IsPotentiallyReusableReplica is used to check if a failed replica is potentially reusable.
// A potentially reusable replica means this failed replica may be able to reuse it later but it’s not valid now due to node/disk down issue.
func IsPotentiallyReusableReplica(r *longhorn.Replica, hardNodeAffinity string) bool {
	if r.Spec.FailedAt == "" {
		return false
	}
	if r.Spec.NodeID == "" || r.Spec.DiskID == "" {
		return false
	}
	if r.Spec.RebuildRetryCount >= FailedReplicaMaxRetryCount {
		return false
	}
	if r.Status.EvictionRequested {
		return false
	}
	if hardNodeAffinity != "" && r.Spec.NodeID != hardNodeAffinity {
		return false
	}
	// TODO: Reuse failed replicas for a SPDK volume
	if r.Spec.BackendStoreDriver == longhorn.BackendStoreDriverTypeV2 {
		return false
	}
	return true
}

func GetLatestFailedReplica(rs ...*longhorn.Replica) (res *longhorn.Replica) {
	if rs == nil {
		return nil
	}

	var latestFailedAt time.Time
	for _, r := range rs {
		failedAt, err := util.ParseTime(r.Spec.FailedAt)
		if err != nil {
			logrus.Errorf("Failed to check replica %v failure timestamp %v: %v", r.Name, r.Spec.FailedAt, err)
			continue
		}
		if res == nil || failedAt.After(latestFailedAt) {
			res = r
			latestFailedAt = failedAt
		}
	}
	return res
}

func (rcs *ReplicaScheduler) IsSchedulableToDisk(size int64, requiredStorage int64, info *DiskSchedulingInfo) bool {
	// StorageReserved = the space is already used by 3rd party + the space will be used by 3rd party.
	// StorageAvailable = the space can be used by 3rd party or Longhorn system.
	// There is no (direct) relationship between StorageReserved and StorageAvailable.
	return info.StorageMaximum > 0 && info.StorageAvailable > 0 &&
		info.StorageAvailable-requiredStorage > int64(float64(info.StorageMaximum)*float64(info.MinimalAvailablePercentage)/100) &&
		(size+info.StorageScheduled) <= int64(float64(info.StorageMaximum-info.StorageReserved)*float64(info.OverProvisioningPercentage)/100)
}

func (rcs *ReplicaScheduler) isDiskNotFull(info *DiskSchedulingInfo) bool {
	// StorageAvailable = the space can be used by 3rd party or Longhorn system.
	return info.StorageMaximum > 0 && info.StorageAvailable > 0 &&
		info.StorageAvailable > int64(float64(info.StorageMaximum)*float64(info.MinimalAvailablePercentage)/100)
}

func (rcs *ReplicaScheduler) GetDiskSchedulingInfo(disk longhorn.DiskSpec, diskStatus *longhorn.DiskStatus) (*DiskSchedulingInfo, error) {
	// get StorageOverProvisioningPercentage and StorageMinimalAvailablePercentage settings
	overProvisioningPercentage, err := rcs.ds.GetSettingAsInt(types.SettingNameStorageOverProvisioningPercentage)
	if err != nil {
		return nil, err
	}
	minimalAvailablePercentage, err := rcs.ds.GetSettingAsInt(types.SettingNameStorageMinimalAvailablePercentage)
	if err != nil {
		return nil, err
	}
	info := &DiskSchedulingInfo{
		StorageAvailable:           diskStatus.StorageAvailable,
		StorageScheduled:           diskStatus.StorageScheduled,
		StorageReserved:            disk.StorageReserved,
		StorageMaximum:             diskStatus.StorageMaximum,
		OverProvisioningPercentage: overProvisioningPercentage,
		MinimalAvailablePercentage: minimalAvailablePercentage,
	}
	return info, nil
}

func (rcs *ReplicaScheduler) CheckReplicasSizeExpansion(v *longhorn.Volume, oldSize, newSize int64) (diskScheduleMultiError util.MultiError, err error) {
	defer func() {
		err = errors.Wrapf(err, "error while CheckReplicasSizeExpansion for volume %v", v.Name)
	}()

	replicas, err := rcs.ds.ListVolumeReplicas(v.Name)
	if err != nil {
		return nil, err
	}
	diskIDToReplicaCount := map[string]int64{}
	diskIDToDiskInfo := map[string]*DiskSchedulingInfo{}
	for _, r := range replicas {
		if r.Spec.NodeID == "" {
			continue
		}
		node, err := rcs.ds.GetNode(r.Spec.NodeID)
		if err != nil {
			return nil, err
		}
		diskSpec, diskStatus, ok := findDiskSpecAndDiskStatusInNode(r.Spec.DiskID, node)
		if !ok {
			return util.NewMultiError(longhorn.ErrorReplicaScheduleDiskNotFound),
				fmt.Errorf("cannot find the disk %v in node %v", r.Spec.DiskID, node.Name)
		}
		diskInfo, err := rcs.GetDiskSchedulingInfo(diskSpec, &diskStatus)
		if err != nil {
			return util.NewMultiError(longhorn.ErrorReplicaScheduleDiskUnavailable),
				fmt.Errorf("failed to GetDiskSchedulingInfo %v", err)
		}
		diskIDToDiskInfo[r.Spec.DiskID] = diskInfo
		diskIDToReplicaCount[r.Spec.DiskID] = diskIDToReplicaCount[r.Spec.DiskID] + 1
	}

	expandingSize := newSize - oldSize
	for diskID, diskInfo := range diskIDToDiskInfo {
		requestingSizeExpansionOnDisk := expandingSize * diskIDToReplicaCount[diskID]
		if !rcs.IsSchedulableToDisk(requestingSizeExpansionOnDisk, 0, diskInfo) {
			logrus.Errorf("Cannot schedule %v more bytes to disk %v with %+v", requestingSizeExpansionOnDisk, diskID, diskInfo)
			return util.NewMultiError(longhorn.ErrorReplicaScheduleInsufficientStorage),
				fmt.Errorf("cannot schedule %v more bytes to disk %v with %+v", requestingSizeExpansionOnDisk, diskID, diskInfo)
		}
	}
	return nil, nil
}

func findDiskSpecAndDiskStatusInNode(diskUUID string, node *longhorn.Node) (longhorn.DiskSpec, longhorn.DiskStatus, bool) {
	for diskName, diskStatus := range node.Status.DiskStatus {
		if diskStatus.DiskUUID == diskUUID {
			diskSpec := node.Spec.Disks[diskName]
			return diskSpec, *diskStatus, true
		}
	}
	return longhorn.DiskSpec{}, longhorn.DiskStatus{}, false
}

func getCurrentNodesAndZones(replicas map[string]*longhorn.Replica, nodeInfo map[string]*longhorn.Node) (map[string]*longhorn.Node,
	map[string]bool, map[string]bool, map[string]bool) {
	usedNodes := map[string]*longhorn.Node{}
	usedZones := map[string]bool{}
	onlyEvictingNodes := map[string]bool{}
	onlyEvictingZones := map[string]bool{}

	for _, r := range replicas {
		if r.Spec.NodeID != "" && r.DeletionTimestamp == nil && r.Spec.FailedAt == "" {
			if node, ok := nodeInfo[r.Spec.NodeID]; ok {
				if r.Status.EvictionRequested {
					if _, ok := usedNodes[r.Spec.NodeID]; !ok {
						// This is an evicting replica on a thus far unused node. We won't change this again unless we
						// find a non-evicting replica on this node.
						onlyEvictingNodes[node.Name] = true
					}
					if used := usedZones[node.Status.Zone]; !used {
						// This is an evicting replica in a thus far unused zone. We won't change this again unless we
						// find a non-evicting replica in this zone.
						onlyEvictingZones[node.Status.Zone] = true
					}
				} else {
					// There is now at least one replica on this node and in this zone that is not evicting.
					onlyEvictingNodes[node.Name] = false
					onlyEvictingZones[node.Status.Zone] = false
				}

				usedNodes[node.Name] = node
				// For empty zone label, we treat them as one zone.
				usedZones[node.Status.Zone] = true
			}
		}
	}

	return usedNodes, usedZones, onlyEvictingNodes, onlyEvictingZones
}
