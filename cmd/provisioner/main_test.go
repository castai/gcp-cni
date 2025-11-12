package main

import (
	"log/slog"
	"net"
	"os"
	"testing"

	"google.golang.org/api/compute/v1"
)

func TestFindAvailableCIDR(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	tests := []struct {
		name           string
		primaryRange   string
		secondaryRanges []string
		prefixBits     int
		wantErr        bool
	}{
		{
			name:         "no conflicts",
			primaryRange: "10.0.0.0/24",
			secondaryRanges: []string{},
			prefixBits:   16,
			wantErr:      false,
		},
		{
			name:         "with existing secondary range",
			primaryRange: "10.0.0.0/16",
			secondaryRanges: []string{"10.1.0.0/16"},
			prefixBits:   16,
			wantErr:      false,
		},
		{
			name:         "multiple existing ranges",
			primaryRange: "10.0.0.0/16",
			secondaryRanges: []string{
				"10.1.0.0/16",
				"10.2.0.0/16",
				"10.3.0.0/16",
			},
			prefixBits: 16,
			wantErr:    false,
		},
		{
			name:         "use 172 range when 10 is occupied",
			primaryRange: "10.0.0.0/8",
			secondaryRanges: []string{},
			prefixBits:   16,
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subnet := &compute.Subnetwork{
				IpCidrRange: tt.primaryRange,
				SecondaryIpRanges: make([]*compute.SubnetworkSecondaryRange, 0),
			}

			for _, cidr := range tt.secondaryRanges {
				subnet.SecondaryIpRanges = append(subnet.SecondaryIpRanges, &compute.SubnetworkSecondaryRange{
					RangeName:   "test",
					IpCidrRange: cidr,
				})
			}

			// Collect existing ranges
			existingRanges := []string{subnet.IpCidrRange}
			for _, r := range subnet.SecondaryIpRanges {
				existingRanges = append(existingRanges, r.IpCidrRange)
			}

			allocatedCIDR, err := findAvailableCIDR(existingRanges, tt.prefixBits, logger)
			if (err != nil) != tt.wantErr {
				t.Errorf("findAvailableCIDR() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				t.Logf("Allocated CIDR: %s", allocatedCIDR)

				// Verify the allocated CIDR is valid
				_, allocatedNet, err := net.ParseCIDR(allocatedCIDR)
				if err != nil {
					t.Errorf("Allocated CIDR is invalid: %v", err)
					return
				}

				// Verify it doesn't conflict with existing ranges
				primaryNet := mustParseCIDR(tt.primaryRange)
				if networksOverlap(allocatedNet, primaryNet) {
					t.Errorf("Allocated CIDR %s overlaps with primary range %s", allocatedCIDR, tt.primaryRange)
				}

				for _, existing := range tt.secondaryRanges {
					existingNet := mustParseCIDR(existing)
					if networksOverlap(allocatedNet, existingNet) {
						t.Errorf("Allocated CIDR %s overlaps with existing range %s", allocatedCIDR, existing)
					}
				}

				// Verify the prefix length is correct
				ones, _ := allocatedNet.Mask.Size()
				if ones != tt.prefixBits {
					t.Errorf("Allocated CIDR has prefix /%d, want /%d", ones, tt.prefixBits)
				}
			}
		})
	}
}

func TestNetworksOverlap(t *testing.T) {
	tests := []struct {
		name     string
		cidrA    string
		cidrB    string
		wantOverlap bool
	}{
		{
			name:     "completely separate",
			cidrA:    "10.0.0.0/16",
			cidrB:    "10.1.0.0/16",
			wantOverlap: false,
		},
		{
			name:     "exact match",
			cidrA:    "10.0.0.0/16",
			cidrB:    "10.0.0.0/16",
			wantOverlap: true,
		},
		{
			name:     "A contains B",
			cidrA:    "10.0.0.0/8",
			cidrB:    "10.1.0.0/16",
			wantOverlap: true,
		},
		{
			name:     "B contains A",
			cidrA:    "10.1.0.0/16",
			cidrB:    "10.0.0.0/8",
			wantOverlap: true,
		},
		{
			name:     "partial overlap",
			cidrA:    "10.0.0.0/16",
			cidrB:    "10.0.128.0/17",
			wantOverlap: true,
		},
		{
			name:     "adjacent no overlap",
			cidrA:    "10.0.0.0/16",
			cidrB:    "10.1.0.0/16",
			wantOverlap: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			netA := mustParseCIDR(tt.cidrA)
			netB := mustParseCIDR(tt.cidrB)

			gotOverlap := networksOverlap(netA, netB)
			if gotOverlap != tt.wantOverlap {
				t.Errorf("networksOverlap(%s, %s) = %v, want %v", tt.cidrA, tt.cidrB, gotOverlap, tt.wantOverlap)
			}
		})
	}
}

func mustParseCIDR(cidr string) *net.IPNet {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	return ipNet
}
