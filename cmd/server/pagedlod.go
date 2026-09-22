package main

// pagedlod.go implements Smart3D-style OSGB PagedLOD generation from a
// textured OBJ model produced by OpenMVS TextureMesh.
//
// Pipeline:
//  1. Parse the OBJ (v/vt/f/usemtl) to extract vertices, UVs and triangles.
//  2. Split triangles into an N x N spatial grid on XY centroid.
//  3. Per tile, build three LOD levels (full / 1/4 / 1/16 faces) by
//     vertex-cluster simplification and export each to an OBJ fragment.
//  4. Convert fragments to OSGB via osgconv, extract Geodes, wrap in
//     PagedLOD referencing child (finer) tiles, reconvert to OSGB.
//  5. Validate every OSGB by osgconv round-trip, face count and references.

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// OBJ parsing
// ---------------------------------------------------------------------------

// objTriangle is one 0-based face referencing mesh.vertices indices.
type objTriangle struct {
	a, b, c int
	mat     int // index into mesh.materials (-1 = no material)
}

// objVertex holds position (x,y,z) and optional UV (u,v).
type objVertex struct {
	x, y, z float64
	u, v    float64
}

// objMesh is the parsed result of a textured OBJ file.
type objMesh struct {
	vertices  []objVertex
	triangles []objTriangle
	materials []string
	mtlFile   string
	mtlDir    string // directory containing the OBJ (and MTL/textures)
}

// parseObjTextured reads an OBJ emitted by OpenMVS TextureMesh.
func parseObjTextured(path string) (*objMesh, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open OBJ: %w", err)
	}
	defer f.Close()

	mesh := &objMesh{mtlDir: filepath.Dir(path)}
	var positions []objVertex
	var uvs []objVertex
	type rawFace struct {
		vi    [3]int
		vt    [3]int
		hasUV bool
		mat   int
	}
	var rawFaces []rawFace
	var curMat int = -1
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "v "):
			fl := strings.Fields(line[2:])
			if len(fl) < 3 {
				continue
			}
			x, _ := strconv.ParseFloat(fl[0], 64)
			y, _ := strconv.ParseFloat(fl[1], 64)
			z, _ := strconv.ParseFloat(fl[2], 64)
			positions = append(positions, objVertex{x: x, y: y, z: z})
		case strings.HasPrefix(line, "vt "):
			fl := strings.Fields(line[3:])
			if len(fl) < 2 {
				continue
			}
			u, _ := strconv.ParseFloat(fl[0], 64)
			v, _ := strconv.ParseFloat(fl[1], 64)
			uvs = append(uvs, objVertex{u: u, v: v})
		case strings.HasPrefix(line, "usemtl "):
			name := strings.TrimSpace(line[7:])
			found := false
			for i, m := range mesh.materials {
				if m == name {
					curMat = i
					found = true
					break
				}
			}
			if !found {
				mesh.materials = append(mesh.materials, name)
				curMat = len(mesh.materials) - 1
			}
		case strings.HasPrefix(line, "mtllib "):
			mesh.mtlFile = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "f "):
			fl := strings.Fields(line[2:])
			if len(fl) < 3 {
				continue
			}
			var face rawFace
			face.mat = curMat
			for i := 0; i < 3; i++ {
				parts := strings.Split(fl[i], "/")
				vi, _ := strconv.Atoi(parts[0])
				if vi > 0 {
					face.vi[i] = vi - 1
				} else {
					face.vi[i] = len(positions) + vi
				}
				if len(parts) > 1 && parts[1] != "" {
					ti, _ := strconv.Atoi(parts[1])
					if ti > 0 {
						face.vt[i] = ti - 1
					} else {
						face.vt[i] = len(uvs) + ti
					}
					face.hasUV = true
				}
			}
			rawFaces = append(rawFaces, face)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan OBJ: %w", err)
	}
	if len(positions) == 0 || len(rawFaces) == 0 {
		return nil, fmt.Errorf("OBJ has no geometry: vertices=%d faces=%d", len(positions), len(rawFaces))
	}

	// Deduplicate (pos,uv) pairs into a combined vertex pool.
	type key struct{ p, u int }
	cache := make(map[key]int, len(positions))
	mesh.triangles = make([]objTriangle, 0, len(rawFaces))
	for _, rf := range rawFaces {
		var idx [3]int
		for i := 0; i < 3; i++ {
			uvIdx := -1
			if rf.hasUV {
				uvIdx = rf.vt[i]
			}
			k := key{p: rf.vi[i], u: uvIdx}
			id, ok := cache[k]
			if !ok {
				id = len(mesh.vertices)
				cache[k] = id
				v := positions[rf.vi[i]]
				if uvIdx >= 0 && uvIdx < len(uvs) {
					v.u = uvs[uvIdx].u
					v.v = uvs[uvIdx].v
				}
				mesh.vertices = append(mesh.vertices, v)
			}
			idx[i] = id
		}
		mesh.triangles = append(mesh.triangles,
			objTriangle{a: idx[0], b: idx[1], c: idx[2], mat: rf.mat})
	}
	return mesh, nil
}

