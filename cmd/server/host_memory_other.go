//go:build !windows

package main

// host_memory_other.go reads total physical memory on Linux and other Unix
// systems. It must stay free of cgo so the binary remains static, which is what
// lets the image run on a minimal base without glibc surprises.

import (
	"os"
	"strconv"
	"strings"
)

// hostMemoryBytes returns total physical memory by reading /proc/meminfo.
//
// This is the host figure. Inside a container the cgroup limit is the value that
// matters, and availableMemoryBytes consults that first.
func hostMemoryBytes() (uint64, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		// /proc/meminfo reports kibibytes.
		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || kib == 0 {
			return 0, false
		}
		return kib * 1024, true
	}
	return 0, false
}
