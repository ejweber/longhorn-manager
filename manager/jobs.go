package manager

import (
	"fmt"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"

	"github.com/yasker/lm-rewrite/engineapi"
	"github.com/yasker/lm-rewrite/orchestrator"
	"github.com/yasker/lm-rewrite/types"
	"github.com/yasker/lm-rewrite/util"
)

func (v *Volume) registerJob(jobType JobType, assoicateID string, data map[string]string, errCh chan error) (string, error) {
	job := &Job{
		ID:          util.UUID(),
		AssoicateID: assoicateID,
		Type:        jobType,
		State:       JobStateOngoing,
		CreatedAt:   util.Now(),
		Data:        data,
	}

	v.setJob(job)
	go v.waitForJob(job.ID, errCh)
	return job.ID, nil
}

func (v *Volume) waitForJob(jobID string, errCh chan error) {
	err := <-errCh
	job := v.getJob(jobID)
	updateJob := *job
	updateJob.CompletedAt = util.Now()

	if err != nil {
		updateJob.State = JobStateFailed
		updateJob.Error = err
		logrus.Errorf("job %v failed: %v", jobID, err)
	} else {
		updateJob.State = JobStateSucceed
	}
	v.setJob(&updateJob)
	return
}

func (v *Volume) setJob(job *Job) {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	v.Jobs[job.ID] = job
}

func (v *Volume) getJob(id string) *Job {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	return v.Jobs[id]
}

func (v *Volume) listJobsByTypeAndAssociateID(jobType JobType, assoicateID string) map[string]*Job {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	result := map[string]*Job{}
	for id, job := range v.Jobs {
		if job.Type == jobType && job.AssoicateID == assoicateID {
			result[id] = job
		}
	}
	return result
}

func (v *Volume) listOngoingJobsByType(jobType JobType) map[string]*Job {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	result := map[string]*Job{}
	for id, job := range v.Jobs {
		if job.State == JobStateOngoing && job.Type == jobType {
			result[id] = job
		}
	}
	return result
}

func (v *Volume) jobReplicaCreate(req *orchestrator.Request) (err error) {
	defer func() {
		errors.Wrap(err, "fail to finish job replica create")
	}()
	instance, err := v.m.orch.CreateReplica(req)
	if err != nil {
		return err
	}
	replica := &types.ReplicaInfo{
		InstanceInfo: types.InstanceInfo{
			ID:         instance.ID,
			Type:       types.InstanceTypeReplica,
			Name:       instance.Name,
			NodeID:     req.NodeID,
			IP:         instance.IP,
			Running:    instance.Running,
			VolumeName: v.Name,
		},
	}

	if err := v.m.kv.CreateVolumeReplica(replica); err != nil {
		return err
	}

	v.setReplica(replica)
	return nil
}

func (v *Volume) jobReplicaRebuild(req *orchestrator.Request) (err error) {
	defer func() {
		errors.Wrap(err, "fail to finish job replica rebuild")
	}()

	if err := v.jobReplicaCreate(req); err != nil {
		return err
	}

	replicaName := req.InstanceName

	if err := v.startReplica(replicaName); err != nil {
		return err
	}

	replica := v.Replicas[replicaName]
	if replica == nil {
		return fmt.Errorf("cannot find replica %v", replicaName)
	}

	engine, err := v.m.engines.NewEngineClient(&engineapi.EngineClientRequest{
		VolumeName:    v.Name,
		ControllerURL: engineapi.GetControllerDefaultURL(v.Controller.IP),
	})
	if err != nil {
		return err
	}

	if replica.IP == "" {
		return fmt.Errorf("cannot add replica %v without IP", replicaName)
	}
	if err := engine.ReplicaAdd(engineapi.GetReplicaDefaultURL(replica.IP)); err != nil {
		return err
	}

	return nil
}

func (v *Volume) jobSnapshotPurge() (err error) {
	defer func() {
		errors.Wrap(err, "fail to finish job replica rebuild")
	}()

	if v.Controller == nil {
		return fmt.Errorf("cannot find volume %v controller", v.Name)
	}
	engine, err := v.m.engines.NewEngineClient(&engineapi.EngineClientRequest{
		VolumeName:    v.Name,
		ControllerURL: engineapi.GetControllerDefaultURL(v.Controller.IP),
	})
	if err != nil {
		return err
	}
	return engine.SnapshotPurge()
}
