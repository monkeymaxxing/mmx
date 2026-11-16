//go:build !linux

package facts

import (
	"fmt"
	"net"
)

// The controller can run on macOS. Linux observations are only collected on
// managed nodes; local engine tests keep unrelated resources usable here.
func mountCapacity(string) (int64, int64, error) {
	return 0, 0, fmt.Errorf("filesystem capacity requires a Linux node")
}
func defaultRoutes([]net.Interface) ([]any, error) {
	return nil, fmt.Errorf("routing facts require a Linux node")
}
