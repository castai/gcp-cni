package ipam

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/castai/gcp-cni/pkg/apis/ipam/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	// MaxRetries is the maximum number of retries for allocation due to conflicts
	MaxRetries = 10
	// RetryDelay is the base delay between retries (with exponential backoff)
	RetryDelay = 100 * time.Millisecond
)

var (
	// IPPoolGVR is the GroupVersionResource for IPPool
	IPPoolGVR = schema.GroupVersionResource{
		Group:    "ipam.gcp-cni.cast.ai",
		Version:  "v1alpha1",
		Resource: "ippools",
	}
)

// Allocator handles IP allocation from IPPool resources
type Allocator struct {
	client dynamic.Interface
}

// NewAllocator creates a new IP allocator
func NewAllocator(client dynamic.Interface) *Allocator {
	return &Allocator{
		client: client,
	}
}

// AllocationRequest contains the details needed to allocate an IP
type AllocationRequest struct {
	PoolName     string
	PodName      string
	PodNamespace string
	PodUID       string
	NodeName     string
	RequestedIP  string // Optional: specific IP requested (for migration)
}

// AllocationResult contains the allocated IP and related information
type AllocationResult struct {
	IP                 string
	CIDR               string
	Subnet             string
	SecondaryRangeName string
}

// Allocate allocates an IP address from the specified pool
// It uses optimistic locking (resourceVersion) to handle concurrent allocations
func (a *Allocator) Allocate(ctx context.Context, req *AllocationRequest) (*AllocationResult, error) {
	var lastErr error

	for i := 0; i < MaxRetries; i++ {
		if i > 0 {
			// Exponential backoff
			delay := RetryDelay * time.Duration(1<<uint(i-1))
			time.Sleep(delay)
		}

		result, err := a.tryAllocate(ctx, req)
		if err == nil {
			return result, nil
		}

		// If it's a conflict error, retry
		if errors.IsConflict(err) {
			lastErr = err
			continue
		}

		// For other errors, return immediately
		return nil, err
	}

	return nil, fmt.Errorf("failed to allocate IP after %d retries: %w", MaxRetries, lastErr)
}

// tryAllocate attempts a single allocation with optimistic locking
func (a *Allocator) tryAllocate(ctx context.Context, req *AllocationRequest) (*AllocationResult, error) {
	// Get the current IPPool
	poolUnstructured, err := a.client.Resource(IPPoolGVR).Get(ctx, req.PoolName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get IPPool %s: %w", req.PoolName, err)
	}

	// Convert to IPPool type
	pool := &v1alpha1.IPPool{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(poolUnstructured.Object, pool); err != nil {
		return nil, fmt.Errorf("failed to convert unstructured to IPPool: %w", err)
	}

	// Initialize allocations map if nil
	if pool.Spec.Allocations == nil {
		pool.Spec.Allocations = make(map[string]v1alpha1.IPAllocation)
	}

	var allocatedIP string

	// If a specific IP is requested (migration case), try to allocate it
	if req.RequestedIP != "" {
		if _, exists := pool.Spec.Allocations[req.RequestedIP]; exists {
			return nil, fmt.Errorf("requested IP %s is already allocated", req.RequestedIP)
		}
		allocatedIP = req.RequestedIP
	} else {
		// Find an available IP
		availableIP, err := findAvailableIP(pool.Spec.CIDR, pool.Spec.Allocations)
		if err != nil {
			return nil, fmt.Errorf("failed to find available IP: %w", err)
		}
		allocatedIP = availableIP
	}

	// Add the allocation
	pool.Spec.Allocations[allocatedIP] = v1alpha1.IPAllocation{
		PodName:      req.PodName,
		PodNamespace: req.PodNamespace,
		PodUID:       req.PodUID,
		NodeName:     req.NodeName,
		AllocatedAt:  metav1.Now(),
	}

	// Update status
	capacity := calculateCapacity(pool.Spec.CIDR)
	pool.Status.Capacity = capacity
	pool.Status.Allocated = len(pool.Spec.Allocations)
	pool.Status.Available = capacity - pool.Status.Allocated
	pool.Status.LastUpdated = metav1.Now()

	// Convert back to unstructured
	updatedUnstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pool)
	if err != nil {
		return nil, fmt.Errorf("failed to convert IPPool to unstructured: %w", err)
	}

	// Update with optimistic locking (resourceVersion check)
	_, err = a.client.Resource(IPPoolGVR).Update(ctx, &unstructured.Unstructured{Object: updatedUnstructured}, metav1.UpdateOptions{})
	if err != nil {
		return nil, err // Will be IsConflict error if another update happened
	}

	return &AllocationResult{
		IP:                 allocatedIP,
		CIDR:               pool.Spec.CIDR,
		Subnet:             pool.Spec.Subnet,
		SecondaryRangeName: pool.Spec.SecondaryRangeName,
	}, nil
}

