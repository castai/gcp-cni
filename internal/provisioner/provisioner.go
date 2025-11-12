package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	networkconnectivity "cloud.google.com/go/networkconnectivity/apiv1"
	"github.com/castai/gcp-cni/pkg/apis/ipam/v1alpha1"
	"github.com/samber/lo"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type Provisioner struct {
	logger *slog.Logger

	subnetworkClient       *compute.SubnetworksClient
	internalRangeClient    *networkconnectivity.InternalRangeClient
	instancesClient        *compute.InstancesClient
	regionOperationsClient *compute.RegionOperationsClient
	dynamicClient          dynamic.Interface
}

func NewProvisioner(ctx context.Context, logger *slog.Logger) (*Provisioner, error) {
	subnetworksClient, err := compute.NewSubnetworksRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create subnetworks client: %w", err)
	}

	internalRangesClient, err := networkconnectivity.NewInternalRangeClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create internal ranges client: %w", err)
	}

	instancesClient, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create instances client: %w", err)
	}

	regionOperationsClient, err := compute.NewRegionOperationsRESTClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create region operations client: %w", err)
	}

	dynamicClient, err := buildDynamicClient()
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}

	return &Provisioner{
		logger:                 logger,
		subnetworkClient:       subnetworksClient,
		internalRangeClient:    internalRangesClient,
		instancesClient:        instancesClient,
		regionOperationsClient: regionOperationsClient,
		dynamicClient:          dynamicClient,
	}, nil
}

// buildDynamicClient creates a Kubernetes dynamic client
func buildDynamicClient() (dynamic.Interface, error) {
	var config *rest.Config
	var err error

	// Try in-cluster config first
	config, err = rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("get home directory: %w", err)
			}
			kubeconfig = homeDir + "/.kube/config"
		}

		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("build kubeconfig: %w", err)
		}
	}

	// Register our API types
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add types to scheme: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}

	return dynamicClient, nil
}

func (p *Provisioner) Provision(ctx context.Context, secondaryRangeName *string) error {
	clusterInfo, err := getClusterInfo(ctx, p.instancesClient, p.logger)
	if err != nil {
		return fmt.Errorf("get cluster info: %w", err)
	}

	p.logger.Info("Cluster information retrieved",
		slog.String("project_id", clusterInfo.projectID),
		slog.String("region", clusterInfo.region),
		slog.String("network", clusterInfo.networkName),
		slog.String("subnetwork", clusterInfo.subnetworkName),
	)

	subnet, err := p.subnetworkClient.Get(ctx, &computepb.GetSubnetworkRequest{
		Project:    clusterInfo.projectID,
		Region:     clusterInfo.region,
		Subnetwork: clusterInfo.subnetworkName,
	})
	if err != nil {
		return fmt.Errorf("get subnetwork: %w", err)
	}

	p.logger.Info("Current subnet configuration",
		slog.String("primary_cidr", subnet.GetIpCidrRange()),
		slog.Int("secondary_ranges_count", len(subnet.GetSecondaryIpRanges())),
		slog.String("secondary_ranges", fmt.Sprintf("%v", lo.Map(subnet.GetSecondaryIpRanges(),
			func(r *computepb.SubnetworkSecondaryRange, _ int) string {
				return fmt.Sprintf("%s:%s", r.GetRangeName(), r.GetIpCidrRange())
			}))),
	)

	for _, r := range subnet.GetSecondaryIpRanges() {
		p.logger.Debug("Existing secondary range",
			slog.String("name", r.GetRangeName()),
			slog.String("cidr", r.GetIpCidrRange()),
		)
		if r.GetRangeName() == *secondaryRangeName {
			p.logger.Info("Secondary range already exists",
				slog.String("name", r.GetRangeName()),
				slog.String("cidr", r.GetIpCidrRange()),
			)

			// Ensure IPPool exists for the existing range
			subnetURL := fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s",
				clusterInfo.projectID,
				clusterInfo.region,
				clusterInfo.subnetworkName,
			)

			if err := p.createOrUpdateIPPool(ctx, clusterInfo.subnetworkName, r.GetIpCidrRange(), subnetURL, *secondaryRangeName); err != nil {
				p.logger.Error("Failed to ensure IPPool resource exists",
					slog.String("error", err.Error()),
				)
				return fmt.Errorf("ensure IPPool resource: %w", err)
			}

			return nil
		}
	}

	internalRangeCIDR, err := allocateInternalRange(ctx, p.internalRangeClient, clusterInfo, *secondaryRangeName, p.logger)

	p.logger.Info("Creating secondary IP range on subnet",
		slog.String("name", *secondaryRangeName),
		slog.String("cidr", internalRangeCIDR),
	)

	patchSubnet := computepb.Subnetwork{
		Fingerprint: subnet.Fingerprint,
	}
	internalRangePath := fmt.Sprintf("//networkconnectivity.googleapis.com/projects/%s/locations/global/internalRanges/%s", clusterInfo.projectID, *secondaryRangeName)
	patchSubnet.SecondaryIpRanges = append(subnet.SecondaryIpRanges, &computepb.SubnetworkSecondaryRange{
		RangeName:             proto.String(*secondaryRangeName),
		ReservedInternalRange: proto.String(internalRangePath),
		IpCidrRange:           proto.String(internalRangeCIDR),
	})

	op, err := p.subnetworkClient.Patch(ctx, &computepb.PatchSubnetworkRequest{
		Project:            clusterInfo.projectID,
		Region:             clusterInfo.region,
		Subnetwork:         clusterInfo.subnetworkName,
		SubnetworkResource: &patchSubnet,
	})
	if err != nil {
		return fmt.Errorf("update subnetwork: %w", err)
	}

	p.logger.Info("Waiting for subnet update operation to complete",
		slog.String("operation", op.Proto().GetName()),
	)

	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("wait for subnet update: %w", err)
	}

	p.logger.Info("VPC provisioning completed successfully",
		slog.String("reservation_name", *secondaryRangeName),
		slog.String("secondary_range_name", *secondaryRangeName),
		slog.String("cidr", internalRangeCIDR),
	)

	// Create IPPool resource for the secondary range
	subnetURL := fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s",
		clusterInfo.projectID,
		clusterInfo.region,
		clusterInfo.subnetworkName,
	)

	if err := p.createOrUpdateIPPool(ctx, clusterInfo.subnetworkName, internalRangeCIDR, subnetURL, *secondaryRangeName); err != nil {
		p.logger.Error("Failed to create IPPool resource",
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("create IPPool resource: %w", err)
	}

	p.logger.Info("IPPool resource created successfully",
		slog.String("pool_name", fmt.Sprintf("ippool-%s", clusterInfo.subnetworkName)),
		slog.String("cidr", internalRangeCIDR),
	)

	return nil
}

