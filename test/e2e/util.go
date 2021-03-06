/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

package e2e

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/cache"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/clientcmd"
	clientcmdapi "github.com/GoogleCloudPlatform/kubernetes/pkg/client/clientcmd/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/cloudprovider"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/fields"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubectl"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util/wait"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/watch"

	"github.com/davecgh/go-spew/spew"
	"golang.org/x/crypto/ssh"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	// Initial pod start can be delayed O(minutes) by slow docker pulls
	// TODO: Make this 30 seconds once #4566 is resolved.
	podStartTimeout = 5 * time.Minute

	// String used to mark pod deletion
	nonExist = "NonExist"

	// How often to poll pods and nodes.
	poll = 5 * time.Second

	// service accounts are provisioned after namespace creation
	// a service account is required to support pod creation in a namespace as part of admission control
	serviceAccountProvisionTimeout = 2 * time.Minute

	// How long to try single API calls (like 'get' or 'list'). Used to prevent
	// transient failures from failing tests.
	singleCallTimeout = 30 * time.Second

	// How long nodes have to be "ready" when a test begins. They should already
	// be "ready" before the test starts, so this is small.
	nodeReadyInitialTimeout = 20 * time.Second

	// How long pods have to be "ready" when a test begins. They should already
	// be "ready" before the test starts, so this is small.
	podReadyBeforeTimeout = 20 * time.Second

	// How wide to print pod names, by default. Useful for aligning printing to
	// quickly scan through output.
	podPrintWidth = 55
)

type CloudConfig struct {
	ProjectID         string
	Zone              string
	Cluster           string
	MasterName        string
	NodeInstanceGroup string
	NumNodes          int
	ClusterTag        string

	Provider cloudprovider.Interface
}

type TestContextType struct {
	KubeConfig     string
	KubeContext    string
	CertDir        string
	Host           string
	RepoRoot       string
	Provider       string
	CloudConfig    CloudConfig
	KubectlPath    string
	OutputDir      string
	prefix         string
	MinStartupPods int
}

var testContext TestContextType

type ContainerFailures struct {
	status   *api.ContainerStateTerminated
	restarts int
}

// Convenient wrapper around cache.Store that returns list of api.Pod instead of interface{}.
type podStore struct {
	cache.Store
	stopCh chan struct{}
}

func newPodStore(c *client.Client, namespace string, label labels.Selector, field fields.Selector) *podStore {
	lw := &cache.ListWatch{
		ListFunc: func() (runtime.Object, error) {
			return c.Pods(namespace).List(label, field)
		},
		WatchFunc: func(rv string) (watch.Interface, error) {
			return c.Pods(namespace).Watch(label, field, rv)
		},
	}
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	stopCh := make(chan struct{})
	cache.NewReflector(lw, &api.Pod{}, store, 0).RunUntil(stopCh)
	return &podStore{store, stopCh}
}

func (s *podStore) List() []*api.Pod {
	objects := s.Store.List()
	pods := make([]*api.Pod, 0)
	for _, o := range objects {
		pods = append(pods, o.(*api.Pod))
	}
	return pods
}

func (s *podStore) Stop() {
	close(s.stopCh)
}

type RCConfig struct {
	Client        *client.Client
	Image         string
	Name          string
	Namespace     string
	PollInterval  time.Duration
	Timeout       time.Duration
	PodStatusFile *os.File
	Replicas      int

	// Env vars, set the same for every pod.
	Env map[string]string

	// Extra labels added to every pod.
	Labels map[string]string

	// Ports to declare in the container (map of name to containerPort).
	Ports map[string]int

	// Pointer to a list of pods; if non-nil, will be set to a list of pods
	// created by this RC by RunRC.
	CreatedPods *[]*api.Pod
}

func Logf(format string, a ...interface{}) {
	fmt.Fprintf(GinkgoWriter, "INFO: "+format+"\n", a...)
}

func Failf(format string, a ...interface{}) {
	Fail(fmt.Sprintf(format, a...), 1)
}

func Skipf(format string, args ...interface{}) {
	Skip(fmt.Sprintf(format, args...))
}

func SkipUnlessNodeCountIsAtLeast(minNodeCount int) {
	if testContext.CloudConfig.NumNodes < minNodeCount {
		Skipf("Requires at least %d nodes (not %d)", minNodeCount, testContext.CloudConfig.NumNodes)
	}
}

func SkipIfProviderIs(unsupportedProviders ...string) {
	if providerIs(unsupportedProviders...) {
		Skipf("Not supported for providers %v (found %s)", unsupportedProviders, testContext.Provider)
	}
}

func SkipUnlessProviderIs(supportedProviders ...string) {
	if !providerIs(supportedProviders...) {
		Skipf("Only supported for providers %v (not %s)", supportedProviders, testContext.Provider)
	}
}

func providerIs(providers ...string) bool {
	for _, provider := range providers {
		if strings.ToLower(provider) == strings.ToLower(testContext.Provider) {
			return true
		}
	}
	return false
}

type podCondition func(pod *api.Pod) (bool, error)

// podReady returns whether pod has a condition of Ready with a status of true.
func podReady(pod *api.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == api.PodReady && cond.Status == api.ConditionTrue {
			return true
		}
	}
	return false
}

// logPodStates logs basic info of provided pods for debugging.
func logPodStates(pods []api.Pod) {
	// Find maximum widths for pod, node, and phase strings for column printing.
	maxPodW, maxNodeW, maxPhaseW := len("POD"), len("NODE"), len("PHASE")
	for _, pod := range pods {
		if len(pod.ObjectMeta.Name) > maxPodW {
			maxPodW = len(pod.ObjectMeta.Name)
		}
		if len(pod.Spec.NodeName) > maxNodeW {
			maxNodeW = len(pod.Spec.NodeName)
		}
		if len(pod.Status.Phase) > maxPhaseW {
			maxPhaseW = len(pod.Status.Phase)
		}
	}
	// Increase widths by one to separate by a single space.
	maxPodW++
	maxNodeW++
	maxPhaseW++

	// Log pod info. * does space padding, - makes them left-aligned.
	Logf("%-[1]*[2]s %-[3]*[4]s %-[5]*[6]s %[7]s",
		maxPodW, "POD", maxNodeW, "NODE", maxPhaseW, "PHASE", "CONDITIONS")
	for _, pod := range pods {
		Logf("%-[1]*[2]s %-[3]*[4]s %-[5]*[6]s %[7]s",
			maxPodW, pod.ObjectMeta.Name, maxNodeW, pod.Spec.NodeName, maxPhaseW, pod.Status.Phase, pod.Status.Conditions)
	}
	Logf("") // Final empty line helps for readability.
}

// podRunningReady checks whether pod p's phase is running and it has a ready
// condition of status true.
func podRunningReady(p *api.Pod) (bool, error) {
	// Check the phase is running.
	if p.Status.Phase != api.PodRunning {
		return false, fmt.Errorf("want pod '%s' on '%s' to be '%v' but was '%v'",
			p.ObjectMeta.Name, p.Spec.NodeName, api.PodRunning, p.Status.Phase)
	}
	// Check the ready condition is true.
	if !podReady(p) {
		return false, fmt.Errorf("pod '%s' on '%s' didn't have condition {%v %v}; conditions: %v",
			p.ObjectMeta.Name, p.Spec.NodeName, api.PodReady, api.ConditionTrue, p.Status.Conditions)
	}
	return true, nil
}

