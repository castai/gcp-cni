package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/compute/metadata"
)

type clusterInfo struct {
	projectID      string
	region         string
	subnetworkName string
	networkName    string
}

// TODO: get information from actual GKE cluster API,
// TODO: assume single subnet for cluster
func getClusterInfo(ctx context.Context, instancesClient *compute.InstancesClient, logger *slog.Logger) (*clusterInfo, error) {
	projectID, err := metadata.ProjectID()
	if err != nil {
		return nil, fmt.Errorf("failed to get project ID from metadata: %w", err)
	}

	zone, err := metadata.Zone()
	if err != nil {
		return nil, fmt.Errorf("failed to get zone from metadata: %w", err)
	}

	region := ""
	if len(zone) > 2 {
		region = zone[:len(zone)-2]
	} else {
		return nil, fmt.Errorf("cannot determine region from zone: %s", zone)
	}

	instanceName, err := metadata.InstanceName()
	if err != nil {
		return nil, fmt.Errorf("failed to get instance name from metadata: %w", err)
	}

	instance, err := instancesClient.Get(ctx, &computepb.GetInstanceRequest{
		Project:  projectID,
		Zone:     zone,
		Instance: instanceName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get instance details: %w", err)
	}

	if len(instance.GetNetworkInterfaces()) == 0 {
		return nil, fmt.Errorf("instance has no network interfaces")
	}

	networkPath := instance.GetNetworkInterfaces()[0].GetNetwork()
	networkParts := strings.Split(networkPath, "/")
	networkName := ""
	if len(networkParts) > 0 {
		networkName = networkParts[len(networkParts)-1]
	} else {
		return nil, fmt.Errorf("failed to parse network name from: %s", networkPath)
	}

	subnetworkPath := instance.GetNetworkInterfaces()[0].GetSubnetwork()
	subnetworkParts := strings.Split(subnetworkPath, "/")
	subnetworkName := ""
	if len(subnetworkParts) > 0 {
		subnetworkName = subnetworkParts[len(subnetworkParts)-1]
	} else {
		return nil, fmt.Errorf("failed to parse subnetwork name from: %s", subnetworkPath)
	}

	return &clusterInfo{
		projectID:      projectID,
		region:         region,
		subnetworkName: subnetworkName,
		networkName:    networkName,
	}, nil
}
