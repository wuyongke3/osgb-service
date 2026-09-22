package main

// pipeline.go runs the photogrammetry pipeline: the COLMAP/OpenMVS stages, the
// OSGB conversion entry points, per-stage checkpoints, subprocess execution and
// the helpers that translate failures into something an operator can act on.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	// Register the decoders this file needs. preprocessImages calls
	// image.DecodeConfig to record each image's width and height, and the
	// registration is per-package state: without these blank imports the
	// dimensions would silently come back as zero for JPEG and PNG. They happen
	// to be registered elsewhere in the package today, but relying on another
	// file's side effect would break silently the moment that import changes.
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func (m *JobManager) run(id string) {
	job := m.snapshot(id)
	if job == nil {
		return
	}
	defer func() {
		finished := m.snapshot(id)
		if finished == nil || finished.PlanID == "" {
			return
		}
		status := "failed"
		if finished.Status == "completed" {
			status = "completed"
		}
		m.updatePlan(finished.PlanID, func(plan *Plan) { plan.Status = status })
	}()
	inputPath := job.InputPath
	jobDir, err := filepath.Abs(filepath.Join(m.config.DataDir, "jobs", id))
	if err != nil {
		m.fail(id, fmt.Errorf("resolve job workspace: %w", err))
		return
	}
	projectDir := inputPath
	imageDir := inputPath
	if info, err := os.Stat(filepath.Join(inputPath, "images")); err == nil && info.IsDir() {
		imageDir = filepath.Join(inputPath, "images")
	}
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		m.fail(id, fmt.Errorf("create job workspace: %w", err))
		return
	}

	m.setPhase(id, phasePreparing, 5, fmt.Sprintf("using local image directory: %s", imageDir))
	m.logLine(id, "system", fmt.Sprintf("Detected %d image(s), %.2f GB", job.InputCount, float64(job.InputBytes)/(1024*1024*1024)))
	manifest, err := m.preprocessImages(id, imageDir, jobDir)
	if err != nil {
		m.fail(id, err)
		return
	}

	if strings.EqualFold(m.config.PipelineMode, "external") {
		m.runExternal(id, projectDir, imageDir, jobDir)
		return
	}
	m.runNative(id, imageDir, jobDir, manifest)
}

// runExternal preserves compatibility with an existing ODX/ODM-style engine.
func (m *JobManager) runExternal(id, projectDir, imageDir, jobDir string) {
	if m.config.ReconBin == "" {
		m.fail(id, errors.New("PIPELINE_MODE=external requires RECON_BIN"))
		return
	}
	m.setPhase(id, phaseReconstruction, 15, "running configured reconstruction engine")
	vars := map[string]string{
		"{job_dir}":     jobDir,
		"{project_dir}": projectDir,
		"{input_dir}":   imageDir,
		"{output_path}": filepath.Clean(m.jobOutputPath(id)),
	}
	if err := m.runCommand(id, m.config.ReconBin, m.config.ReconArgs, vars, projectDir, nil); err != nil {
		m.fail(id, err)
		return
	}

	m.setPhase(id, phaseLocating, 78, "locating reconstructed textured model")
	modelPath, err := findModel(projectDir)
	if err != nil {
		m.fail(id, err)
		return
	}
	m.update(id, func(job *Job) {
		job.ModelPath = modelPath
	})
	m.logLine(id, "system", "Found reconstructed model: "+modelPath)

	if m.config.OSGBBin == "" {
		m.fail(id, errors.New("PIPELINE_MODE=external requires OSGB_BIN"))
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.jobOutputPath(id)), 0o755); err != nil {
		m.fail(id, fmt.Errorf("create output directory: %w", err))
		return
	}
	vars["{obj_path}"] = modelPath
	vars["{model_path}"] = modelPath
	vars["{output_path}"] = m.jobOutputPath(id)
	m.setPhase(id, phaseConversion, 84, "converting textured model to OSGB")
	if err := m.runCommand(id, m.config.OSGBBin, m.config.OSGBArgs, vars, projectDir, nil); err != nil {
		m.fail(id, err)
		return
	}

	m.update(id, func(job *Job) {
		job.Status = "completed"
		job.Phase = phaseCompleted
		job.Progress = 100
	})
	m.logLine(id, "system", "OSGB conversion completed: "+m.jobOutputPath(id))
}