// waitForPodsRunningReady waits up to timeout to ensure that all pods in
// namespace ns are running and ready, requiring that it finds at least minPods.
// It has separate behavior from other 'wait for' pods functions in that it re-
// queries the list of pods on every iteration. This is useful, for example, in
// cluster startup, because the number of pods increases while waiting.
func waitForPodsRunningReady(ns string, minPods int, timeout time.Duration) error {
	c, err := loadClient()
	if err != nil {
		return err
	}
	start := time.Now()
	Logf("Waiting up to %v for all pods (need at least %d) in namespace '%s' to be running and ready",
		timeout, minPods, ns)
	if wait.Poll(poll, timeout, func() (bool, error) {
		// We get the new list of pods in every iteration beause more pods come
		// online during startup and we want to ensure they are also checked.
		podList, err := c.Pods(ns).List(labels.Everything(), fields.Everything())
		if err != nil {
			Logf("Error getting pods in namespace '%s': %v", ns, err)
			return false, nil
		}
		nOk, badPods := 0, []api.Pod{}
		for _, pod := range podList.Items {
			if res, err := podRunningReady(&pod); res && err == nil {
				nOk++
			} else {
				badPods = append(badPods, pod)
			}
		}
		Logf("%d / %d pods in namespace '%s' are running and ready (%d seconds elapsed)",
			nOk, len(podList.Items), ns, int(time.Since(start).Seconds()))
		if nOk == len(podList.Items) && nOk >= minPods {
			return true, nil
		}
		logPodStates(badPods)
		return false, nil
	}) != nil {
		return fmt.Errorf("Not all pods in namespace '%s' running and ready within %v", ns, timeout)
	}
	return nil
}

func waitForServiceAccountInNamespace(c *client.Client, ns, serviceAccountName string, timeout time.Duration) error {
	Logf("Waiting up to %v for service account %s to be provisioned in ns %s", timeout, serviceAccountName, ns)
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(poll) {
		_, err := c.ServiceAccounts(ns).Get(serviceAccountName)
		if err != nil {
			Logf("Get service account %s in ns %s failed, ignoring for %v: %v", serviceAccountName, ns, poll, err)
			continue
		}
		Logf("Service account %s in ns %s found. (%v)", serviceAccountName, ns, time.Since(start))
		return nil
	}
	return fmt.Errorf("Service account %s in namespace %s not ready within %v", serviceAccountName, ns, timeout)
}

func waitForPodCondition(c *client.Client, ns, podName, desc string, timeout time.Duration, condition podCondition) error {
	Logf("Waiting up to %[1]v for pod %-[2]*[3]s status to be %[4]s", timeout, podPrintWidth, podName, desc)
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(poll) {
		pod, err := c.Pods(ns).Get(podName)
		if err != nil {
			// Aligning this text makes it much more readable
			Logf("Get pod %-[1]*[2]s in namespace '%[3]s' failed, ignoring for %[4]v. Error: %[5]v",
				podPrintWidth, podName, ns, poll, err)
			continue
		}
		done, err := condition(pod)
		if done {
			return err
		}
		Logf("Waiting for pod %-[1]*[2]s in namespace '%[3]s' status to be '%[4]s'"+
			"(found phase: %[5]q, readiness: %[6]t) (%[7]v elapsed)",
			podPrintWidth, podName, ns, desc, pod.Status.Phase, podReady(pod), time.Since(start))
	}
	return fmt.Errorf("gave up waiting for pod '%s' to be '%s' after %v", podName, desc, timeout)
}

// waitForDefaultServiceAccountInNamespace waits for the default service account to be provisioned
// the default service account is what is associated with pods when they do not specify a service account
// as a result, pods are not able to be provisioned in a namespace until the service account is provisioned
func waitForDefaultServiceAccountInNamespace(c *client.Client, namespace string) error {
	return waitForServiceAccountInNamespace(c, namespace, "default", serviceAccountProvisionTimeout)
}

// waitForPersistentVolumePhase waits for a PersistentVolume to be in a specific phase or until timeout occurs, whichever comes first.
func waitForPersistentVolumePhase(phase api.PersistentVolumePhase, c *client.Client, pvName string, poll, timeout time.Duration) error {
	Logf("Waiting up to %v for PersistentVolume %s to have phase %s", timeout, pvName, phase)
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(poll) {
		pv, err := c.PersistentVolumes().Get(pvName)
		if err != nil {
			Logf("Get persistent volume %s in failed, ignoring for %v: %v", pvName, poll, err)
			continue
		} else {
			if pv.Status.Phase == phase {
				Logf("PersistentVolume %s found and phase=%s (%v)", pvName, phase, time.Since(start))
				return nil
			} else {
				Logf("PersistentVolume %s found but phase is %s instead of %s.", pvName, pv.Status.Phase, phase)
			}
		}
	}
	return fmt.Errorf("PersistentVolume %s not in phase %s within %v", pvName, phase, timeout)
}

// createTestingNS should be used by every test, note that we append a common prefix to the provided test name.
// Please see NewFramework instead of using this directly.
func createTestingNS(baseName string, c *client.Client) (*api.Namespace, error) {
	namespaceObj := &api.Namespace{
		ObjectMeta: api.ObjectMeta{
			GenerateName: fmt.Sprintf("e2e-tests-%v-", baseName),
			Namespace:    "",
		},
		Status: api.NamespaceStatus{},
	}
	// Be robust about making the namespace creation call.
	var got *api.Namespace
	if err := wait.Poll(poll, singleCallTimeout, func() (bool, error) {
		var err error
		got, err = c.Namespaces().Create(namespaceObj)
		if err != nil {
			return false, nil
		}
		return true, nil
	}); err != nil {
		return nil, err
	}

	if err := waitForDefaultServiceAccountInNamespace(c, got.Name); err != nil {
		return nil, err
	}
	return got, nil
}

// deleteTestingNS checks whether all e2e based existing namespaces are in the Terminating state
// and waits until they are finally deleted.
func deleteTestingNS(c *client.Client) error {
	// TODO: Since we don't have support for bulk resource deletion in the API,
	// while deleting a namespace we are deleting all objects from that namespace
	// one by one (one deletion == one API call). This basically exposes us to
	// throttling - currently controller-manager has a limit of max 20 QPS.
	// Once #10217 is implemented and used in namespace-controller, deleting all
	// object from a given namespace should be much faster and we will be able
	// to lower this timeout.
	// However, now Density test is producing ~26000 events and Load capacity test
	// is producing ~35000 events, thus assuming there are no other requests it will
	// take ~30 minutes to fully delete the namespace. Thus I'm setting it to 60
	// minutes to avoid any timeouts here.
	timeout := 60 * time.Minute

	Logf("Waiting for terminating namespaces to be deleted...")
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(15 * time.Second) {
		namespaces, err := c.Namespaces().List(labels.Everything(), fields.Everything())
		if err != nil {
			Logf("Listing namespaces failed: %v", err)
			continue
		}
		terminating := 0
		for _, ns := range namespaces.Items {
			if strings.HasPrefix(ns.ObjectMeta.Name, "e2e-tests-") {
				if ns.Status.Phase == api.NamespaceActive {
					return fmt.Errorf("Namespace %s is active", ns)
				}
				terminating++
			}
		}
		if terminating == 0 {
			return nil
		}
	}
	return fmt.Errorf("Waiting for terminating namespaces to be deleted timed out")
}

