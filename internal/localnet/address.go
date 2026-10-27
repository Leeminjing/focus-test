// Package localnet defines the local transport boundary shared by both binaries.
package localnet

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
)

// ValidateAddress accepts literal loopback IPs only, including an ephemeral port.
func ValidateAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("address must contain a literal loopback IP and port")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return errors.New("only literal loopback addresses are supported")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return errors.New("port must be between 0 and 65535")
	}
	return nil
}
