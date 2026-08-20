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
	statFieldUTime     = 14
	statFieldSTime     = 15
	statFieldThreads   = 20
	statFieldStartTime = 22
	statFieldRSS       = 24
)

// userHZ is the kernel clock tick the times in /proc/<pid>/stat are counted in.
//
// It is a compile-time kernel constant, fixed at 100 on every mainstream Linux
// build; reading it properly needs sysconf(_SC_CLK_TCK), which is a cgo call
// this daemon deliberately does not make.
const userHZ = 100

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

// ProcessUsage samples the resources a running process is using.
//
// The whole sample comes from a single read of /proc/<pid>/stat, so the start
// time it is verified against belongs to the same snapshot as the numbers: a
// PID recycled between two reads could otherwise be reported as the execution.
//
// The sample covers the process itself, not its whole group: children are
// counted only once the process has reaped them, which is what utime/stime
// already accumulate.
func ProcessUsage(pid int, startTime uint64) (Usage, error) {
	fields, err := processStat(pid)
	if err != nil {
		return Usage{}, err
	}

	sampled, err := statValue(fields, statFieldStartTime, pid)
	if err != nil {
		return Usage{}, err
	}

	if startTime != 0 && sampled != startTime {
		return Usage{}, fmt.Errorf("pid %d is no longer the fingerprinted process", pid)
	}

	utime, err := statValue(fields, statFieldUTime, pid)
	if err != nil {
		return Usage{}, err
	}

	stime, err := statValue(fields, statFieldSTime, pid)
	if err != nil {
		return Usage{}, err
	}

	threads, err := statValue(fields, statFieldThreads, pid)
	if err != nil {
		return Usage{}, err
	}

	rssPages, err := statValue(fields, statFieldRSS, pid)
	if err != nil {
		return Usage{}, err
	}

	return Usage{
		CPUSeconds: float64(utime+stime) / userHZ,
		//nolint:gosec // an RSS in pages and a thread count never approach the int64 limit
		RSSBytes: int64(rssPages) * int64(os.Getpagesize()),
		//nolint:gosec // see above
		Threads: int(threads),
	}, nil
}

// statField reads one numeric field from /proc/<pid>/stat.
func statField(pid, field int) (uint64, error) {
	fields, err := processStat(pid)
	if err != nil {
		return 0, err
	}

	return statValue(fields, field, pid)
}

// processStat returns the fields of /proc/<pid>/stat from the third one on.
func processStat(pid int) ([]string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, fmt.Errorf("reading stat of pid %d: %w", pid, err)
	}

	// The comm field is parenthesised and may contain spaces or parentheses, so
	// parsing starts after its closing parenthesis.
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return nil, errors.New("malformed /proc stat entry")
	}

	return strings.Fields(string(raw[end+1:])), nil
}

// statValue picks one 1-indexed field out of a parsed stat line.
func statValue(fields []string, field, pid int) (uint64, error) {
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
