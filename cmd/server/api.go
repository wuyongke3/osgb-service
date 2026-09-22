package main

// api.go implements the HTTP surface: configuration, uploads, jobs, plans,
// server-sent events and the static frontend.

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func (m *JobManager) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "go": runtime.Version()})
}

func (m *JobManager) writeConfig(w http.ResponseWriter) {
	config := m.configSnapshot()
	dependencies := map[string]bool{}
	for name, binary := range map[string]string{
		"colmap": config.ColmapBin, "interface_colmap": config.InterfaceBin,
		"densify_point_cloud": config.DensifyBin, "reconstruct_mesh": config.ReconstructBin,
		"refine_mesh": config.RefineBin, "texture_mesh": config.TextureBin,
		"osgconv": config.OSGConvBin, "legacy_reconstruction": config.ReconBin,
		"legacy_osgb": config.OSGBBin,
	} {
		dependencies[name] = commandAvailable(binary)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pipeline_mode":             config.PipelineMode,
		"reconstruction_configured": config.ReconBin != "",
		"reconstruction_binary":     config.ReconBin,
		"osgb_configured":           config.OSGBBin != "",
		"osgb_binary":               config.OSGBBin,
		"georef_configured":         commandAvailable(config.GeorefBin),
		"lod_configured":            commandAvailable(config.LODBin),
		"native_pipeline_ready":     nativePipelineReady(config),
		"dependencies":              dependencies,
		"tool_paths":                toolPaths(config),
		"config_file":               config.ConfigFile,
		"output_root":               config.OutputRoot,
		"native_directory_picker":   runtime.GOOS == "windows",
		"deployment":                deploymentSnapshot(),
	})
}

// deploymentSnapshot deliberately describes the safe Docker installation
// workflow instead of attempting to run apt or Docker from the HTTP process.
// The service runs as an unprivileged user in the production image; granting it
// access to the Docker socket would effectively grant the Web UI host-root
// privileges. Tools are therefore installed reproducibly while rebuilding the
// image from Dockerfile.
func deploymentSnapshot() map[string]any {
	if isContainerRuntime() {
		return map[string]any{
			"kind":                 "docker",
			"tool_install_mode":    "image-build",
			"tool_install_command": "docker compose up -d --build --force-recreate",
			"automatic":            true,
			"can_execute_here":     false,
			"note":                 "工具在 Dockerfile 构建阶段自动安装；Web 服务不直接执行 apt，也不挂载 Docker socket。",
		}
	}
	return map[string]any{
		"kind":                 runtime.GOOS,
		"tool_install_mode":    "manual",
		"tool_install_command": "",
		"automatic":            false,
		"can_execute_here":     false,
		"note":                 "当前服务未运行在受支持的容器安装环境中，请通过系统包管理器安装工具。",
	}
}

