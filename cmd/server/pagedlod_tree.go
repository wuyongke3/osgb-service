package main

// pagedlod_tree.go builds Smart3D-style PagedLOD OSGB trees from a textured
// OBJ model.  Every LOD level is emitted as a textured OBJ, converted to
// OSGB, round-tripped to OSGT, injected into a PagedLOD node, then converted
// back to binary OSGB.  Finally every OSGB is re-read and validated.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type osgbTileStats struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	Size          int64  `json:"size"`
	Valid         bool   `json:"valid"`
	Error         string `json:"error,omitempty"`
	Vertices      int64  `json:"vertices"`
	Faces         int64  `json:"faces"`
	PagedLODs     int64  `json:"paged_lods"`
	Geodes        int64  `json:"geodes"`
	ChildFile     string `json:"child_file,omitempty"`
	Textured      bool   `json:"textured"`
	Metadata      bool   `json:"metadata"`
	TextureImages int64  `json:"texture_images"`
}

// tileBoundaryTolerance returns the distance from a tile's bounding box border
// within which vertices are pinned during simplification. It is derived from the
// tile's own extent so that the locked band stays proportional when the source
// model uses any unit (metres, feet, or OpenMVS local coordinates).
//
// A band that is too wide would lock most vertices and defeat simplification,
// so it is capped at a small fraction of the tile size. Meshes with a
// degenerate extent return 0, which disables locking for that tile.
func tileBoundaryTolerance(mesh *objMesh) float64 {
	b := computeBounds(mesh)
	extent := math.Max(b.maxX-b.minX, math.Max(b.maxY-b.minY, b.maxZ-b.minZ))
	if !(extent > 0) || math.IsNaN(extent) || math.IsInf(extent, 0) {
		return 0
	}
	return extent / 64.0
}

// computeTileCenterRadius returns bounding sphere center & radius of a mesh.
func computeTileCenterRadius(mesh *objMesh) (cx, cy, cz, radius float64) {
	b := computeBounds(mesh)
	cx = (b.minX + b.maxX) / 2
	cy = (b.minY + b.maxY) / 2
	cz = (b.minZ + b.maxZ) / 2
	radius = math.Sqrt(
		(b.maxX-b.minX)*(b.maxX-b.minX)+
			(b.maxY-b.minY)*(b.maxY-b.minY)+
			(b.maxZ-b.minZ)*(b.maxZ-b.minZ)) / 2
	if radius < 0.001 {
		radius = 0.001
	}
	return
}

// extractGeodeBlock extracts the outermost osg::Geode { ... } block.
func extractGeodeBlock(osgtPath string) (string, error) {
	data, err := os.ReadFile(osgtPath)
	if err != nil {
		return "", fmt.Errorf("read OSGT: %w", err)
	}
	return extractGeodeBlockFromString(string(data), osgtPath)
}

func extractGeodeBlockFromString(text, srcName string) (string, error) {
	idx := strings.Index(text, "osg::Geode {")
	if idx < 0 {
		return "", fmt.Errorf("no Geode found in %s", srcName)
	}
	depth := 0
	started := false
	for i := idx; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
			started = true
		case '}':
			depth--
		}
		if started && depth == 0 {
			return text[idx : i+1], nil
		}
	}
	return "", fmt.Errorf("unbalanced braces in Geode of %s", srcName)
}

// renumberUniqueIDs adds offset to every "UniqueID N".
func renumberUniqueIDs(block string, offset int) string {
	var b strings.Builder
	i := 0
	for i < len(block) {
		j := strings.Index(block[i:], "UniqueID ")
		if j < 0 {
			b.WriteString(block[i:])
			break
		}
		b.WriteString(block[i : i+j])
		i += j
		k := i + len("UniqueID ")
		start := k
		for k < len(block) && block[k] >= '0' && block[k] <= '9' {
			k++
		}
		if k > start {
			n := 0
			for _, ch := range block[start:k] {
				n = n*10 + int(ch-'0')
			}
			fmt.Fprintf(&b, "UniqueID %d", n+offset)
			i = k
		} else {
			b.WriteString("UniqueID ")
			i += len("UniqueID ")
		}
	}
	return b.String()
}

