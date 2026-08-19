package runner

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// Field positions in /proc/<pid>/stat, 1-indexed as documented in proc(5).
const (
	statFieldPgrp      = 5
	statFieldStartTime = 22
)

// processStartTime returns the process start time in clock ticks since boot.
//
// Together with the PID it forms a fingerprint that survives PID recycling:
// after a daemon restart, a stored PID alone proves nothing, because the kernel
// may have handed that number to an unrelated program (docs/SPEC.md §8).
func processStartTime(pid int) (uint64, error) {
	return statField(pid, statFieldStartTime)
}

// processGroup returns the process group id of a running process.
func processGroup(pid int) (int, error) {
	pgrp, err := statField(pid, statFieldPgrp)
	if err != nil {
		return 0, err
	}

	if pgrp > math.MaxInt32 {
		return 0, fmt.Errorf("process group %d of pid %d is out of range", pgrp, pid)
	}

	return int(pgrp), nil
}

// SameProcess reports whether pid is still the exact process that was
// fingerprinted with startTime. A false result means the original process is
// gone and the PID must not be signalled.
func SameProcess(pid int, startTime uint64) bool {
	if pid <= 0 || startTime == 0 {
		return false
	}

	current, err := processStartTime(pid)
	if err != nil {
		return false
	}

	return current == startTime
}

// Adopt builds a handle for a process the daemon did not start, so that a
// surviving orphan can still be signalled during reconciliation. The process
// cannot be waited on — it is no longer a child of this daemon.
func Adopt(pid int, startTime uint64) (*Handle, error) {
	if !SameProcess(pid, startTime) {
		return nil, fmt.Errorf("pid %d is no longer the fingerprinted process", pid)
	}

	pgid, err := processGroup(pid)
	if err != nil {
		return nil, err
	}

	return &Handle{PID: pid, PIDStartTime: startTime, pgid: pgid}, nil
}

// statField reads one numeric field from /proc/<pid>/stat.
func statField(pid, field int) (uint64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("reading stat of pid %d: %w", pid, err)
	}

	// The comm field is parenthesised and may contain spaces or parentheses, so
	// parsing starts after its closing parenthesis.
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return 0, errors.New("malformed /proc stat entry")
	}

	fields := strings.Fields(string(raw[end+1:]))

	// fields[0] is the third stat field (state), hence the offset.
	index := field - 3
	if index < 0 || index >= len(fields) {
		return 0, fmt.Errorf("stat field %d is missing for pid %d", field, pid)
	}

	value, err := strconv.ParseUint(fields[index], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing stat field %d of pid %d: %w", field, pid, err)
	}

	return value, nil
}
