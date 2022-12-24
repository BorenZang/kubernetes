/*
Copyright 2021 The Kubernetes Authors.

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

package e2enode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	kubeletpodresourcesv1 "k8s.io/kubelet/pkg/apis/podresources/v1"
	"k8s.io/kubernetes/pkg/kubelet/apis/podresources"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/devicemanager/checkpoint"
	"k8s.io/kubernetes/pkg/kubelet/util"
	admissionapi "k8s.io/pod-security-admission/api"

	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2etestfiles "k8s.io/kubernetes/test/e2e/framework/testfiles"
	testutils "k8s.io/kubernetes/test/utils"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

const (
	devicePluginDir = "/var/lib/kubelet/device-plugins"
	checkpointName  = "kubelet_internal_checkpoint"
)

// Serial because the test updates kubelet configuration.
var _ = SIGDescribe("Device Manager  [Serial] [Feature:DeviceManager][NodeFeature:DeviceManager]", func() {
	checkpointFullPath := filepath.Join(devicePluginDir, checkpointName)
	f := framework.NewDefaultFramework("devicemanager-test")
	f.NamespacePodSecurityEnforceLevel = admissionapi.LevelPrivileged

	ginkgo.Context("With SRIOV devices in the system", func() {
		// this test wants to reproduce what happened in https://github.com/kubernetes/kubernetes/issues/102880
		ginkgo.It("should be able to recover V1 (aka pre-1.20) checkpoint data and reject pods before device re-registration", func(ctx context.Context) {
			if sriovdevCount, err := countSRIOVDevices(); err != nil || sriovdevCount == 0 {
				e2eskipper.Skipf("this test is meant to run on a system with at least one configured VF from SRIOV device")
			}

			configMap := getSRIOVDevicePluginConfigMap(framework.TestContext.SriovdpConfigMapFile)
			sd := setupSRIOVConfigOrFail(ctx, f, configMap)

			waitForSRIOVResources(ctx, f, sd)

			cntName := "gu-container"
			// we create and delete a pod to make sure the internal device manager state contains a pod allocation
			ginkgo.By(fmt.Sprintf("Successfully admit one guaranteed pod with 1 core, 1 %s device", sd.resourceName))
			var initCtnAttrs []tmCtnAttribute
			ctnAttrs := []tmCtnAttribute{
				{
					ctnName:       cntName,
					cpuRequest:    "1000m",
					cpuLimit:      "1000m",
					deviceName:    sd.resourceName,
					deviceRequest: "1",
					deviceLimit:   "1",
				},
			}

			podName := "gu-pod-rec-pre-1"
			framework.Logf("creating pod %s attrs %v", podName, ctnAttrs)
			pod := makeTopologyManagerTestPod(podName, ctnAttrs, initCtnAttrs)
			pod = e2epod.NewPodClient(f).CreateSync(ctx, pod)

			// now we need to simulate a node drain, so we remove all the pods, including the sriov device plugin.

			ginkgo.By("deleting the pod")
			// note we delete right now because we know the current implementation of devicemanager will NOT
			// clean up on pod deletion. When this changes, the deletion needs to be done after the test is done.
			deletePodSyncByName(ctx, f, pod.Name)
			waitForAllContainerRemoval(ctx, pod.Name, pod.Namespace)

			ginkgo.By("teardown the sriov device plugin")
			// since we will NOT be recreating the plugin, we clean up everything now
			teardownSRIOVConfigOrFail(ctx, f, sd)

			ginkgo.By("stopping the kubelet")
			killKubelet("SIGSTOP")

			ginkgo.By("rewriting the kubelet checkpoint file as v1")
			err := rewriteCheckpointAsV1(devicePluginDir, checkpointName)
			// make sure we remove any leftovers
			defer os.Remove(checkpointFullPath)
			framework.ExpectNoError(err)

			// this mimics a kubelet restart after the upgrade
			// TODO: is SIGTERM (less brutal) good enough?
			ginkgo.By("killing the kubelet")
			killKubelet("SIGKILL")

			ginkgo.By("waiting for the kubelet to be ready again")
			// Wait for the Kubelet to be ready.
			gomega.Eventually(ctx, func(ctx context.Context) bool {
				nodes, err := e2enode.TotalReady(ctx, f.ClientSet)
				framework.ExpectNoError(err)
				return nodes == 1
			}, time.Minute, time.Second).Should(gomega.BeTrue())

			// note we DO NOT start the sriov device plugin. This is intentional.
			// issue#102880 reproduces because of a race on startup caused by corrupted device manager
			// state which leads to v1.Node object not updated on apiserver.
			// So to hit the issue we need to receive the pod *before* the device plugin registers itself.
			// The simplest and safest way to reproduce is just avoid to run the device plugin again

			podName = "gu-pod-rec-post-2"
			framework.Logf("creating pod %s attrs %v", podName, ctnAttrs)
			pod = makeTopologyManagerTestPod(podName, ctnAttrs, initCtnAttrs)

			pod = e2epod.NewPodClient(f).Create(ctx, pod)
			err = e2epod.WaitForPodCondition(ctx, f.ClientSet, f.Namespace.Name, pod.Name, "Failed", 30*time.Second, func(pod *v1.Pod) (bool, error) {
				if pod.Status.Phase != v1.PodPending {
					return true, nil
				}
				return false, nil
			})
			framework.ExpectNoError(err)
			pod, err = e2epod.NewPodClient(f).Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			if pod.Status.Phase != v1.PodFailed {
				framework.Failf("pod %s not failed: %v", pod.Name, pod.Status)
			}

			framework.Logf("checking pod %s status reason (%s)", pod.Name, pod.Status.Reason)
			if !isUnexpectedAdmissionError(pod) {
				framework.Failf("pod %s failed for wrong reason: %q", pod.Name, pod.Status.Reason)
			}

			deletePodSyncByName(ctx, f, pod.Name)
		})

		ginkgo.It("should be able to recover V1 (aka pre-1.20) checkpoint data and update topology info on device re-registration", func(ctx context.Context) {
			if sriovdevCount, err := countSRIOVDevices(); err != nil || sriovdevCount == 0 {
				e2eskipper.Skipf("this test is meant to run on a system with at least one configured VF from SRIOV device")
			}

			endpoint, err := util.LocalEndpoint(defaultPodResourcesPath, podresources.Socket)
			framework.ExpectNoError(err)

			configMap := getSRIOVDevicePluginConfigMap(framework.TestContext.SriovdpConfigMapFile)

			sd := setupSRIOVConfigOrFail(ctx, f, configMap)
			waitForSRIOVResources(ctx, f, sd)

			cli, conn, err := podresources.GetV1Client(endpoint, defaultPodResourcesTimeout, defaultPodResourcesMaxSize)
			framework.ExpectNoError(err)

			resp, err := cli.GetAllocatableResources(ctx, &kubeletpodresourcesv1.AllocatableResourcesRequest{})
			conn.Close()
			framework.ExpectNoError(err)

			suitableDevs := 0
			for _, dev := range resp.GetDevices() {
				for _, node := range dev.GetTopology().GetNodes() {
					if node.GetID() != 0 {
						suitableDevs++
					}
				}
			}
			if suitableDevs == 0 {
				teardownSRIOVConfigOrFail(ctx, f, sd)
				e2eskipper.Skipf("no devices found on NUMA Cell other than 0")
			}

			cntName := "gu-container"
			// we create and delete a pod to make sure the internal device manager state contains a pod allocation
			ginkgo.By(fmt.Sprintf("Successfully admit one guaranteed pod with 1 core, 1 %s device", sd.resourceName))
			var initCtnAttrs []tmCtnAttribute
			ctnAttrs := []tmCtnAttribute{
				{
					ctnName:       cntName,
					cpuRequest:    "1000m",
					cpuLimit:      "1000m",
					deviceName:    sd.resourceName,
					deviceRequest: "1",
					deviceLimit:   "1",
				},
			}

			podName := "gu-pod-rec-pre-1"
			framework.Logf("creating pod %s attrs %v", podName, ctnAttrs)
			pod := makeTopologyManagerTestPod(podName, ctnAttrs, initCtnAttrs)
			pod = e2epod.NewPodClient(f).CreateSync(ctx, pod)

			// now we need to simulate a node drain, so we remove all the pods, including the sriov device plugin.

			ginkgo.By("deleting the pod")
			// note we delete right now because we know the current implementation of devicemanager will NOT
			// clean up on pod deletion. When this changes, the deletion needs to be done after the test is done.
			deletePodSyncByName(ctx, f, pod.Name)
			waitForAllContainerRemoval(ctx, pod.Name, pod.Namespace)

			ginkgo.By("teardown the sriov device plugin")
			// no need to delete the config now (speed up later)
			deleteSRIOVPodOrFail(ctx, f, sd)

			ginkgo.By("stopping the kubelet")
			killKubelet("SIGSTOP")

			ginkgo.By("rewriting the kubelet checkpoint file as v1")
			err = rewriteCheckpointAsV1(devicePluginDir, checkpointName)
			// make sure we remove any leftovers
			defer os.Remove(checkpointFullPath)
			framework.ExpectNoError(err)

			// this mimics a kubelet restart after the upgrade
			// TODO: is SIGTERM (less brutal) good enough?
			ginkgo.By("killing the kubelet")
			killKubelet("SIGKILL")

			ginkgo.By("waiting for the kubelet to be ready again")
			// Wait for the Kubelet to be ready.
			gomega.Eventually(ctx, func(ctx context.Context) bool {
				nodes, err := e2enode.TotalReady(ctx, f.ClientSet)
				framework.ExpectNoError(err)
				return nodes == 1
			}, time.Minute, time.Second).Should(gomega.BeTrue())

			sd2 := &sriovData{
				configMap:      sd.configMap,
				serviceAccount: sd.serviceAccount,
			}
			sd2.pod = createSRIOVPodOrFail(ctx, f)
			ginkgo.DeferCleanup(teardownSRIOVConfigOrFail, f, sd2)
			waitForSRIOVResources(ctx, f, sd2)

			compareSRIOVResources(sd, sd2)

			cli, conn, err = podresources.GetV1Client(endpoint, defaultPodResourcesTimeout, defaultPodResourcesMaxSize)
			framework.ExpectNoError(err)
			defer conn.Close()

			resp2, err := cli.GetAllocatableResources(ctx, &kubeletpodresourcesv1.AllocatableResourcesRequest{})
			framework.ExpectNoError(err)

			cntDevs := stringifyContainerDevices(resp.GetDevices())
			cntDevs2 := stringifyContainerDevices(resp2.GetDevices())
			if cntDevs != cntDevs2 {
				framework.Failf("different allocatable resources expected %v got %v", cntDevs, cntDevs2)
			}
		})

	})

	ginkgo.Context("With sample device plugin", func(ctx context.Context) {
		var deviceCount int = 2
		var devicePluginPod *v1.Pod
		var testPods []*v1.Pod

		ginkgo.BeforeEach(func() {
			ginkgo.By("Wait for node to be ready")
			gomega.Eventually(func() bool {
				nodes, err := e2enode.TotalReady(ctx, f.ClientSet)
				framework.ExpectNoError(err)
				return nodes == 1
			}, time.Minute, time.Second).Should(gomega.BeTrue())

			ginkgo.By("Scheduling a sample device plugin pod")
			data, err := e2etestfiles.Read(SampleDevicePluginDS2YAML)
			if err != nil {
				framework.Fail(err.Error())
			}
			ds := readDaemonSetV1OrDie(data)

			dp := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: sampleDevicePluginName,
				},
				Spec: ds.Spec.Template.Spec,
			}

			devicePluginPod = e2epod.NewPodClient(f).CreateSync(ctx, dp)

			ginkgo.By("Waiting for devices to become available on the local node")
			gomega.Eventually(func() bool {
				node, ready := getLocalTestNode(ctx, f)
				return ready && numberOfSampleResources(node) > 0
			}, 5*time.Minute, framework.Poll).Should(gomega.BeTrue())
			framework.Logf("Successfully created device plugin pod")

			devsLen := int64(deviceCount) // shortcut
			ginkgo.By("Waiting for the resource exported by the sample device plugin to become available on the local node")
			gomega.Eventually(func() bool {
				node, ready := getLocalTestNode(ctx, f)
				return ready &&
					numberOfDevicesCapacity(node, resourceName) == devsLen &&
					numberOfDevicesAllocatable(node, resourceName) == devsLen
			}, 30*time.Second, framework.Poll).Should(gomega.BeTrue())
		})

		ginkgo.AfterEach(func() {
			ginkgo.By("Deleting the device plugin pod")
			e2epod.NewPodClient(f).DeleteSync(ctx, devicePluginPod.Name, metav1.DeleteOptions{}, time.Minute)

			ginkgo.By("Deleting any Pods created by the test")
			l, err := e2epod.NewPodClient(f).List(context.TODO(), metav1.ListOptions{})
			framework.ExpectNoError(err)
			for _, p := range l.Items {
				if p.Namespace != f.Namespace.Name {
					continue
				}

				framework.Logf("Deleting pod: %s", p.Name)
				e2epod.NewPodClient(f).DeleteSync(ctx, p.Name, metav1.DeleteOptions{}, 2*time.Minute)
			}

			restartKubelet(true)

			ginkgo.By("Waiting for devices to become unavailable on the local node")
			gomega.Eventually(func() bool {
				node, ready := getLocalTestNode(ctx, f)
				return ready && numberOfSampleResources(node) <= 0
			}, 5*time.Minute, framework.Poll).Should(gomega.BeTrue())
		})

		ginkgo.It("should recover device plugin pod first, then pod consuming devices", func() {
			var err error
			podCMD := "cat /tmp/Dev-* && sleep inf"

			ginkgo.By(fmt.Sprintf("creating %d pods requiring %q", deviceCount, resourceName))
			var devReqPods []*v1.Pod
			for idx := 0; idx < deviceCount; idx++ {
				pod := makeBusyboxDeviceRequiringPod(resourceName, podCMD)
				devReqPods = append(devReqPods, pod)
			}
			testPods = e2epod.NewPodClient(f).CreateBatch(ctx, devReqPods)

			ginkgo.By("making sure all the pods are ready")
			waitForPodConditionBatch(ctx, f, testPods, "Ready", 120*time.Second, testutils.PodRunningReady)

			ginkgo.By("stopping the kubelet")
			startKubelet := stopKubelet()

			ginkgo.By("stopping all the local containers - using CRI")
			rs, _, err := getCRIClient()
			framework.ExpectNoError(err)
			sandboxes, err := rs.ListPodSandbox(ctx, &runtimeapi.PodSandboxFilter{})
			framework.ExpectNoError(err)
			for _, sandbox := range sandboxes {
				gomega.Expect(sandbox.Metadata).ToNot(gomega.BeNil())
				ginkgo.By(fmt.Sprintf("deleting pod using CRI: %s/%s -> %s", sandbox.Metadata.Namespace, sandbox.Metadata.Name, sandbox.Id))

				err := rs.RemovePodSandbox(ctx, sandbox.Id)
				framework.ExpectNoError(err)
			}

			ginkgo.By("restarting the kubelet")
			startKubelet()

			ginkgo.By("waiting for the kubelet to be ready again")
			// Wait for the Kubelet to be ready.
			gomega.Eventually(func() bool {
				nodes, err := e2enode.TotalReady(ctx, f.ClientSet)
				framework.ExpectNoError(err)
				return nodes == 1
			}, time.Minute, time.Second).Should(gomega.BeTrue())

			// TODO: here we need to tolerate pods waiting to be recreated

			ginkgo.By("making sure all the pods are ready after the recovery")
			waitForPodConditionBatch(ctx, f, testPods, "Ready", 120*time.Second, testutils.PodRunningReady)

			ginkgo.By("removing all the pods")
			deleteBatch(ctx, f, testPods)
		})

	})

})

func compareSRIOVResources(expected, got *sriovData) {
	if expected.resourceName != got.resourceName {
		framework.Failf("different SRIOV resource name: expected %q got %q", expected.resourceName, got.resourceName)
	}
	if expected.resourceAmount != got.resourceAmount {
		framework.Failf("different SRIOV resource amount: expected %d got %d", expected.resourceAmount, got.resourceAmount)
	}
}

func isUnexpectedAdmissionError(pod *v1.Pod) bool {
	re := regexp.MustCompile(`Unexpected.*Admission.*Error`)
	return re.MatchString(pod.Status.Reason)
}

func rewriteCheckpointAsV1(dir, name string) error {
	ginkgo.By(fmt.Sprintf("Creating temporary checkpoint manager (dir=%q)", dir))
	checkpointManager, err := checkpointmanager.NewCheckpointManager(dir)
	if err != nil {
		return err
	}
	cp := checkpoint.New(make([]checkpoint.PodDevicesEntry, 0), make(map[string][]string))
	err = checkpointManager.GetCheckpoint(name, cp)
	if err != nil {
		return err
	}

	ginkgo.By(fmt.Sprintf("Read checkpoint %q %#v", name, cp))

	podDevices, registeredDevs := cp.GetDataInLatestFormat()
	podDevicesV1 := convertPodDeviceEntriesToV1(podDevices)
	cpV1 := checkpoint.NewV1(podDevicesV1, registeredDevs)

	blob, err := cpV1.MarshalCheckpoint()
	if err != nil {
		return err
	}

	// TODO: why `checkpointManager.CreateCheckpoint(name, cpV1)` doesn't seem to work?
	ckPath := filepath.Join(dir, name)
	os.WriteFile(filepath.Join("/tmp", name), blob, 0600)
	return os.WriteFile(ckPath, blob, 0600)
}

func convertPodDeviceEntriesToV1(entries []checkpoint.PodDevicesEntry) []checkpoint.PodDevicesEntryV1 {
	entriesv1 := []checkpoint.PodDevicesEntryV1{}
	for _, entry := range entries {
		deviceIDs := []string{}
		for _, perNUMANodeDevIDs := range entry.DeviceIDs {
			deviceIDs = append(deviceIDs, perNUMANodeDevIDs...)
		}
		entriesv1 = append(entriesv1, checkpoint.PodDevicesEntryV1{
			PodUID:        entry.PodUID,
			ContainerName: entry.ContainerName,
			ResourceName:  entry.ResourceName,
			DeviceIDs:     deviceIDs,
			AllocResp:     entry.AllocResp,
		})
	}
	return entriesv1
}

func stringifyContainerDevices(devs []*kubeletpodresourcesv1.ContainerDevices) string {
	entries := []string{}
	for _, dev := range devs {
		devIDs := dev.GetDeviceIds()
		if devIDs != nil {
			for _, devID := range dev.DeviceIds {
				nodes := dev.GetTopology().GetNodes()
				if nodes != nil {
					for _, node := range nodes {
						entries = append(entries, fmt.Sprintf("%s[%s]@NUMA=%d", dev.ResourceName, devID, node.GetID()))
					}
				} else {
					entries = append(entries, fmt.Sprintf("%s[%s]@NUMA=none", dev.ResourceName, devID))
				}
			}
		} else {
			entries = append(entries, dev.ResourceName)
		}
	}
	sort.Strings(entries)
	return strings.Join(entries, ", ")
}

func waitForPodConditionBatch(ctx context.Context, f *framework.Framework, pods []*v1.Pod, conditionDesc string, timeout time.Duration, condition func(pod *v1.Pod) (bool, error)) {
	var wg sync.WaitGroup
	for _, pod := range pods {
		wg.Add(1)
		go func(podNS, podName string) {
			defer ginkgo.GinkgoRecover()
			defer wg.Done()

			err := e2epod.WaitForPodCondition(ctx, f.ClientSet, podNS, podName, conditionDesc, timeout, condition)
			framework.ExpectNoError(err, "pod %s/%s did not go running", podNS, podName)
			framework.Logf("pod %s/%s running", podNS, podName)
		}(pod.Namespace, pod.Name)
	}
	wg.Wait()
}

func deleteBatch(ctx context.Context, f *framework.Framework, pods []*v1.Pod) {
	var wg sync.WaitGroup
	for _, pod := range pods {
		wg.Add(1)
		go func(podNS, podName string) {
			defer ginkgo.GinkgoRecover()
			defer wg.Done()

			deletePodSyncByName(ctx, f, podName)
			waitForAllContainerRemoval(ctx, podName, podNS)
		}(pod.Namespace, pod.Name)
	}
	wg.Wait()
}

func makeBusyboxDeviceRequiringPod(resourceName, cmd string) *v1.Pod {
	podName := "device-manager-test-" + string(uuid.NewUUID())
	rl := v1.ResourceList{
		v1.ResourceName(resourceName): *resource.NewQuantity(1, resource.DecimalSI),
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever,
			Containers: []v1.Container{{
				Image: busyboxImage,
				Name:  podName,
				// Runs the specified command in the test pod.
				Command: []string{"sh", "-c", cmd},
				Resources: v1.ResourceRequirements{
					Limits:   rl,
					Requests: rl,
				},
			}},
		},
	}
}