// writePagedLODOSGT writes an OSGT with a PagedLOD whose child Geode is
// geodeBlock. childFile=="" means leaf.
func osgMetadataBlock(lines []string) string {
	var b strings.Builder
	b.WriteString("      UserDataContainer TRUE {\n")
	b.WriteString("        osg::DefaultUserDataContainer {\n")
	b.WriteString("          UniqueID 900001 \n")
	b.WriteString("          Name \"osgb_metadata\" \n")
	b.WriteString(fmt.Sprintf("          UDC_Descriptions %d {\n", len(lines)))
	for _, line := range lines {
		b.WriteString("            \"" + strings.ReplaceAll(strings.ReplaceAll(line, "\\", "\\\\"), "\"", "\\\"") + "\" \n")
	}
	b.WriteString("          }\n")
	b.WriteString("        }\n")
	b.WriteString("      }\n")
	return b.String()
}

func writePagedLODOSGT(path, databaseDir, childFile, geodeBlock string, centerX, centerY, centerZ, radius float64, threshold float64, metadata []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 64*1024)
	dbPath := filepath.ToSlash(databaseDir)
	if !strings.HasSuffix(dbPath, "/") {
		dbPath += "/"
	}
	fmt.Fprintln(w, "#Ascii Scene ")
	fmt.Fprintln(w, "#Version 161 ")
	fmt.Fprintln(w, "#Generator OpenSceneGraph 3.6.5 ")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "osg::Group {")
	fmt.Fprintln(w, "  UniqueID 1 ")
	fmt.Fprintln(w, "  Children 1 {")
	fmt.Fprintln(w, "    osg::PagedLOD {")
	fmt.Fprintln(w, "      UniqueID 2 ")
	fmt.Fprint(w, osgMetadataBlock(metadata))
	fmt.Fprintln(w, "      CenterMode USER_DEFINED_CENTER ")
	fmt.Fprintf(w, "      UserCenter %.6f %.6f %.6f %.6f \n", centerX, centerY, centerZ, radius)
	fmt.Fprintln(w, "      RangeMode PIXEL_SIZE_ON_SCREEN ")
	if childFile != "" {
		fmt.Fprintln(w, "      RangeList 2 {")
		fmt.Fprintf(w, "        0 %.6f \n", threshold)
		fmt.Fprintf(w, "        %.6f 3.40282e+38 \n", threshold)
		fmt.Fprintln(w, "      }")
		fmt.Fprintf(w, "      DatabasePath TRUE \"%s/\" \n", dbPath)
		fmt.Fprintln(w, "      RangeDataList 2 {")
		fmt.Fprintln(w, "        \"\" ")
		fmt.Fprintf(w, "        \"%s\" \n", childFile)
		fmt.Fprintln(w, "      }")
		fmt.Fprintln(w, "      PriorityList 2 {")
		fmt.Fprintln(w, "        0 1 ")
		fmt.Fprintln(w, "        0 1 ")
		fmt.Fprintln(w, "      }")
	} else {
		fmt.Fprintln(w, "      RangeList 1 {")
		fmt.Fprintln(w, "        0 3.40282e+38 ")
		fmt.Fprintln(w, "      }")
		fmt.Fprintf(w, "      DatabasePath TRUE \"%s/\" \n", dbPath)
		fmt.Fprintln(w, "      RangeDataList 1 {")
		fmt.Fprintln(w, "        \"\" ")
		fmt.Fprintln(w, "      }")
		fmt.Fprintln(w, "      PriorityList 1 {")
		fmt.Fprintln(w, "        0 1 ")
		fmt.Fprintln(w, "      }")
	}
	fmt.Fprintln(w, "      Children 1 {")
	fmt.Fprintln(w, geodeBlock)
	fmt.Fprintln(w, "      }")
	fmt.Fprintln(w, "    }")
	fmt.Fprintln(w, "  }")
	fmt.Fprintln(w, "}")
	return w.Flush()
}

