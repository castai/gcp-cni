package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// IPPool represents a pool of IP addresses available for allocation
type IPPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPPoolSpec   `json:"spec"`
	Status IPPoolStatus `json:"status,omitempty"`
}

// IPPoolSpec defines the desired state of IPPool
type IPPoolSpec struct {
	// CIDR is the IP range for this pool (e.g., "10.111.0.0/16")
	CIDR string `json:"cidr"`

	// Subnet is the GCP subnetwork URL where this pool is allocated
	Subnet string `json:"subnet"`

	// SecondaryRangeName is the name of the secondary range on the subnet
	// +optional
	SecondaryRangeName string `json:"secondaryRangeName,omitempty"`

	// Allocations maps IP addresses to their allocation details
	// +optional
	Allocations map[string]IPAllocation `json:"allocations,omitempty"`
}

// IPAllocation represents a single IP allocation
type IPAllocation struct {
	// PodName is the name of the pod using this IP
	PodName string `json:"podName"`

	// PodNamespace is the namespace of the pod
	PodNamespace string `json:"podNamespace"`

	// PodUID is the unique identifier of the pod
	PodUID string `json:"podUID"`

	// NodeName is the node where this IP is assigned
	NodeName string `json:"nodeName"`

	// AllocatedAt is the timestamp when the IP was allocated
	// +optional
	AllocatedAt metav1.Time `json:"allocatedAt,omitempty"`
}

// IPPoolStatus represents the observed state of IPPool
type IPPoolStatus struct {
	// Capacity is the total number of IPs in the pool
	// +optional
	Capacity int `json:"capacity,omitempty"`

	// Allocated is the number of currently allocated IPs
	// +optional
	Allocated int `json:"allocated,omitempty"`

	// Available is the number of available IPs
	// +optional
	Available int `json:"available,omitempty"`

	// LastUpdated is the last time the status was updated
	// +optional
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// IPPoolList contains a list of IPPool
type IPPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []IPPool `json:"items"`
}