// normalizeOpenMVSPly makes OpenMVS-generated binary PLY headers compatible with
// the OSG 3.6.5 PLY plugin. It rewrites header type names only; binary payload
func (m *JobManager) runNative(id, imageDir, jobDir, manifest string) {
	database := filepath.Join(jobDir, "database.db")
	sparse := filepath.Join(jobDir, "sparse")
	undistorted := filepath.Join(jobDir, "dense")
	scene := filepath.Join(jobDir, "scene.mvs")
	dense := filepath.Join(jobDir, "dense.mvs")
	mesh := filepath.Join(jobDir, "mesh.mvs")
	refined := filepath.Join(jobDir, "refined.mvs")
	textured := filepath.Join(jobDir, "textured.mvs")
	if err := os.MkdirAll(sparse, 0o755); err != nil {
		m.fail(id, fmt.Errorf("create sparse workspace: %w", err))
		return
	}
	common := map[string]string{
		"{job_dir}": jobDir, "{input_dir}": imageDir, "{manifest}": manifest,
		"{database}": database, "{sparse_dir}": sparse, "{dense_dir}": undistorted, "{scene_mvs}": scene,
		"{dense_mvs}": dense, "{mesh_mvs}": mesh, "{textured_mvs}": textured,
		"{refined_mvs}": refined,
		"{output_path}": m.jobOutputPath(id),
	}
	// A previous OpenMVS run may have already exported a usable mesh PLY even
	// when writing mesh.mvs failed (for example on memory pressure). Reuse it
	// as a delivery fallback and avoid repeating the expensive reconstruction.
	ply := filepath.Join(jobDir, "complete_mesh.ply")
	if !fileExists(ply) {
		ply = filepath.Join(jobDir, "dense_mesh.ply")
	}
	if fileExists(ply) {
		m.logLine(id, "system", "Preparing OpenMVS PLY for OSGB conversion")
		if err := convertFullmeshFaceIndicesToUnsignedByte(ply); err != nil {
			m.fail(id, fmt.Errorf("prepare fullmesh PLY: %w", err))
			return
		}
		if _, err := normalizeOpenMVSPly(ply); err != nil {
			m.fail(id, fmt.Errorf("normalize OpenMVS PLY: %w", err))
			return
		}
		if err := validateOSGBInput(ply); err != nil {
			m.fail(id, fmt.Errorf("validate PLY input: %w", err))
			return
		}
		common["{model_path}"] = ply
		common["{obj_path}"] = ply
		m.update(id, func(job *Job) { job.ModelPath = ply })
		if err := os.MkdirAll(filepath.Dir(m.jobOutputPath(id)), 0o755); err != nil {
			m.fail(id, err)
			return
		}
		m.setPhase(id, phaseConversion, 96, "OpenSceneGraph osgconv: PLY -> OSGB")
		if err := m.runCommand(id, m.config.OSGConvBin, `"{obj_path}" "{output_path}"`, common, jobDir, nil); err != nil {
			m.fail(id, err)
			return
		}
		if err := m.validateOSGBOutput(id, m.jobOutputPath(id), ply); err != nil {
			m.fail(id, fmt.Errorf("validate OSGB output: %w", err))
			return
		}
		m.update(id, func(job *Job) { job.Status = "completed"; job.Phase = phaseCompleted; job.Progress = 100 })
		m.logLine(id, "system", "OSGB conversion completed and validated: "+m.jobOutputPath(id))
		return
	}
	// Record the compute plan before the expensive stages start. Feature
	// matching dominates total runtime, so making the chosen strategy, the
	// candidate pair count and the worker count visible in the log is what makes
	// a slow run diagnosable instead of mysterious.
	imageCount := 0
	if current := m.snapshot(id); current != nil {
		imageCount = current.InputCount
	}
	m.logLine(id, "system", fmt.Sprintf("%s; threads=%d", colmapCPUNote, m.config.resolveThreads()))
	m.logLine(id, "system", describeMatcherStrategy(m.config, imageCount))
	steps := []struct {
		phase string
		pct   int
		name  string
		bin   string
		args  string
		work  string
		// argv, when non-nil, is used verbatim instead of expanding args via the
		// placeholder template. It exists for the COLMAP stages, whose option
		// names depend on the detected COLMAP version, and whose CPU-only
		// switches and thread counts are computed rather than hard-coded.
		argv []string
	}{
		{phaseFeatures, 14, "Extract SIFT features (COLMAP, CPU)", m.config.ColmapBin, "",
			jobDir, featureExtractorArgs(m.config, database, imageDir)},
		{phaseMatching, 24, "Feature matching (COLMAP, CPU)", m.config.ColmapBin, "",
			jobDir, matcherArgs(m.config, database)},
		{phaseSfM, 36, "SfM and BA (COLMAP)", m.config.ColmapBin, `mapper --database_path "{database}" --image_path "{input_dir}" --output_path "{sparse_dir}"`, jobDir, nil},
		{phaseDense, 46, "Undistort images (COLMAP)", m.config.ColmapBin, `image_undistorter --image_path "{input_dir}" --input_path "{sparse_dir}/0" --output_path "{dense_dir}" --output_type COLMAP --num_threads ` + fmt.Sprint(m.config.resolveThreads()), jobDir, nil},
		{phaseDense, 52, "Import COLMAP scene (OpenMVS)", m.config.InterfaceBin, `-i "{dense_dir}" -o "{scene_mvs}" --image-folder "{dense_dir}/images"`, jobDir, nil},
		{phaseDense, 64, "Dense fusion (OpenMVS)", m.config.DensifyBin, m.config.DenseArgs, jobDir, nil},
		// OpenMVS 2.4: -p is the input point-cloud override; -o is the mesh
		// output prefix (it writes mesh.mvs and mesh.ply).  Passing mesh.mvs to
		// -p makes the following RefineMesh stage try to load an invalid PLY.
		{phaseMesh, 74, "Mesh reconstruction (OpenMVS)", m.config.ReconstructBin, `"{dense_mvs}" -o "{mesh_mvs}" --max-threads ` + fmt.Sprint(m.config.resolveThreads()), jobDir, nil},
		{phaseMesh, 80, "Mesh refinement (OpenMVS)", m.config.RefineBin, `"{dense_mvs}" -m "{mesh_mvs}" -o "{refined_mvs}" --max-threads ` + fmt.Sprint(m.config.resolveThreads()), jobDir, nil},
		{phaseTexture, 86, "Texture mapping (OpenMVS)", m.config.TextureBin, `"{dense_mvs}" -m "{refined_mvs}" -o "{textured_mvs}" --export-type obj --max-threads ` + fmt.Sprint(m.config.resolveThreads()), jobDir, nil},
	}
	for _, step := range steps {
		if m.checkpointExists(jobDir, step.name) {
			m.logLine(id, "system", "Checkpoint found, skipping completed step: "+step.name)
			continue
		}
		m.setPhase(id, step.phase, step.pct, step.name)
		if err := m.runCommand(id, step.bin, step.args, common, step.work, step.argv); err != nil {
			m.update(id, func(job *Job) { job.Resumable = true; job.Checkpoint = step.name })
			m.fail(id, fmt.Errorf("%s: %w", step.name, err))
			return
		}
		m.writeCheckpoint(jobDir, step.name)
		m.update(id, func(job *Job) { job.Checkpoint = step.name })
	}
	m.setPhase(id, phaseGeoreference, 90, "Check georeference and CRS")
	modelPath, err := findModel(jobDir)
	if err != nil {
		m.fail(id, err)
		return
	}
	common["{model_path}"] = modelPath
	if m.config.GeorefBin != "" {
		if err := m.runCommand(id, m.config.GeorefBin, m.config.GeorefArgs, common, jobDir, nil); err != nil {
			m.fail(id, fmt.Errorf("georeference and CRS check failed: %w", err))
			return
		}
	} else {
		m.logLine(id, "system", "No georeference tool configured (GEOREF_BIN is empty); skipping CRS check")
	}
	m.update(id, func(job *Job) { job.ModelPath = modelPath })
	m.setPhase(id, phaseLOD, 93, "Generate generic OSGB nodes")
	if m.config.LODBin != "" {
		if err := m.runCommand(id, m.config.LODBin, m.config.LODArgs, common, jobDir, nil); err != nil {
			m.fail(id, fmt.Errorf("optional LOD preprocessing failed: %w", err))
			return
		}
	} else {
		m.logLine(id, "system", "No external LOD tool configured (LOD_BIN is empty); the built-in Smart3D PagedLOD builder will produce the tree")
	}
	if m.config.OSGConvBin == "" {
		m.fail(id, errors.New("OSGCONV_BIN is not configured"))
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.jobOutputPath(id)), 0o755); err != nil {
		m.fail(id, fmt.Errorf("create output directory: %w", err))
		return
	}
	common["{obj_path}"] = modelPath
	common["{model_path}"] = modelPath
	common["{output_path}"] = m.jobOutputPath(id)
	if err := validateOSGBInput(modelPath); err != nil {
		m.fail(id, fmt.Errorf("validate model input: %w", err))
		return
	}
	m.setPhase(id, phaseConversion, 96, "Build Smart3D PagedLOD OSGB tree")
	if err := m.runSmart3DPagedLOD(id, modelPath); err != nil {
		m.fail(id, fmt.Errorf("Smart3D PagedLOD conversion: %w", err))
		return
	}
	m.update(id, func(job *Job) { job.Status = "completed"; job.Phase = phaseCompleted; job.Progress = 100 })
	m.logLine(id, "system", "Smart3D PagedLOD conversion completed and validated: "+m.jobOutputPath(id))
}

