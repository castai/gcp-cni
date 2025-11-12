// Copyright 2015 CNI authors
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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
	"github.com/gofrs/flock"
	logging "github.com/k8snetworkplumbingwg/cni-log"
	"github.com/samber/lo"
	"github.com/sanity-io/litter"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/castai/gcp-cni/pkg/apis/ipam/v1alpha1"
	"github.com/castai/gcp-cni/pkg/ipam"
)

type PluginConf struct {
	types.NetConf

	Args          map[string]string      `json:"args"`
	RuntimeConfig map[string]interface{} `json:"runtimeConfig"`
	IPPoolName    string                 `json:"ipPoolName,omitempty"` // Name of the IPPool resource to use
}

func parseConfig(stdin []byte) (*PluginConf, error) {
	conf := PluginConf{}

	if err := json.Unmarshal(stdin, &conf); err != nil {
		return nil, fmt.Errorf("failed to parse network configuration: %w", err)
	}

	if err := version.ParsePrevResult(&conf.NetConf); err != nil {
		return nil, fmt.Errorf("could not parse prevResult: %w", err)
	}

	return &conf, nil
}

func main() {
	logging.SetLogFile("/tmp/gcp-ipam.log")
	logging.SetLogLevel(logging.DebugLevel)
	logging.SetLogStderr(true)

	skel.PluginMainFuncs(skel.CNIFuncs{
		Add:   cmdAdd,
		Check: cmdCheck,
		Del:   cmdDel,
	}, version.All, bv.BuildString("gcp-ipam"))
}

func cmdCheck(args *skel.CmdArgs) error {
	return nil
}

