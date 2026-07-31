//go:build !windows

package browser

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processTrackingSupported indicates whether this platform's implementation
// of findChromePIDsForDataDir/killProcessTree/processAlive is functional.
// Gates startProcessMonitor and Close()'s kill-verify so unsupported
// platforms (Windows, for now) don't log false "unexpected exit" crashes.
const processTrackingSupported = true

// findChromePIDsForDataDir returns the PIDs of ORPHANED browser processes
// (reparented to PID 1) whose command line references the given
// --user-data-dir. Helper processes carry --type=<renderer|gpu-process|...>
// and are filtered out; only the top-level browser process owns the profile.
//
// Only orphans (ppid == 1) are returned. A live process still holding the
// profile is not reaped -- Chrome's own singleton lock already produces a
// clear "profile in use" error in that case, and killing someone else's
// live browser (e.g. a concurrent ATR session) would be a serious
// regression. Note: under a subreaper (e.g. systemd --user on some Linux
// setups) orphans reparent to the subreaper rather than PID 1, so this
// check degrades safely to "skip" rather than over-reaping.
func findChromePIDsForDataDir(userDataDir string) ([]int, error) {
	out, err := exec.Command("ps", "-Ao", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	needle := "--user-data-dir=" + userDataDir

	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !argMatches(trimmed, needle) || strings.Contains(trimmed, "--type=") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		if ppid != 1 {
			// Held by a live process; do not reap.
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// argMatches reports whether line contains needle as a whole command-line
// argument (i.e. followed by end-of-string or a space), not merely as a
// substring. This prevents "--user-data-dir=/x/opal-session" from matching
// a process actually running "--user-data-dir=/x/opal-session-2".
func argMatches(line, needle string) bool {
	i := strings.Index(line, needle)
	if i < 0 {
		return false
	}
	rest := line[i+len(needle):]
	return rest == "" || strings.HasPrefix(rest, " ")
}

// killProcessTree terminates pid and its descendants, escalating to SIGKILL.
// We do NOT signal the process group: go-rod does not set Setpgid, so the
// browser shares the daemon's process group and a negative-PID kill would
// take out ATR itself.
func killProcessTree(pid int) {
	if pid <= 1 || pid == os.Getpid() {
		return
	}

	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		waitGone(pid, 2*time.Second)
	}

	// Re-enumerate children after the parent is confirmed dead/killed rather
	// than using a pre-kill snapshot, to avoid signalling a PID that has
	// since been recycled by an unrelated process.
	for _, c := range childPIDs(pid) {
		if c > 1 && c != os.Getpid() {
			_ = syscall.Kill(c, syscall.SIGKILL)
		}
	}
}

// waitAllGone blocks until every pid in pids is no longer alive, or timeout
// elapses. Returns true only if all pids exited within the timeout.
func waitAllGone(pids []int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allGone := true
		for _, pid := range pids {
			if processAlive(pid) {
				allGone = false
				break
			}
		}
		if allGone {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range pids {
		if processAlive(pid) {
			return false
		}
	}
	return true
}

func waitGone(pid int, timeout time.Duration) {
	waitAllGone([]int{pid}, timeout)
}

func childPIDs(parent int) []int {
	out, err := exec.Command("ps", "-Ao", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	var kids []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 == nil && err2 == nil && ppid == parent {
			kids = append(kids, pid)
		}
	}
	return kids
}

// processAlive reports whether pid refers to a live process this user can
// signal. Note: returns true for zombies (defunct-but-not-yet-reaped) since
// Signal(0) succeeds against them, and returns false for another user's
// live process (EPERM) -- both are benign edge cases for our use here.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