// GetAllocation retrieves allocation information for an existing IP without modifying the pool
func (a *Allocator) GetAllocation(ctx context.Context, poolName, ip string) (*AllocationResult, error) {
	// Get the current IPPool
	poolUnstructured, err := a.client.Resource(IPPoolGVR).Get(ctx, poolName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get IPPool %s: %w", poolName, err)
	}

	// Convert to IPPool type
	pool := &v1alpha1.IPPool{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(poolUnstructured.Object, pool); err != nil {
		return nil, fmt.Errorf("failed to convert unstructured to IPPool: %w", err)
	}

	// Check if the IP is allocated
	if pool.Spec.Allocations == nil {
		return nil, fmt.Errorf("IP %s not found in pool %s", ip, poolName)
	}

	if _, exists := pool.Spec.Allocations[ip]; !exists {
		return nil, fmt.Errorf("IP %s not found in pool %s", ip, poolName)
	}

	return &AllocationResult{
		IP:                 ip,
		CIDR:               pool.Spec.CIDR,
		Subnet:             pool.Spec.Subnet,
		SecondaryRangeName: pool.Spec.SecondaryRangeName,
	}, nil
}

// Release releases an IP address back to the pool
func (a *Allocator) Release(ctx context.Context, poolName, ip string) error {
	var lastErr error

	for i := 0; i < MaxRetries; i++ {
		if i > 0 {
			delay := RetryDelay * time.Duration(1<<uint(i-1))
			time.Sleep(delay)
		}

		err := a.tryRelease(ctx, poolName, ip)
		if err == nil {
			return nil
		}

		if errors.IsConflict(err) {
			lastErr = err
			continue
		}

		return err
	}

	return fmt.Errorf("failed to release IP after %d retries: %w", MaxRetries, lastErr)
}

// tryRelease attempts a single IP release with optimistic locking
func (a *Allocator) tryRelease(ctx context.Context, poolName, ip string) error {
	// Get the current IPPool
	poolUnstructured, err := a.client.Resource(IPPoolGVR).Get(ctx, poolName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get IPPool %s: %w", poolName, err)
	}

	// Convert to IPPool type
	pool := &v1alpha1.IPPool{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(poolUnstructured.Object, pool); err != nil {
		return fmt.Errorf("failed to convert unstructured to IPPool: %w", err)
	}

	// Remove the allocation
	if pool.Spec.Allocations != nil {
		delete(pool.Spec.Allocations, ip)
	}

	// Update status
	capacity := calculateCapacity(pool.Spec.CIDR)
	pool.Status.Capacity = capacity
	pool.Status.Allocated = len(pool.Spec.Allocations)
	pool.Status.Available = capacity - pool.Status.Allocated
	pool.Status.LastUpdated = metav1.Now()

	// Convert back to unstructured
	updatedUnstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(pool)
	if err != nil {
		return fmt.Errorf("failed to convert IPPool to unstructured: %w", err)
	}

	// Update with optimistic locking
	_, err = a.client.Resource(IPPoolGVR).Update(ctx, &unstructured.Unstructured{Object: updatedUnstructured}, metav1.UpdateOptions{})
	return err
}

// findAvailableIP finds the first available IP in the CIDR range
func findAvailableIP(cidr string, allocations map[string]v1alpha1.IPAllocation) (string, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR %s: %w", cidr, err)
	}

	// Start from the first IP in the range
	currentIP := ip.Mask(ipNet.Mask)

	// Skip network address (first IP)
	currentIP = nextIP(currentIP)

	// Iterate through the range
	for ipNet.Contains(currentIP) {
		ipStr := currentIP.String()

		// Skip broadcast address (last IP in range)
		if isBroadcast(currentIP, ipNet) {
			break
		}

		// Check if this IP is available
		if _, exists := allocations[ipStr]; !exists {
			return ipStr, nil
		}

		currentIP = nextIP(currentIP)
	}

	return "", fmt.Errorf("no available IPs in CIDR %s", cidr)
}

// nextIP returns the next IP address
func nextIP(ip net.IP) net.IP {
	next := make(net.IP, len(ip))
	copy(next, ip)

	for i := len(next) - 1; i >= 0; i-- {
		next[i]++
		if next[i] > 0 {
			break
		}
	}

	return next
}

// isBroadcast checks if an IP is the broadcast address for the network
func isBroadcast(ip net.IP, ipNet *net.IPNet) bool {
	broadcast := make(net.IP, len(ip))
	for i := range ip {
		broadcast[i] = ip[i] | ^ipNet.Mask[i]
	}
	return ip.Equal(broadcast)
}

// calculateCapacity calculates the total number of usable IPs in a CIDR range
func calculateCapacity(cidr string) int {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return 0
	}

	ones, bits := ipNet.Mask.Size()
	// Total IPs = 2^(bits - ones)
	// Usable IPs = Total - 2 (network and broadcast addresses)
	totalIPs := 1 << uint(bits-ones)

	// For /32, there's only 1 usable IP
	if ones == bits {
		return 1
	}

	// For others, subtract network and broadcast addresses
	return totalIPs - 2
}

// ipToInt converts an IP address to uint32
func ipToInt(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return binary.BigEndian.Uint32(ip)
}
