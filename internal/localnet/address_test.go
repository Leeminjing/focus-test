package localnet_test

import (
	"testing"

	"focus-test-runtime/internal/localnet"
)

func TestLoopbackBoundary(t *testing.T) {
	for _, address := range []string{"127.0.0.1:7337", "[::1]:7337", "127.0.0.1:0"} {
		if err := localnet.ValidateAddress(address); err != nil {
			t.Errorf("rejected %s: %v", address, err)
		}
	}
	for _, address := range []string{":7337", "0.0.0.0:7337", "[::]:7337", "localhost:7337", "192.0.2.1:7337", "127.0.0.1:-1", "127.0.0.1:65536", "127.0.0.1"} {
		if err := localnet.ValidateAddress(address); err == nil {
			t.Errorf("accepted %s", address)
		}
	}
}