func (m *JobManager) runDenseToOSGB(id, jobDir string) {
	job := m.snapshot(id)
	if job == nil {
		return
	}
	scene := filepath.Join(jobDir, "scene.mvs")
	dense := filepath.Join(jobDir, "dense.mvs")
	mesh := filepath.Join(jobDir, "mesh.mvs")
	refined := filepath.Join(jobDir, "refined.mvs")
	textured := filepath.Join(jobDir, "textured.mvs")
	common := map[string]string{
		"{job_dir}": jobDir, "{scene_mvs}": scene, "{dense_mvs}": dense,
		"{mesh_mvs}": mesh, "{refined_mvs}": refined, "{textured_mvs}": textured,
		"{output_path}": m.jobOutputPath(id),
	}
	// DensifyPointCloud writes dense.mvs and dense.ply. Preserve these
	// checkpoints when resuming so a completed multi-hour dense pass is not
	// repeated after a downstream failure.
	if _, err := os.Stat(dense); err == nil {
		m.logLine(id, "system", "Reusing existing dense.mvs; skipping dense reconstruction")
	} else {
		m.setPhase(id, phaseDense, 64, "Continue dense reconstruction from saved scene (OpenMVS)")
		if err := m.runCommand(id, m.config.DensifyBin, m.config.DenseArgs, common, jobDir, nil); err != nil {
			m.fail(id, fmt.Errorf("Dense fusion (OpenMVS) %w", err))
			return
		}
	}
	if ply := filepath.Join(jobDir, "dense_mesh.ply"); fileExists(ply) {
		m.logLine(id, "system", "Preparing OpenMVS PLY for OSGB conversion")
		if err := convertFullmeshFaceIndicesToUnsignedByte(ply); err != nil {
			m.fail(id, fmt.Errorf("prepare fullmesh PLY: %w", err))
			return
		}
		if _, err := normalizeOpenMVSPly(ply); err != nil {
			m.fail(id, fmt.Errorf("normalize OpenMVS PLY: %w", err))
			return
		}
		if err := validateOSGBInput(ply); err != nil {
			m.fail(id, fmt.Errorf("validate PLY input: %w", err))
			return
		}
		common["{model_path}"] = ply
		common["{obj_path}"] = ply
		m.update(id, func(job *Job) { job.ModelPath = ply })
		if err := os.MkdirAll(filepath.Dir(m.jobOutputPath(id)), 0o755); err != nil {
			m.fail(id, err)
			return
		}
		m.setPhase(id, phaseConversion, 96, "OpenSceneGraph osgconv: PLY -> OSGB")
		if err := m.runCommand(id, m.config.OSGConvBin, `"{obj_path}" "{output_path}"`, common, jobDir, nil); err != nil {
			m.fail(id, err)
			return
		}
		if err := m.validateOSGBOutput(id, m.jobOutputPath(id), ply); err != nil {
			m.fail(id, fmt.Errorf("validate OSGB output: %w", err))
			return
		}
		m.update(id, func(job *Job) { job.Status = "completed"; job.Phase = phaseCompleted; job.Progress = 100 })
		m.logLine(id, "system", "OSGB conversion completed and validated: "+m.jobOutputPath(id))
		return
	}
	steps := []struct {
		phase           string
		pct             int
		name, bin, args string
	}{
		{phaseMesh, 74, "Mesh reconstruction (OpenMVS)", m.config.ReconstructBin, `"{dense_mvs}" -o "{mesh_mvs}" --max-threads ` + fmt.Sprint(m.config.resolveThreads())},
		{phaseMesh, 80, "Mesh refinement (OpenMVS)", m.config.RefineBin, `"{dense_mvs}" -m "{mesh_mvs}" -o "{refined_mvs}" --max-threads ` + fmt.Sprint(m.config.resolveThreads())},
		{phaseTexture, 86, "Texture mapping (OpenMVS)", m.config.TextureBin, `"{dense_mvs}" -m "{refined_mvs}" -o "{textured_mvs}" --export-type obj --max-threads ` + fmt.Sprint(m.config.resolveThreads())},
	}
	for _, step := range steps {
		if m.checkpointExists(jobDir, step.name) {
			m.logLine(id, "system", "Checkpoint found, skipping completed step: "+step.name)
			continue
		}
		m.setPhase(id, step.phase, step.pct, step.name)
		if err := m.runCommand(id, step.bin, step.args, common, jobDir, nil); err != nil {
			m.update(id, func(job *Job) { job.Resumable = true; job.Checkpoint = step.name })
			m.fail(id, fmt.Errorf("%s: %w", step.name, err))
			return
		}
		m.writeCheckpoint(jobDir, step.name)
		m.update(id, func(job *Job) { job.Checkpoint = step.name })
	}
	modelPath, err := findModel(jobDir)
	if err != nil {
		m.fail(id, err)
		return
	}
	common["{model_path}"], common["{obj_path}"] = modelPath, modelPath
	common["{output_path}"] = m.jobOutputPath(id)
	m.update(id, func(job *Job) { job.ModelPath = modelPath })
	m.setPhase(id, phaseGeoreference, 90, "Check georeference and CRS")
	if m.config.GeorefBin != "" {
		if err := m.runCommand(id, m.config.GeorefBin, m.config.GeorefArgs, common, jobDir, nil); err != nil {
			m.fail(id, fmt.Errorf("georeference and CRS check failed: %w", err))
			return
		}
	} else {
		m.logLine(id, "system", "No georeference tool configured (GEOREF_BIN is empty); skipping CRS check")
	}
	m.setPhase(id, phaseLOD, 93, "Generate generic OSGB nodes")
	if m.config.LODBin != "" {
		if err := m.runCommand(id, m.config.LODBin, m.config.LODArgs, common, jobDir, nil); err != nil {
			m.fail(id, fmt.Errorf("optional LOD preprocessing failed: %w", err))
			return
		}
	} else {
		m.logLine(id, "system", "No external LOD tool configured (LOD_BIN is empty); the built-in Smart3D PagedLOD builder will produce the tree")
	}
	if err := os.MkdirAll(filepath.Dir(m.jobOutputPath(id)), 0o755); err != nil {
		m.fail(id, fmt.Errorf("create output directory: %w", err))
		return
	}
	m.setPhase(id, phaseConversion, 96, "Build Smart3D PagedLOD OSGB tree")
	if err := m.runSmart3DPagedLOD(id, modelPath); err != nil {
		m.fail(id, fmt.Errorf("Smart3D PagedLOD conversion: %w", err))
		return
	}
	m.update(id, func(job *Job) { job.Status = "completed"; job.Phase = phaseCompleted; job.Progress = 100 })
	m.logLine(id, "system", "Smart3D PagedLOD conversion completed and validated: "+m.jobOutputPath(id))
}

