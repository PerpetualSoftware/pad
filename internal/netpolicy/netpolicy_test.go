package netpolicy

import (
	"net"
	"testing"
)

func TestBlocked(t *testing.T) {
	blocked := []string{
		// the ranges BUG-3358 found URL import reaching
		"198.18.0.1", "198.19.255.254", "0.0.0.1", "240.0.0.1", "255.255.255.255",
		"192.0.0.8", "192.0.2.1", "198.51.100.1", "203.0.113.1",
		// what it already blocked
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1",
		"0.0.0.0", "224.0.0.1",
		// IPv6
		"::1", "::", "fe80::1", "fc00::1", "ff02::1", "2001:db8::1",
		"::ffff:198.18.0.1", "::ffff:127.0.0.1", // IPv4-mapped
		"::c612:1",        // IPv4-compatible 198.18.0.1
		"64:ff9b::c612:1", // NAT64 of 198.18.0.1: judged as the embedded IPv4
		"64:ff9b::7f00:1", // NAT64 of 127.0.0.1
		"192.88.99.2",     // 6to4 relay anycast (codex r1)
		"100:0:0:1::1",    // dummy prefix (codex r1)
		"3fff::1",         // documentation (codex r1)
		"5f00::1",         // SRv6 SIDs (codex r1)
		"64:ff9b:1::1",    // NAT64 local-use
		"2002:c612:1::1",  // 6to4 of 198.18.0.1
		"2001::1",         // Teredo
		"100::1",          // discard-only
		"fec0::1",         // site-local
	}
	for _, s := range blocked {
		if ip := net.ParseIP(s); !Blocked(ip) {
			t.Errorf("Blocked(%s) = false, want true", s)
		}
	}
	allowed := []string{
		"8.8.8.8", "1.1.1.1", "93.184.216.34", "198.17.255.255", "198.20.0.1", "100.128.0.1",
		"2606:4700:4700::1111", "2001:4860:4860::8888", "2a00:1450:4001::1",
		// globally reachable exceptions inside reserved blocks (codex r1)
		"192.0.0.9", "192.0.0.10", "2001:1::1", "2001:1::2", "2001:1::3",
		"2001:3::1", "2001:4:112::1", "2001:20::1", "2001:30::1",
		// NAT64 of a PUBLIC IPv4 address is that address (codex r1)
		"64:ff9b::808:808",
	}
	for _, s := range allowed {
		if ip := net.ParseIP(s); Blocked(ip) {
			t.Errorf("Blocked(%s) = true, want false (public address)", s)
		}
	}
	// Malformed slices are refused, never judged by predicates that answer
	// false for them (codex r1).
	for _, ip := range []net.IP{nil, {}, {1}, make(net.IP, 15), make(net.IP, 17)} {
		if !Blocked(ip) {
			t.Errorf("Blocked(%d-byte slice) = false, want true", len(ip))
		}
	}
}

// BUG-3660: the provider metadata endpoints we know of, each with the range
// that blocks it. A new provider endpoint goes here first; if no existing
// range covers it, it goes in platformAddresses.
func TestBlocked_CloudMetadataCensus(t *testing.T) {
	for _, c := range []struct{ endpoint, ip string }{
		{"AWS/GCP/Azure/Oracle/DO/Hetzner/OpenStack/Huawei IMDS", "169.254.169.254"},
		{"AWS ECS task credentials", "169.254.170.2"},
		{"AWS EKS pod identity", "169.254.170.23"},
		{"AWS Route 53 resolver", "169.254.169.253"},
		{"AWS IMDS IPv6", "fd00:ec2::254"},
		{"AWS EKS pod identity IPv6", "fd00:ec2::23"},
		{"GCP metadata IPv6", "fd20:ce::254"},
		{"Azure WireServer", "168.63.129.16"},
		{"Azure WireServer, IPv4-mapped", "::ffff:168.63.129.16"},
		{"Azure WireServer, NAT64", "64:ff9b::a83f:8110"},
		{"Alibaba metadata", "100.100.100.200"},
		{"Oracle Cloud IMDS IPv6", "fd00:c1::a9fe:a9fe"},
		{"Oracle Cloud Classic metadata", "192.0.0.192"},
		{"Tencent metadata", "169.254.0.23"},
		{"Scaleway metadata", "169.254.42.42"},
		{"Scaleway metadata IPv6", "fd00:42::42"},
	} {
		if !Blocked(net.ParseIP(c.ip)) {
			t.Errorf("%s (%s): Blocked = false, want true", c.endpoint, c.ip)
		}
	}
	// The WireServer entry is one address, not Microsoft's public space.
	for _, s := range []string{"168.63.129.15", "168.63.129.17"} {
		if Blocked(net.ParseIP(s)) {
			t.Errorf("Blocked(%s) = true, want false (neighbour of WireServer)", s)
		}
	}
}
