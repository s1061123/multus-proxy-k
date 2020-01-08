/*
Copyright 2017 The Kubernetes Authors.

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

package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"k8s.io/klog"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	netdefv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	netdefutils "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/utils"

	"google.golang.org/grpc"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	k8sutils "k8s.io/kubernetes/pkg/kubelet/util"
	docker "github.com/docker/docker/client"

	/*
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"

	"k8s.io/klog"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	apiservice "k8s.io/kubernetes/pkg/api/v1/service"
	"github.com/s1061123/multus-proxy-k/pkg/proxy/metrics"
	utilproxy "github.com/s1061123/multus-proxy-k/pkg/proxy/util"
	utilnet "k8s.io/utils/net"
	*/
)

// PodInfo contains information that defines a pod.
type PodInfo struct {
	name string
	networkNamespace string
	networkAttachments []*NetworkSelectionElement
	networkStatus []netdefv1.NetworkStatus
}

func (info *PodInfo) GetMultusNetIFs() []string {
	results := []string{}

	for _, status := range info.networkStatus[1:] {
		results = append(results, status.Interface)
	}
	return results
}

func (info *PodInfo) String() string {
	return fmt.Sprintf("pod:%s", info.name)
}

func (info *PodInfo) Name() string {
	return info.name
}

func (info *PodInfo) NetworkNamespace() string {
	return info.networkNamespace
}

func (info *PodInfo) NetworkAttachments() []*NetworkSelectionElement {
	return info.networkAttachments
}

func (info *PodInfo) NetworkStatus() []netdefv1.NetworkStatus {
	return info.networkStatus
}

//XXX: should it use npwg client?
// NetworkSelectionElement represents one element of the JSON format
// Network Attachment Selection Annotation as described in section 4.1.2
// of the CRD specification.
type NetworkSelectionElement struct {
    // Name contains the name of the Network object this element selects
    Name string `json:"name"`
    // Namespace contains the optional namespace that the network referenced
    // by Name exists in
    Namespace string `json:"namespace,omitempty"`
    // InterfaceRequest contains an optional requested name for the
    // network interface this attachment will create in the container
    InterfaceRequest string `json:"interface,omitempty"`
}

type podChange struct {
	previous PodMap
	current PodMap
}

// PodChangeTracker carries state about uncommitted changes to an arbitrary number of
// Pods in the node, keyed by their namespace and name
type PodChangeTracker struct {
	// lock protects items.
	lock sync.Mutex
	// items maps a service to its serviceChange.
	items map[types.NamespacedName]*podChange
	recorder   record.EventRecorder

	crioClient pb.RuntimeServiceClient
	crioConn *grpc.ClientConn
}

func (pct *PodChangeTracker) String() string {
	return fmt.Sprintf("podChange: %v", pct.items)
}