func waitForPodRunningInNamespace(c *client.Client, podName string, namespace string) error {
	return waitForPodCondition(c, namespace, podName, "running", podStartTimeout, func(pod *api.Pod) (bool, error) {
		if pod.Status.Phase == api.PodRunning {
			Logf("Found pod '%s' on node '%s'", podName, pod.Spec.NodeName)
			return true, nil
		}
		if pod.Status.Phase == api.PodFailed {
			return true, fmt.Errorf("Giving up; pod went into failed status: \n%s", spew.Sprintf("%#v", pod))
		}
		return false, nil
	})
}

func waitForPodRunning(c *client.Client, podName string) error {
	return waitForPodRunningInNamespace(c, podName, api.NamespaceDefault)
}

// waitForPodNotPending returns an error if it took too long for the pod to go out of pending state.
func waitForPodNotPending(c *client.Client, ns, podName string) error {
	return waitForPodCondition(c, ns, podName, "!pending", podStartTimeout, func(pod *api.Pod) (bool, error) {
		if pod.Status.Phase != api.PodPending {
			Logf("Saw pod '%s' in namespace '%s' out of pending state (found '%q')", podName, ns, pod.Status.Phase)
			return true, nil
		}
		return false, nil
	})
}

// waitForPodSuccessInNamespace returns nil if the pod reached state success, or an error if it reached failure or ran too long.
func waitForPodSuccessInNamespace(c *client.Client, podName string, contName string, namespace string) error {
	return waitForPodCondition(c, namespace, podName, "success or failure", podStartTimeout, func(pod *api.Pod) (bool, error) {
		// Cannot use pod.Status.Phase == api.PodSucceeded/api.PodFailed due to #2632
		ci, ok := api.GetContainerStatus(pod.Status.ContainerStatuses, contName)
		if !ok {
			Logf("No Status.Info for container '%s' in pod '%s' yet", contName, podName)
		} else {
			if ci.State.Terminated != nil {
				if ci.State.Terminated.ExitCode == 0 {
					By("Saw pod success")
					return true, nil
				} else {
					return true, fmt.Errorf("pod '%s' terminated with failure: %+v", podName, ci.State.Terminated)
				}
			} else {
				Logf("Nil State.Terminated for container '%s' in pod '%s' in namespace '%s' so far", contName, podName, namespace)
			}
		}
		return false, nil
	})
}

// waitForPodSuccess returns nil if the pod reached state success, or an error if it reached failure or ran too long.
// The default namespace is used to identify pods.
func waitForPodSuccess(c *client.Client, podName string, contName string) error {
	return waitForPodSuccessInNamespace(c, podName, contName, api.NamespaceDefault)
}

// waitForRCPodOnNode returns the pod from the given replication controller (decribed by rcName) which is scheduled on the given node.
// In case of failure or too long waiting time, an error is returned.
func waitForRCPodOnNode(c *client.Client, ns, rcName, node string) (*api.Pod, error) {
	label := labels.SelectorFromSet(labels.Set(map[string]string{"name": rcName}))
	var p *api.Pod = nil
	err := wait.Poll(10*time.Second, 5*time.Minute, func() (bool, error) {
		Logf("Waiting for pod %s to appear on node %s", rcName, node)
		pods, err := c.Pods(ns).List(label, fields.Everything())
		if err != nil {
			return false, err
		}
		for _, pod := range pods.Items {
			if pod.Spec.NodeName == node {
				Logf("Pod %s found on node %s", pod.Name, node)
				p = &pod
				return true, nil
			}
		}
		return false, nil
	})
	return p, err
}

// waitForRCPodToDisappear returns nil if the pod from the given replication controller (decribed by rcName) no longer exists.
// In case of failure or too long waiting time, an error is returned.
func waitForRCPodToDisappear(c *client.Client, ns, rcName, podName string) error {
	label := labels.SelectorFromSet(labels.Set(map[string]string{"name": rcName}))
	return wait.Poll(20*time.Second, 5*time.Minute, func() (bool, error) {
		Logf("Waiting for pod %s to disappear", podName)
		pods, err := c.Pods(ns).List(label, fields.Everything())
		if err != nil {
			return false, err
		}
		found := false
		for _, pod := range pods.Items {
			if pod.Name == podName {
				Logf("Pod %s still exists", podName)
				found = true
			}
		}
		if !found {
			Logf("Pod %s no longer exists", podName)
			return true, nil
		}
		return false, nil
	})
}

// waitForService waits until the service appears (exist == true), or disappears (exist == false)
func waitForService(c *client.Client, namespace, name string, exist bool, interval, timeout time.Duration) error {
	err := wait.Poll(interval, timeout, func() (bool, error) {
		_, err := c.Services(namespace).Get(name)
		if err != nil {
			Logf("Get service %s in namespace %s failed (%v).", name, namespace, err)
			return !exist, nil
		} else {
			Logf("Service %s in namespace %s found.", name, namespace)
			return exist, nil
		}
	})
	if err != nil {
		stateMsg := map[bool]string{true: "to appear", false: "to disappear"}
		return fmt.Errorf("error waiting for service %s/%s %s: %v", namespace, name, stateMsg[exist], err)
	}
	return nil
}

// waitForReplicationController waits until the RC appears (exist == true), or disappears (exist == false)
func waitForReplicationController(c *client.Client, namespace, name string, exist bool, interval, timeout time.Duration) error {
	err := wait.Poll(interval, timeout, func() (bool, error) {
		_, err := c.ReplicationControllers(namespace).Get(name)
		if err != nil {
			Logf("Get ReplicationController %s in namespace %s failed (%v).", name, namespace, err)
			return !exist, nil
		} else {
			Logf("ReplicationController %s in namespace %s found.", name, namespace)
			return exist, nil
		}
	})
	if err != nil {
		stateMsg := map[bool]string{true: "to appear", false: "to disappear"}
		return fmt.Errorf("error waiting for ReplicationController %s/%s %s: %v", namespace, name, stateMsg[exist], err)
	}
	return nil
}

// Context for checking pods responses by issuing GETs to them and verifying if the answer with pod name.
type podResponseChecker struct {
	c              *client.Client
	ns             string
	label          labels.Selector
	controllerName string
	respondName    bool // Whether the pod should respond with its own name.
	pods           *api.PodList
}