func runOSGConvQuiet(osgconv, input, output, workDir string) error {
	cmd := exec.Command(osgconv, input, output)
	if workDir != "" {
		cmd.Dir = workDir
	} else {
		cmd.Dir = filepath.Dir(output)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("osgconv %s -> %s: %v: %s", input, output, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// tileNameWidth returns the digit width that a grid of the given size requires.
// A single width is used for both axes so that lexicographic name order matches
// numeric (row, col) order, which matters because the tree is walked and sorted
// by path name. It never returns less than 3, preserving the original
// "Tile_+003_+012" layout for ordinary grids.
func tileNameWidth(gridX, gridY int) int {
	width := 3
	for _, n := range []int{gridX, gridY} {
		if n < 0 {
			n = -n
		}
		if w := len(strconv.Itoa(n)); w > width {
			width = w
		}
	}
	return width
}

// tileName builds a Smart3D-style tile directory name from a grid row/column.
//
// The sign is emitted from the actual value rather than hard-coded as "+", so
// the naming stays correct if tiles are ever addressed from a negative origin
// (Smart3D itself writes e.g. Tile_-003_-012). width comes from tileNameWidth
// so that a grid larger than 999 indices cannot overflow its field and silently
// produce names that sort in the wrong order.
func tileName(row, col, width int) string {
	return fmt.Sprintf("Tile_%s_%s", padSigned(row, width), padSigned(col, width))
}

// padSigned renders value with an explicit sign and at least width digits,
// e.g. padSigned(3, 3) == "+003" and padSigned(-12, 3) == "-012".
func padSigned(value, width int) string {
	sign := "+"
	if value < 0 {
		sign = "-"
	}
	digits := strconv.Itoa(absInt(value))
	for len(digits) < width {
		digits = "0" + digits
	}
	return sign + digits
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// buildSmart3DOSGB builds the full Smart3D-style PagedLOD tree under outputDir.

// downscaleTexture creates a resized copy (max dim = maxDim) of src image in
// outDir and returns the filename that callers should reference plus an error.
//
// Unlike the previous signature it reports failure instead of silently
// returning srcName: a silent failure left the caller referencing a texture
// that was never copied into the output directory, which made osgconv fail
// later with a confusing missing-file error far from the real cause.
//
// When the source is already within the limit it is copied byte-for-byte so
// no unnecessary JPEG re-encode degrades it.
func downscaleTexture(srcPath, outDir, texName string, maxDim int) (string, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return texName, fmt.Errorf("open texture %s: %w", srcPath, err)
	}
	img, _, err := image.Decode(f)
	closeErr := f.Close()
	if err != nil {
		return texName, fmt.Errorf("decode texture %s: %w", srcPath, err)
	}
	if closeErr != nil {
		return texName, fmt.Errorf("close texture %s: %w", srcPath, closeErr)
	}
	dstPath := filepath.Join(outDir, texName)
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxDim && h <= maxDim {
		// No resize needed; copy the original bytes untouched.
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return texName, fmt.Errorf("read texture %s: %w", srcPath, err)
		}
		if err := os.WriteFile(dstPath, data, 0o644); err != nil {
			return texName, fmt.Errorf("write texture %s: %w", dstPath, err)
		}
		return texName, nil
	}
	scale := float64(maxDim) / float64(max(w, h))
	nw, nh := int(float64(w)*scale), int(float64(h)*scale)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	downscaleBox(img, dst, b, nw, nh)
	of, err := os.Create(dstPath)
	if err != nil {
		return texName, fmt.Errorf("create texture %s: %w", dstPath, err)
	}
	encodeErr := jpeg.Encode(of, dst, &jpeg.Options{Quality: 85})
	closeOutErr := of.Close()
	if encodeErr != nil {
		os.Remove(dstPath)
		return texName, fmt.Errorf("encode texture %s: %w", dstPath, encodeErr)
	}
	if closeOutErr != nil {
		os.Remove(dstPath)
		return texName, fmt.Errorf("close texture %s: %w", dstPath, closeOutErr)
	}
	return texName, nil
}

// downscaleBox resizes src into dst using an area-average (box) filter.
//
// The previous implementation picked a single nearest source pixel per output
// pixel, which aliases badly on large reductions: an 8192px texture reduced to
// 2048px samples only 1 of every 4 source pixels per axis, dropping 15/16 of
// the image content and producing visible moire and speckle on roof edges and
// road markings. Averaging the whole source footprint per output pixel keeps
// the result close to the original appearance.
func downscaleBox(src image.Image, dst *image.RGBA, srcBounds image.Rectangle, nw, nh int) {
	sw, sh := srcBounds.Dx(), srcBounds.Dy()
	for y := 0; y < nh; y++ {
		// Source footprint of this output row, at least one pixel tall.
		y0 := srcBounds.Min.Y + y*sh/nh
		y1 := srcBounds.Min.Y + (y+1)*sh/nh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0 := srcBounds.Min.X + x*sw/nw
			x1 := srcBounds.Min.X + (x+1)*sw/nw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sr, sg, sb, sa, count uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					r, g, b, a := src.At(sx, sy).RGBA()
					sr += uint64(r)
					sg += uint64(g)
					sb += uint64(b)
					sa += uint64(a)
					count++
				}
			}
			if count == 0 {
				continue
			}
			// RGBA() returns 16-bit values; shift back to 8-bit.
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(sr / count >> 8),
				G: uint8(sg / count >> 8),
				B: uint8(sb / count >> 8),
				A: uint8(sa / count >> 8),
			})
		}
	}
}