func (pct *PodChangeTracker) newPodInfo(pod *v1.Pod) (*PodInfo, error) {
	annotationString, ok := pod.ObjectMeta.Annotations["k8s.v1.cni.cncf.io/networks"]
	networks := []*NetworkSelectionElement{}

	// parse networkAnnotation
	if !ok {
		networks = nil
	} else {
		//klog.Info("XXX: Pod:", pod.ObjectMeta.Name, ": Annotations!:", annotationString)
		if strings.IndexAny(annotationString, "[{\"") >= 0 {
			if err := json.Unmarshal([]byte(annotationString), &networks); err != nil {
				return nil, fmt.Errorf("Invalid json at k8s.v1.cni.cncf.io/networks")
			}
		} else {
			for _, item := range strings.Split(annotationString, ",") {
				item = strings.TrimSpace(item)

				netNsName, networkName, netIfName, err := parsePodNetworkObjectName(item)
				if err == nil {
					networks = append(networks, &NetworkSelectionElement{
						Name: networkName,
						Namespace: netNsName,
						InterfaceRequest: netIfName,
					})
				}
			}
		}
		//klog.Info("XXX: networks!:", networks)
	}

	// parse networkStatus
	statuses, _ := netdefutils.GetNetworkStatus(pod)

	// get Container netns
	procPrefix := ""
	netNamespace := ""
	if len(pod.Status.ContainerStatuses) == 0 {
		return nil, fmt.Errorf("XXX: No container status")
	}

	//klog.Infof("XXX: ContainerID %v", pod.Status.ContainerStatuses[0].ContainerID)
	runtimeKind := strings.Split(pod.Status.ContainerStatuses[0].ContainerID, ":")
	if runtimeKind[0] == "docker" {
		containerID := strings.TrimPrefix(pod.Status.ContainerStatuses[0].ContainerID, "docker://")
		if len(containerID) > 0 {
			c, err := docker.NewEnvClient()
			if err != nil {
				panic(err)
			}

			//klog.Info("XXX: Send Req!")
			c.NegotiateAPIVersion(context.TODO())
			json, err := c.ContainerInspect(context.TODO(), containerID)
			if err != nil {
				klog.Info("XXX: err1:", err)
				return nil, fmt.Errorf("failed to get container info: %v", err)
			}
			if json.NetworkSettings == nil {
				klog.Info("XXX: err2:", json)
				return nil, fmt.Errorf("failed to get container info: %v", err)
			}
			netNamespace = fmt.Sprintf("%s/proc/%d/ns/net", procPrefix, json.State.Pid)
			//klog.Info("XXX: namespace:", netNamespace)
		}
	} else { // crio
		containerID := strings.TrimPrefix(pod.Status.ContainerStatuses[0].ContainerID, "cri-o://")
		if len(containerID) > 0 {
			request := &pb.ContainerStatusRequest{
				ContainerId: containerID,
				Verbose:     true,
			}
			r, err := pct.crioClient.ContainerStatus(context.TODO(), request)
			if err != nil {
				klog.Info("XXX: ERR1")
				return nil, fmt.Errorf("cannot get containerStatus")
			}

			if pid, ok := r.Info["pid"]; ok {
				klog.Infof("XXX: PID: %v", pid)
				netNamespace = fmt.Sprintf("%s/proc/%s/ns/net", procPrefix, pid)
			}
		}
	}

	//Docker/cri-o

	info := &PodInfo{
		name: pod.ObjectMeta.Name,
		networkAttachments: networks,
		networkStatus: statuses,
		networkNamespace: netNamespace,
	}
	return info, nil
}

func NewPodChangeTracker(recorder record.EventRecorder) *PodChangeTracker {
	crioClient, crioConn, err := GetCrioRuntimeClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get crio client!")
		return nil
	}

	return &PodChangeTracker{
		items:           make(map[types.NamespacedName]*podChange),
		recorder: recorder,
		crioClient: crioClient,
		crioConn: crioConn,
	}
}

func (pct *PodChangeTracker) podToPodMap(pod *v1.Pod) PodMap {
	if pod == nil {
		return nil
	}

	podMap := make(PodMap)
	podinfo, err := pct.newPodInfo(pod)
	if err != nil {
		return nil
	}

	podMap[types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}] = *podinfo
	return podMap
}

func (pct *PodChangeTracker) Update(previous, current *v1.Pod) bool {
	pod := current

	if pct == nil {
		return false
	}

	if pod == nil {
		pod = previous
	}
	if pod == nil {
		return false
	}
	namespacedName := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}

	//klog.Infof("XXX: Update: %v -> %v", previous, current)
	pct.lock.Lock()
	defer pct.lock.Unlock()

	change, exists := pct.items[namespacedName]
	if !exists {
		change = &podChange{}
		//klog.Infof("XXX:prev:")
		prevPodMap := pct.podToPodMap(previous)
		// XXX: nilcheck
		change.previous = prevPodMap
		pct.items[namespacedName] = change
	}
	//klog.Infof("XXX: cur:")
	curPodMap := pct.podToPodMap(current)
	// XXX: nilcheck
	change.current = curPodMap
	if reflect.DeepEqual(change.previous, change.current) {
		delete(pct.items, namespacedName)
	}
	//klog.Infof("XXX: result:%v", pct.items)
	return len(pct.items) > 0
}

type PodMap map[types.NamespacedName]PodInfo

// Update updates podMap base on the given changes
func (pm *PodMap)Update(changes *PodChangeTracker) {
	if pm != nil {
		pm.apply(changes)
	}
}