// checkAllResponses issues GETs to all pods in the context and verify they reply with pod name.
func (r podResponseChecker) checkAllResponses() (done bool, err error) {
	successes := 0
	currentPods, err := r.c.Pods(r.ns).List(r.label, fields.Everything())
	Expect(err).NotTo(HaveOccurred())
	for i, pod := range r.pods.Items {
		// Check that the replica list remains unchanged, otherwise we have problems.
		if !isElementOf(pod.UID, currentPods) {
			return false, fmt.Errorf("pod with UID %s is no longer a member of the replica set.  Must have been restarted for some reason.  Current replica set: %v", pod.UID, currentPods)
		}
		body, err := r.c.Get().
			Prefix("proxy").
			Namespace(r.ns).
			Resource("pods").
			Name(string(pod.Name)).
			Do().
			Raw()
		if err != nil {
			Logf("Controller %s: Failed to GET from replica %d [%s]: %v:", r.controllerName, i+1, pod.Name, err)
			continue
		}
		// The response checker expects the pod's name unless !respondName, in
		// which case it just checks for a non-empty response.
		got := string(body)
		what := ""
		if r.respondName {
			what = "expected"
			want := pod.Name
			if got != want {
				Logf("Controller %s: Replica %d [%s] expected response %q but got %q",
					r.controllerName, i+1, pod.Name, want, got)
				continue
			}
		} else {
			what = "non-empty"
			if len(got) == 0 {
				Logf("Controller %s: Replica %d [%s] expected non-empty response",
					r.controllerName, i+1, pod.Name)
				continue
			}
		}
		successes++
		Logf("Controller %s: Got %s result from replica %d [%s]: %q, %d of %d required successes so far",
			r.controllerName, what, i+1, pod.Name, got, successes, len(r.pods.Items))
	}
	if successes < len(r.pods.Items) {
		return false, nil
	}
	return true, nil
}

func loadConfig() (*client.Config, error) {
	switch {
	case testContext.KubeConfig != "":
		fmt.Printf(">>> testContext.KubeConfig: %s\n", testContext.KubeConfig)
		c, err := clientcmd.LoadFromFile(testContext.KubeConfig)
		if err != nil {
			return nil, fmt.Errorf("error loading KubeConfig: %v", err.Error())
		}
		if testContext.KubeContext != "" {
			fmt.Printf(">>> testContext.KubeContext: %s\n", testContext.KubeContext)
			c.CurrentContext = testContext.KubeContext
		}
		return clientcmd.NewDefaultClientConfig(*c, &clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: testContext.Host}}).ClientConfig()
	default:
		return nil, fmt.Errorf("KubeConfig must be specified to load client config")
	}
}

func loadClient() (*client.Client, error) {
	config, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("error creating client: %v", err.Error())
	}
	c, err := client.New(config)
	if err != nil {
		return nil, fmt.Errorf("error creating client: %v", err.Error())
	}
	return c, nil
}

// randomSuffix provides a random string to append to pods,services,rcs.
// TODO: Allow service names to have the same form as names
//       for pods and replication controllers so we don't
//       need to use such a function and can instead
//       use the UUID utilty function.
func randomSuffix() string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return strconv.Itoa(r.Int() % 10000)
}

func expectNoError(err error, explain ...interface{}) {
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), explain...)
}

// Stops everything from filePath from namespace ns and checks if everything maching selectors from the given namespace is correctly stopped.
func cleanup(filePath string, ns string, selectors ...string) {
	By("using stop to clean up resources")
	var nsArg string
	if ns != "" {
		nsArg = fmt.Sprintf("--namespace=%s", ns)
	}
	runKubectl("stop", "-f", filePath, nsArg)

	for _, selector := range selectors {
		resources := runKubectl("get", "pods,rc,se", "-l", selector, "--no-headers", nsArg)
		if resources != "" {
			Failf("Resources left running after stop:\n%s", resources)
		}
	}
}

// validatorFn is the function which is individual tests will implement.
// we may want it to return more than just an error, at some point.
type validatorFn func(c *client.Client, podID string) error

// validateController is a generic mechanism for testing RC's that are running.
// It takes a container name, a test name, and a validator function which is plugged in by a specific test.
// "containername": this is grepped for.
// "containerImage" : this is the name of the image we expect to be launched.  Not to confuse w/ images (kitten.jpg)  which are validated.
// "testname":  which gets bubbled up to the logging/failure messages if errors happen.
// "validator" function: This function is given a podID and a client, and it can do some specific validations that way.
func validateController(c *client.Client, containerImage string, replicas int, containername string, testname string, validator validatorFn, ns string) {
	getPodsTemplate := "--template={{range.items}}{{.metadata.name}} {{end}}"
	// NB: kubectl adds the "exists" function to the standard template functions.
	// This lets us check to see if the "running" entry exists for each of the containers
	// we care about. Exists will never return an error and it's safe to check a chain of
	// things, any one of which may not exist. In the below template, all of info,
	// containername, and running might be nil, so the normal index function isn't very
	// helpful.
	// This template is unit-tested in kubectl, so if you change it, update the unit test.
	// You can read about the syntax here: http://golang.org/pkg/text/template/.
	getContainerStateTemplate := fmt.Sprintf(`--template={{if (exists . "status" "containerStatuses")}}{{range .status.containerStatuses}}{{if (and (eq .name "%s") (exists . "state" "running"))}}true{{end}}{{end}}{{end}}`, containername)

	getImageTemplate := fmt.Sprintf(`--template={{if (exists . "status" "containerStatuses")}}{{range .status.containerStatuses}}{{if eq .name "%s"}}{{.image}}{{end}}{{end}}{{end}}`, containername)

	By(fmt.Sprintf("waiting for all containers in %s pods to come up.", testname)) //testname should be selector
	for start := time.Now(); time.Since(start) < podStartTimeout; time.Sleep(5 * time.Second) {
		getPodsOutput := runKubectl("get", "pods", "-o", "template", getPodsTemplate, "--api-version=v1", "-l", testname, fmt.Sprintf("--namespace=%v", ns))
		pods := strings.Fields(getPodsOutput)
		if numPods := len(pods); numPods != replicas {
			By(fmt.Sprintf("Replicas for %s: expected=%d actual=%d", testname, replicas, numPods))
			continue
		}
		var runningPods []string
		for _, podID := range pods {
			running := runKubectl("get", "pods", podID, "-o", "template", getContainerStateTemplate, "--api-version=v1", fmt.Sprintf("--namespace=%v", ns))
			if running != "true" {
				Logf("%s is created but not running", podID)
				continue
			}

			currentImage := runKubectl("get", "pods", podID, "-o", "template", getImageTemplate, "--api-version=v1", fmt.Sprintf("--namespace=%v", ns))
			if currentImage != containerImage {
				Logf("%s is created but running wrong image; expected: %s, actual: %s", podID, containerImage, currentImage)
				continue
			}

			// Call the generic validator function here.
			// This might validate for example, that (1) getting a url works and (2) url is serving correct content.
			if err := validator(c, podID); err != nil {
				Logf("%s is running right image but validator function failed: %v", podID, err)
				continue
			}

			Logf("%s is verified up and running", podID)
			runningPods = append(runningPods, podID)
		}
		// If we reach here, then all our checks passed.
		if len(runningPods) == replicas {
			return
		}
	}
	// Reaching here means that one of more checks failed multiple times.  Assuming its not a race condition, something is broken.
	Failf("Timed out after %v seconds waiting for %s pods to reach valid state", podStartTimeout.Seconds(), testname)
}

