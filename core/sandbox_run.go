/*
Copyright 2021 Mirantis

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

package core

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Mirantis/cri-dockerd/config"
	"github.com/Mirantis/cri-dockerd/utils/errors"
	v1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// RunPodSandbox creates and starts a pod-level sandbox. Runtimes should ensure
// the sandbox is in ready state.
// For docker, PodSandbox is implemented by a container holding the network
// namespace for the pod.
// Note: docker doesn't use LogDirectory (yet).
func (ds *dockerService) RunPodSandbox(
	ctx context.Context,
	r *v1.RunPodSandboxRequest,
) (*v1.RunPodSandboxResponse, error) {
	containerConfig := r.GetConfig()

	// Step 1: Pull the image for the sandbox.
	image := defaultSandboxImage
	podSandboxImage := ds.podSandboxImage
	if len(podSandboxImage) != 0 {
		image = podSandboxImage
	}

	// NOTE: To use a custom sandbox image in a private repository, users need to configure the nodes with credentials properly.
	// see: http://kubernetes.io/docs/user-guide/images/#configuring-nodes-to-authenticate-to-a-private-repository
	// Only pull sandbox image when it's not present - v1.PullIfNotPresent.
	if err := ensureSandboxImageExists(ds.client, image); err != nil {
		return nil, err
	}

	// Step 2: Create the sandbox container.
	createConfig, err := ds.makeSandboxDockerConfig(containerConfig, image)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to make sandbox docker config for pod %q: %v",
			containerConfig.Metadata.Name,
			err,
		)
	}
	// Map Kubernetes runtimeClassName to Docker runtime.
	runtimeHandler, err := ds.getRuntimeFromRuntimeClassName(r.GetRuntimeHandler())
	if err != nil {
		return nil, err
	}
	// TODO: find a better way to pass runtime from K8s Pod to containers
	createConfig.Config.Labels[runtimeLabelName] = runtimeHandler
	createResp, err := ds.client.CreateContainer(*createConfig)
	if err != nil {
		createResp, err = recoverFromCreationConflictIfNeeded(ds.client, *createConfig, err)
	}

	if err != nil || createResp == nil {
		return nil, fmt.Errorf(
			"failed to create a sandbox for pod %q: %v",
			containerConfig.Metadata.Name,
			err,
		)
	}
	resp := &v1.RunPodSandboxResponse{PodSandboxId: createResp.ID}

	ds.setNetworkReady(createResp.ID, false)
	defer func(e *error) {
		// Set networking ready depending on the error return of
		// the parent function
		if *e == nil {
			ds.setNetworkReady(createResp.ID, true)
		}
	}(&err)

	// Step 3: Create Sandbox Checkpoint.
	if err = ds.checkpointManager.CreateCheckpoint(createResp.ID, constructPodSandboxCheckpoint(containerConfig)); err != nil {
		return nil, err
	}

	// Step 4: Start the sandbox container.
	// Assume kubelet's garbage collector would remove the sandbox later, if
	// startContainer failed.
	err = ds.client.StartContainer(createResp.ID)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to start sandbox container for pod %q: %v",
			containerConfig.Metadata.Name,
			err,
		)
	}

	// Rewrite resolv.conf file generated by docker.
	// NOTE: cluster dns settings aren't passed anymore to docker api in all cases,
	// not only for pods with host network: the resolver conf will be overwritten
	// after sandbox creation to override docker's behaviour. This resolv.conf
	// file is shared by all containers of the same pod, and needs to be modified
	// only once per pod.
	if dnsConfig := containerConfig.GetDnsConfig(); dnsConfig != nil {
		containerInfo, err := ds.client.InspectContainer(createResp.ID)
		if err != nil {
			return nil, fmt.Errorf(
				"failed to inspect sandbox container for pod %q: %v",
				containerConfig.Metadata.Name,
				err,
			)
		}

		if err := rewriteResolvFile(containerInfo.ResolvConfPath, dnsConfig.Servers, dnsConfig.Searches, dnsConfig.Options); err != nil {
			return nil, fmt.Errorf(
				"rewrite resolv.conf failed for pod %q: %v",
				containerConfig.Metadata.Name,
				err,
			)
		}
	}

	// Do not invoke network plugins if in hostNetwork mode.
	if containerConfig.GetLinux().GetSecurityContext().GetNamespaceOptions().GetNetwork() == v1.NamespaceMode_NODE {
		return resp, nil
	}

	// Step 5: Setup networking for the sandbox.
	// All pod networking is setup by a CNI plugin discovered at startup time.
	// This plugin assigns the pod ip, sets up routes inside the sandbox,
	// creates interfaces etc. In theory, its jurisdiction ends with pod
	// sandbox networking, but it might insert iptables rules or open ports
	// on the host as well, to satisfy parts of the pod spec that aren't
	// recognized by the CNI standard yet.
	cID := config.BuildContainerID(runtimeName, createResp.ID)
	networkOptions := make(map[string]string)
	if dnsConfig := containerConfig.GetDnsConfig(); dnsConfig != nil {
		// Build DNS options.
		dnsOption, err := json.Marshal(dnsConfig)
		if err != nil {
			return nil, fmt.Errorf(
				"failed to marshal dns config for pod %q: %v",
				containerConfig.Metadata.Name,
				err,
			)
		}
		networkOptions["dns"] = string(dnsOption)
	}
	err = ds.network.SetUpPod(
		containerConfig.GetMetadata().Namespace,
		containerConfig.GetMetadata().Name,
		cID,
		containerConfig.Annotations,
		networkOptions,
	)
	if err != nil {
		errList := []error{
			fmt.Errorf(
				"failed to set up sandbox container %q network for pod %q: %v",
				createResp.ID,
				containerConfig.Metadata.Name,
				err,
			),
		}

		// Ensure network resources are cleaned up even if the plugin
		// succeeded but an error happened between that success and here.
		err = ds.network.TearDownPod(containerConfig.GetMetadata().Namespace, containerConfig.GetMetadata().Name, cID)
		if err != nil {
			errList = append(
				errList,
				fmt.Errorf(
					"failed to clean up sandbox container %q network for pod %q: %v",
					createResp.ID,
					containerConfig.Metadata.Name,
					err,
				),
			)
		}

		err = ds.client.StopContainer(createResp.ID, defaultSandboxGracePeriod)
		if err != nil {
			errList = append(
				errList,
				fmt.Errorf(
					"failed to stop sandbox container %q for pod %q: %v",
					createResp.ID,
					containerConfig.Metadata.Name,
					err,
				),
			)
		}

		return resp, errors.NewAggregate(errList)
	}

	return resp, nil
}
