/*
Copyright 2024 The Fluid Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vineyard

import (
	"fmt"
	"time"

	"github.com/fluid-cloudnative/fluid/pkg/common"
	"github.com/fluid-cloudnative/fluid/pkg/ddc/base/portallocator"

	datav1alpha1 "github.com/fluid-cloudnative/fluid/api/v1alpha1"
	"github.com/fluid-cloudnative/fluid/pkg/utils"
	"github.com/fluid-cloudnative/fluid/pkg/utils/transformer"
)

func (e *VineyardEngine) transform(runtime *datav1alpha1.VineyardRuntime) (value *Vineyard, err error) {
	if runtime == nil {
		err = fmt.Errorf("the vineyardRuntime is null")
		return
	}
	defer utils.TimeTrack(time.Now(), "VineyardRuntime.Transform", "name", runtime.Name)

	dataset, err := utils.GetDataset(e.Client, e.name, e.namespace)
	if err != nil {
		return value, err
	}

	value = &Vineyard{
		Owner: transformer.GenerateOwnerReferenceFromObject(runtime),
	}
	value.FullnameOverride = e.name
	value.OwnerDatasetId = utils.GetDatasetId(e.namespace, e.name, e.runtimeInfo.GetOwnerDatasetUID())
	value.TieredStore, err = e.transformTieredStore(runtime)
	if err != nil {
		return
	}

	err = e.transformMasters(runtime, dataset, value)
	if err != nil {
		return
	}

	err = e.transformWorkers(runtime, value)
	if err != nil {
		return
	}

	e.transformFuse(runtime, value)

	e.transformPodMetadata(runtime, value)

	// allocate ports for hostnetwork mode
	if datav1alpha1.IsHostNetwork(runtime.Spec.Master.NetworkMode) ||
		datav1alpha1.IsHostNetwork(runtime.Spec.Worker.NetworkMode) {
		e.Log.Info("allocatePorts for hostnetwork mode")
		err = e.allocatePorts(value, runtime)
		if err != nil {
			return
		}
	}

	// TODO:(caoye) implement the metrics exporter
	value.DisablePrometheus = true
	return value, nil
}

func (e *VineyardEngine) transformMasters(runtime *datav1alpha1.VineyardRuntime,
	dataset *datav1alpha1.Dataset,
	value *Vineyard,
) (err error) {
	value.Master = Master{}
	if runtime.Spec.Master.Replicas == 0 {
		value.Master.Replicas = 1
	} else {
		value.Master.Replicas = runtime.Spec.Master.Replicas
	}

	image := runtime.Spec.Master.Image
	imageTag := runtime.Spec.Master.ImageTag
	imagePullPolicy := runtime.Spec.Master.ImagePullPolicy

	value.Master.Image, value.Master.ImageTag, value.Master.ImagePullPolicy = e.parseMasterImage(image, imageTag, imagePullPolicy)

	if len(runtime.Spec.Master.Env) > 0 {
		value.Master.Env = runtime.Spec.Master.Env
	} else {
		value.Master.Env = map[string]string{}
	}
	options := e.transformMasterOptions(runtime)
	if len(options) != 0 {
		value.Master.Options = options
	}

	nodeSelector := e.transformMasterSelector(runtime)
	if len(nodeSelector) != 0 {
		value.Master.NodeSelector = nodeSelector
	}

	e.transformResourcesForMaster(runtime, value)

	// parse master pod network mode
	value.Master.HostNetwork = datav1alpha1.IsHostNetwork(runtime.Spec.Worker.NetworkMode)

	ports := e.transformMasterPorts(runtime)
	if len(ports) != 0 {
		value.Master.Ports = ports
	}

	err = e.transformMasterVolumes(runtime, value)
	if err != nil {
		e.Log.Error(err, "failed to transform volumes for master")
	}

	return
}

func (e *VineyardEngine) transformWorkers(runtime *datav1alpha1.VineyardRuntime, value *Vineyard) (err error) {
	value.Worker = Worker{}
	// respect the replicas of runtime if the replicas of worker and runtime are both specified
	value.Worker.Replicas = runtime.Spec.Replicas

	image := runtime.Spec.Worker.Image
	imageTag := runtime.Spec.Worker.ImageTag
	imagePullPolicy := runtime.Spec.Worker.ImagePullPolicy

	value.Worker.Image, value.Worker.ImageTag, value.Worker.ImagePullPolicy = e.parseWorkerImage(image, imageTag, imagePullPolicy)

	if len(runtime.Spec.Worker.Env) > 0 {
		value.Worker.Env = runtime.Spec.Worker.Env
	} else {
		value.Worker.Env = map[string]string{}
	}

	if len(runtime.Spec.Worker.NodeSelector) > 0 {
		value.Worker.NodeSelector = runtime.Spec.Worker.NodeSelector
	} else {
		value.Worker.NodeSelector = map[string]string{}
	}

	if err := e.transformResourcesForWorker(runtime, value); err != nil {
		return err
	}

	// parse worker pod network mode
	value.Worker.HostNetwork = datav1alpha1.IsHostNetwork(runtime.Spec.Worker.NetworkMode)

	ports := e.transformWorkerPorts(runtime)
	if len(ports) != 0 {
		value.Worker.Ports = ports
	}

	err = e.transformWorkerVolumes(runtime, value)
	if err != nil {
		e.Log.Error(err, "failed to transform volumes for worker")
	}

	return
}

func (e *VineyardEngine) transformFuse(runtime *datav1alpha1.VineyardRuntime, value *Vineyard) {
	value.Fuse = Fuse{}
	image := runtime.Spec.Fuse.Image
	imageTag := runtime.Spec.Fuse.ImageTag
	imagePullPolicy := runtime.Spec.Fuse.ImagePullPolicy
	value.Fuse.Image, value.Fuse.ImageTag, value.Fuse.ImagePullPolicy = e.parseFuseImage(image, imageTag, imagePullPolicy)

	if len(runtime.Spec.Fuse.Env) > 0 {
		value.Fuse.Env = runtime.Spec.Fuse.Env
	} else {
		value.Fuse.Env = map[string]string{}
	}
	value.Fuse.CleanPolicy = runtime.Spec.Fuse.CleanPolicy

	value.Fuse.NodeSelector = e.transformFuseNodeSelector(runtime)
	value.Fuse.HostPID = common.HostPIDEnabled(runtime.Annotations)

	value.Fuse.TargetPath = e.getMountPoint()

	// parse fuse pod network mode
	value.Fuse.HostNetwork = datav1alpha1.IsHostNetwork(runtime.Spec.Fuse.NetworkMode)

	options := e.transformFuseOptions(runtime, value)
	if len(options) != 0 {
		value.Fuse.Options = options
	}
	e.transformResourcesForFuse(runtime, value)
}

func (e *VineyardEngine) transformMasterSelector(runtime *datav1alpha1.VineyardRuntime) map[string]string {
	properties := map[string]string{}
	if runtime.Spec.Master.NodeSelector != nil {
		properties = runtime.Spec.Master.NodeSelector
	}
	return properties
}

func (e *VineyardEngine) transformMasterPorts(runtime *datav1alpha1.VineyardRuntime) map[string]int {
	ports := map[string]int{
		MasterClientName: MasterClientPort,
		MasterPeerName:   MasterPeerPort,
	}
	if len(runtime.Spec.Master.Ports) > 0 {
		for key, value := range runtime.Spec.Master.Ports {
			ports[key] = value
		}
	}
	return ports
}

func (e *VineyardEngine) transformMasterOptions(runtime *datav1alpha1.VineyardRuntime) map[string]string {
	options := map[string]string{
		WorkerReserveMemory: DefaultWorkerReserveMemoryValue,
		WorkerEtcdPrefix:    DefaultWorkerEtcdPrefixValue,
	}
	if len(runtime.Spec.Master.Options) > 0 {
		for key, value := range runtime.Spec.Master.Options {
			options[key] = value
		}
	}
	return options
}

func (e *VineyardEngine) transformWorkerOptions(runtime *datav1alpha1.VineyardRuntime) map[string]string {
	options := map[string]string{}
	if len(runtime.Spec.Worker.Options) > 0 {
		options = runtime.Spec.Worker.Options
	}

	return options
}

func (e *VineyardEngine) transformFuseOptions(runtime *datav1alpha1.VineyardRuntime, value *Vineyard) map[string]string {
	options := map[string]string{
		VineyarddSize: DefaultSize,
		EtcdEndpoint:  fmt.Sprintf("http://%s-master-0.%s-master.%s:%d", value.FullnameOverride, value.FullnameOverride, e.namespace, value.Master.Ports[MasterClientName]),
		EtcdPrefix:    DefaultEtcdPrefix,
	}

	if len(runtime.Spec.Fuse.Options) > 0 {
		for key, value := range runtime.Spec.Fuse.Options {
			options[key] = value
		}
	}
	return options
}

func (e *VineyardEngine) transformWorkerPorts(runtime *datav1alpha1.VineyardRuntime) map[string]int {
	ports := map[string]int{
		WorkerRPCName:      WorkerRPCPort,
		WorkerExporterName: WorkerExporterPort,
	}
	if len(runtime.Spec.Worker.Ports) > 0 {
		for key, value := range runtime.Spec.Worker.Ports {
			ports[key] = value
		}
	}
	return ports
}

func (e *VineyardEngine) transformFuseNodeSelector(runtime *datav1alpha1.VineyardRuntime) map[string]string {
	nodeSelector := map[string]string{}
	nodeSelector[utils.GetFuseLabelName(runtime.Namespace, runtime.Name, e.runtimeInfo.GetOwnerDatasetUID())] = "true"
	return nodeSelector
}

func (e *VineyardEngine) transformTieredStore(runtime *datav1alpha1.VineyardRuntime) (TieredStore, error) {
	if len(runtime.Spec.TieredStore.Levels) == 0 {
		return TieredStore{}, fmt.Errorf("the tieredstore is empty")
	}

	tieredStore := TieredStore{}
	for _, level := range runtime.Spec.TieredStore.Levels {
		if level.MediumType == "MEM" {
			tieredStore.Levels = append(tieredStore.Levels, Level{
				MediumType: level.MediumType,
				Quota:      level.Quota,
			})
		} else {
			tieredStore.Levels = append(tieredStore.Levels, Level{
				Level:        1,
				MediumType:   level.MediumType,
				VolumeType:   level.VolumeType,
				VolumeSource: level.VolumeSource,
				Path:         level.Path,
				Quota:        level.Quota,
				QuotaList:    level.QuotaList,
				High:         level.High,
				Low:          level.Low,
			})
		}
	}

	return tieredStore, nil
}

func (e *VineyardEngine) allocatePorts(value *Vineyard, runtime *datav1alpha1.VineyardRuntime) error {
	expectedMasterPortNum, expectedWorkerPortNum := 0, 0
	if datav1alpha1.IsHostNetwork(runtime.Spec.Master.NetworkMode) {
		expectedMasterPortNum = 2
	}
	if datav1alpha1.IsHostNetwork(runtime.Spec.Worker.NetworkMode) {
		expectedWorkerPortNum = 2
	}
	expectedPortNum := expectedMasterPortNum + expectedWorkerPortNum

	allocator, err := portallocator.GetRuntimePortAllocator()
	if err != nil {
		e.Log.Error(err, "can't get runtime port allocator")
		return err
	}

	allocatedPorts, err := allocator.GetAvailablePorts(expectedPortNum)
	if err != nil {
		e.Log.Error(err, "can't get available ports", "expected port num", expectedPortNum)
		return err
	}

	index := 0
	if expectedMasterPortNum > 0 {
		value.Master.Ports[MasterClientName] = allocatedPorts[index]
		value.Master.Ports[MasterPeerName] = allocatedPorts[index+1]
		index += 2
	}

	if expectedWorkerPortNum > 0 {
		value.Worker.Ports[WorkerRPCName] = allocatedPorts[index]
		value.Worker.Ports[WorkerExporterName] = allocatedPorts[index+1]
	}

	return nil
}

func (e *VineyardEngine) transformPodMetadata(runtime *datav1alpha1.VineyardRuntime, value *Vineyard) {
	// transform labels
	commonLabels := utils.UnionMapsWithOverride(map[string]string{}, runtime.Spec.PodMetadata.Labels)
	value.Master.Labels = utils.UnionMapsWithOverride(commonLabels, runtime.Spec.Master.PodMetadata.Labels)
	value.Worker.Labels = utils.UnionMapsWithOverride(commonLabels, runtime.Spec.Worker.PodMetadata.Labels)
	value.Fuse.Labels = utils.UnionMapsWithOverride(commonLabels, runtime.Spec.Fuse.PodMetadata.Labels)

	// transform annotations
	commonAnnotations := utils.UnionMapsWithOverride(map[string]string{}, runtime.Spec.PodMetadata.Annotations)
	value.Master.Annotations = utils.UnionMapsWithOverride(commonAnnotations, runtime.Spec.Master.PodMetadata.Annotations)
	value.Worker.Annotations = utils.UnionMapsWithOverride(commonAnnotations, runtime.Spec.Worker.PodMetadata.Annotations)
	value.Fuse.Annotations = utils.UnionMapsWithOverride(commonAnnotations, runtime.Spec.Fuse.PodMetadata.Annotations)
}