// filterTexMapForMesh returns only materials actually referenced by mesh.
func filterTexMapForMesh(mesh *objMesh, texMap map[string]string) map[string]string {
	if texMap == nil {
		return nil
	}
	used := make(map[int]bool)
	for _, tri := range mesh.triangles {
		if tri.mat >= 0 && tri.mat < len(mesh.materials) {
			used[tri.mat] = true
		}
	}
	out := make(map[string]string, len(used))
	for matIdx, mat := range mesh.materials {
		if used[matIdx] {
			if tex, ok := texMap[mat]; ok {
				out[mat] = tex
			}
		}
	}
	return out
}
func buildSmart3DOSGB(mesh *objMesh, texMap map[string]string, osgconv string, outputDir string, gridX, gridY int, logFn func(string)) error {
	if gridX <= 0 {
		gridX = 1
	}
	if gridY <= 0 {
		gridY = 1
	}
	if logFn == nil {
		logFn = func(string) {}
	}
	dataDir := filepath.Join(outputDir, "Data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create Data dir: %w", err)
	}
	workDir := filepath.Join(outputDir, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}
	bounds := computeBounds(mesh)
	tiles := tileTriangles(mesh, bounds, gridX, gridY)
	// Report how vertically stretched the tiles are. Tiles are assigned by XY
	// centroid, so steep or vertical surfaces produce tiles whose Z extent far
	// exceeds their map footprint and whose bounding spheres are therefore
	// oversized. Surfacing the ratio makes that condition diagnosable instead of
	// leaving the operator to wonder why LOD switches look wrong.
	if spread := tileZSpread(mesh, bounds, tiles, gridX, gridY); spread > 1 {
		logFn(fmt.Sprintf("tile vertical spread ratio %.2f (tile Z extent / map footprint); "+
			"values well above 1 mean steep or vertical surfaces dominate and the flat XY grid "+
			"under-represents them", spread))
	}
	uid := 1000
	nextUID := func() int { uid += 1000; return uid }
	nameWidth := tileNameWidth(gridX, gridY)

	type tileEntry struct {
		name                              string
		centerX, centerY, centerZ, radius float64
		entryPath                         string
	}
	var entries []tileEntry

	for row := 0; row < gridY; row++ {
		for col := 0; col < gridX; col++ {
			idx := row*gridX + col
			name := tileName(row, col, nameWidth)
			tileDir := filepath.Join(dataDir, name)
			if err := os.MkdirAll(tileDir, 0o755); err != nil {
				return err
			}
			triIdx := tiles[idx]
			if len(triIdx) == 0 {
				logFn("tile " + name + " is empty, skipped")
				continue
			}
			subMesh := extractSubMesh(mesh, triIdx)
			cx, cy, cz, radius := computeTileCenterRadius(subMesh)
			// Lock the tile's outer border so neighbouring tiles keep sampling
			// their shared edge at identical positions; without this each tile
			// simplifies independently and the tree opens cracks.
			tolerance := tileBoundaryTolerance(subMesh)
			lod1 := simplifyMeshPreservingBoundary(subMesh, max(1, len(subMesh.triangles)/4), tolerance)
			lod2 := simplifyMeshPreservingBoundary(subMesh, max(1, len(subMesh.triangles)/16), tolerance)
			if len(lod2.triangles) == 0 {
				lod2 = lod1
				logFn(fmt.Sprintf("tile %s: coarse LOD empty, using L1", name))
			}
			if len(lod1.triangles) == 0 {
				lod1 = subMesh
				logFn(fmt.Sprintf("tile %s: mid LOD empty, using full mesh", name))
			}
			if len(lod2.triangles) == 0 {
				lod2 = lod1
			}
			levels := []*objMesh{lod2, lod1, subMesh}
			entry := tileEntry{name: name, centerX: cx, centerY: cy, centerZ: cz, radius: radius}
			for li, level := range levels {
				base := fmt.Sprintf("%s_L%d", name, li)
				objPath := filepath.Join(workDir, base+".obj")
				tileTexMap := filterTexMapForMesh(level, texMap)
				if err := writeTexturedOBJ(level, tileTexMap, objPath); err != nil {
					return fmt.Errorf("write tile %s L%d: %w", name, li, err)
				}
				osgbPath := filepath.Join(tileDir, base+".osgb")
				// Convert OBJ -> OSGB in workDir so relative texture paths resolve.
				if err := runOSGConvQuiet(osgconv, base+".obj", osgbPath, workDir); err != nil {
					return fmt.Errorf("osgconv tile %s L%d: %w", name, li, err)
				}
				// OSGB -> OSGT so we can inject the Geode.
				tmpOSGT := osgbPath + ".tmp.osgt"
				if err := runOSGConvQuiet(osgconv, osgbPath, tmpOSGT, tileDir); err != nil {
					return fmt.Errorf("roundtrip tile %s L%d: %w", name, li, err)
				}
				geode, err := extractGeodeBlock(tmpOSGT)
				if err != nil {
					return err
				}
				geode = renumberUniqueIDs(geode, nextUID())
				os.Remove(tmpOSGT)

				childFile := ""
				threshold := radius * float64(uint(1)<<(2-li))
				if li < len(levels)-1 {
					childFile = fmt.Sprintf("%s_L%d.osgb", name, li+1)
				}
				// Build PagedLOD OSGT.
				osgtPath := filepath.Join(tileDir, base+".osgt")
				if err := writePagedLODOSGT(osgtPath, tileDir, childFile, geode, cx, cy, cz, radius, threshold, []string{
					"container=Smart3D OSGB PagedLOD",
					fmt.Sprintf("tile=%s", name),
					fmt.Sprintf("level=%d", li),
					fmt.Sprintf("vertices=%d", len(level.vertices)),
					fmt.Sprintf("faces=%d", len(level.triangles)),
					fmt.Sprintf("child=%s", childFile),
				}); err != nil {
					return err
				}
				finalOSGB := osgbPath
				if err := runOSGConvQuiet(osgconv, base+".osgt", finalOSGB, tileDir); err != nil {
					return fmt.Errorf("osgconv pagedlod tile %s L%d: %w", name, li, err)
				}
				if err := os.Remove(osgtPath); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove intermediate OSGT %s: %w", osgtPath, err)
				}
				if li == 0 {
					entry.entryPath = finalOSGB
				}
			}
			entries = append(entries, entry)
			logFn(fmt.Sprintf("tile %s: verts=%d faces=%d levels=%d", name, len(subMesh.vertices), len(subMesh.triangles), len(levels)))
		}
	}

	if len(entries) == 0 {
		return fmt.Errorf("no non-empty tiles produced")
	}
	rootOSGT := filepath.Join(dataDir, "root.osgt")
	f, err := os.Create(rootOSGT)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 64*1024)
	fmt.Fprintln(w, "#Ascii Scene ")
	fmt.Fprintln(w, "#Version 161 ")
	fmt.Fprintln(w, "#Generator OpenSceneGraph 3.6.5 ")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "osg::Group {")
	fmt.Fprintln(w, "  UniqueID 1 ")
	fmt.Fprint(w, osgMetadataBlock([]string{
		"container=Smart3D OSGB",
		"root=true",
		fmt.Sprintf("tiles=%d", len(entries)),
		fmt.Sprintf("created=%s", time.Now().UTC().Format("2006-01-02T15:04:05Z")),
		"generator=osgb-service",
	}))
	fmt.Fprintf(w, "  Children %d {\n", len(entries))
	childIdx := 2
	for _, e := range entries {
		fmt.Fprintln(w, "    osg::PagedLOD {")
		fmt.Fprintf(w, "      UniqueID %d \n", childIdx)
		fmt.Fprintln(w, "      CenterMode USER_DEFINED_CENTER ")
		fmt.Fprintf(w, "      UserCenter %.6f %.6f %.6f %.6f \n", e.centerX, e.centerY, e.centerZ, e.radius)
		fmt.Fprintln(w, "      RangeMode PIXEL_SIZE_ON_SCREEN ")
		fmt.Fprintln(w, "      RangeList 1 {")
		fmt.Fprintln(w, "        0 3.40282e+38 ")
		fmt.Fprintln(w, "      }")
		dbPath := filepath.ToSlash(dataDir)
		if !strings.HasSuffix(dbPath, "/") {
			dbPath += "/"
		}
		fmt.Fprintf(w, "      DatabasePath TRUE \"%s\" \n", dbPath)
		fmt.Fprintln(w, "      RangeDataList 1 {")
		rel, err := filepath.Rel(dataDir, e.entryPath)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "        \"%s\" \n", filepath.ToSlash(rel))
		fmt.Fprintln(w, "      }")
		fmt.Fprintln(w, "      PriorityList 1 {")
		fmt.Fprintln(w, "        0 1 ")
		fmt.Fprintln(w, "      }")
		fmt.Fprintln(w, "    }")
		childIdx++
	}
	fmt.Fprintln(w, "  }")
	fmt.Fprintln(w, "}")
	if err := w.Flush(); err != nil {
		return err
	}
	f.Close()
	rootOSGB := filepath.Join(outputDir, "root.osgb")
	if err := runOSGConvQuiet(osgconv, "root.osgt", rootOSGB, dataDir); err != nil {
		return fmt.Errorf("osgconv root: %w", err)
	}
	if err := os.Remove(rootOSGT); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove intermediate root OSGT: %w", err)
	}
	if err := os.RemoveAll(workDir); err != nil {
		return fmt.Errorf("remove intermediate work directory: %w", err)
	}
	logFn("Smart3D PagedLOD tree built successfully")
	return nil
}

