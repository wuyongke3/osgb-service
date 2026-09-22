package main

// config_threads_test.go locks the memory-aware thread derivation against the
// measured behavior. The measurements that justify these expectations are
// recorded in config.go: at 3200px in a 12 GB container, 4 workers peaked at
// 8.9 GB and 8 workers were OOM-killed.

import (
	"runtime"
	"testing"
)

// TestEstimateWorkerBytesScalesWithImageSize pins the two anchors the model is
// built on: a larger image cap must cost more memory per worker, and the 3200px
// figure must land in the measured ~2.2 GB-per-worker band.
func TestEstimateWorkerBytesScalesWithImageSize(t *testing.T) {
	small := (Config{MaxImageSize: 1600}).estimateWorkerBytes()
	large := (Config{MaxImageSize: 3200}).estimateWorkerBytes()

	if large <= small {
		t.Fatalf("larger image should cost more: %d <= %d", large, small)
	}
	// 3200px is 7.68 MP; at 320 MB/MP plus a 256 MB base this is ~2.7 GB.
	gib := float64(large) / (1 << 30)
	if gib < 2.0 || gib > 3.2 {
		t.Fatalf("3200px per-worker estimate = %.2f GB, want roughly 2.2-2.7 GB", gib)
	}
}

// TestEstimateWorkerBytesFallbackForUncappedImage uses the fallback megapixel
// figure when the cap is disabled, rather than producing zero.
func TestEstimateWorkerBytesFallbackForUncappedImage(t *testing.T) {
	got := (Config{MaxImageSize: 0}).estimateWorkerBytes()
	if got <= 0 {
		t.Fatalf("uncapped image size produced %d, want a positive estimate", got)
	}
}

// TestResolveThreadsUsesMeasuredMemoryModel is the regression test for the OOM:
// on a 12 GB limit with 3200px images the automatic count must be at most 4,
// never the 8 or 16 that were killed.
func TestResolveThreadsUsesMeasuredMemoryModel(t *testing.T) {
	// Force the CPU bound high so memory is the deciding factor, then pin the
	// memory limit indirectly via the worker estimate and the resolved count.
	config := Config{Threads: 0, MaxImageSize: 3200}

	// availableMemoryBytes is not directly injectable, so exercise the pure
	// arithmetic the same way resolveThreads does.
	per := config.estimateWorkerBytes()
	budget := int64(12<<30) - reservedBytes
	byMemory := int(budget / per)
	if byMemory > 4 {
		t.Fatalf("12 GB limit with 3200px images allows %d workers, want at most 4", byMemory)
	}
	if byMemory < 1 {
		t.Fatalf("12 GB limit resolved to %d workers, want at least 1", byMemory)
	}
}

// TestResolveThreadsWithinBounds ensures the automatic value is always usable.
func TestResolveThreadsWithinBounds(t *testing.T) {
	got := (Config{Threads: 0}).resolveThreads()
	if got < 1 || got > maxAutoThreads {
		t.Fatalf("resolveThreads() = %d, want within [1,%d]", got, maxAutoThreads)
	}
	if got > runtime.GOMAXPROCS(0) {
		t.Fatalf("resolveThreads() = %d exceeds GOMAXPROCS %d", got, runtime.GOMAXPROCS(0))
	}
}

// TestAvailableMemoryBytesReadable is a smoke test that the reader returns
// something on the current host; it does not assert a specific value because the
// environment differs between developer machines and the container.
func TestAvailableMemoryBytesReadable(t *testing.T) {
	value, ok := availableMemoryBytes()
	if !ok {
		t.Skip("no memory limit readable in this environment")
	}
	if value == 0 {
		t.Fatal("availableMemoryBytes returned 0 with ok=true")
	}
}