// kubectlCmd runs the kubectl executable through the wrapper script.
func kubectlCmd(args ...string) *exec.Cmd {
	defaultArgs := []string{}

	// Reference a --server option so tests can run anywhere.
	if testContext.Host != "" {
		defaultArgs = append(defaultArgs, "--"+clientcmd.FlagAPIServer+"="+testContext.Host)
	}
	if testContext.KubeConfig != "" {
		defaultArgs = append(defaultArgs, "--"+clientcmd.RecommendedConfigPathFlag+"="+testContext.KubeConfig)

		// Reference the KubeContext
		if testContext.KubeContext != "" {
			defaultArgs = append(defaultArgs, "--"+clientcmd.FlagContext+"="+testContext.KubeContext)
		}

	} else {
		if testContext.CertDir != "" {
			defaultArgs = append(defaultArgs,
				fmt.Sprintf("--certificate-authority=%s", filepath.Join(testContext.CertDir, "ca.crt")),
				fmt.Sprintf("--client-certificate=%s", filepath.Join(testContext.CertDir, "kubecfg.crt")),
				fmt.Sprintf("--client-key=%s", filepath.Join(testContext.CertDir, "kubecfg.key")))
		}
	}
	kubectlArgs := append(defaultArgs, args...)

	//We allow users to specify path to kubectl, so you can test either "kubectl" or "cluster/kubectl.sh"
	//and so on.
	cmd := exec.Command(testContext.KubectlPath, kubectlArgs...)

	//caller will invoke this and wait on it.
	return cmd
}

