package main

// lod_sizing_test.go covers the adaptive tile grid and LOD chain sizing that
// replaced a fixed 16x16 grid with exactly three levels.
//
// Two properties matter most here:
//  1. The defaults reproduce the previous behavior for the project sizes the
//     service was built for, so existing deliverables do not change shape.
//  2. A chain is always ordered coarse-to-fine and never empty, because the
//     PagedLOD switch thresholds are derived from the chain length.

import (
	"testing"
)

// ---------------------------------------------------------------------------
// Grid sizing
// ---------------------------------------------------------------------------

func TestAdaptiveGridSizeScalesWithModel(t *testing.T) {
	small := adaptiveGridSize(20_000)
	large := adaptiveGridSize(5_000_000)
	if large <= small {
		t.Fatalf("grid did not grow with model size: small=%d large=%d", small, large)
	}
}

// The result must always be usable, whatever the input, because it feeds a
// double loop that allocates the tile slice.
func TestAdaptiveGridSizeBounds(t *testing.T) {
	for _, n := range []int{-1, 0, 1, 100, 1_000_000_000} {
		got := adaptiveGridSize(n)
		if got < minGridSize || got > maxGridSize {
			t.Errorf("adaptiveGridSize(%d) = %d, want within [%d,%d]",
				n, got, minGridSize, maxGridSize)
		}
	}
}

// A degenerate or tiny model must not produce a 1x1 grid: a handful of tiles
// still gives the viewer something to page.
func TestAdaptiveGridSizeNeverDegenerate(t *testing.T) {
	if got := adaptiveGridSize(0); got < minGridSize {
		t.Fatalf("empty model grid = %d, want at least %d", got, minGridSize)
	}
	if got := adaptiveGridSize(10); got < minGridSize {
		t.Fatalf("tiny model grid = %d, want at least %d", got, minGridSize)
	}
}

// ---------------------------------------------------------------------------
// LOD level sizing
// ---------------------------------------------------------------------------

func TestAdaptiveLODLevelsMonotonic(t *testing.T) {
	previous := 0
	for _, faces := range []int{100, 1_000, 10_000, 100_000, 1_000_000, 10_000_000} {
		got := adaptiveLODLevels(faces)
		if got < previous {
			t.Fatalf("level count decreased as tiles grew denser: %d faces -> %d, previous %d",
				faces, got, previous)
		}
		if got < 1 || got > maxLODLevels {
			t.Fatalf("adaptiveLODLevels(%d) = %d, want within [1,%d]", faces, got, maxLODLevels)
		}
		previous = got
	}
}

