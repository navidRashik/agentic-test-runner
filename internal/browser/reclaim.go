package browser

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// reclaimProfile makes a persisted user-data-dir safe to launch against: kill
// any ORPHANED browser still holding it (see findChromePIDsForDataDir for
// the orphan-only definition), drop the resulting stale singleton lock, and
// clear Chrome's crash flag so startup takes the normal code path.
func reclaimProfile(userDataDir string) error {
	if !processTrackingSupported {
		return nil
	}

	pids, err := findChromePIDsForDataDir(userDataDir)
	if err != nil {
		return fmt.Errorf("scan for orphaned browsers: %w", err)
	}
	for _, pid := range pids {
		log.Printf("[browser] reaping orphaned browser pid=%d holding %s", pid, userDataDir)
		killProcessTree(pid)
	}
	if len(pids) > 0 && !waitAllGone(pids, 2*time.Second) {
		// Don't touch profile files while something might still be writing
		// to them; leave cleanup for the next launch attempt.
		return fmt.Errorf("browser still holding %s after reap attempt; skipping profile reset", userDataDir)
	}

	clearStaleSingletonLocks(userDataDir)
	return resetProfileExitState(userDataDir)
}

// clearStaleSingletonLocks removes Chrome's profile-lock artifacts. Chrome's
// ProcessSingleton treats a dangling SingletonLock/SingletonSocket pair as a
// live peer and takes an unusual hand-off path at startup. Only safe to call
// once no live process owns the profile (reclaimProfile only reaches here
// after confirming the orphan(s) it killed have actually exited).
func clearStaleSingletonLocks(userDataDir string) {
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		p := filepath.Join(userDataDir, name)
		if _, err := os.Lstat(p); err == nil {
			if err := os.Remove(p); err != nil {
				log.Printf("[browser] could not remove stale %s: %v", name, err)
			}
		}
	}
}

// resetProfileExitState rewrites Chrome's crash bookkeeping. After a SIGSEGV
// Chrome leaves profile.exit_type="Crashed", which sends the next launch down
// the session-restore / profile-recovery path instead of normal startup.
func resetProfileExitState(userDataDir string) error {
	prefsPath := filepath.Join(userDataDir, "Default", "Preferences")
	raw, err := os.ReadFile(prefsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // fresh profile
		}
		return err
	}

	var prefs map[string]any
	if err := json.Unmarshal(raw, &prefs); err != nil {
		return fmt.Errorf("parse %s: %w", prefsPath, err)
	}

	profile, ok := prefs["profile"].(map[string]any)
	if !ok {
		profile = map[string]any{}
		prefs["profile"] = profile
	}
	if profile["exit_type"] == "Normal" && profile["exited_cleanly"] == true {
		return nil
	}
	log.Printf("[browser] clearing crash flag on %s (was exit_type=%v)", prefsPath, profile["exit_type"])
	profile["exit_type"] = "Normal"
	profile["exited_cleanly"] = true

	updated, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	tmp := prefsPath + ".atr-tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(updated); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, prefsPath)
}