func runKubectl(args ...string) string {
	var stdout, stderr bytes.Buffer
	cmd := kubectlCmd(args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	Logf("Running '%s %s'", cmd.Path, strings.Join(cmd.Args, " "))
	if err := cmd.Run(); err != nil {
		Failf("Error running %v:\nCommand stdout:\n%v\nstderr:\n%v\n", cmd, cmd.Stdout, cmd.Stderr)
		return ""
	}
	Logf(stdout.String())
	// TODO: trimspace should be unnecessary after switching to use kubectl binary directly
	return strings.TrimSpace(stdout.String())
}

// testContainerOutput runs testContainerOutputInNamespace with the default namespace.
func testContainerOutput(scenarioName string, c *client.Client, pod *api.Pod, containerIndex int, expectedOutput []string) {
	testContainerOutputInNamespace(scenarioName, c, pod, containerIndex, expectedOutput, api.NamespaceDefault)
}

// testContainerOutputInNamespace runs the given pod in the given namespace and waits
// for all of the containers in the podSpec to move into the 'Success' status.  It retrieves
// the exact container log and searches for lines of expected output.
func testContainerOutputInNamespace(scenarioName string, c *client.Client, pod *api.Pod, containerIndex int, expectedOutput []string, ns string) {
	By(fmt.Sprintf("Creating a pod to test %v", scenarioName))

	defer c.Pods(ns).Delete(pod.Name, nil)
	if _, err := c.Pods(ns).Create(pod); err != nil {
		Failf("Failed to create pod: %v", err)
	}

	// Wait for client pod to complete.
	var containerName string
	for id, container := range pod.Spec.Containers {
		expectNoError(waitForPodSuccessInNamespace(c, pod.Name, container.Name, ns))
		if id == containerIndex {
			containerName = container.Name
		}
	}
	if containerName == "" {
		Failf("Invalid container index: %d", containerIndex)
	}

	// Grab its logs.  Get host first.
	podStatus, err := c.Pods(ns).Get(pod.Name)
	if err != nil {
		Failf("Failed to get pod status: %v", err)
	}

	By(fmt.Sprintf("Trying to get logs from node %s pod %s container %s: %v",
		podStatus.Spec.NodeName, podStatus.Name, containerName, err))
	var logs []byte
	start := time.Now()

	// Sometimes the actual containers take a second to get started, try to get logs for 60s
	for time.Now().Sub(start) < (60 * time.Second) {
		logs, err = c.Get().
			Prefix("proxy").
			Resource("nodes").
			Name(podStatus.Spec.NodeName).
			Suffix("containerLogs", ns, podStatus.Name, containerName).
			Do().
			Raw()
		fmt.Sprintf("pod logs:%v\n", string(logs))
		By(fmt.Sprintf("pod logs:%v\n", string(logs)))
		if strings.Contains(string(logs), "Internal Error") {
			By(fmt.Sprintf("Failed to get logs from node %q pod %q container %q: %v",
				podStatus.Spec.NodeName, podStatus.Name, containerName, string(logs)))
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}

	for _, m := range expectedOutput {
		Expect(string(logs)).To(ContainSubstring(m), "%q in container output", m)
	}
}

// podInfo contains pod information useful for debugging e2e tests.
type podInfo struct {
	oldHostname string
	oldPhase    string
	hostname    string
	phase       string
}

// PodDiff is a map of pod name to podInfos
type PodDiff map[string]*podInfo

// Print formats and prints the give PodDiff.
func (p PodDiff) Print(ignorePhases util.StringSet) {
	for name, info := range p {
		if ignorePhases.Has(info.phase) {
			continue
		}
		if info.phase == nonExist {
			Logf("Pod %v was deleted, had phase %v and host %v", name, info.oldPhase, info.oldHostname)
			continue
		}
		phaseChange, hostChange := false, false
		msg := fmt.Sprintf("Pod %v ", name)
		if info.oldPhase != info.phase {
			phaseChange = true
			if info.oldPhase == nonExist {
				msg += fmt.Sprintf("in phase %v ", info.phase)
			} else {
				msg += fmt.Sprintf("went from phase: %v -> %v ", info.oldPhase, info.phase)
			}
		}
		if info.oldHostname != info.hostname {
			hostChange = true
			if info.oldHostname == nonExist || info.oldHostname == "" {
				msg += fmt.Sprintf("assigned host %v ", info.hostname)
			} else {
				msg += fmt.Sprintf("went from host: %v -> %v ", info.oldHostname, info.hostname)
			}
		}
		if phaseChange || hostChange {
			Logf(msg)
		}
	}
}

// Diff computes a PodDiff given 2 lists of pods.
func Diff(oldPods []*api.Pod, curPods []*api.Pod) PodDiff {
	podInfoMap := PodDiff{}

	// New pods will show up in the curPods list but not in oldPods. They have oldhostname/phase == nonexist.
	for _, pod := range curPods {
		podInfoMap[pod.Name] = &podInfo{hostname: pod.Spec.NodeName, phase: string(pod.Status.Phase), oldHostname: nonExist, oldPhase: nonExist}
	}

	// Deleted pods will show up in the oldPods list but not in curPods. They have a hostname/phase == nonexist.
	for _, pod := range oldPods {
		if info, ok := podInfoMap[pod.Name]; ok {
			info.oldHostname, info.oldPhase = pod.Spec.NodeName, string(pod.Status.Phase)
		} else {
			podInfoMap[pod.Name] = &podInfo{hostname: nonExist, phase: nonExist, oldHostname: pod.Spec.NodeName, oldPhase: string(pod.Status.Phase)}
		}
	}
	return podInfoMap
}

// RunRC Launches (and verifies correctness) of a Replication Controller
// and will wait for all pods it spawns to become "Running".
// It's the caller's responsibility to clean up externally (i.e. use the
// namespace lifecycle for handling cleanup).
func RunRC(config RCConfig) error {
	maxContainerFailures := int(math.Max(1.0, float64(config.Replicas)*.01))
	label := labels.SelectorFromSet(labels.Set(map[string]string{"name": config.Name}))

	By(fmt.Sprintf("%v Creating replication controller %s", time.Now(), config.Name))
	rc := &api.ReplicationController{
		ObjectMeta: api.ObjectMeta{
			Name: config.Name,
		},
		Spec: api.ReplicationControllerSpec{
			Replicas: config.Replicas,
			Selector: map[string]string{
				"name": config.Name,
			},
			Template: &api.PodTemplateSpec{
				ObjectMeta: api.ObjectMeta{
					Labels: map[string]string{"name": config.Name},
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  config.Name,
							Image: config.Image,
							Ports: []api.ContainerPort{{ContainerPort: 80}},
						},
					},
				},
			},
		},
	}
	if config.Env != nil {
		for k, v := range config.Env {
			c := &rc.Spec.Template.Spec.Containers[0]
			c.Env = append(c.Env, api.EnvVar{Name: k, Value: v})
		}
	}
	if config.Labels != nil {
		for k, v := range config.Labels {
			rc.Spec.Template.ObjectMeta.Labels[k] = v
		}
	}
	if config.Ports != nil {
		for k, v := range config.Ports {
			c := &rc.Spec.Template.Spec.Containers[0]
			c.Ports = append(c.Ports, api.ContainerPort{Name: k, ContainerPort: v})
		}
	}
	_, err := config.Client.ReplicationControllers(config.Namespace).Create(rc)
	if err != nil {
		return fmt.Errorf("Error creating replication controller: %v", err)
	}
	Logf("%v Created replication controller with name: %v, namespace: %v, replica count: %v", time.Now(), rc.Name, config.Namespace, rc.Spec.Replicas)
	podStore := newPodStore(config.Client, config.Namespace, label, fields.Everything())
	defer podStore.Stop()

	interval := config.PollInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	oldPods := make([]*api.Pod, 0)
	oldRunning := 0
	lastChange := time.Now()
	for oldRunning != config.Replicas && time.Since(lastChange) < timeout {
		time.Sleep(interval)

		running := 0
		waiting := 0
		pending := 0
		unknown := 0
		inactive := 0
		failedContainers := 0
		pods := podStore.List()
		if config.CreatedPods != nil {
			*config.CreatedPods = pods
		}
		for _, p := range pods {
			if p.Status.Phase == api.PodRunning {
				running++
				for _, v := range FailedContainers(p) {
					failedContainers = failedContainers + v.restarts
				}
			} else if p.Status.Phase == api.PodPending {
				if p.Spec.NodeName == "" {
					waiting++
				} else {
					pending++
				}
			} else if p.Status.Phase == api.PodSucceeded || p.Status.Phase == api.PodFailed {
				inactive++
			} else if p.Status.Phase == api.PodUnknown {
				unknown++
			}
		}

		Logf("%v %v Pods: %d out of %d created, %d running, %d pending, %d waiting, %d inactive, %d unknown ",
			time.Now(), rc.Name, len(pods), config.Replicas, running, pending, waiting, inactive, unknown)
		if config.PodStatusFile != nil {
			fmt.Fprintf(config.PodStatusFile, "%s, %d, running, %d, pending, %d, waiting, %d, inactive, %d, unknown\n", time.Now(), running, pending, waiting, inactive, unknown)
		}

		if failedContainers > maxContainerFailures {
			return fmt.Errorf("%d containers failed which is more than allowed %d", failedContainers, maxContainerFailures)
		}
		if len(pods) < len(oldPods) || len(pods) > config.Replicas {
			// This failure mode includes:
			// kubelet is dead, so node controller deleted pods and rc creates more
			//	- diagnose by noting the pod diff below.
			// pod is unhealthy, so replication controller creates another to take its place
			//	- diagnose by comparing the previous "2 Pod states" lines for inactive pods
			errorStr := fmt.Sprintf("Number of reported pods changed: %d vs %d", len(pods), len(oldPods))
			Logf("%v, pods that changed since the last iteration:", errorStr)
			Diff(oldPods, pods).Print(util.NewStringSet())
			return fmt.Errorf(errorStr)
		}

		if len(pods) > len(oldPods) || running > oldRunning {
			lastChange = time.Now()
		}
		oldPods = pods
		oldRunning = running
	}

	if oldRunning != config.Replicas {
		return fmt.Errorf("Only %d pods started out of %d", oldRunning, config.Replicas)
	}
	return nil
}

func ScaleRC(c *client.Client, ns, name string, size uint) error {
	By(fmt.Sprintf("%v Scaling replication controller %s in namespace %s to %d", time.Now(), name, ns, size))
	scaler, err := kubectl.ScalerFor("ReplicationController", kubectl.NewScalerClient(c))
	if err != nil {
		return err
	}
	waitForReplicas := kubectl.NewRetryParams(5*time.Second, 5*time.Minute)
	if err = scaler.Scale(ns, name, size, nil, nil, waitForReplicas); err != nil {
		return err
	}
	return waitForRCPodsRunning(c, ns, name)
}

// Wait up to 10 minutes for pods to become Running.
func waitForRCPodsRunning(c *client.Client, ns, rcName string) error {
	running := false
	label := labels.SelectorFromSet(labels.Set(map[string]string{"name": rcName}))
	podStore := newPodStore(c, ns, label, fields.Everything())
	defer podStore.Stop()
	for start := time.Now(); time.Since(start) < 10*time.Minute; time.Sleep(5 * time.Second) {
		pods := podStore.List()
		for _, p := range pods {
			if p.Status.Phase != api.PodRunning {
				continue
			}
		}
		running = true
		break
	}
	if !running {
		return fmt.Errorf("Timeout while waiting for replication controller %s pods to be running", rcName)
	}
	return nil
}

// Delete a Replication Controller and all pods it spawned
func DeleteRC(c *client.Client, ns, name string) error {
	By(fmt.Sprintf("%v Deleting replication controller %s in namespace %s", time.Now(), name, ns))
	reaper, err := kubectl.ReaperForReplicationController(c, 10*time.Minute)
	if err != nil {
		return err
	}
	startTime := time.Now()
	_, err = reaper.Stop(ns, name, 0, api.NewDeleteOptions(0))
	deleteRCTime := time.Now().Sub(startTime)
	Logf("Deleting RC took: %v", deleteRCTime)
	return err
}

