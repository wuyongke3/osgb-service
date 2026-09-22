package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const testOSGConv = `E:/OpenSceneGraph-3.6.5-VC2022-64-2025-04/OpenSceneGraph-3.6.5-VC2022-64-2025-04/bin/osgconv.exe`

func makeTestTexture(t *testing.T, dir string) string {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	texPath := filepath.Join(dir, "test_tex.png")
	if err := os.WriteFile(texPath, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return "test_tex.png"
}

func writeTestTexturedOBJ(t *testing.T, path string) {
	dir := filepath.Dir(path)
	texName := makeTestTexture(t, dir)
	mtl := "newmtl test_mat\nKa 1 1 1\nKd 1 1 1\nmap_Kd " + texName + "\n"
	if err := os.WriteFile(filepath.Join(dir, "test.mtl"), []byte(mtl), 0644); err != nil {
		t.Fatal(err)
	}
	obj := "mtllib test.mtl\n" +
		"usemtl test_mat\n" +
		"v 0 0 0\nv 1 0 0\nv 1 1 0\nv 0 1 0\nv 0.5 0.5 1\n" +
		"vt 0 0\nvt 1 0\nvt 1 1\nvt 0 1\nvt 0.5 0.5\n" +
		"f 1/1 2/2 3/3\nf 1/1 3/3 4/4\nf 1/1 2/2 5/5\nf 2/2 3/3 5/5\nf 3/3 4/4 5/5\nf 4/4 1/1 5/5\n"
	if err := os.WriteFile(path, []byte(obj), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestParseObjTextured(t *testing.T) {
	dir := t.TempDir()
	objPath := filepath.Join(dir, "test.obj")
	writeTestTexturedOBJ(t, objPath)
	mesh, err := parseObjTextured(objPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(mesh.vertices) == 0 || len(mesh.triangles) == 0 {
		t.Fatalf("no geometry: verts=%d tris=%d", len(mesh.vertices), len(mesh.triangles))
	}
	if len(mesh.materials) != 1 || mesh.materials[0] != "test_mat" {
		t.Fatalf("unexpected materials: %v", mesh.materials)
	}
	hasUV := false
	for _, v := range mesh.vertices {
		if v.u != 0 || v.v != 0 {
			hasUV = true
			break
		}
	}
	if !hasUV {
		t.Fatal("no non-zero UV found")
	}
	t.Logf("parsed: %d verts, %d tris, %d mats", len(mesh.vertices), len(mesh.triangles), len(mesh.materials))
}

func TestSimplifyMesh(t *testing.T) {
	dir := t.TempDir()
	objPath := filepath.Join(dir, "test.obj")
	writeTestTexturedOBJ(t, objPath)
	mesh, err := parseObjTextured(objPath)
	if err != nil {
		t.Fatal(err)
	}
	simplified := simplifyMesh(mesh, 2)
	if len(simplified.triangles) > len(mesh.triangles) {
		t.Fatalf("simplification increased faces: %d -> %d", len(mesh.triangles), len(simplified.triangles))
	}
	t.Logf("simplified: %d -> %d tris", len(mesh.triangles), len(simplified.triangles))
}

// makeGridMesh builds a regular subdivided quad in the XY plane so that
// simplification has enough faces to actually reduce.
func makeGridMesh(n int) *objMesh {
	mesh := &objMesh{materials: []string{"m"}}
	for y := 0; y <= n; y++ {
		for x := 0; x <= n; x++ {
			mesh.vertices = append(mesh.vertices,
				objVertex{x: float64(x), y: float64(y), z: 0,
					u: float64(x) / float64(n), v: float64(y) / float64(n)})
		}
	}
	at := func(x, y int) int { return y*(n+1) + x }
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			mesh.triangles = append(mesh.triangles,
				objTriangle{a: at(x, y), b: at(x+1, y), c: at(x+1, y+1), mat: 0},
				objTriangle{a: at(x, y), b: at(x+1, y+1), c: at(x, y+1), mat: 0})
		}
	}
	return mesh
}

// boundsOf reports the axis-aligned bounds of a mesh's vertex positions.
func boundsOf(mesh *objMesh) (minX, minY, maxX, maxY float64) {
	b := computeBounds(mesh)
	return b.minX, b.minY, b.maxX, b.maxY
}

// TestSimplifyPreservesBoundary is the regression test for crack prevention:
// with a boundary tolerance, the simplified mesh must keep exactly the same
// outer silhouette as the source so that adjacent tiles still line up.
func TestSimplifyPreservesBoundary(t *testing.T) {
	mesh := makeGridMesh(16)
	srcMinX, srcMinY, srcMaxX, srcMaxY := boundsOf(mesh)

	tolerance := tileBoundaryTolerance(mesh)
	if tolerance <= 0 {
		t.Fatalf("tileBoundaryTolerance returned %v, want > 0", tolerance)
	}

	simplified := simplifyMeshPreservingBoundary(mesh, len(mesh.triangles)/8, tolerance)
	if len(simplified.triangles) >= len(mesh.triangles) {
		t.Fatalf("expected face reduction, got %d -> %d",
			len(mesh.triangles), len(simplified.triangles))
	}

	minX, minY, maxX, maxY := boundsOf(simplified)
	const eps = 1e-9
	if math.Abs(minX-srcMinX) > eps || math.Abs(minY-srcMinY) > eps ||
		math.Abs(maxX-srcMaxX) > eps || math.Abs(maxY-srcMaxY) > eps {
		t.Fatalf("boundary moved: src=[%v,%v]-[%v,%v] got=[%v,%v]-[%v,%v]",
			srcMinX, srcMinY, srcMaxX, srcMaxY, minX, minY, maxX, maxY)
	}
}

// TestSimplifyWithoutToleranceMovesBoundary documents the behaviour that the
// tolerance parameter exists to fix: with tolerance 0 (the legacy behaviour)
// clustering is free to pull the silhouette inwards.
func TestSimplifyWithoutToleranceMovesBoundary(t *testing.T) {
	mesh := makeGridMesh(16)
	srcMinX, srcMinY, srcMaxX, srcMaxY := boundsOf(mesh)

	simplified := simplifyMeshPreservingBoundary(mesh, len(mesh.triangles)/8, 0)
	if len(simplified.triangles) >= len(mesh.triangles) {
		t.Fatalf("expected face reduction, got %d -> %d",
			len(mesh.triangles), len(simplified.triangles))
	}

	minX, minY, maxX, maxY := boundsOf(simplified)
	if minX == srcMinX && minY == srcMinY && maxX == srcMaxX && maxY == srcMaxY {
		t.Log("note: unbounded clustering happened to preserve bounds on this grid")
		return
	}
	t.Logf("unbounded bounds src=[%v,%v]-[%v,%v] got=[%v,%v]-[%v,%v] (expected to differ)",
		srcMinX, srcMinY, srcMaxX, srcMaxY, minX, minY, maxX, maxY)
}

// TestSimplifiedMeshStillTiles verifies that boundary preservation does not
// break the invariant relied on by tileTriangles: every face survives into
// exactly one tile.
func TestSimplifiedMeshStillTiles(t *testing.T) {
	mesh := makeGridMesh(16)
	simplified := simplifyMeshPreservingBoundary(mesh, len(mesh.triangles)/8, tileBoundaryTolerance(mesh))
	if len(simplified.triangles) == 0 {
		t.Fatal("simplification removed every face")
	}
	tiles := tileTriangles(simplified, computeBounds(simplified), 4, 4)
	total := 0
	for _, tl := range tiles {
		total += len(tl)
	}
	if total != len(simplified.triangles) {
		t.Fatalf("tiles lost faces: got %d want %d", total, len(simplified.triangles))
	}
}

// TestTileBoundaryToleranceDegenerate covers the guards for flat or empty
// meshes, which must disable locking rather than divide by zero.
func TestTileBoundaryToleranceDegenerate(t *testing.T) {
	if got := tileBoundaryTolerance(&objMesh{}); got != 0 {
		t.Fatalf("empty mesh tolerance = %v, want 0", got)
	}
	single := &objMesh{vertices: []objVertex{{x: 5, y: 5, z: 5}}}
	if got := tileBoundaryTolerance(single); got != 0 {
		t.Fatalf("single-point mesh tolerance = %v, want 0", got)
	}
}

// TestTileZSpreadFlatAndSteep verifies the diagnostic that flags when a flat XY
// grid is a poor fit for the terrain.
func TestTileZSpreadFlatAndSteep(t *testing.T) {
	flat := makeGridMesh(16) // all z == 0
	b := computeBounds(flat)
	tiles := tileTriangles(flat, b, 4, 4)
	if got := tileZSpread(flat, b, tiles, 4, 4); got != 0 {
		t.Fatalf("flat mesh spread = %v, want 0", got)
	}

	// Same footprint, but with a tall vertical wall in one tile: give one
	// corner vertex a large Z so that tile's Z extent dominates its footprint.
	steep := makeGridMesh(16)
	steep.vertices[0].z = 100
	steep.vertices[1].z = 100
	sb := computeBounds(steep)
	stiles := tileTriangles(steep, sb, 4, 4)
	got := tileZSpread(steep, sb, stiles, 4, 4)
	if got <= 1 {
		t.Fatalf("steep mesh spread = %v, want > 1", got)
	}
	t.Logf("steep spread ratio = %.2f", got)
}

// TestTileZSpreadEmptyInputs guards the degenerate paths.
func TestTileZSpreadEmptyInputs(t *testing.T) {
	mesh := makeGridMesh(4)
	b := computeBounds(mesh)
	if got := tileZSpread(mesh, b, nil, 0, 0); got != 0 {
		t.Fatalf("zero grid spread = %v, want 0", got)
	}
}

// TestTileNamePreservesLegacyFormat pins the Smart3D-style naming that shipped
// before the sign/width fix, so existing tile trees keep the same directory
// layout and sorting order.
func TestTileNamePreservesLegacyFormat(t *testing.T) {
	cases := []struct {
		row, col int
		want     string
	}{
		{0, 0, "Tile_+000_+000"},
		{3, 12, "Tile_+003_+012"},
		{15, 15, "Tile_+015_+015"},
		{999, 5, "Tile_+999_+005"},
	}
	width := tileNameWidth(16, 16)
	if width != 3 {
		t.Fatalf("tileNameWidth(16,16) = %d, want 3", width)
	}
	for _, c := range cases {
		if got := tileName(c.row, c.col, width); got != c.want {
			t.Errorf("tileName(%d,%d) = %q, want %q", c.row, c.col, got, c.want)
		}
	}
}

// TestTileNameHandlesOverflowAndNegative covers the two defects the old
// hard-coded "Tile_+%03d_+%03d" format had: a grid wider than 999 indices
// overflowed the field and broke name sorting, and a negative origin produced
// a misleading "+" sign.
func TestTileNameHandlesOverflowAndNegative(t *testing.T) {
	width := tileNameWidth(1200, 1200)
	if width != 4 {
		t.Fatalf("tileNameWidth(1200,1200) = %d, want 4", width)
	}
	if got := tileName(1000, 2, width); got != "Tile_+1000_+0002" {
		t.Fatalf("overflow name = %q, want %q", got, "Tile_+1000_+0002")
	}
	if got := tileName(-12, -3, 3); got != "Tile_-012_-003" {
		t.Fatalf("negative name = %q, want %q", got, "Tile_-012_-003")
	}
}

// TestTileNameSortsNumerically is the property that matters for a tree walk:
// string order must agree with (row, col) order, including across the width
// boundary that used to break sorting.
func TestTileNameSortsNumerically(t *testing.T) {
	// Deliberately straddle the 3-digit boundary that exposed the bug.
	width := tileNameWidth(1002, 2)
	var names []string
	for row := 998; row <= 1001; row++ {
		for col := 0; col <= 1; col++ {
			names = append(names, tileName(row, col, width))
		}
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for i := range names {
		if names[i] != sorted[i] {
			t.Fatalf("name order not stable at %d:\n got %v\nwant %v", i, names, sorted)
		}
	}
}

// TestTileNameWidthFloor pins the minimum width so ordinary grids keep the
// legacy three-digit layout.
func TestTileNameWidthFloor(t *testing.T) {
	for _, c := range [][2]int{{1, 1}, {16, 16}, {100, 100}} {
		if got := tileNameWidth(c[0], c[1]); got != 3 {
			t.Errorf("tileNameWidth(%d,%d) = %d, want 3", c[0], c[1], got)
		}
	}
}

func TestTileTriangles(t *testing.T) {
	dir := t.TempDir()
	objPath := filepath.Join(dir, "test.obj")
	writeTestTexturedOBJ(t, objPath)
	mesh, err := parseObjTextured(objPath)
	if err != nil {
		t.Fatal(err)
	}
	b := computeBounds(mesh)
	tiles := tileTriangles(mesh, b, 2, 2)
	total := 0
	for _, tl := range tiles {
		total += len(tl)
	}
	if total != len(mesh.triangles) {
		t.Fatalf("tiles lost faces: got %d want %d", total, len(mesh.triangles))
	}
}

func TestExtractSubMesh(t *testing.T) {
	dir := t.TempDir()
	objPath := filepath.Join(dir, "test.obj")
	writeTestTexturedOBJ(t, objPath)
	mesh, err := parseObjTextured(objPath)
	if err != nil {
		t.Fatal(err)
	}
	triIdx := []int{0, 1, 2}
	sub := extractSubMesh(mesh, triIdx)
	if len(sub.triangles) != 3 {
		t.Fatalf("sub mesh faces: got %d want 3", len(sub.triangles))
	}
	if len(sub.vertices) == 0 {
		t.Fatal("sub mesh has no vertices")
	}
}

func TestWriteTexturedOBJ(t *testing.T) {
	dir := t.TempDir()
	objPath := filepath.Join(dir, "test.obj")
	writeTestTexturedOBJ(t, objPath)
	mesh, err := parseObjTextured(objPath)
	if err != nil {
		t.Fatal(err)
	}
	texMap := parseMTL(mesh)
	outPath := filepath.Join(dir, "out", "output.obj")
	if err := writeTexturedOBJ(mesh, texMap, outPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatal("output OBJ not created")
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "output.mtl")); err != nil {
		t.Fatal("output MTL not created")
	}
}

func TestBuildSmart3DOSGB(t *testing.T) {
	if !fileExists(testOSGConv) {
		t.Skip("osgconv not available")
	}
	dir := t.TempDir()
	objPath := filepath.Join(dir, "test.obj")
	writeTestTexturedOBJ(t, objPath)
	mesh, err := parseObjTextured(objPath)
	if err != nil {
		t.Fatal(err)
	}
	texMap := parseMTL(mesh)
	outDir := filepath.Join(dir, "output")
	logFn := func(msg string) { t.Log(msg) }
	if err := buildSmart3DOSGB(mesh, texMap, testOSGConv, outDir, 2, 2, logFn); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(outDir, "root.osgb")
	if !fileExists(rootPath) {
		t.Fatal("root.osgb not created")
	}
	stats, err := validateSmart3DTree(testOSGConv, outDir, logFn)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) == 0 {
		t.Fatal("no OSGB validated")
	}
	for _, s := range stats {
		if !s.Valid {
			t.Errorf("tile %s failed validation: %s", s.Name, s.Error)
		}
	}
}

func TestValidateOSGBTile(t *testing.T) {
	if !fileExists(testOSGConv) {
		t.Skip("osgconv not available")
	}
	dir := t.TempDir()
	objPath := filepath.Join(dir, "test.obj")
	writeTestTexturedOBJ(t, objPath)
	osgbPath := filepath.Join(dir, "test.osgb")
	if err := runOSGConv(testOSGConv, objPath, osgbPath); err != nil {
		t.Fatal(err)
	}
	stats, err := validateOSGBTile(testOSGConv, osgbPath, false)

	if err != nil {
		t.Fatal(err)
	}
	if !stats.Valid {
		t.Fatalf("validation failed: %s", stats.Error)
	}
	if stats.Faces == 0 {
		t.Fatal("no faces found in OSGB")
	}
	t.Logf("validated: verts=%d faces=%d geodes=%d plods=%d", stats.Vertices, stats.Faces, stats.Geodes, stats.PagedLODs)
}

func TestPagedLODMetadataSurvivesOSGB(t *testing.T) {
	if !fileExists(testOSGConv) {
		t.Skip("osgconv not available")
	}
	dir := t.TempDir()
	objPath := filepath.Join(dir, "test.obj")
	writeTestTexturedOBJ(t, objPath)
	mesh, err := parseObjTextured(objPath)
	if err != nil {
		t.Fatal(err)
	}
	texMap := parseMTL(mesh)
	outDir := filepath.Join(dir, "output")
	if err := buildSmart3DOSGB(mesh, texMap, testOSGConv, outDir, 1, 1, func(string) {}); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(outDir, "root.osgb")
	rootStats, err := validateOSGBTile(testOSGConv, rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rootStats.Metadata {
		t.Fatalf("root metadata missing")
	}
	var tilePath string
	filepath.WalkDir(filepath.Join(outDir, "Data"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(strings.ToLower(path), ".osgb") {
			tilePath = path
			return filepath.SkipAll
		}
		return nil
	})
	if tilePath == "" {
		t.Fatal("no tile OSGB found")
	}
	stats, err := validateOSGBTile(testOSGConv, tilePath, true)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Metadata {
		t.Fatalf("root metadata missing")
	}
	if !stats.Valid {
		t.Fatalf("root validation failed: %s", stats.Error)
	}
}