func isContainerRuntime() bool {
	if strings.EqualFold(os.Getenv("CONTAINER_RUNTIME"), "docker") {
		return true
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}

func (m *JobManager) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		m.writeConfig(w)
		return
	}
	if r.Method != http.MethodPut {
		methodNotAllowed(w)
		return
	}
	var request struct {
		Tools map[string]string `json:"tools"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&request); err != nil || len(request.Tools) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_TOOL_CONFIG", "tools must contain one or more supported tool paths")
		return
	}
	if err := m.updateToolPaths(request.Tools); err != nil {
		writeError(w, http.StatusConflict, "TOOL_CONFIG_UPDATE_FAILED", err.Error())
		return
	}
	m.writeConfig(w)
}

func (m *JobManager) handlePickDirectory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if runtime.GOOS != "windows" {
		writeError(w, http.StatusNotImplemented, "PICKER_UNAVAILABLE", "native directory picker is currently available on Windows only")
		return
	}
	path, err := pickWindowsPath(r.URL.Query().Get("kind"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PICKER_ERROR", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
}

func safeUploadRelativePath(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	clean := pathpkg.Clean(strings.TrimPrefix(name, "/"))
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") || clean == ".." || pathpkg.IsAbs(clean) {
		return "", errors.New("invalid uploaded filename")
	}
	return filepath.FromSlash(clean), nil
}

// handleUpload accepts one or more browser-selected image files. The browser
// may send directory-relative names, which are preserved beneath DATA_DIR.
func (m *JobManager) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, m.config.UploadMaxBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_UPLOAD", "upload could not be read or exceeds MAX_UPLOAD_GB")
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, "NO_FILES", "multipart field files is required")
		return
	}
	id := "upload-" + newID()
	destination := filepath.Join(m.config.DataDir, "uploads", id)
	var stored int
	for _, header := range files {
		relative, err := safeUploadRelativePath(header.Filename)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_FILENAME", err.Error())
			return
		}
		if !isSupportedImage(relative) {
			continue
		}
		path := filepath.Join(destination, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			writeError(w, http.StatusInternalServerError, "UPLOAD_STORAGE_ERROR", err.Error())
			return
		}
		source, err := header.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, "UPLOAD_READ_ERROR", err.Error())
			return
		}
		target, err := os.Create(path)
		if err == nil {
			_, err = io.Copy(target, source)
			closeErr := target.Close()
			if err == nil {
				err = closeErr
			}
		}
		_ = source.Close()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "UPLOAD_WRITE_ERROR", err.Error())
			return
		}
		stored++
	}
	count, bytes, err := inspectInputPath(destination)
	if err != nil || stored == 0 || count == 0 {
		writeError(w, http.StatusBadRequest, "NO_SUPPORTED_IMAGES", "upload must contain readable JPG, PNG, TIFF, WebP images")
		return
	}
	upload := Upload{ID: id, Path: destination, Files: count, Bytes: bytes, CreatedAt: time.Now()}
	data, _ := json.MarshalIndent(upload, "", "  ")
	_ = os.WriteFile(filepath.Join(destination, "upload.json"), data, 0o644)
	writeJSON(w, http.StatusCreated, upload)
}

func (m *JobManager) handlePlans(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/plans" {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{"plans": m.planSnapshots()})
			return
		}
		// Batch deletion: DELETE /api/plans with {"ids": [...]}.
		if r.Method == http.MethodDelete {
			m.handleDeletePlans(w, r, "")
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var request struct {
			Name        string `json:"name"`
			UploadID    string `json:"upload_id"`
			InputPath   string `json:"input_path"`
			ScheduledAt string `json:"scheduled_at"`
			StartNow    bool   `json:"start_now"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_JSON", "invalid plan request")
			return
		}
		request.Name = strings.TrimSpace(request.Name)
		request.UploadID = strings.TrimSpace(request.UploadID)
		request.InputPath = strings.TrimSpace(request.InputPath)
		if request.Name == "" || (request.UploadID == "" && request.InputPath == "") || (request.UploadID != "" && request.InputPath != "") {
			writeError(w, http.StatusBadRequest, "INVALID_PLAN", "name and exactly one of upload_id or input_path are required")
			return
		}
		var scheduledAt *time.Time
		if request.ScheduledAt != "" {
			parsed, err := time.Parse(time.RFC3339, request.ScheduledAt)
			if err != nil {
				writeError(w, http.StatusBadRequest, "INVALID_SCHEDULE", "scheduled_at must be RFC3339")
				return
			}
			scheduledAt = &parsed
		}
		if request.UploadID != "" {
			if _, _, err := inspectInputPath(filepath.Join(m.config.DataDir, "uploads", request.UploadID)); err != nil {
				writeError(w, http.StatusBadRequest, "INVALID_UPLOAD", "uploaded image set was not found")
				return
			}
		} else if _, _, err := inspectInputPath(request.InputPath); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_INPUT_PATH", err.Error())
			return
		}
		plan := m.createPlan(request.Name, request.UploadID, request.InputPath, scheduledAt)
		if request.StartNow {
			job, err := m.startPlan(plan.ID)
			if err != nil {
				writeError(w, http.StatusConflict, "PLAN_START_FAILED", err.Error())
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]any{"plan": m.planSnapshot(plan.ID), "job": job})
			return
		}
		writeJSON(w, http.StatusCreated, plan)
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "plans" && parts[3] == "run" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		job, err := m.startPlan(parts[2])
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, "PLAN_NOT_FOUND", "plan not found")
			} else {
				writeError(w, http.StatusConflict, "PLAN_START_FAILED", err.Error())
			}
			return
		}
		writeJSON(w, http.StatusAccepted, job)
		return
	}
	// Single-plan deletion: DELETE /api/plans/:id.
	if len(parts) == 3 && parts[0] == "api" && parts[1] == "plans" {
		m.handleDeletePlans(w, r, parts[2])
		return
	}
	http.NotFound(w, r)
}

