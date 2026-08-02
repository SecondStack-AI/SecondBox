//go:build !linux

package deployconfig

import "fmt"

func filesystemDevice(string) (uint64, error) {
	return 0, fmt.Errorf("same-host Runner filesystem validation requires Linux")
}
