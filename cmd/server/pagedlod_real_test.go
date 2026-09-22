package main

import (
	"os"
	"testing"
)

// TestRealDataSmart3DOSGB runs the full Smart3D PagedLOD pipeline on the real
// textured OBJ produced by OpenMVS. It is skipped unless SMART3D_REAL_TEST
// is set to 1 and the model exists.
func TestRealDataSmart3DOSGB(t *testing.T) {
	if os.Getenv("SMART3D_REAL_TEST") != "1" {
		t.Skip("set SMART3D_REAL_TEST=1 to run")
	}
	objPath := os.Getenv("SMART3D_REAL_OBJ")
	if objPath == "" {
		t.Skip("set SMART3D_REAL_OBJ to a textured OBJ to run")
	}
	if !fileExists(objPath) {
		t.Skipf("model not found: %s", objPath)
	}
	mesh, err := parseObjTextured(objPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("parsed: %d verts %d tris %d mats", len(mesh.vertices), len(mesh.triangles), len(mesh.materials))
	texMap := parseMTL(mesh)
	outDir := os.Getenv("SMART3D_REAL_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}
	logFn := func(msg string) { t.Log(msg) }
	if err := buildSmart3DOSGB(mesh, texMap, testOSGConv, outDir, 16, 16, logFn); err != nil {
		t.Fatal(err)
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
	t.Logf("validated %d OSGB tiles", len(stats))
}