// validateOSGBTile round-trips a binary OSGB and inspects the OSGT.
func validateOSGBTile(osgconv, osgbPath string, requireMetadata bool) (*osgbTileStats, error) {
	info, err := os.Stat(osgbPath)
	if err != nil {
		return nil, fmt.Errorf("not found: %w", err)
	}
	stats := &osgbTileStats{
		Name:  strings.TrimSuffix(filepath.Base(osgbPath), ".osgb"),
		Path:  osgbPath,
		Size:  info.Size(),
		Valid: true,
	}
	if info.Size() == 0 {
		stats.Valid = false
		stats.Error = "file is empty"
		return stats, nil
	}
	osgt := osgbPath + ".validate.osgt"
	defer os.Remove(osgt)
	if err := runOSGConvQuiet(osgconv, osgbPath, osgt, filepath.Dir(osgbPath)); err != nil {
		stats.Valid = false
		stats.Error = "read-back failed: " + err.Error()
		return stats, nil
	}
	data, err := os.ReadFile(osgt)
	if err != nil {
		stats.Valid = false
		stats.Error = "cannot open OSGT: " + err.Error()
		return stats, nil
	}
	text := string(data)
	stats.Textured = strings.Contains(text, "osg::Texture2D ")
	stats.Metadata = strings.Contains(text, "UDC_Descriptions ") && strings.Contains(text, "Name \"osgb_metadata\"")
	stats.TextureImages = int64(strings.Count(text, "osg::Texture2D "))
	stats.PagedLODs = int64(strings.Count(text, "osg::PagedLOD "))
	stats.Geodes = int64(strings.Count(text, "osg::Geode "))
	// Extract geometry counts from Geode blocks.
	verts, faces := int64(0), int64(0)
	searchPos := 0
	for {
		gi := strings.Index(text[searchPos:], "osg::Geode ")
		if gi < 0 {
			break
		}
		gi += searchPos
		// Find matching brace of this Geode.
		depth := 0
		started := false
		end := len(text) - 1
		for i := gi; i < len(text); i++ {
			switch text[i] {
			case '{':
				depth++
				started = true
			case '}':
				depth--
			}
			if started && depth == 0 {
				end = i
				break
			}
		}
		block := text[gi:end]
		verts += int64(strings.Count(block, "VertexArray TRUE "))
		// Count indices in DrawElements vectors.
		for _, m := range []string{"DrawElementsUShort", "DrawElementsUInt", "DrawArrays"} {
			pos := 0
			for {
				off := strings.Index(block[pos:], m)
				if off < 0 {
					break
				}
				pos += off
				// Find "vector N" or "Count N" after this marker.
				seg := block[pos:min(len(block), pos+1000)]
				if vIdx := strings.Index(seg, "vector "); vIdx >= 0 {
					fields := strings.Fields(seg[vIdx : vIdx+64])
					if len(fields) >= 2 {
						if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
							if m == "DrawArrays" {
								faces += n / 3
							} else {
								faces += n / 3
							}
						}
					}
					pos += vIdx
					continue
				}
				pos += len(m)
			}
		}
		searchPos = end
	}
	stats.Vertices = verts
	stats.Faces = faces
	// childFile
	if idx := strings.Index(text, "RangeDataList"); idx >= 0 {
		seg := text[idx:min(len(text), idx+4000)]
		if j := strings.Index(seg, ".osgb"); j >= 0 {
			start := strings.LastIndex(seg[:j], "\"")
			end := strings.Index(seg[j:], "\"")
			if start >= 0 && end > 0 {
				stats.ChildFile = seg[start+1 : j+end]
			}
		}
	}
	// child file exists?
	if stats.ChildFile != "" {
		child := filepath.Join(filepath.Dir(osgbPath), stats.ChildFile)
		if !fileExists(child) {
			stats.Valid = false
			stats.Error = "child file missing: " + stats.ChildFile
		}
	}
	if stats.Faces == 0 && stats.PagedLODs == 0 {
		stats.Valid = false
		stats.Error = "no PagedLOD and no faces"
	}
	if requireMetadata && !stats.Metadata {
		stats.Valid = false
		stats.Error = "metadata missing"
	}
	if !stats.Textured {
		stats.Valid = false
		stats.Error = "no embedded texture images detected"
	}
	if stats.Faces == 0 && stats.PagedLODs > 0 {
		stats.Valid = false
		stats.Error = "PagedLOD exists but inline Geode has no geometry"
	}
	return stats, nil
}