// preprocessImages creates a reproducible manifest and validates readable image headers.
// It deliberately does not transcode pixels, preserving survey originals bit-for-bit.
func (m *JobManager) preprocessImages(id, imageDir, jobDir string) (string, error) {
	manifestPath := filepath.Join(jobDir, "images.manifest.json")
	type item struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		Width  int    `json:"width,omitempty"`
		Height int    `json:"height,omitempty"`
	}
	items := make([]item, 0)
	err := filepath.WalkDir(imageDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !isSupportedImage(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := item{Path: path, Size: info.Size()}
		if file, err := os.Open(path); err == nil {
			if bounds, _, decodeErr := image.DecodeConfig(file); decodeErr == nil {
				value.Width, value.Height = bounds.Width, bounds.Height
			}
			_ = file.Close()
		}
		items = append(items, value)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("preprocess images: %w", err)
	}
	file, err := os.Create(manifestPath)
	if err != nil {
		return "", fmt.Errorf("write image manifest: %w", err)
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(map[string]any{"created_at": time.Now(), "image_count": len(items), "images": items}); err != nil {
		return "", fmt.Errorf("write image manifest: %w", err)
	}
	m.logLine(id, "system", fmt.Sprintf("Preprocess complete: %d images; manifest=%s", len(items), manifestPath))
	return manifestPath, nil
}

func (m *JobManager) jobOutputPath(id string) string {
	job := m.snapshot(id)
	return job.OutputPath
}

// runSmart3DPagedLOD converts a textured OBJ into a Smart3D-style PagedLOD
// OSGB tree rooted at the directory of job.OutputPath. It is used when the
// source model is a textured OBJ produced by OpenMVS TextureMesh.
func (m *JobManager) runSmart3DPagedLOD(id, modelPath string) error {
	mesh, err := parseObjTextured(modelPath)
	if err != nil {
		return fmt.Errorf("parse OBJ for PagedLOD: %w", err)
	}
	texMap := parseMTL(mesh)
	outputPath := m.jobOutputPath(id)
	if outputPath == "" {
		return fmt.Errorf("job output path is empty")
	}
	outDir := filepath.Dir(outputPath)
	logFn := func(msg string) {
		m.logLine(id, "system", msg)
	}
	// Size the tile grid and LOD depth from the model unless the operator
	// pinned them. A fixed 16x16 grid wasted work on small models (hundreds of
	// empty tiles) while leaving large models with oversized tiles.
	opts := resolveTreeOptions(len(mesh.triangles), m.config.TileGrid, m.config.LODLevels, logFn)
	m.logLine(id, "system", fmt.Sprintf(
		"Building Smart3D PagedLOD tree: model=%s outDir=%s faces=%d grid=%dx%d lod_levels=%d",
		modelPath, outDir, len(mesh.triangles), opts.GridX, opts.GridY, opts.LODLevels))
	if opts.GridX <= 0 || opts.GridY <= 0 || opts.LODLevels <= 0 {
		return fmt.Errorf("resolved an invalid tree layout: grid=%dx%d levels=%d", opts.GridX, opts.GridY, opts.LODLevels)
	}
	if err := buildSmart3DOSGBWithOptions(mesh, texMap, m.config.OSGConvBin, outDir, opts); err != nil {
		return fmt.Errorf("build Smart3D PagedLOD: %w", err)
	}
	logFn("Validating Smart3D PagedLOD tree...")
	stats, verr := validateSmart3DTree(m.config.OSGConvBin, outDir, logFn)
	if verr != nil {
		return fmt.Errorf("validate Smart3D PagedLOD: %w", verr)
	}
	if len(stats) == 0 {
		return fmt.Errorf("no OSGB files produced")
	}
	failed := 0
	var totalVerts, totalFaces int64
	var failedNames []string
	for _, s := range stats {
		if !s.Valid {
			failed++
			if len(failedNames) < maxReportedFailedTiles {
				failedNames = append(failedNames, s.Name+": "+s.Error)
			}
		}
		totalVerts += s.Vertices
		totalFaces += s.Faces
	}
	summary := osgbOutputStats{
		Vertices:    totalVerts,
		Faces:       totalFaces,
		Tiles:       int64(len(stats)),
		ValidTiles:  int64(len(stats) - failed),
		FailedTiles: int64(failed),
		Textured:    true,
	}
	m.update(id, func(job *Job) {
		job.Stats = &summary
	})

	// A fully invalid tree is never deliverable, and a tree that lost more than
	// the tolerated fraction of tiles is too incomplete to hand over. Everything
	// else is delivered with the damaged tiles reported explicitly, so an
	// overnight run is not discarded because of one bad tile.
	if failed > 0 {
		ratio := float64(failed) / float64(len(stats))
		logFn(fmt.Sprintf("VALIDATE SUMMARY: %d of %d tiles failed validation (%.2f%%); details: %s",
			failed, len(stats), ratio*100, strings.Join(failedNames, " | ")))
		if err := checkTileFailureTolerance(failed, len(stats)); err != nil {
			return fmt.Errorf("%w; first failures: %s", err, strings.Join(failedNames, " | "))
		}
		logFn(fmt.Sprintf("Continuing with partial delivery: %d valid tiles retained, %d failed tiles skipped",
			len(stats)-failed, failed))
	}
	logFn(fmt.Sprintf("Smart3D PagedLOD tree complete: tiles=%d verts=%d faces=%d", len(stats), totalVerts, totalFaces))
	return nil
}

const (
	// maxFailedTileRatio is the share of tiles that may fail validation while the
	// tree is still delivered. The delivered tree is complete enough to use, and
	// every failed tile is reported in the job log, stats and validation report.
	maxFailedTileRatio = 0.10
	// maxReportedFailedTiles bounds how many failure details are logged, so a
	// systematically broken run does not emit hundreds of identical lines.
	maxReportedFailedTiles = 10
)

// checkTileFailureTolerance decides whether a tree with some invalid tiles may
// still be delivered. It returns nil when the damage is within tolerance, and a
// descriptive error when the result would be too incomplete to be useful.
//
// This is factored out of the pipeline so the policy is unit-testable without
// running osgconv over a real mesh.
func checkTileFailureTolerance(failed, total int) error {
	if failed <= 0 {
		return nil
	}
	if total <= 0 {
		return errors.New("no tiles were produced")
	}
	if failed >= total {
		return fmt.Errorf("Smart3D validation failed for all %d tiles", total)
	}
	ratio := float64(failed) / float64(total)
	if ratio > maxFailedTileRatio {
		return fmt.Errorf("Smart3D validation failed for %d of %d tiles (%.2f%%), above the tolerated %.2f%%",
			failed, total, ratio*100, maxFailedTileRatio*100)
	}
	return nil
}
func (m *JobManager) setPhase(id, phase string, progress int, message string) {
	m.update(id, func(job *Job) {
		job.Status = "running"
		job.Phase = phase
		job.Progress = progress
	})
	m.logLine(id, "system", message)
}

func (m *JobManager) checkpointExists(jobDir, name string) bool {
	_, err := os.Stat(filepath.Join(jobDir, ".checkpoint-"+safeCheckpointName(name)))
	return err == nil
}

func (m *JobManager) writeCheckpoint(jobDir, name string) {
	path := filepath.Join(jobDir, ".checkpoint-"+safeCheckpointName(name))
	_ = os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}

func safeCheckpointName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func (m *JobManager) fail(id string, err error) {
	if current := m.snapshot(id); current != nil && current.Status == "failed" && current.Error == errJobcanceled {
		return
	}
	m.update(id, func(job *Job) {
		job.Status = "failed"
		// Keep the current phase: it identifies the step that actually failed.
		job.Error = humanizeJobError(err)
	})
	m.logLine(id, "fatal", humanizeJobError(err))
	m.logger.Error("job failed", "job_id", id, "error", err)
}

func humanizeJobError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "signal: killed"):
		return message + "; 进程被系统终止，通常表示 Docker/Linux 内存不足。请增加 Docker 内存，或降低 CPU 线程数和影像尺寸。"
	case strings.Contains(message, "context deadline exceeded"):
		return message + "; 当前阶段执行超时。"
	default:
		return message
	}
}

