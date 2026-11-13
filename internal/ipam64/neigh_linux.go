//go:build linux

package ipam64

import (
	"net/netip"
	"syscall"
)

// neighbors liest die Nachbartabelle des Hosts (IPv4) per Netlink.
func neighbors() (map[netip.Addr]Neighbor, error) {
	b, err := syscall.NetlinkRIB(syscall.RTM_GETNEIGH, syscall.AF_INET)
	if err != nil {
		return nil, err
	}
	return parseNeigh(b), nil
}
