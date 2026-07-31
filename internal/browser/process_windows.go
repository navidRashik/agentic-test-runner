//go:build windows

package browser

import "time"

const processTrackingSupported = false

func findChromePIDsForDataDir(userDataDir string) ([]int, error) {
	return nil, nil
}

func killProcessTree(pid int) {}

func waitAllGone(pids []int, timeout time.Duration) bool {
	return true
}

func processAlive(pid int) bool {
	return false
}
