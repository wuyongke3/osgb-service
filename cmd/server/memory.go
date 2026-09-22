package main

// memory.go reads the memory budget the service should plan against.
//
// Inside a container the meaningful number is the cgroup limit, not the host's
// total RAM: a 12 GB container on a 32 GB host must size its worker count for
// 12 GB. Reading the host figure instead is what allowed an earlier version to
// choose a parallelism that was OOM-killed.
//
// Everything here is best effort. If no figure can be read the caller keeps its
// conservative default rather than assuming unlimited memory.

import (
	"os"
	"strconv"
	"strings"
)

// availableMemoryBytes reports the memory limit that applies to this process.
//
// The second return value is false when no reliable figure is available, in
// which case callers should not use the first.
func availableMemoryBytes() (uint64, bool) {
	// cgroup v2 (Docker with unified hierarchy, modern systemd hosts).
	if limit, ok := readCgroupLimit("/sys/fs/cgroup/memory.max"); ok {
		return limit, true
	}
	// cgroup v1 (older Docker and many current Kubernetes runtimes).
	if limit, ok := readCgroupLimit("/sys/fs/cgroup/memory/memory.limit_in_bytes"); ok {
		return limit, true
	}
	// Outside a container, fall back to the host's total physical memory.
	if total, ok := hostMemoryBytes(); ok {
		return total, true
	}
	return 0, false
}

// cgroupUnlimited is the sentinel cgroup writes when no limit is set. Some
// runtimes write a very large number instead of the literal string, so any value
// above this threshold is also treated as unlimited.
const cgroupUnlimited = uint64(1) << 62

// readCgroupLimit parses one cgroup memory file.
//
// It returns false for a missing file, an unparseable value, or the "max"
// sentinel that means unlimited, because in all three cases the file tells us
// nothing useful.
func readCgroupLimit(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	text := strings.TrimSpace(string(data))
	if text == "" || text == "max" {
		return 0, false
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, false
	}
	if value == 0 || value >= cgroupUnlimited {
		return 0, false
	}
	return value, true
}