func waitForInstanceOperation(ctx context.Context, service *compute.Service, projectID, zone, opName string) error {
	for {
		op, err := service.ZoneOperations.Get(projectID, zone, opName).Context(ctx).Do()
		if err != nil {
			return err
		}
		if op.Status == "DONE" {
			if op.Error != nil {
				var errs []string
				for _, e := range op.Error.Errors {
					errs = append(errs, e.Message)
				}
				return fmt.Errorf("operation failed: %s", strings.Join(errs, ", "))
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func isNotFound(err error) bool {
	gerr, ok := err.(*googleapi.Error)
	return ok && gerr.Code == 404
}

func cmdAdd(args *skel.CmdArgs) error {
	addTimeStart := time.Now()
	operation := "ADD"
	fileLock := flock.New("/var/run/gcp-ipam.lock")
	fileLock.Lock()
	logging.Debugf("[%s] Acquired file lock time %v", operation, time.Since(addTimeStart))
	defer fileLock.Unlock()

	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	logging.Debugf("[%s] Processing CNI add command: %+v", operation, args.Args)
	logging.Debugf("[%s] Configuration: %+v", operation, conf)

	k8sclient, err := buildKubeClient()
	if err != nil {
		return fmt.Errorf("failed to build k8s client: %w", err)
	}

	cniArgs := lo.SliceToMap(strings.Split(args.Args, ";"), func(s string) (string, string) {
		parts := strings.SplitN(s, "=", 2)
		if len(parts) == 2 {
			return parts[0], parts[1]
		}
		return parts[0], ""
	})

	startTime := time.Now()
	p, err := k8sclient.CoreV1().Pods(cniArgs["K8S_POD_NAMESPACE"]).Get(context.TODO(), cniArgs["K8S_POD_NAME"], metav1.GetOptions{})
	logging.Infof("[%s][K8s Operation] Get pod %s/%s took %v", operation, cniArgs["K8S_POD_NAMESPACE"], cniArgs["K8S_POD_NAME"], time.Since(startTime))
	if err != nil {
		return fmt.Errorf("failed to get pod %s/%s: %w", cniArgs["K8S_POD_NAMESPACE"], cniArgs["K8S_POD_NAME"], err)
	}

	const LiveIPAnnotation = "live.cast.ai/ip"
	reqIP, isMigrationFlow := p.Annotations[LiveIPAnnotation]
	origInst, hasOriginalInstance := p.Annotations["live.cast.ai/original-instance"]

	ctx := context.Background()
	client, err := google.DefaultClient(ctx, compute.CloudPlatformScope)
	if err != nil {
		return fmt.Errorf("failed to create google default client: %w", err)
	}

	computeService, projectID, zone, region, instanceName, err := getInstanceInfo(client)
	if err != nil {
		return err
	}

	startTime = time.Now()
	instance, err := computeService.Instances.Get(projectID, zone, instanceName).Context(ctx).Do()
	logging.Infof("[%s][Cloud Operation] Get instance %s took %v", operation, instanceName, time.Since(startTime))
	if err != nil {
		return fmt.Errorf("failed to get instance details: %w", err)
	}

	logging.Debugf("[%s] Instance details: %+v", operation, instance)

	subnetwork := instance.NetworkInterfaces[0].Subnetwork
	subnetworkParts := strings.Split(subnetwork, "/")
	subnetwork = subnetworkParts[len(subnetworkParts)-1]

	startTime = time.Now()
	subnet, err := computeService.Subnetworks.Get(projectID, region, subnetwork).Context(ctx).Do()
	logging.Infof("[%s][Cloud Operation] Get subnetwork %s took %v", operation, subnetwork, time.Since(startTime))
	if err != nil {
		return fmt.Errorf("failed to get subnetwork details: %w", err)
	}

	// Build dynamic client for IPPool access
	dynamicClient, err := buildDynamicClient()
	if err != nil {
		return fmt.Errorf("failed to build dynamic client: %w", err)
	}

	// Create IP allocator
	allocator := ipam.NewAllocator(dynamicClient)

	// Determine IPPool name - default to subnet-based naming if not configured
	poolName := conf.IPPoolName
	if poolName == "" {
		// Default to a pool name based on the subnet
		poolName = fmt.Sprintf("ippool-%s", subnetwork)
	}

	var newAddress string
	var allocationResult *ipam.AllocationResult

	// Only allocate IP when this is not a migration flow
	// For migration, the IP is already allocated in the pool
	if !isMigrationFlow {
		// Allocate IP from the pool
		allocationReq := &ipam.AllocationRequest{
			PoolName:     poolName,
			PodName:      cniArgs["K8S_POD_NAME"],
			PodNamespace: cniArgs["K8S_POD_NAMESPACE"],
			PodUID:       string(p.UID),
			NodeName:     instanceName,
		}

		startTime = time.Now()
		allocationResult, err = allocator.Allocate(ctx, allocationReq)
		logging.Infof("[%s][K8s Operation] Allocate IP from pool %s took %v", operation, poolName, time.Since(startTime))
		if err != nil {
			return fmt.Errorf("failed to allocate IP from pool %s: %w", poolName, err)
		}

		newAddress = allocationResult.IP
		logging.Infof("[%s] Allocated IP %s from pool %s", operation, newAddress, poolName)
	} else {
		// Migration flow - use the requested IP directly
		newAddress = reqIP
		logging.Infof("[%s] Migration flow - using existing IP %s", operation, newAddress)

		// Get allocation result for the migrated IP to retrieve secondary range info
		startTime = time.Now()
		allocationResult, err = allocator.GetAllocation(ctx, poolName, reqIP)
		logging.Infof("[%s][K8s Operation] Get allocation for IP %s from pool %s took %v", operation, reqIP, poolName, time.Since(startTime))
		if err != nil {
			return fmt.Errorf("failed to get allocation for IP %s from pool %s: %w", reqIP, poolName, err)
		}
	}

	if hasOriginalInstance {
		logging.Infof("[%s] Migrating IP %s from original instance %s", operation, reqIP, origInst)
		startTime = time.Now()
		origInstance, err := computeService.Instances.Get(projectID, zone, origInst).Context(ctx).Do()
		logging.Infof("[%s][Cloud Operation] Get original instance %s took %v", operation, origInst, time.Since(startTime))
		if err != nil {
			return fmt.Errorf("failed to get original instance: %w", err)
		}

		removed := lo.Filter(origInstance.NetworkInterfaces[0].AliasIpRanges, func(a *compute.AliasIpRange, _ int) bool {
			return a.IpCidrRange != fmt.Sprintf("%s/32", reqIP)
		})

		startTime = time.Now()
		c, err := computeService.Instances.UpdateNetworkInterface(projectID, zone, origInst, origInstance.NetworkInterfaces[0].Name, &compute.NetworkInterface{
			Fingerprint:   origInstance.NetworkInterfaces[0].Fingerprint,
			AliasIpRanges: removed,
		}).Do()
		logging.Infof("[%s][Cloud Operation] Update network interface on original instance %s took %v", operation, origInst, time.Since(startTime))
		if err != nil {
			return fmt.Errorf("failed to update network interface: %w", err)
		}

		startTime = time.Now()
		if err := waitForInstanceOperation(ctx, computeService, projectID, zone, c.Name); err != nil {
			return fmt.Errorf("failed to wait for network interface update operation: %w", err)
		}
		logging.Infof("[%s][Cloud Operation] Wait for network interface update operation on original instance took %v", operation, time.Since(startTime))
	}

	// Use secondary range name from allocation result, default to "live" if empty
	secondaryRangeName := allocationResult.SecondaryRangeName
	if secondaryRangeName == "" {
		secondaryRangeName = "live"
	}

	startTime = time.Now()
	c, err := computeService.Instances.UpdateNetworkInterface(projectID, zone, instanceName, instance.NetworkInterfaces[0].Name, &compute.NetworkInterface{
		Fingerprint: instance.NetworkInterfaces[0].Fingerprint,
		AliasIpRanges: append(instance.NetworkInterfaces[0].AliasIpRanges, &compute.AliasIpRange{
			IpCidrRange:         fmt.Sprintf("%s/32", newAddress),
			SubnetworkRangeName: secondaryRangeName,
		}),
	}).Do()
	logging.Infof("[%s][Cloud Operation] Update network interface on instance %s took %v", operation, instanceName, time.Since(startTime))
	if err != nil {
		return fmt.Errorf("failed to update network interface: %w", err)
	}

	startTime = time.Now()
	if err := waitForInstanceOperation(ctx, computeService, projectID, zone, c.Name); err != nil {
		return fmt.Errorf("failed to wait for network interface update operation: %w", err)
	}
	logging.Infof("[%s][Cloud Operation] Wait for network interface update operation took %v", operation, time.Since(startTime))

	_, ipNet, err := net.ParseCIDR(subnet.IpCidrRange)
	if err != nil {
		return fmt.Errorf("failed to parse subnetwork CIDR %s: %w", subnet.IpCidrRange, err)
	}
	logging.Infof("Allocation result: %+v", allocationResult)
	ipNet.IP = net.ParseIP(newAddress)
	gw, _, _ := net.ParseCIDR(allocationResult.CIDR)
	gw[15] = gw[15] + 1
	logging.Infof("[%s] Assigned IP %s to pod %s/%s with gateway %+v", operation, newAddress, cniArgs["K8S_POD_NAMESPACE"], cniArgs["K8S_POD_NAME"], gw)
	_, r, _ := net.ParseCIDR("0.0.0.0/0")
	result := &current.Result{
		CNIVersion: current.ImplementedSpecVersion,
		IPs: []*current.IPConfig{
			{
				Address: *ipNet,
				Gateway: gw,
			},
		},

		Routes: []*types.Route{
			{
				Dst: *r,
			},
		},
	}

	logging.Infof("[%s] CNI add command completed in %v", operation, time.Since(addTimeStart))
	return types.PrintResult(result, conf.CNIVersion)
}

func getInstanceInfo(client *http.Client) (*compute.Service, string, string, string, string, error) {
	computeService, err := compute.New(client)
	if err != nil {
		return nil, "", "", "", "", fmt.Errorf("failed to create compute service: %w", err)
	}

	projectID, err := metadata.ProjectID()
	if err != nil {
		return nil, "", "", "", "", fmt.Errorf("failed to get project ID from metadata: %w", err)
	}

	zone, err := metadata.Zone()
	if err != nil {
		return nil, "", "", "", "", fmt.Errorf("failed to get zone from metadata: %w", err)
	}
	region := ""
	if region == "" {
		if len(zone) > 2 {
			region = zone[:len(zone)-2]
		} else {
			return nil, "", "", "", "", fmt.Errorf("cannot determine region from zone: %s", zone)
		}
	}

	instanceName, err := metadata.InstanceName()
	if err != nil {
		return nil, "", "", "", "", fmt.Errorf("failed to get instance name from metadata: %w", err)
	}
	return computeService, projectID, zone, region, instanceName, err
}

func cmdDel(args *skel.CmdArgs) error {
	delTimeStart := time.Now()
	operation := "DEL"
	if args.Netns == "" {
		return nil
	}
	fileLock := flock.New("/var/run/gcp-ipam.lock")
	fileLock.Lock()
	logging.Debugf("[%s] Acquired file lock time %v", operation, time.Since(delTimeStart))
	defer fileLock.Unlock()

	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	logging.Debugf("[%s] Processing CNI del command: %+v", operation, args.Args)
	logging.Debugf("[%s] Configuration: %+v", operation, string(args.StdinData))

	cniArgs := lo.SliceToMap(strings.Split(args.Args, ";"), func(s string) (string, string) {
		parts := strings.SplitN(s, "=", 2)
		if len(parts) == 2 {
			return parts[0], parts[1]
		}
		return parts[0], ""
	})

	k8sclient, err := buildKubeClient()
	if err != nil {
		return fmt.Errorf("failed to build k8s client: %w", err)
	}

	startTime := time.Now()
	p, err := k8sclient.CoreV1().Pods(cniArgs["K8S_POD_NAMESPACE"]).Get(context.TODO(), cniArgs["K8S_POD_NAME"], metav1.GetOptions{})
	logging.Infof("[%s][K8s Operation] Get pod %s/%s took %v", operation, cniArgs["K8S_POD_NAMESPACE"], cniArgs["K8S_POD_NAME"], time.Since(startTime))
	if err != nil {
		return fmt.Errorf("failed to get pod %s/%s: %w", cniArgs["K8S_POD_NAMESPACE"], cniArgs["K8S_POD_NAME"], err)
	}

	// Check if this is a migration flow - if so, don't release the IP from the pool
	const MoveOutAnnotation = "live.cast.ai/move-out-ip"
	_, isMigrationFlow := p.Annotations[MoveOutAnnotation]
	if isMigrationFlow {
		logging.Infof("[%s] Migration flow detected (moveout annotation present), skipping IP release from pool", operation)
	}

	ip := p.Status.PodIPs[0].IP

	ctx := context.Background()
	client, err := google.DefaultClient(ctx, compute.CloudPlatformScope)
	if err != nil {
		return fmt.Errorf("failed to create google default client: %w", err)
	}

	computeService, projectID, zone, _, instanceName, err := getInstanceInfo(client)
	if err != nil {
		return err
	}

	startTime = time.Now()
	instance, err := computeService.Instances.Get(projectID, zone, instanceName).Context(ctx).Do()
	logging.Infof("[%s][Cloud Operation] Get instance %s took %v", operation, instanceName, time.Since(startTime))
	if err != nil {
		return fmt.Errorf("failed to get instance details: %w", err)
	}

	startTime = time.Now()
	i, err := computeService.Instances.Get(projectID, zone, instanceName).Context(ctx).Do()
	logging.Infof("[%s][Cloud Operation] Get instance (2nd call) %s took %v", operation, instanceName, time.Since(startTime))
	if err != nil {
		return fmt.Errorf("failed to get instance: %w", err)
	}
	removed := lo.Filter(i.NetworkInterfaces[0].AliasIpRanges, func(a *compute.AliasIpRange, _ int) bool {
		return a.IpCidrRange != fmt.Sprintf("%s/32", ip)
	})

	logging.Infof("[%s] Removing IP %s from instance %s", operation, ip, instance.Name)
	logging.Infof("[%s] Current IPs on instance: %+v", operation, litter.Sdump(i.NetworkInterfaces[0].AliasIpRanges))
	logging.Infof("[%s] IPs to be left on instance: %+v", operation, litter.Sdump(removed))

	startTime = time.Now()
	c, err := computeService.Instances.UpdateNetworkInterface(projectID, zone, instanceName, i.NetworkInterfaces[0].Name, &compute.NetworkInterface{
		Fingerprint:   i.NetworkInterfaces[0].Fingerprint,
		AliasIpRanges: removed,
	}).Do()
	logging.Infof("[%s][Cloud Operation] Update network interface on instance %s took %v", operation, instanceName, time.Since(startTime))
	if err != nil {
		return fmt.Errorf("failed to update network interface: %w", err)
	}

	startTime = time.Now()
	if err := waitForInstanceOperation(ctx, computeService, projectID, zone, c.Name); err != nil {
		return fmt.Errorf("failed to wait for network interface update operation: %w", err)
	}
	logging.Infof("[%s][Cloud Operation] Wait for network interface update operation took %v", operation, time.Since(startTime))

	// Release IP from the pool only if this is not a migration flow
	if !isMigrationFlow {
		dynamicClient, err := buildDynamicClient()
		if err != nil {
			logging.Errorf("[%s] Failed to build dynamic client for IP release: %v", operation, err)
			// Don't fail the entire operation if we can't release from pool
			// The IP is already removed from the instance
			return nil
		}

		allocator := ipam.NewAllocator(dynamicClient)

		// Determine pool name
		subnetwork := instance.NetworkInterfaces[0].Subnetwork
		subnetworkParts := strings.Split(subnetwork, "/")
		subnetwork = subnetworkParts[len(subnetworkParts)-1]

		poolName := conf.IPPoolName
		if poolName == "" {
			poolName = fmt.Sprintf("ippool-%s", subnetwork)
		}

		startTime = time.Now()
		if err := allocator.Release(ctx, poolName, ip); err != nil {
			logging.Errorf("[%s] Failed to release IP %s from pool %s: %v", operation, ip, poolName, err)
			// Don't fail the entire operation - IP is already removed from instance
		} else {
			logging.Infof("[%s][K8s Operation] Release IP %s from pool %s took %v", operation, ip, poolName, time.Since(startTime))
			logging.Infof("[%s] Released IP %s from pool %s", operation, ip, poolName)
		}
	}

	logging.Infof("[%s] CNI del command completed in %v", operation, time.Since(delTimeStart))
	return nil
}

func buildKubeClient() (*kubernetes.Clientset, error) {
	kubeconfig := "/var/lib/kubelet/kubeconfig"

	conf, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(conf)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}

func buildDynamicClient() (dynamic.Interface, error) {
	kubeconfig := "/var/lib/kubelet/kubeconfig"

	conf, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}

	// Register our API types
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add types to scheme: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(conf)
	if err != nil {
		return nil, err
	}

	return dynamicClient, nil
}