func validateSmart3DTree(osgconv, outputDir string, logFn func(string)) ([]*osgbTileStats, error) {
	dataDir := filepath.Join(outputDir, "Data")
	if _, err := os.Stat(dataDir); err != nil {
		return nil, fmt.Errorf("Data dir missing: %w", err)
	}
	rootPath := filepath.Join(outputDir, "root.osgb")
	rootStats, err := validateOSGBTile(osgconv, rootPath, false)
	if err != nil {
		return nil, fmt.Errorf("validate root OSGB: %w", err)
	}
	if !rootStats.Metadata {
		return nil, fmt.Errorf("root OSGB metadata missing")
	}
	logFn("VALIDATE OK root.osgb: metadata present")
	var allStats []*osgbTileStats
	err = filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".osgb") {
			return nil
		}
		stats, tileErr := validateOSGBTile(osgconv, path, true)
		if tileErr != nil {
			logFn("VALIDATE ERROR " + path + ": " + tileErr.Error())
			return nil
		}
		if stats.Valid {
			logFn(fmt.Sprintf("VALIDATE OK %s: verts=%d faces=%d plods=%d geodes=%d", stats.Name, stats.Vertices, stats.Faces, stats.PagedLODs, stats.Geodes))
		} else {
			logFn("VALIDATE FAIL " + stats.Name + ": " + stats.Error)
		}
		allStats = append(allStats, stats)
		return nil
	})
	if err != nil {
		return allStats, err
	}
	if err := writeValidationReport(outputDir, allStats); err != nil {
		logFn("write validation report failed: " + err.Error())
	}
	return allStats, nil
}

func writeValidationReport(outputDir string, stats []*osgbTileStats) error {
	report := struct {
		TotalTiles  int              `json:"total_tiles"`
		ValidTiles  int              `json:"valid_tiles"`
		FailedTiles int              `json:"failed_tiles"`
		Tiles       []*osgbTileStats `json:"tiles"`
	}{Tiles: stats}
	for _, s := range stats {
		if s.Valid {
			report.ValidTiles++
		} else {
			report.FailedTiles++
		}
	}
	report.TotalTiles = len(stats)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outputDir, "validation_report.json"), data, 0o644)
}