func (m *JobManager) handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"jobs": m.snapshots()})
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var request struct {
		InputPath  string `json:"input_path"`
		OutputPath string `json:"output_path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "expected input_path and output_path")
		return
	}
	inputPath := strings.TrimSpace(request.InputPath)
	outputPath := strings.TrimSpace(request.OutputPath)
	count, bytes, err := inspectInputPath(inputPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_INPUT_PATH", err.Error())
		return
	}
	if err := validateOutputPath(outputPath, m.config.OutputRoot); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_OUTPUT_PATH", err.Error())
		return
	}
	outputPath = normalizeOutputPath(outputPath)
	id := newID()
	job := m.create(id, inputPath, outputPath, count, bytes)
	go m.run(id)
	writeJSON(w, http.StatusAccepted, job)
}

func (m *JobManager) handleCancel(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	if err := m.cancelJob(id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "job not found")
			return
		}
		writeError(w, http.StatusConflict, "JOB_NOT_RUNNING", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, m.snapshot(id))
}

func (m *JobManager) handleDownload(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	job := m.snapshot(id)
	if job == nil {
		writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "job not found")
		return
	}
	if job.Status != "completed" {
		writeError(w, http.StatusConflict, "ARTIFACT_NOT_READY", "OSGB artifact is not ready")
		return
	}
	if job.PlanID == "" {
		writeError(w, http.StatusConflict, "DOWNLOAD_REQUIRES_PLAN", "create and run a production plan before downloading an artifact package")
		return
	}
	root := filepath.Dir(job.OutputPath)
	deliverablesRoot := filepath.Join(m.config.DataDir, "deliverables")
	if !pathWithin(deliverablesRoot, root) {
		writeError(w, http.StatusForbidden, "INVALID_ARTIFACT_PATH", "artifact is outside the managed deliverables directory")
		return
	}
	if _, err := os.Stat(filepath.Join(root, "root.osgb")); err != nil {
		// Non-PagedLOD external workflows may write directly to OutputPath.
		if _, directErr := os.Stat(job.OutputPath); directErr != nil {
			writeError(w, http.StatusNotFound, "ARTIFACT_NOT_FOUND", "OSGB artifact was not found")
			return
		}
		root = filepath.Dir(job.OutputPath)
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"osgb-%s.zip\"", id))
	zipWriter := zip.NewWriter(w)
	defer zipWriter.Close()
	err := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		if strings.HasPrefix(filepath.ToSlash(relative), "work/") || strings.HasSuffix(strings.ToLower(relative), ".osgt") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relative)
		header.Method = zip.Deflate
		target, err := zipWriter.CreateHeader(header)
		if err != nil {
			return err
		}
		source, err := os.Open(filePath)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(target, source)
		closeErr := source.Close()
		return firstError(copyErr, closeErr)
	})
	if err != nil {
		m.logger.Error("download archive failed", "job_id", id, "error", err)
	}
}

func pathWithin(root, candidate string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(rootAbs, candidateAbs)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func commandAvailable(binary string) bool {
	if binary == "" {
		return false
	}
	_, err := exec.LookPath(binary)
	return err == nil
}

func nativePipelineReady(config Config) bool {
	if !strings.EqualFold(config.PipelineMode, "native") {
		return false
	}
	return commandAvailable(config.ColmapBin) && commandAvailable(config.InterfaceBin) &&
		commandAvailable(config.DensifyBin) && commandAvailable(config.ReconstructBin) &&
		commandAvailable(config.RefineBin) && commandAvailable(config.TextureBin) &&
		commandAvailable(config.OSGConvBin)
}

func normalizeOutputPath(path string) string {
	clean := filepath.Clean(path)
	if strings.EqualFold(filepath.Ext(clean), ".osgb") {
		return clean
	}
	return filepath.Join(clean, "model.osgb")
}

func (m *JobManager) handleJob(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "api" || parts[1] != "jobs" {
		http.NotFound(w, r)
		return
	}
	id := parts[2]
	if len(parts) == 4 && parts[3] == "cancel" {
		m.handleCancel(w, r, id)
		return
	}
	if len(parts) == 4 && parts[3] == "resume-dense" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if err := m.resumeFromDense(id); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "job not found")
			} else {
				writeError(w, http.StatusConflict, "RESUME_NOT_AVAILABLE", err.Error())
			}
			return
		}
		writeJSON(w, http.StatusAccepted, m.snapshot(id))
		return
	}
	if len(parts) == 4 && parts[3] == "resume" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if err := m.resumeJob(id); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "job not found")
			} else {
				writeError(w, http.StatusConflict, "RESUME_NOT_AVAILABLE", err.Error())
			}
			return
		}
		writeJSON(w, http.StatusAccepted, m.snapshot(id))
		return
	}
	if len(parts) == 4 && parts[3] == "events" {
		m.handleEvents(w, r, id)
		return
	}
	if len(parts) == 4 && parts[3] == "download" {
		m.handleDownload(w, r, id)
		return
	}
	job := m.snapshot(id)
	if job == nil {
		writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (m *JobManager) handleEvents(w http.ResponseWriter, r *http.Request, id string) {
	channel, unsubscribe, err := m.subscribe(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "job not found")
		return
	}
	defer unsubscribe()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "SSE_UNSUPPORTED", "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	writeSSE(w, "snapshot", m.snapshot(id))
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case update, open := <-channel:
			if !open {
				return
			}
			writeSSE(w, update.Type, update.Job)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func (m *JobManager) serveStatic(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(filepath.Clean(r.URL.Path), string(filepath.Separator))
	if path == "." || path == "" {
		path = "index.html"
	}
	if strings.HasPrefix(path, "api") {
		http.NotFound(w, r)
		return
	}
	filePath := filepath.Join("web", filepath.FromSlash(path))
	if _, err := os.Stat(filePath); err != nil {
		filePath = filepath.Join("web", "index.html")
	}
	http.ServeFile(w, r, filePath)
}

func (m *JobManager) jobRoutes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/jobs" {
		m.handleCreate(w, r)
		return
	}
	m.handleJob(w, r)
}
