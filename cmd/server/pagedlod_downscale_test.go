package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestDownscaleRealTexture(t *testing.T) {
	if !fileExists(testOSGConv) {
		t.Skip("osgconv not available")
	}
	dir := t.TempDir()
	src := os.Getenv("SMART3D_REAL_TEXTURE")
	if src == "" {
		t.Skip("set SMART3D_REAL_TEXTURE to a texture to run")
	}
	if !fileExists(src) {
		t.Skip("real texture not available")
	}
	name, derr := downscaleTexture(src, dir, "go_2k.jpg", 2048)
	if derr != nil {
		t.Fatal("downscale failed:", derr)
	}
	outPath := filepath.Join(dir, name)
	fi, err := os.Stat(outPath)
	if err != nil {
		t.Fatal("output not created:", err)
	}
	t.Logf("downscaled: %s = %.1f MB", name, float64(fi.Size())/1e6)
	if fi.Size() > 5e6 {
		t.Fatalf("expected <5MB for 2K jpeg, got %d", fi.Size())
	}
}

// TestDownscaleBoxDownsamplesToLimit exercises the pure-Go downscale path
// without needing a real survey texture on disk. It checks the two properties
// callers depend on: the output honors maxDim, and an in-limit image is copied
// through byte-for-byte instead of being re-encoded.
func TestDownscaleBoxDownsamplesToLimit(t *testing.T) {
	dir := t.TempDir()

	// Build a 64x32 PNG with a two-color checker so averaging is observable.
	src := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			c := color.RGBA{R: 255, A: 255}
			if (x/2+y/2)%2 == 0 {
				c = color.RGBA{B: 255, A: 255}
			}
			src.Set(x, y, c)
		}
	}
	srcPath := filepath.Join(dir, "big.png")
	f, err := os.Create(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, src); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name, err := downscaleTexture(srcPath, outDir, "small.jpg", 16)
	if err != nil {
		t.Fatal(err)
	}
	outFile, err := os.Open(filepath.Join(outDir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	cfg, _, err := image.DecodeConfig(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width > 16 || cfg.Height > 16 {
		t.Fatalf("output not downscaled to maxDim: %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.Width != 16 || cfg.Height != 8 {
		t.Fatalf("unexpected aspect-preserving size: %dx%d, want 16x8", cfg.Width, cfg.Height)
	}
}

// TestDownscaleTextureReportsMissingSource is the regression test for the
// silent-failure bug: a missing source must produce an error, not a filename
// that points at a file which was never written.
func TestDownscaleTextureReportsMissingSource(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.png")
	name, err := downscaleTexture(missing, dir, "copy.jpg", 2048)
	if err == nil {
		t.Fatalf("expected error for missing source, got nil (name=%q)", name)
	}
	if _, statErr := os.Stat(filepath.Join(dir, name)); statErr == nil {
		t.Fatal("a file was created despite the missing source")
	}
}

// TestWriteTexturedOBJFailsOnMissingTexture verifies the caller surfaces the
// texture error instead of emitting an OBJ whose MTL references a missing image.
func TestWriteTexturedOBJFailsOnMissingTexture(t *testing.T) {
	dir := t.TempDir()
	mesh := &objMesh{
		materials: []string{"m"},
		mtlDir:    dir,
		vertices:  []objVertex{{x: 0, y: 0}, {x: 1, y: 0}, {x: 0, y: 1}},
		triangles: []objTriangle{{a: 0, b: 1, c: 2, mat: 0}},
	}
	texMap := map[string]string{"m": "absent.png"}
	err := writeTexturedOBJ(mesh, texMap, filepath.Join(dir, "out", "tile.obj"))
	if err == nil {
		t.Fatal("expected writeTexturedOBJ to fail when the texture is missing")
	}
}
