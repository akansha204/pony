//go:build linux

package driver

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func sessionProcessGroups(sessionID int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("scan /proc: %w", err)
	}

	pgids := make(map[int]struct{})

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		raw, err := os.ReadFile(
			fmt.Sprintf("/proc/%d/stat", pid),
		)
		if err != nil {
			continue // process exited between ReadDir and ReadFile
		}

		stat := string(raw)

		closeParen := strings.LastIndex(stat, ")")
		if closeParen < 0 {
			continue
		}

		fields := strings.Fields(stat[closeParen+1:])
		if len(fields) < 4 {
			continue
		}

		if fields[0] == "Z" {
			continue
		}

		pgrp, err1 := strconv.Atoi(fields[2])
		sid, err2 := strconv.Atoi(fields[3])

		if err1 != nil || err2 != nil {
			continue
		}

		if sid == sessionID && pgrp > 0 {
			pgids[pgrp] = struct{}{}
		}
	}

	out := make([]int, 0, len(pgids))
	for pgid := range pgids {
		out = append(out, pgid)
	}

	return out, nil
}

// signalSessionProcessGroups sends sig to every live group in sessionID.The leader usually has PID == SID == PGID; job-control children add groups.
func signalSessionProcessGroups(
	sessionID int,
	sig syscall.Signal,
) error {
	pgids, err := sessionProcessGroups(sessionID)
	if err != nil {
		return err
	}

	var firstErr error

	for _, pgid := range pgids {
		err := syscall.Kill(-pgid, sig)

		if err == nil || errors.Is(err, syscall.ESRCH) {
			continue
		}

		if firstErr == nil {
			firstErr = fmt.Errorf(
				"signal process group %d: %w",
				pgid,
				err,
			)
		}
	}

	return firstErr
}

// waitSessionEmpty polls until no live groups remain in sessionID.It must not rely on h.done: the leader can be reaped while job-control children are still running in their own groups.
func waitSessionEmpty(
	ctxDone <-chan struct{},
	sessionID int,
	timeout time.Duration,
) (bool, error) {
	if timeout <= 0 {
		pgids, err := sessionProcessGroups(sessionID)
		if err != nil {
			return false, err
		}
		return len(pgids) == 0, nil
	}

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	for {
		pgids, err := sessionProcessGroups(sessionID)
		if err != nil {
			return false, err
		}

		if len(pgids) == 0 {
			return true, nil
		}

		select {
		case <-ctxDone:
			return false, nil

		case <-ticker.C:
			// Re-scan at the top of the loop.

		case <-timeoutTimer.C:
			// One final scan prevents reporting a timeout if the last process exited at approximately the same instant.
			pgids, err := sessionProcessGroups(sessionID)
			if err != nil {
				return false, err
			}
			return len(pgids) == 0, nil
		}
	}
}