// parseMTL extracts the map_Kd texture image for each material name.
// It returns a map material_name -> texture image filename.
func parseMTL(obj *objMesh) map[string]string {
	if obj.mtlFile == "" {
		return nil
	}
	mtlPath := filepath.Join(obj.mtlDir, obj.mtlFile)
	f, err := os.Open(mtlPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	result := make(map[string]string)
	var curName string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "newmtl ") {
			curName = strings.TrimSpace(line[7:])
		} else if strings.HasPrefix(line, "map_Kd ") && curName != "" {
			tex := strings.TrimSpace(line[7:])
			result[curName] = tex
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Bounding box and spatial tiling
// ---------------------------------------------------------------------------

// meshBounds is an axis-aligned bounding box.
type meshBounds struct {
	minX, minY, minZ float64
	maxX, maxY, maxZ float64
}

func computeBounds(mesh *objMesh) meshBounds {
	b := meshBounds{
		minX: math.MaxFloat64, minY: math.MaxFloat64, minZ: math.MaxFloat64,
		maxX: math.Inf(-1), maxY: math.Inf(-1), maxZ: math.Inf(-1),
	}
	for _, v := range mesh.vertices {
		if v.x < b.minX {
			b.minX = v.x
		}
		if v.y < b.minY {
			b.minY = v.y
		}
		if v.z < b.minZ {
			b.minZ = v.z
		}
		if v.x > b.maxX {
			b.maxX = v.x
		}
		if v.y > b.maxY {
			b.maxY = v.y
		}
		if v.z > b.maxZ {
			b.maxZ = v.z
		}
	}
	return b
}

// tileTriangles splits triangles into gridX x gridY cells using their
// XY centroid. It returns a 2D slice indexed [row][col] of face indices.
//
// Note: assignment is purely horizontal. This matches the Smart3D/S3C tile
// convention (tiles are addressed by map coordinate), but it means a tile may
// contain geometry whose Z range is much taller than the tile's XY footprint --
// typical for cliffs, facades and overhangs. tileZSpread reports that condition
// so an operator can tell when a flat-grid tiling is a poor fit for the survey.
func tileTriangles(mesh *objMesh, b meshBounds, gridX, gridY int) [][]int {
	w := b.maxX - b.minX
	h := b.maxY - b.minY
	if w <= 0 {
		w = 1
	}
	if h <= 0 {
		h = 1
	}
	result := make([][]int, gridX*gridY)
	for i := range result {
		result[i] = make([]int, 0)
	}
	for fi, tri := range mesh.triangles {
		cx := (mesh.vertices[tri.a].x + mesh.vertices[tri.b].x + mesh.vertices[tri.c].x) / 3.0
		cy := (mesh.vertices[tri.a].y + mesh.vertices[tri.b].y + mesh.vertices[tri.c].y) / 3.0
		col := int((cx - b.minX) / w * float64(gridX))
		row := int((cy - b.minY) / h * float64(gridY))
		if col < 0 {
			col = 0
		}
		if col >= gridX {
			col = gridX - 1
		}
		if row < 0 {
			row = 0
		}
		if row >= gridY {
			row = gridY - 1
		}
		idx := row*gridX + col
		result[idx] = append(result[idx], fi)
	}
	return result
}

// tileZSpread reports how vertically stretched the tiles produced by
// tileTriangles are. For each non-empty tile it computes (maxZ-minZ) and
// compares it against the tile's horizontal footprint, returning the largest
// ratio found.
//
// A ratio near or below 1 means the terrain is effectively flat inside tiles and
// the XY grid is a good fit. A large ratio means tiles contain tall vertical
// structures (cliff faces, building facades, overhangs), where a single tile
// spans a much larger vertical extent than its map footprint. Such tiles get an
// oversized bounding sphere, which inflates the PagedLOD switch thresholds so
// coarse levels stay on screen too long. The value is reported in the job log so
// this is visible rather than silent; it is a diagnostic, not an error.
func tileZSpread(mesh *objMesh, b meshBounds, tiles [][]int, gridX, gridY int) float64 {
	if gridX <= 0 || gridY <= 0 {
		return 0
	}
	footX := (b.maxX - b.minX) / float64(gridX)
	footY := (b.maxY - b.minY) / float64(gridY)
	footprint := math.Max(footX, footY)
	if footprint <= 0 {
		return 0
	}
	worst := 0.0
	for _, triIndices := range tiles {
		if len(triIndices) == 0 {
			continue
		}
		tile := extractSubMesh(mesh, triIndices)
		tb := computeBounds(tile)
		spread := (tb.maxZ - tb.minZ) / footprint
		if spread > worst {
			worst = spread
		}
	}
	return worst
}

// extractSubMesh creates a new objMesh from a subset of triangle indices.
// Vertices are remapped to the new pool; the material list is preserved.
func extractSubMesh(src *objMesh, triIndices []int) *objMesh {
	dst := &objMesh{materials: src.materials, mtlFile: src.mtlFile, mtlDir: src.mtlDir}
	cache := make(map[int]int, len(triIndices)*3)
	dst.triangles = make([]objTriangle, 0, len(triIndices))
	for _, ti := range triIndices {
		tri := src.triangles[ti]
		var idx [3]int
		for j, vi := range [3]int{tri.a, tri.b, tri.c} {
			id, ok := cache[vi]
			if !ok {
				id = len(dst.vertices)
				cache[vi] = id
				dst.vertices = append(dst.vertices, src.vertices[vi])
			}
			idx[j] = id
		}
		dst.triangles = append(dst.triangles, objTriangle{a: idx[0], b: idx[1], c: idx[2], mat: tri.mat})
	}
	return dst
}

// ---------------------------------------------------------------------------
// Vertex-cluster simplification
// ---------------------------------------------------------------------------

// simplifyMesh reduces the mesh to approximately targetFaces triangles using
// a uniform 3D grid vertex-clustering approach. The grid resolution is set
// adaptively so that each occupied cluster contributes ~1 face.
//
// Deprecated: prefer simplifyMeshPreservingBoundary, which additionally locks
// vertices lying on the mesh border so that independently simplified tiles do
// not pull apart at their shared edges. Kept for callers/tests that simplify a
// mesh that is not part of a tiled tree.
func simplifyMesh(src *objMesh, targetFaces int) *objMesh {
	return simplifyMeshPreservingBoundary(src, targetFaces, 0)
}

// simplifyMeshPreservingBoundary reduces src to approximately targetFaces
// triangles by uniform 3D grid vertex clustering.
//
// boundaryTolerance > 0 enables boundary preservation: any vertex lying within
// that distance (in mesh units) of the mesh's axis-aligned bounding box border
// is treated as a locked vertex. Locked vertices are never merged with other
// vertices and never moved, so the outer silhouette of the simplified mesh
// matches the original exactly. When every tile of a PagedLOD tree is simplified
// this way, adjacent tiles keep sampling their shared edge at the same
// positions and the tree does not open cracks between tiles.
//
// Passing boundaryTolerance == 0 reproduces the original cluster-everything
// behaviour.
func simplifyMeshPreservingBoundary(src *objMesh, targetFaces int, boundaryTolerance float64) *objMesh {
	if len(src.triangles) <= targetFaces {
		return src
	}
	b := computeBounds(src)
	w := b.maxX - b.minX
	h := b.maxY - b.minY
	d := b.maxZ - b.minZ
	if w <= 0 {
		w = 1
	}
	if h <= 0 {
		h = 1
	}
	if d <= 0 {
		d = 0.001
	}
	// Aim for cube root of targetFaces so each cell holds ~1 triangle.
	n := int(math.Cbrt(float64(targetFaces) * 2))
	if n < 4 {
		n = 4
	}
	if n > 512 {
		n = 512
	}
	stepX := w / float64(n)
	stepY := h / float64(n)
	stepZ := d / float64(n)
	if stepX <= 0 {
		stepX = 0.001
	}
	if stepY <= 0 {
		stepY = 0.001
	}
	if stepZ <= 0 {
		stepZ = 0.001
	}

	// locked marks vertices that must keep their exact position. A zero
	// tolerance disables the feature entirely.
	//
	// Each axis is only considered when the mesh actually extends along it.
	// For a flat mesh (a common case: a single terrain tile with no vertical
	// relief) every vertex trivially sits on both Z faces of the bounding box,
	// so testing Z unconditionally would lock the entire tile and silently
	// disable simplification altogether.
	lockX := boundaryTolerance > 0 && w > boundaryTolerance
	lockY := boundaryTolerance > 0 && h > boundaryTolerance
	lockZ := boundaryTolerance > 0 && d > boundaryTolerance
	var locked []bool
	if lockX || lockY || lockZ {
		locked = make([]bool, len(src.vertices))
		for vi, v := range src.vertices {
			if lockX && (v.x-b.minX <= boundaryTolerance || b.maxX-v.x <= boundaryTolerance) ||
				lockY && (v.y-b.minY <= boundaryTolerance || b.maxY-v.y <= boundaryTolerance) ||
				lockZ && (v.z-b.minZ <= boundaryTolerance || b.maxZ-v.z <= boundaryTolerance) {
				locked[vi] = true
			}
		}
	}

	// Assign each vertex to a cluster ID. Locked vertices are assigned a
	// private, deterministic cluster so that each becomes its own output
	// vertex at its original position.
	cluster := make(map[int][3]int, len(src.vertices))
	clusterCentroid := make(map[[3]int][]int)
	// Locked clusters are keyed in a disjoint space starting at -1 going down,
	// which cannot collide with the non-negative grid coordinates.
	nextLocked := -1
	for vi, v := range src.vertices {
		if locked != nil && locked[vi] {
			key := [3]int{nextLocked, 0, 0}
			nextLocked--
			cluster[vi] = key
			clusterCentroid[key] = []int{vi}
			continue
		}
		cx := int((v.x - b.minX) / stepX)
		cy := int((v.y - b.minY) / stepY)
		cz := int((v.z - b.minZ) / stepZ)
		if cx < 0 {
			cx = 0
		}
		if cy < 0 {
			cy = 0
		}
		if cz < 0 {
			cz = 0
		}
		if cx >= n {
			cx = n - 1
		}
		if cy >= n {
			cy = n - 1
		}
		if cz >= n {
			cz = n - 1
		}
		key := [3]int{cx, cy, cz}
		cluster[vi] = key
		clusterCentroid[key] = append(clusterCentroid[key], vi)
	}

	// Compute centroid per cluster, mapping cluster key -> new vertex ID.
	dst := &objMesh{materials: src.materials, mtlFile: src.mtlFile, mtlDir: src.mtlDir}
	clusterToVert := make(map[[3]int]int, len(clusterCentroid))
	// Sort cluster keys for determinism.
	keys := make([][3]int, 0, len(clusterCentroid))
	for k := range clusterCentroid {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		if keys[i][1] != keys[j][1] {
			return keys[i][1] < keys[j][1]
		}
		return keys[i][2] < keys[j][2]
	})
	for _, k := range keys {
		verts := clusterCentroid[k]
		var sx, sy, sz, su, sv float64
		count := float64(len(verts))
		for _, vi := range verts {
			v := src.vertices[vi]
			sx += v.x
			sy += v.y
			sz += v.z
			su += v.u
			sv += v.v
		}
		newV := objVertex{x: sx / count, y: sy / count, z: sz / count, u: su / count, v: sv / count}
		clusterToVert[k] = len(dst.vertices)
		dst.vertices = append(dst.vertices, newV)
	}

	// Emit faces whose three corners map to distinct clusters.
	seen := make(map[[3]int]bool)
	dst.triangles = make([]objTriangle, 0, len(src.triangles)/2)
	for _, tri := range src.triangles {
		ka := cluster[tri.a]
		kb := cluster[tri.b]
		kc := cluster[tri.c]
		if ka == kb || kb == kc || ka == kc {
			continue
		}
		ia := clusterToVert[ka]
		ib := clusterToVert[kb]
		ic := clusterToVert[kc]
		face := [3]int{ia, ib, ic}
		// Canonical sort to detect duplicates.
		var sorted [3]int
		copy(sorted[:], face[:])
		if sorted[0] > sorted[1] {
			sorted[0], sorted[1] = sorted[1], sorted[0]
		}
		if sorted[1] > sorted[2] {
			sorted[1], sorted[2] = sorted[2], sorted[1]
		}
		if sorted[0] > sorted[1] {
			sorted[0], sorted[1] = sorted[1], sorted[0]
		}
		key := sorted
		if seen[key] {
			continue
		}
		seen[key] = true
		dst.triangles = append(dst.triangles, objTriangle{a: ia, b: ib, c: ic, mat: tri.mat})
	}
	return dst
}

// ---------------------------------------------------------------------------
// OBJ export
// ---------------------------------------------------------------------------

// writeTexturedOBJ writes a simplified textured OBJ with MTL file. The MTL
// references the original texture images (copied to the output dir).
func writeTexturedOBJ(mesh *objMesh, texMap map[string]string, objPath string) error {
	if err := os.MkdirAll(filepath.Dir(objPath), 0o755); err != nil {
		return err
	}
	mtlName := strings.TrimSuffix(filepath.Base(objPath), filepath.Ext(objPath)) + ".mtl"
	mtlPath := filepath.Join(filepath.Dir(objPath), mtlName)

	// Copy texture images to the output directory so osgconv can find them.
	// Large textures (8K -> 2K) are downscaled to reduce the resulting OSGB
	// size. A failure here is fatal: without the image in outDir the MTL would
	// reference a file that osgconv cannot resolve, producing a textureless
	// tile that later passes unnoticed in a huge job.
	outDir := filepath.Dir(objPath)
	for _, mat := range mesh.materials {
		if tex, ok := texMap[mat]; ok {
			srcTex := filepath.Join(mesh.mtlDir, tex)
			dstTex := filepath.Join(outDir, tex)
			if fileExists(dstTex) {
				continue
			}
			if !fileExists(srcTex) {
				return fmt.Errorf("texture %s for material %s not found in %s", tex, mat, mesh.mtlDir)
			}
			if _, err := downscaleTexture(srcTex, outDir, tex, 2048); err != nil {
				return err
			}
		}
	}

	// Write MTL
	var mtl strings.Builder
	mtl.WriteString("# Auto-generated MTL\n")
	for _, mat := range mesh.materials {
		mtl.WriteString("newmtl " + mat + "\n")
		mtl.WriteString("Ka 1.000 1.000 1.000\nKd 1 1 1\nKs 0 0 0\nillum 1\n")
		if tex, ok := texMap[mat]; ok {
			mtl.WriteString("map_Kd " + tex + "\n")
		}
		mtl.WriteString("\n")
	}
	if err := os.WriteFile(mtlPath, []byte(mtl.String()), 0644); err != nil {
		return fmt.Errorf("write MTL: %w", err)
	}

	// Write OBJ
	f, err := os.Create(objPath)
	if err != nil {
		return fmt.Errorf("create OBJ: %w", err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1024*1024)
	fmt.Fprintf(w, "mtllib %s\n", mtlName)
	for _, v := range mesh.vertices {
		if v.u != 0 || v.v != 0 {
			fmt.Fprintf(w, "v %.6f %.6f %.6f\nvt %.6f %.6f\n", v.x, v.y, v.z, v.u, v.v)
		} else {
			fmt.Fprintf(w, "v %.6f %.6f %.6f\n", v.x, v.y, v.z)
		}
	}
	// Group faces by material for correct usemtl ordering.
	lastMat := -2
	for _, tri := range mesh.triangles {
		if tri.mat != lastMat {
			matName := ""
			if tri.mat >= 0 && tri.mat < len(mesh.materials) {
				matName = mesh.materials[tri.mat]
			}
			fmt.Fprintf(w, "usemtl %s\n", matName)
			lastMat = tri.mat
		}
		if mesh.vertices[tri.a].u != 0 || mesh.vertices[tri.a].v != 0 {
			fmt.Fprintf(w, "f %d/%d %d/%d %d/%d\n",
				tri.a+1, tri.a+1, tri.b+1, tri.b+1, tri.c+1, tri.c+1)
		} else {
			fmt.Fprintf(w, "f %d %d %d\n", tri.a+1, tri.b+1, tri.c+1)
		}
	}
	return w.Flush()
}

// ---------------------------------------------------------------------------
// osgconv helpers
// ---------------------------------------------------------------------------

func runOSGConv(osgconv, input, output string) error {
	return runOSGConvInDir(osgconv, input, output, filepath.Dir(input))
}

func runOSGConvInDir(osgconv, input, output, workDir string) error {
	cmd := exec.Command(osgconv, input, output)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if workDir != "" {
		cmd.Dir = workDir
	} else {
		cmd.Dir = filepath.Dir(output)
	}
	return cmd.Run()
}