func (pm *PodMap) apply(changes *PodChangeTracker) {
	if pm == nil || changes == nil {
		return
	}

	changes.lock.Lock()
	defer changes.lock.Unlock()
	for _, change := range changes.items {
		//klog.Infof("XXX: apply:  %v -> %v", change.previous, change.current)
		pm.unmerge(change.previous)
		pm.merge(change.current)
	}
	// clear changes after applying them to ServiceMap.
	changes.items = make(map[types.NamespacedName]*podChange)
	return
}

func (pm *PodMap)merge(other PodMap) {
	for podName, info := range other {
		(*pm)[podName] = info
	}
}

func (pm *PodMap)unmerge(other PodMap) {
	for podName := range other {
		delete(*pm, podName)
	}
}

//XXX: for debug, to be removed
func (pm *PodMap)String() string {
	if pm == nil {
		return ""
	}
	str := ""
	for _, v := range *pm {
		str = fmt.Sprintf("%s\n\tpod: %s", str, v.Name())
	}
	return str
}

// =====================================
// misc functions...
// =====================================

func parsePodNetworkObjectName(podnetwork string) (string, string, string, error) {
    var netNsName string
    var netIfName string
    var networkName string

    slashItems := strings.Split(podnetwork, "/")
    if len(slashItems) == 2 {
        netNsName = strings.TrimSpace(slashItems[0])
        networkName = slashItems[1]
    } else if len(slashItems) == 1 {
        networkName = slashItems[0]
    } else {
        return "", "", "", fmt.Errorf("parsePodNetworkObjectName: Invalid network object (failed at '/')")
    }

    atItems := strings.Split(networkName, "@")
    networkName = strings.TrimSpace(atItems[0])
    if len(atItems) == 2 {
        netIfName = strings.TrimSpace(atItems[1])
    } else if len(atItems) != 1 {
        return "", "", "", fmt.Errorf("parsePodNetworkObjectName: Invalid network object (failed at '@')")
    }

    // Check and see if each item matches the specification for valid attachment name.
    // "Valid attachment names must be comprised of units of the DNS-1123 label format"
    // [a-z0-9]([-a-z0-9]*[a-z0-9])?
    // And we allow at (@), and forward slash (/) (units separated by commas)
    // It must start and end alphanumerically.
    allItems := []string{netNsName, networkName, netIfName}
    for i := range allItems {
        matched, _ := regexp.MatchString("^[a-z0-9]([-a-z0-9]*[a-z0-9])?$", allItems[i])
        if !matched && len([]rune(allItems[i])) > 0 {
            return "", "", "", fmt.Errorf(fmt.Sprintf("parsePodNetworkObjectName: Failed to parse: one or more items did not match comma-delimited format (must consist of lower case alphanumeric characters). Must start and end with an alphanumeric character), mismatch @ '%v'", allItems[i]))
        }
    }

    return netNsName, networkName, netIfName, nil
}

func getRuntimeClientConnection() (*grpc.ClientConn, error) {
    //return nil, fmt.Errorf("--runtime-endpoint is not set")
    //Docker/cri-o
    RuntimeEndpoint := "unix:///var/run/crio/crio.sock"
    Timeout := 10 * time.Second

    addr, dialer, err := k8sutils.GetAddressAndDialer(RuntimeEndpoint)
    if err != nil {
        return nil, err
    }

    conn, err := grpc.Dial(addr, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(Timeout), grpc.WithDialer(dialer))
    if err != nil {
	    return nil, fmt.Errorf("failed to connect, make sure you are running as root and the runtime has been started: %v", err)
    }
    return conn, nil
}

// GetCrioRuntimeClient retrieves crio grpc client
func GetCrioRuntimeClient() (pb.RuntimeServiceClient, *grpc.ClientConn, error) {
    // Set up a connection to the server.
    conn, err := getRuntimeClientConnection()
    if err != nil {
        return nil, nil, fmt.Errorf("failed to connect: %v", err)
    }
    runtimeClient := pb.NewRuntimeServiceClient(conn)
    return runtimeClient, conn, nil
}

// CloseCrioConnection closes grpc connection in client
func CloseCrioConnection(conn *grpc.ClientConn) error {
    if conn == nil {
        return nil
    }
    return conn.Close()
}


