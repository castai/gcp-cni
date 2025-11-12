package provisioner

import (
	"context"
	"fmt"
	"log/slog"

	networkconnectivity "cloud.google.com/go/networkconnectivity/apiv1"
	"cloud.google.com/go/networkconnectivity/apiv1/networkconnectivitypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	internalRangePrefixLength = 16
	targetCidr                = "10.0.0.0/8"
)

// Auto allocate an internal IP range reservation
func allocateInternalRange(ctx context.Context, internalRangesClient *networkconnectivity.InternalRangeClient, c *clusterInfo, addressName string, logger *slog.Logger) (string, error) {
	parent := fmt.Sprintf("projects/%s/locations/global", c.projectID)
	resourceName := fmt.Sprintf("%s/internalRanges/%s", parent, addressName)

	// Check if reservation already exists
	existingRange, err := internalRangesClient.GetInternalRange(ctx, &networkconnectivitypb.GetInternalRangeRequest{
		Name: resourceName,
	})
	if err == nil {
		existingCIDR := existingRange.GetIpCidrRange()
		logger.Info("Internal IP range reservation already exists",
			slog.String("address_name", addressName),
			slog.String("cidr", existingCIDR),
		)
		return existingCIDR, nil
	}

	// If error is not 404, it's a real error
	if !isNotFound(err) {
		return "", fmt.Errorf("failed to check existing reservation: %w", err)
	}

	// Create the internal IP address range reservation
	logger.Info("Creating internal IP address range reservation",
		slog.String("name", addressName),
		slog.Int("prefix_length", internalRangePrefixLength),
		slog.String("network", c.networkName),
	)

	// Build the network URL
	networkURL := fmt.Sprintf("projects/%s/global/networks/%s", c.projectID, c.networkName)

	internalRange := &networkconnectivitypb.InternalRange{
		Name:            addressName,
		Network:         networkURL,
		PrefixLength:    int32(internalRangePrefixLength),
		TargetCidrRange: []string{targetCidr},
		Usage:           networkconnectivitypb.InternalRange_FOR_VPC,
		Description:     "Reserved internal IP range for GCP CNI",
	}

	op, err := internalRangesClient.CreateInternalRange(ctx, &networkconnectivitypb.CreateInternalRangeRequest{
		Parent:          parent,
		InternalRangeId: addressName,
		InternalRange:   internalRange,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create internal range reservation: %w", err)
	}

	logger.Info("Waiting for internal range reservation operation to complete",
		slog.String("operation", op.Name()),
	)

	createdRange, err := op.Wait(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to wait for internal range reservation: %w", err)
	}

	logger.Info("Internal IP range reservation created successfully",
		slog.String("address_name", createdRange.GetName()),
		slog.String("cidr", createdRange.GetIpCidrRange()),
	)

	return createdRange.GetIpCidrRange(), nil
}

func isNotFound(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.NotFound
}