// createOrUpdateIPPool creates or updates an IPPool resource for the secondary range
func (p *Provisioner) createOrUpdateIPPool(ctx context.Context, subnetworkName, cidr, subnetURL, secondaryRangeName string) error {
	poolName := fmt.Sprintf("ippool-%s", subnetworkName)

	// Define IPPool GVR
	ipPoolGVR := schema.GroupVersionResource{
		Group:    "ipam.gcp-cni.cast.ai",
		Version:  "v1alpha1",
		Resource: "ippools",
	}

	// Create IPPool object
	ipPool := &v1alpha1.IPPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "ipam.gcp-cni.cast.ai/v1alpha1",
			Kind:       "IPPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: poolName,
		},
		Spec: v1alpha1.IPPoolSpec{
			CIDR:               cidr,
			Subnet:             subnetURL,
			SecondaryRangeName: secondaryRangeName,
			Allocations:        make(map[string]v1alpha1.IPAllocation),
		},
	}

	// Convert to unstructured
	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ipPool)
	if err != nil {
		return fmt.Errorf("convert to unstructured: %w", err)
	}

	unstructuredIPPool := &unstructured.Unstructured{Object: unstructuredObj}

	// Try to get existing IPPool
	existing, err := p.dynamicClient.Resource(ipPoolGVR).Get(ctx, poolName, metav1.GetOptions{})
	if err == nil {
		// IPPool exists, update it
		p.logger.Info("IPPool already exists, updating",
			slog.String("pool_name", poolName),
		)

		// Preserve existing allocations and resourceVersion
		existingIPPool := &v1alpha1.IPPool{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(existing.Object, existingIPPool); err != nil {
			return fmt.Errorf("convert existing IPPool: %w", err)
		}

		// Update only CIDR, Subnet, and SecondaryRangeName, keep existing allocations
		ipPool.Spec.Allocations = existingIPPool.Spec.Allocations
		ipPool.ObjectMeta.ResourceVersion = existingIPPool.ObjectMeta.ResourceVersion

		// Convert to unstructured again with updated data
		unstructuredObj, err = runtime.DefaultUnstructuredConverter.ToUnstructured(ipPool)
		if err != nil {
			return fmt.Errorf("convert to unstructured: %w", err)
		}
		unstructuredIPPool = &unstructured.Unstructured{Object: unstructuredObj}

		_, err = p.dynamicClient.Resource(ipPoolGVR).Update(ctx, unstructuredIPPool, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("update IPPool: %w", err)
		}

		p.logger.Info("IPPool updated successfully", slog.String("pool_name", poolName))
		return nil
	}

	// IPPool doesn't exist, create it
	p.logger.Info("Creating new IPPool",
		slog.String("pool_name", poolName),
		slog.String("cidr", cidr),
	)

	_, err = p.dynamicClient.Resource(ipPoolGVR).Create(ctx, unstructuredIPPool, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create IPPool: %w", err)
	}

	p.logger.Info("IPPool created successfully", slog.String("pool_name", poolName))
	return nil
}