// Convenient wrapper around listing nodes supporting retries.
func listNodes(c *client.Client, label labels.Selector, field fields.Selector) (*api.NodeList, error) {
	var nodes *api.NodeList
	var errLast error
	if wait.Poll(poll, singleCallTimeout, func() (bool, error) {
		nodes, errLast = c.Nodes().List(label, field)
		return errLast == nil, nil
	}) != nil {
		return nil, fmt.Errorf("listNodes() failed with last error: %v", errLast)
	}
	return nodes, nil
}

// FailedContainers inspects all containers in a pod and returns failure
// information for containers that have failed or been restarted.
// A map is returned where the key is the containerID and the value is a
// struct containing the restart and failure information
func FailedContainers(pod *api.Pod) map[string]ContainerFailures {
	var state ContainerFailures
	states := make(map[string]ContainerFailures)

	statuses := pod.Status.ContainerStatuses
	if len(statuses) == 0 {
		return nil
	} else {
		for _, status := range statuses {
			if status.State.Terminated != nil {
				states[status.ContainerID] = ContainerFailures{status: status.State.Terminated}
			} else if status.LastTerminationState.Terminated != nil {
				states[status.ContainerID] = ContainerFailures{status: status.LastTerminationState.Terminated}
			}
			if status.RestartCount > 0 {
				var ok bool
				if state, ok = states[status.ContainerID]; !ok {
					state = ContainerFailures{}
				}
				state.restarts = status.RestartCount
				states[status.ContainerID] = state
			}
		}
	}

	return states
}

// Prints the histogram of the events and returns the number of bad events.
func BadEvents(events []*api.Event) int {
	type histogramKey struct {
		reason string
		source string
	}
	histogram := make(map[histogramKey]int)
	for _, e := range events {
		histogram[histogramKey{reason: e.Reason, source: e.Source.Component}]++
	}
	for key, number := range histogram {
		Logf("- reason: %s, source: %s -> %d", key.reason, key.source, number)
	}
	badPatterns := []string{"kill", "fail"}
	badEvents := 0
	for key, number := range histogram {
		for _, s := range badPatterns {
			if strings.Contains(key.reason, s) {
				Logf("WARNING %d events from %s with reason: %s", number, key.source, key.reason)
				badEvents += number
				break
			}
		}
	}
	return badEvents
}

// NodeSSHHosts returns SSH-able host names for all nodes. It returns an error
// if it can't find an external IP for every node, though it still returns all
// hosts that it found in that case.
func NodeSSHHosts(c *client.Client) ([]string, error) {
	var hosts []string
	nodelist, err := c.Nodes().List(labels.Everything(), fields.Everything())
	if err != nil {
		return hosts, fmt.Errorf("error getting nodes: %v", err)
	}
	for _, n := range nodelist.Items {
		for _, addr := range n.Status.Addresses {
			// Use the first external IP address we find on the node, and
			// use at most one per node.
			// TODO(mbforbes): Use the "preferred" address for the node, once
			// such a thing is defined (#2462).
			if addr.Type == api.NodeExternalIP {
				hosts = append(hosts, addr.Address+":22")
				break
			}
		}
	}

	// Error if any node didn't have an external IP.
	if len(hosts) != len(nodelist.Items) {
		return hosts, fmt.Errorf(
			"only found %d external IPs on nodes, but found %d nodes. Nodelist: %v",
			len(hosts), len(nodelist.Items), nodelist)
	}
	return hosts, nil
}

// SSH synchronously SSHs to a node running on provider and runs cmd. If there
// is no error performing the SSH, the stdout, stderr, and exit code are
// returned.
func SSH(cmd, host, provider string) (string, string, int, error) {
	// Get a signer for the provider.
	signer, err := getSigner(provider)
	if err != nil {
		return "", "", 0, fmt.Errorf("error getting signer for provider %s: '%v'", provider, err)
	}

	user := os.Getenv("KUBE_SSH_USER")
	// RunSSHCommand will default to Getenv("USER") if user == ""
	return util.RunSSHCommand(cmd, user, host, signer)
}

// getSigner returns an ssh.Signer for the provider ("gce", etc.) that can be
// used to SSH to their nodes.
func getSigner(provider string) (ssh.Signer, error) {
	// Get the directory in which SSH keys are located.
	keydir := filepath.Join(os.Getenv("HOME"), ".ssh")

	// Select the key itself to use. When implementing more providers here,
	// please also add them to any SSH tests that are disabled because of signer
	// support.
	keyfile := ""
	switch provider {
	case "gce", "gke":
		keyfile = "google_compute_engine"
	case "aws":
		keyfile = "kube_aws_rsa"
	default:
		return nil, fmt.Errorf("getSigner(...) not implemented for %s", provider)
	}
	key := filepath.Join(keydir, keyfile)
	Logf("Using SSH key: %s", key)

	return util.MakePrivateKeySignerFromFile(key)
}

// checkPodsRunning returns whether all pods whose names are listed in podNames
// in namespace ns are running and ready, using c and waiting at most timeout.
func checkPodsRunningReady(c *client.Client, ns string, podNames []string, timeout time.Duration) bool {
	np, desc := len(podNames), "running and ready"
	Logf("Waiting up to %v for the following %d pods to be %s: %s", timeout, np, desc, podNames)
	result := make(chan bool, len(podNames))
	for ix := range podNames {
		// Launch off pod readiness checkers.
		go func(name string) {
			err := waitForPodCondition(c, ns, name, desc, timeout, podRunningReady)
			result <- err == nil
		}(podNames[ix])
	}
	// Wait for them all to finish.
	success := true
	// TODO(mbforbes): Change to `for range` syntax and remove logging once we
	// support only Go >= 1.4.
	for _, podName := range podNames {
		if !<-result {
			Logf("Pod %-[1]*[2]s failed to be %[3]s.", podPrintWidth, podName, desc)
			success = false
		}
	}
	Logf("Wanted all %d pods to be %s. Result: %t. Pods: %v", np, desc, success, podNames)
	return success
}

// waitForNodeToBeReady returns whether node name is ready within timeout.
func waitForNodeToBeReady(c *client.Client, name string, timeout time.Duration) bool {
	return waitForNodeToBe(c, name, true, timeout)
}

// waitForNodeToBeNotReady returns whether node name is not ready (i.e. the
// readiness condition is anything but ready, e.g false or unknown) within
// timeout.
func waitForNodeToBeNotReady(c *client.Client, name string, timeout time.Duration) bool {
	return waitForNodeToBe(c, name, false, timeout)
}

func isNodeReadySetAsExpected(node *api.Node, wantReady bool) bool {
	// Check the node readiness condition (logging all).
	for i, cond := range node.Status.Conditions {
		Logf("Node %s condition %d/%d: type: %v, status: %v",
			node.Name, i+1, len(node.Status.Conditions), cond.Type, cond.Status)
		// Ensure that the condition type is readiness and the status
		// matches as desired.
		if cond.Type == api.NodeReady && (cond.Status == api.ConditionTrue) == wantReady {
			Logf("Successfully found node %s readiness to be %t", node.Name, wantReady)
			return true
		}
	}
	return false
}