// runCommand executes one external pipeline stage.
//
// When argv is non-nil it is used verbatim, which is how the COLMAP stages pass
// arguments that were computed at run time (version-dependent option names, CPU
// thread counts). Otherwise argTemplate is expanded through the placeholder map.
//
// The subprocess environment is forced CPU-only: GPU-related variables are
// cleared so that neither COLMAP nor OpenMVS can attempt CUDA initialisation in
// the deployment container, which has no GPU or NVIDIA runtime.
func (m *JobManager) runCommand(id, binary, argTemplate string, vars map[string]string, workDir string, argv []string) error {
	args := argv
	if args == nil {
		expanded, err := expandArgs(argTemplate, vars)
		if err != nil {
			return fmt.Errorf("parse command arguments: %w", err)
		}
		args = expanded
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("command %q not found: %w", binary, err)
	}
	m.logLine(id, "system", fmt.Sprintf("Running: %s %s", resolved, strings.Join(args, " ")))
	// The context is intentionally rooted at Background rather than derived from
	// any HTTP request: a job is a long-running background task that must
	// survive the browser closing the page or refreshing it. Cancellation comes
	// from two explicit sources instead -- the per-command timeout below, and
	// m.cancel[id] when the operator stops the job via the API.
	ctx, cancel := context.WithTimeout(context.Background(), m.config.CommandTimeout)
	defer cancel()
	m.mu.Lock()
	m.cancel[id] = cancel
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.cancel, id)
		m.mu.Unlock()
	}()
	cmd := exec.CommandContext(ctx, resolved, args...)
	cmd.Dir = workDir
	// COLMAP links Qt and initializes a platform plugin even for CLI commands.
	// Force a headless backend so Docker/Linux runs do not require DISPLAY/X11.
	// CUDA_VISIBLE_DEVICES="" is the strongest portable way to hide any GPU that
	// might be present, backing up the per-command use_gpu=0 switches.
	cmd.Env = append(os.Environ(),
		"QT_QPA_PLATFORM=offscreen",
		"DISPLAY=",
		"CUDA_VISIBLE_DEVICES=",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", binary, err)
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go m.scanOutput(id, "stdout", stdout, &readers)
	go m.scanOutput(id, "stderr", stderr, &readers)
	waitErr := cmd.Wait()
	readers.Wait()
	if ctx.Err() != nil {
		return fmt.Errorf("command %s timed out or was canceled: %w", binary, ctx.Err())
	}
	if waitErr != nil {
		return fmt.Errorf("command %s failed: %w", binary, waitErr)
	}
	return nil
}

func (m *JobManager) scanOutput(id, stream string, reader io.Reader, readers *sync.WaitGroup) {
	defer readers.Done()
	scanner := bufio.NewScanner(reader)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	for scanner.Scan() {
		m.logLine(id, stream, scanner.Text())
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) && !strings.Contains(err.Error(), "file already closed") {
		m.logLine(id, stream, "output read error: "+err.Error())
	}
}