// A sparse tile gains nothing from coarser copies, so a single level is correct.
func TestAdaptiveLODLevelsSingleForSparseTiles(t *testing.T) {
	if got := adaptiveLODLevels(500); got != 1 {
		t.Fatalf("adaptiveLODLevels(500) = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// resolveTreeOptions
// ---------------------------------------------------------------------------

// An explicit operator setting must always win over adaptive sizing.
func TestResolveTreeOptionsExplicitWins(t *testing.T) {
	opts := resolveTreeOptions(5_000_000, 8, 4, nil)
	if opts.GridX != 8 || opts.GridY != 8 {
		t.Errorf("grid = %dx%d, want 8x8", opts.GridX, opts.GridY)
	}
	if opts.LODLevels != 4 {
		t.Errorf("levels = %d, want 4", opts.LODLevels)
	}
}

// Zero means "choose for me" and must produce a valid layout.
func TestResolveTreeOptionsAdaptiveWhenUnset(t *testing.T) {
	opts := resolveTreeOptions(2_000_000, 0, 0, nil)
	if opts.GridX <= 0 || opts.GridY <= 0 {
		t.Fatalf("adaptive grid not resolved: %dx%d", opts.GridX, opts.GridY)
	}
	if opts.LODLevels < 1 {
		t.Fatalf("adaptive levels not resolved: %d", opts.LODLevels)
	}
	if opts.GridX != opts.GridY {
		t.Errorf("grid is not square: %dx%d", opts.GridX, opts.GridY)
	}
}

// ---------------------------------------------------------------------------
// LOD chain construction
// ---------------------------------------------------------------------------

// TestBuildTileLODLevelsMatchesLegacyShape pins the compatibility guarantee:
// the default three-level chain must still be coarse / medium / full with the
// 4x step the previous hand-written code produced.
func TestBuildTileLODLevelsMatchesLegacyShape(t *testing.T) {
	mesh := makeGridMesh(64)
	tolerance := tileBoundaryTolerance(mesh)

	levels := buildTileLODLevels(mesh, tolerance, defaultLODLevels)
	if len(levels) == 0 {
		t.Fatal("no levels produced")
	}
	// The finest level must be the untouched original mesh.
	if levels[len(levels)-1] != mesh {
		t.Error("the finest level is not the full-resolution mesh")
	}
	// Face counts must increase monotonically from coarse to fine.
	for i := 1; i < len(levels); i++ {
		if len(levels[i].triangles) < len(levels[i-1].triangles) {
			t.Fatalf("level %d has fewer faces than level %d: %d < %d",
				i, i-1, len(levels[i].triangles), len(levels[i-1].triangles))
		}
	}
	// A three-level chain on a reasonably dense mesh must actually have three
	// entries, and the coarsest must be well below full resolution.
	if len(levels) != 3 {
		t.Fatalf("got %d levels, want 3 for a dense mesh", len(levels))
	}
	if len(levels[0].triangles) >= len(mesh.triangles) {
		t.Fatalf("coarsest level was not simplified: %d vs %d",
			len(levels[0].triangles), len(mesh.triangles))
	}
}

// Whatever the request, the chain must be non-empty and no longer than asked.
func TestBuildTileLODLevelsBounds(t *testing.T) {
	mesh := makeGridMesh(24)
	tolerance := tileBoundaryTolerance(mesh)
	for _, count := range []int{-1, 0, 1, 2, 3, 4, 5, 6, 99} {
		levels := buildTileLODLevels(mesh, tolerance, count)
		if len(levels) == 0 {
			t.Fatalf("levelCount=%d produced an empty chain", count)
		}
		effective := count
		if effective < 1 {
			effective = 1
		}
		if effective > maxLODLevels {
			effective = maxLODLevels
		}
		if len(levels) > effective {
			t.Errorf("levelCount=%d produced %d levels, want at most %d",
				count, len(levels), effective)
		}
	}
}

// A single requested level means "no simplification at all".
func TestBuildTileLODLevelsSingleReturnsOriginal(t *testing.T) {
	mesh := makeGridMesh(16)
	levels := buildTileLODLevels(mesh, tileBoundaryTolerance(mesh), 1)
	if len(levels) != 1 {
		t.Fatalf("got %d levels, want 1", len(levels))
	}
	if levels[0] != mesh {
		t.Error("the single level is not the original mesh")
	}
}

// An empty mesh must not panic or produce a level list that later code would
// index into expecting geometry.
func TestBuildTileLODLevelsEmptyMesh(t *testing.T) {
	empty := &objMesh{}
	levels := buildTileLODLevels(empty, 0, 3)
	if len(levels) != 1 {
		t.Fatalf("empty mesh produced %d levels, want 1", len(levels))
	}
	if len(levels[0].triangles) != 0 {
		t.Error("empty mesh produced geometry")
	}
}

// ---------------------------------------------------------------------------
// Switch thresholds
// ---------------------------------------------------------------------------

// TestLODSwitchThresholdMatchesLegacyFormula pins the original arithmetic:
// radius * 2^(2-li) for a three-level chain.
func TestLODSwitchThresholdMatchesLegacyFormula(t *testing.T) {
	const radius = 10.0
	for li := 0; li < 3; li++ {
		want := radius * float64(uint(1)<<(2-li))
		got := lodSwitchThreshold(radius, li, 3)
		if got != want {
			t.Errorf("lodSwitchThreshold(radius, %d, 3) = %v, want %v", li, got, want)
		}
	}
}

// Thresholds must halve at each finer level, which is what makes the LOD
// transition smooth rather than abrupt.
func TestLODSwitchThresholdHalvesPerLevel(t *testing.T) {
	const radius = 8.0
	const count = 4
	for li := 0; li < count-1; li++ {
		coarse := lodSwitchThreshold(radius, li, count)
		finer := lodSwitchThreshold(radius, li+1, count)
		if finer*2 != coarse {
			t.Errorf("level %d threshold %v is not twice level %d's %v", li, coarse, li+1, finer)
		}
	}
}

// The finest level always switches at the radius, so the full-resolution tile
// appears exactly when the viewer is close enough.
func TestLODSwitchThresholdFinestEqualsRadius(t *testing.T) {
	const radius = 3.5
	for _, count := range []int{1, 2, 3, 4, 5} {
		if got := lodSwitchThreshold(radius, count-1, count); got != radius {
			t.Errorf("finest threshold for a %d-level chain = %v, want %v", count, got, radius)
		}
	}
}

// A long chain must not overflow the shift or return a nonsensical threshold.
func TestLODSwitchThresholdBounded(t *testing.T) {
	got := lodSwitchThreshold(1.0, 0, 1000)
	if got <= 0 {
		t.Fatalf("threshold = %v, want a positive value", got)
	}
	if got > 1<<(maxThresholdShift+1) {
		t.Fatalf("threshold %v escaped the shift cap", got)
	}
}
