package manager

import (
	longhorn "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta2"
)

func (m *VolumeManager) ListSnapshotCRsRO(volumeName string) (map[string]*longhorn.Snapshot, error) {
	return m.ds.ListVolumeSnapshotsRO(volumeName)
}

func (m *VolumeManager) GetSnapshotCRRO(snapName string) (*longhorn.Snapshot, error) {
	return m.ds.GetSnapshotRO(snapName)
}