// waitForNodeToBe returns whether node name's readiness state matches wantReady
// within timeout. If wantReady is true, it will ensure the node is ready; if
// it's false, it ensures the node is in any state other than ready (e.g. not
// ready or unknown).
func waitForNodeToBe(c *client.Client, name string, wantReady bool, timeout time.Duration) bool {
	Logf("Waiting up to %v for node %s readiness to be %t", timeout, name, wantReady)
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(poll) {
		node, err := c.Nodes().Get(name)
		if err != nil {
			Logf("Couldn't get node %s", name)
			continue
		}

		if isNodeReadySetAsExpected(node, wantReady) {
			return true
		}
	}
	Logf("Node %s didn't reach desired readiness (%t) within %v", name, wantReady, timeout)
	return false
}

// Filters nodes in NodeList in place, removing nodes that do not
// satisfy the given condition
// TODO: consider merging with pkg/client/cache.NodeLister
func filterNodes(nodeList *api.NodeList, fn func(node api.Node) bool) {
	var l []api.Node

	for _, node := range nodeList.Items {
		if fn(node) {
			l = append(l, node)
		}
	}
	nodeList.Items = l
}

// LatencyMetrics stores data about request latency at a given quantile
// broken down by verb (e.g. GET, PUT, LIST) and resource (e.g. pods, services).
type LatencyMetric struct {
	Verb     string
	Resource string
	// 0 <= quantile <=1, e.g. 0.95 is 95%tile, 0.5 is median.
	Quantile float64
	Latency  time.Duration
}

// LatencyMetricByLatency implements sort.Interface for []LatencyMetric based on
// the latency field.
type LatencyMetricByLatency []LatencyMetric

func (a LatencyMetricByLatency) Len() int           { return len(a) }
func (a LatencyMetricByLatency) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a LatencyMetricByLatency) Less(i, j int) bool { return a[i].Latency < a[j].Latency }

func ReadLatencyMetrics(c *client.Client) ([]LatencyMetric, error) {
	body, err := getMetrics(c)
	if err != nil {
		return nil, err
	}
	metrics := make([]LatencyMetric, 0)
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "apiserver_request_latencies_summary{") {
			// Example line:
			// apiserver_request_latencies_summary{resource="namespaces",verb="LIST",quantile="0.99"} 908
			// TODO: This parsing code is long and not readable. We should improve it.
			keyVal := strings.Split(line, " ")
			if len(keyVal) != 2 {
				return nil, fmt.Errorf("Error parsing metric %q", line)
			}
			keyElems := strings.Split(line, "\"")
			if len(keyElems) != 7 {
				return nil, fmt.Errorf("Error parsing metric %q", line)
			}
			resource := keyElems[1]
			verb := keyElems[3]
			quantile, err := strconv.ParseFloat(keyElems[5], 64)
			if err != nil {
				return nil, fmt.Errorf("Error parsing metric %q", line)
			}
			latency, err := strconv.ParseFloat(keyVal[1], 64)
			if err != nil {
				return nil, fmt.Errorf("Error parsing metric %q", line)
			}
			metrics = append(metrics, LatencyMetric{verb, resource, quantile, time.Duration(int64(latency)) * time.Microsecond})
		}
	}
	return metrics, nil
}

// Prints summary metrics for request types with latency above threshold
// and returns number of such request types.
func HighLatencyRequests(c *client.Client, threshold time.Duration, ignoredResources util.StringSet) (int, error) {
	ignoredVerbs := util.NewStringSet("WATCHLIST", "PROXY")

	metrics, err := ReadLatencyMetrics(c)
	if err != nil {
		return 0, err
	}
	sort.Sort(sort.Reverse(LatencyMetricByLatency(metrics)))
	var badMetrics []LatencyMetric
	top := 5
	for _, metric := range metrics {
		if ignoredResources.Has(metric.Resource) || ignoredVerbs.Has(metric.Verb) {
			continue
		}
		isBad := false
		if metric.Latency > threshold &&
			// We are only interested in 99%tile, but for logging purposes
			// it's useful to have all the offending percentiles.
			metric.Quantile <= 0.99 {
			badMetrics = append(badMetrics, metric)
			isBad = true
		}
		if top > 0 || isBad {
			top--
			prefix := ""
			if isBad {
				prefix = "WARNING "
			}
			Logf("%vTop latency metric: %+v", prefix, metric)
		}
	}

	return len(badMetrics), nil
}

// Reset latency metrics in apiserver.
func resetMetrics(c *client.Client) error {
	Logf("Resetting latency metrics in apiserver...")
	body, err := c.Get().AbsPath("/resetMetrics").DoRaw()
	if err != nil {
		return err
	}
	if string(body) != "metrics reset\n" {
		return fmt.Errorf("Unexpected response: ", string(body))
	}
	return nil
}

// Retrieve metrics information
func getMetrics(c *client.Client) (string, error) {
	body, err := c.Get().AbsPath("/metrics").DoRaw()
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// Retrieve debug information
func getDebugInfo(c *client.Client) (map[string]string, error) {
	data := make(map[string]string)
	for _, key := range []string{"block", "goroutine", "heap", "threadcreate"} {
		resp, err := http.Get(c.Get().AbsPath(fmt.Sprintf("debug/pprof/%s", key)).URL().String() + "?debug=2")
		if err != nil {
			Logf("Warning: Error trying to fetch %s debug data: %v", key, err)
			continue
		}
		body, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			Logf("Warning: Error trying to read %s debug data: %v", key, err)
		}
		data[key] = string(body)
	}
	return data, nil
}

func writePerfData(c *client.Client, dirName string, postfix string) error {
	fname := fmt.Sprintf("%s/metrics_%s.txt", dirName, postfix)

	handler, err := os.Create(fname)
	if err != nil {
		return fmt.Errorf("Error creating file '%s': %v", fname, err)
	}

	metrics, err := getMetrics(c)
	if err != nil {
		return fmt.Errorf("Error retrieving metrics: %v", err)
	}

	_, err = handler.WriteString(metrics)
	if err != nil {
		return fmt.Errorf("Error writing metrics: %v", err)
	}

	err = handler.Close()
	if err != nil {
		return fmt.Errorf("Error closing '%s': %v", fname, err)
	}

	debug, err := getDebugInfo(c)
	if err != nil {
		return fmt.Errorf("Error retrieving debug information: %v", err)
	}

	for key, value := range debug {
		fname := fmt.Sprintf("%s/%s_%s.txt", dirName, key, postfix)
		handler, err = os.Create(fname)
		if err != nil {
			return fmt.Errorf("Error creating file '%s': %v", fname, err)
		}
		_, err = handler.WriteString(value)
		if err != nil {
			return fmt.Errorf("Error writing %s: %v", key, err)
		}

		err = handler.Close()
		if err != nil {
			return fmt.Errorf("Error closing '%s': %v", fname, err)
		}
	}
	return nil
}
