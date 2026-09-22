package main

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	phasePreparing      = "preparing"
	phaseFeatures       = "features"
	phaseMatching       = "matching"
	phaseSfM            = "sfm"
	phaseDense          = "dense"
	phaseMesh           = "mesh"
	phaseTexture        = "texture"
	phaseGeoreference   = "georeference"
	phaseLOD            = "lod"
	phaseConversion     = "osgb_conversion"
	phaseReconstruction = "reconstruction" // legacy/external mode
	phaseLocating       = "locating_model" // legacy/external mode
	phaseCompleted      = "completed"
	phaseFailed         = "failed"
)

type Config struct {
	Port           string
	DataDir        string
	ConfigFile     string
	PipelineMode   string
	ColmapBin      string
	InterfaceBin   string
	DensifyBin     string
	ReconstructBin string
	RefineBin      string
	TextureBin     string
	OSGConvBin     string
	DenseArgs      string
	GeorefBin      string
	GeorefArgs     string
	LODBin         string
	LODArgs        string
	ReconBin       string
	ReconArgs      string
	OSGBBin        string
	OSGBArgs       string
	CommandTimeout time.Duration
	OutputRoot     string
	UploadMaxBytes int64
	// Matcher selects the COLMAP feature matching strategy. "sequential" is the
	// default because aerial surveys are captured in flight order, which makes
	// the exhaustive all-pairs search largely redundant work.
	Matcher string
	// MatcherOverlap is the number of neighbouring images each image is matched
	// against when Matcher is "sequential".
	MatcherOverlap int
	// Threads is the worker count handed to COLMAP and OpenMVS. Zero or less
	// means "derive from the number of available CPUs".
	Threads int
	// MaxImageSize caps the long edge fed to SIFT extraction. Lower values are
	// faster but find fewer features.
	MaxImageSize int
}

// resolveThreads returns the worker count to pass to subprocesses.
//
// The service runs CPU-only by design, so thread count is the main performance
// lever. It previously passed a hard-coded 4, which left most of a modern
// server idle: on a 32-core host that used 12% of the available compute.
//
// An explicit Threads setting always wins. Otherwise the count is derived as
// GOMAXPROCS minus one, leaving a core for the Go HTTP server, log scanning and
// the streaming goroutines that read subprocess output. The result is clamped to
// [1, maxAutoThreads] so a container limit of 128 CPUs does not spawn a
// pathological number of workers.
func (c Config) resolveThreads() int {
	if c.Threads > 0 {
		return c.Threads
	}
	available := runtime.GOMAXPROCS(0)
	if available > 1 {
		available--
	}
	if available < 1 {
		available = 1
	}
	if available > maxAutoThreads {
		available = maxAutoThreads
	}
	return available
}

// maxAutoThreads caps the automatically derived worker count. COLMAP and OpenMVS
// both allocate per-thread buffers, and the container runs under a memory limit,
// so unbounded parallelism trades speed for out-of-memory kills.
const maxAutoThreads = 16

func loadConfig() Config {
	dataDir := envOr("DATA_DIR", "./runtime")
	return Config{
		Port:           envOr("PORT", "8080"),
		DataDir:        dataDir,
		ConfigFile:     envOr("CONFIG_FILE", filepath.Join(dataDir, "service.env")),
		PipelineMode:   envOr("PIPELINE_MODE", "native"),
		ColmapBin:      envOr("COLMAP_BIN", "colmap"),
		InterfaceBin:   envOr("OPENMVS_INTERFACE_BIN", "InterfaceCOLMAP"),
		DensifyBin:     envOr("OPENMVS_DENSIFY_BIN", "DensifyPointCloud"),
		ReconstructBin: envOr("OPENMVS_RECONSTRUCT_BIN", "ReconstructMesh"),
		RefineBin:      envOr("OPENMVS_REFINE_BIN", "RefineMesh"),
		TextureBin:     envOr("OPENMVS_TEXTURE_BIN", "TextureMesh"),
		OSGConvBin:     envOr("OSGCONV_BIN", "osgconv"),
		DenseArgs:      envOr("OPENMVS_DENSIFY_ARGS", `"{scene_mvs}" -o "{dense_mvs}" --resolution-level 2 --max-resolution 1600 --number-views 4 --sub-resolution-levels 0 --fusion-mode 0`),
		GeorefBin:      os.Getenv("GEOREF_BIN"),
		GeorefArgs:     envOr("GEOREF_ARGS", `"{model_path}" "{job_dir}"`),
		LODBin:         os.Getenv("LOD_BIN"),
		LODArgs:        envOr("LOD_ARGS", `"{model_path}" "{job_dir}"`),
		ReconBin:       os.Getenv("RECON_BIN"),
		ReconArgs:      envOr("RECON_ARGS", `"{project_dir}" --skip-orthophoto --skip-report`),
		OSGBBin:        os.Getenv("OSGB_BIN"),
		OSGBArgs:       envOr("OSGB_ARGS", `"{obj_path}" "{output_path}"`),
		CommandTimeout: time.Duration(envInt64("COMMAND_TIMEOUT_HOURS", 24)) * time.Hour,
		OutputRoot:     os.Getenv("OUTPUT_ROOT"),
		UploadMaxBytes: envInt64("MAX_UPLOAD_GB", 50) * 1024 * 1024 * 1024,
		Matcher:        envOr("COLMAP_MATCHER", "sequential"),
		MatcherOverlap: int(envInt64("COLMAP_MATCHER_OVERLAP", 10)),
		Threads:        int(envInt64AllowZero("PIPELINE_THREADS", 0)),
		MaxImageSize:   int(envInt64("SIFT_MAX_IMAGE_SIZE", 3200)),
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt64(name string, fallback int64) int64 {
	if value := os.Getenv(name); value != "" {
		var parsed int64
		if _, err := fmt.Sscanf(value, "%d", &parsed); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

// envInt64AllowZero is like envInt64 but accepts 0 as a meaningful value, which
// is how an unset thread count ("auto-detect") is expressed.
func envInt64AllowZero(name string, fallback int64) int64 {
	if value := os.Getenv(name); value != "" {
		var parsed int64
		if _, err := fmt.Sscanf(value, "%d", &parsed); err == nil && parsed >= 0 {
			return parsed
		}
	}
	return fallback
}

type LogEntry struct {
	At      time.Time `json:"at"`
	Stream  string    `json:"stream"`
	Message string    `json:"message"`
}

type Job struct {
	ID         string           `json:"id"`
	Status     string           `json:"status"`
	Phase      string           `json:"phase"`
	Progress   int              `json:"progress"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
	OutputPath string           `json:"output_path"`
	InputPath  string           `json:"input_path"`
	InputCount int              `json:"input_count"`
	InputBytes int64            `json:"input_bytes"`
	ModelPath  string           `json:"model_path,omitempty"`
	Error      string           `json:"error,omitempty"`
	Stats      *osgbOutputStats `json:"stats,omitempty"`
	Logs       []LogEntry       `json:"logs"`
	PlanID     string           `json:"plan_id,omitempty"`
	SourceType string           `json:"source_type,omitempty"`
	Checkpoint string           `json:"checkpoint,omitempty"`
	Resumable  bool             `json:"resumable"`
}

// Plan is a persisted production definition. A plan may start immediately or
// wait until ScheduledAt, and every run creates an independent Job.
type Plan struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	UploadID    string     `json:"upload_id,omitempty"`
	InputPath   string     `json:"input_path,omitempty"`
	Status      string     `json:"status"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	LastJobID   string     `json:"last_job_id,omitempty"`
}

type Upload struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	Files     int       `json:"files"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
}

type toolPath struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Value     string `json:"value"`
	Available bool   `json:"available"`
	Required  bool   `json:"required"`
}

type event struct {
	Type string
	Job  *Job
}

type jobRecord struct {
	job         Job
	subscribers map[chan event]struct{}
}

type JobManager struct {
	mu     sync.RWMutex
	jobs   map[string]*jobRecord
	plans  map[string]*Plan
	cancel map[string]context.CancelFunc
	config Config
	logger *slog.Logger
}

func NewJobManager(config Config, logger *slog.Logger) *JobManager {
	manager := &JobManager{jobs: make(map[string]*jobRecord), plans: make(map[string]*Plan), cancel: make(map[string]context.CancelFunc), config: config, logger: logger}
	manager.loadPersisted()
	manager.loadPlans()
	go manager.runPlanScheduler()
	return manager
}

func (m *JobManager) configSnapshot() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

func toolPaths(config Config) []toolPath {
	items := []toolPath{
		{Key: "COLMAP_BIN", Label: "COLMAP", Value: config.ColmapBin, Required: true},
		{Key: "OPENMVS_INTERFACE_BIN", Label: "OpenMVS InterfaceCOLMAP", Value: config.InterfaceBin, Required: true},
		{Key: "OPENMVS_DENSIFY_BIN", Label: "OpenMVS DensifyPointCloud", Value: config.DensifyBin, Required: true},
		{Key: "OPENMVS_RECONSTRUCT_BIN", Label: "OpenMVS ReconstructMesh", Value: config.ReconstructBin, Required: true},
		{Key: "OPENMVS_REFINE_BIN", Label: "OpenMVS RefineMesh", Value: config.RefineBin, Required: true},
		{Key: "OPENMVS_TEXTURE_BIN", Label: "OpenMVS TextureMesh", Value: config.TextureBin, Required: true},
		{Key: "OSGCONV_BIN", Label: "OpenSceneGraph osgconv", Value: config.OSGConvBin, Required: true},
		{Key: "GEOREF_BIN", Label: "坐标转换工具（可选）", Value: config.GeorefBin},
		{Key: "LOD_BIN", Label: "LOD 前处理工具（可选）", Value: config.LODBin},
	}
	for i := range items {
		items[i].Available = commandAvailable(items[i].Value)
	}
	return items
}

func setToolPath(config *Config, key, value string) bool {
	switch key {
	case "COLMAP_BIN":
		config.ColmapBin = value
	case "OPENMVS_INTERFACE_BIN":
		config.InterfaceBin = value
	case "OPENMVS_DENSIFY_BIN":
		config.DensifyBin = value
	case "OPENMVS_RECONSTRUCT_BIN":
		config.ReconstructBin = value
	case "OPENMVS_REFINE_BIN":
		config.RefineBin = value
	case "OPENMVS_TEXTURE_BIN":
		config.TextureBin = value
	case "OSGCONV_BIN":
		config.OSGConvBin = value
	case "GEOREF_BIN":
		config.GeorefBin = value
	case "LOD_BIN":
		config.LODBin = value
	default:
		return false
	}
	return true
}

func writeToolConfig(path string, values map[string]string) error {
	existing := map[string]string{}
	data, err := os.ReadFile(path)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
			if len(parts) == 2 && parts[0] != "" && !strings.HasPrefix(parts[0], "#") {
				existing[parts[0]] = parts[1]
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for key, value := range values {
		existing[key] = value
	}
	keys := make([]string, 0, len(existing))
	for key := range existing {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	builder.WriteString("# Managed by OSGB service Web tool configuration.\n")
	for _, key := range keys {
		builder.WriteString(key + "=" + existing[key] + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(builder.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func (m *JobManager) updateToolPaths(values map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, record := range m.jobs {
		if record.job.Status == "queued" || record.job.Status == "running" {
			return errors.New("cannot change tool paths while a job is running or queued")
		}
	}
	for key := range values {
		copyConfig := m.config
		if !setToolPath(&copyConfig, key, "") {
			return fmt.Errorf("unsupported tool key: %s", key)
		}
	}
	for key, value := range values {
		setToolPath(&m.config, key, strings.TrimSpace(value))
	}
	copyConfig := m.config
	updated := map[string]string{}
	for _, tool := range toolPaths(copyConfig) {
		updated[tool.Key] = tool.Value
	}
	if err := writeToolConfig(copyConfig.ConfigFile, updated); err != nil {
		return fmt.Errorf("write tool configuration: %w", err)
	}
	return nil
}

func (m *JobManager) snapshot(id string) *Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.jobs[id]
	if !ok {
		return nil
	}
	copyJob := record.job
	copyJob.Logs = append([]LogEntry(nil), record.job.Logs...)
	return &copyJob
}

func (m *JobManager) snapshots() []*Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]*Job, 0, len(m.jobs))
	for _, record := range m.jobs {
		copyJob := record.job
		copyJob.Logs = append([]LogEntry(nil), record.job.Logs...)
		items = append(items, &copyJob)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt.After(items[j].UpdatedAt) })
	return items
}

func (m *JobManager) planSnapshot(id string) *Plan {
	m.mu.RLock()
	defer m.mu.RUnlock()
	plan, ok := m.plans[id]
	if !ok {
		return nil
	}
	copyPlan := *plan
	return &copyPlan
}

func (m *JobManager) planSnapshots() []*Plan {
	m.mu.RLock()
	defer m.mu.RUnlock()
	plans := make([]*Plan, 0, len(m.plans))
	for _, plan := range m.plans {
		copyPlan := *plan
		plans = append(plans, &copyPlan)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].UpdatedAt.After(plans[j].UpdatedAt) })
	return plans
}

func (m *JobManager) persistPlan(plan *Plan) {
	if plan == nil {
		return
	}
	path := filepath.Join(m.config.DataDir, "plans", plan.ID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		m.logger.Warn("persist plan directory failed", "plan_id", plan.ID, "error", err)
		return
	}
	temporary := path + ".tmp"
	data, err := json.MarshalIndent(plan, "", "  ")
	if err == nil {
		err = os.WriteFile(temporary, data, 0o644)
	}
	if err == nil {
		err = os.Rename(temporary, path)
	}
	if err != nil {
		m.logger.Warn("persist plan failed", "plan_id", plan.ID, "error", err)
	}
}

func (m *JobManager) loadPlans() {
	entries, err := os.ReadDir(filepath.Join(m.config.DataDir, "plans"))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.config.DataDir, "plans", entry.Name()))
		if err != nil {
			continue
		}
		var plan Plan
		if err := json.Unmarshal(data, &plan); err != nil || plan.ID == "" {
			m.logger.Warn("load plan failed", "path", entry.Name(), "error", err)
			continue
		}
		m.plans[plan.ID] = &plan
	}
}

func (m *JobManager) updatePlan(id string, mutate func(*Plan)) {
	m.mu.Lock()
	plan, ok := m.plans[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	mutate(plan)
	plan.UpdatedAt = time.Now()
	snapshot := *plan
	m.mu.Unlock()
	m.persistPlan(&snapshot)
}

func (m *JobManager) createPlan(name, uploadID, inputPath string, scheduledAt *time.Time) *Plan {
	now := time.Now()
	plan := &Plan{
		ID: newID(), Name: name, UploadID: uploadID, InputPath: inputPath,
		Status: "draft", ScheduledAt: scheduledAt, CreatedAt: now, UpdatedAt: now,
	}
	if scheduledAt != nil && scheduledAt.After(now) {
		plan.Status = "scheduled"
	}
	m.mu.Lock()
	m.plans[plan.ID] = plan
	m.mu.Unlock()
	m.persistPlan(plan)
	return m.planSnapshot(plan.ID)
}

func (m *JobManager) runPlanScheduler() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		for _, plan := range m.planSnapshots() {
			if plan.Status == "scheduled" && plan.ScheduledAt != nil && !plan.ScheduledAt.After(now) {
				if _, err := m.startPlan(plan.ID); err != nil {
					m.updatePlan(plan.ID, func(p *Plan) { p.Status = "failed" })
					m.logger.Error("scheduled plan failed to start", "plan_id", plan.ID, "error", err)
				}
			}
		}
	}
}

func (m *JobManager) startPlan(id string) (*Job, error) {
	plan := m.planSnapshot(id)
	if plan == nil {
		return nil, os.ErrNotExist
	}
	if plan.Status == "running" {
		return nil, errors.New("plan is already running")
	}
	inputPath := plan.InputPath
	sourceType := "path"
	if plan.UploadID != "" {
		inputPath = filepath.Join(m.config.DataDir, "uploads", plan.UploadID)
		sourceType = "upload"
	}
	count, bytes, err := inspectInputPath(inputPath)
	if err != nil {
		return nil, err
	}
	jobID := newID()
	outputPath := filepath.Join(m.config.DataDir, "deliverables", plan.ID, jobID, "root.osgb")
	m.create(jobID, inputPath, outputPath, count, bytes)
	m.update(jobID, func(j *Job) {
		j.PlanID = plan.ID
		j.SourceType = sourceType
	})
	m.updatePlan(plan.ID, func(p *Plan) {
		p.Status = "running"
		p.LastJobID = jobID
	})
	go m.run(jobID)
	return m.snapshot(jobID), nil
}

func (m *JobManager) cancelJob(id string) error {
	m.mu.RLock()
	cancel, ok := m.cancel[id]
	m.mu.RUnlock()
	if !ok {
		if m.snapshot(id) == nil {
			return os.ErrNotExist
		}
		return errors.New("job is not running")
	}
	cancel()
	m.update(id, func(job *Job) {
		job.Status = "failed"
		job.Phase = phaseFailed
		job.Error = "job cancelled by user"
	})
	m.logLine(id, "system", "job cancelled by user")
	return nil
}

func (m *JobManager) resumeFromDense(id string) error {
	job := m.snapshot(id)
	if job == nil {
		return os.ErrNotExist
	}
	if job.Status == "running" || job.Status == "queued" {
		return errors.New("job is already running")
	}
	jobDir, err := filepath.Abs(filepath.Join(m.config.DataDir, "jobs", id))
	if err != nil {
		return fmt.Errorf("resolve job workspace: %w", err)
	}
	if info, err := os.Stat(filepath.Join(jobDir, "scene.mvs")); err != nil || info.IsDir() {
		return errors.New("scene.mvs is not available; this job cannot resume from dense reconstruction")
	}
	m.update(id, func(job *Job) { job.Status = "queued"; job.Phase = "queued"; job.Progress = 60; job.Error = "" })
	go m.runDenseToOSGB(id, jobDir)
	return nil
}

func (m *JobManager) resumeJob(id string) error {
	job := m.snapshot(id)
	if job == nil {
		return os.ErrNotExist
	}
	if job.Status == "running" || job.Status == "queued" {
		return errors.New("job is already running")
	}
	if job.Status != "failed" || job.Phase == phaseCompleted {
		return errors.New("job is not resumable")
	}
	m.update(id, func(current *Job) {
		current.Status = "queued"
		current.Error = ""
		current.Resumable = false
	})
	go m.run(id)
	return nil
}

func (m *JobManager) update(id string, mutate func(*Job)) {
	m.mu.Lock()
	record, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	mutate(&record.job)
	record.job.UpdatedAt = time.Now()
	snapshot := record.job
	snapshot.Logs = append([]LogEntry(nil), record.job.Logs...)
	subscribers := make([]chan event, 0, len(record.subscribers))
	for subscriber := range record.subscribers {
		subscribers = append(subscribers, subscriber)
	}
	m.mu.Unlock()
	m.persistSnapshot(&snapshot)
	for _, subscriber := range subscribers {
		select {
		case subscriber <- event{Type: "update", Job: &snapshot}:
		default:
		}
	}
}

func (m *JobManager) logLine(id, stream, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	entry := LogEntry{At: time.Now(), Stream: stream, Message: message}
	m.update(id, func(job *Job) {
		job.Logs = append(job.Logs, entry)
		if len(job.Logs) > 5000 {
			job.Logs = job.Logs[len(job.Logs)-5000:]
		}
	})
	m.logger.Info("job output", "job_id", id, "stream", stream, "message", message)
}

func (m *JobManager) subscribe(id string) (chan event, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.jobs[id]
	if !ok {
		return nil, nil, os.ErrNotExist
	}
	channel := make(chan event, 32)
	record.subscribers[channel] = struct{}{}
	return channel, func() {
		m.mu.Lock()
		if record, ok := m.jobs[id]; ok {
			delete(record.subscribers, channel)
		}
		m.mu.Unlock()
	}, nil
}

func (m *JobManager) create(id string, inputPath, outputPath string, count int, bytes int64) *Job {
	now := time.Now()
	record := &jobRecord{job: Job{
		ID: id, Status: "queued", Phase: "queued", Progress: 0,
		CreatedAt: now, UpdatedAt: now, OutputPath: outputPath, InputPath: inputPath,
		InputCount: count, InputBytes: bytes, Logs: make([]LogEntry, 0, 64),
	}, subscribers: make(map[chan event]struct{})}
	m.mu.Lock()
	m.jobs[id] = record
	m.mu.Unlock()
	snapshot := m.snapshot(id)
	m.persistSnapshot(snapshot)
	return snapshot
}

func (m *JobManager) persistSnapshot(job *Job) {
	if job == nil {
		return
	}
	path := filepath.Join(m.config.DataDir, "jobs", job.ID, "job.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		m.logger.Warn("persist job directory failed", "job_id", job.ID, "error", err)
		return
	}
	temporary := path + ".tmp"
	file, err := os.Create(temporary)
	if err != nil {
		m.logger.Warn("persist job failed", "job_id", job.ID, "error", err)
		return
	}
	encoderErr := json.NewEncoder(file).Encode(job)
	closeErr := file.Close()
	if encoderErr != nil || closeErr != nil {
		m.logger.Warn("persist job failed", "job_id", job.ID, "error", firstError(encoderErr, closeErr))
		return
	}
	if err := os.Rename(temporary, path); err != nil {
		m.logger.Warn("replace persisted job failed", "job_id", job.ID, "error", err)
	}
}

func (m *JobManager) loadPersisted() {
	root := filepath.Join(m.config.DataDir, "jobs")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "job.json")
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		var job Job
		decodeErr := json.NewDecoder(file).Decode(&job)
		_ = file.Close()
		if decodeErr != nil || job.ID == "" {
			m.logger.Warn("load persisted job failed", "path", path, "error", decodeErr)
			continue
		}
		if job.Logs == nil {
			job.Logs = make([]LogEntry, 0)
		}
		if job.Status == "running" || job.Status == "queued" {
			job.Status = "failed"
			job.Error = "The service stopped while this job was running; resume from the last successful checkpoint."
		}
		job.Resumable = job.Status == "failed" && job.Phase != phaseCompleted
		m.jobs[job.ID] = &jobRecord{job: job, subscribers: make(map[chan event]struct{})}
	}
}

func firstError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

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
func validateOSGBInput(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("input model not accessible: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("input model is empty: %s", path)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ply":
		header, err := readPlyHeader(path)
		if err != nil {
			return err
		}
		if header.vertexCount <= 0 || !header.hasFaces {
			return fmt.Errorf("PLY has no geometry: vertices=%d faces=%d", header.vertexCount, header.faceCount)
		}
		expected := header.payloadOffset + int64(header.vertexCount)*12
		if info.Size() <= expected {
			return fmt.Errorf("PLY is truncated: size=%d expected_face_payload_after=%d", info.Size(), expected)
		}
	case ".obj":
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		vertices, faces := 0, 0
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "v ") {
				vertices++
			} else if strings.HasPrefix(line, "f ") {
				faces++
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("scan OBJ: %w", err)
		}
		if vertices == 0 || faces == 0 {
			return fmt.Errorf("OBJ has no geometry: vertices=%d faces=%d", vertices, faces)
		}
	case ".glb":
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		var magic uint32
		if err := binary.Read(file, binary.LittleEndian, &magic); err != nil || magic != 0x46546C67 {
			return fmt.Errorf("invalid GLB magic")
		}
	default:
		return fmt.Errorf("unsupported model format: %s", filepath.Ext(path))
	}
	return nil
}

type osgbOutputStats struct {
	Vertices    int64 `json:"vertices"`
	Indices     int64 `json:"indices"`
	Faces       int64 `json:"faces"`
	Geodes      int64 `json:"geodes"`
	Tiles       int64 `json:"tiles,omitempty"`
	ValidTiles  int64 `json:"valid_tiles,omitempty"`
	FailedTiles int64 `json:"failed_tiles,omitempty"`
	Textured    bool  `json:"textured,omitempty"`
}

func parseOSGTStats(path string) (osgbOutputStats, error) {
	stats := osgbOutputStats{}
	file, err := os.Open(path)
	if err != nil {
		return stats, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	inElementVector := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "osg::Geode ") {
			stats.Geodes++
			inElementVector = false
			continue
		}
		if strings.HasPrefix(line, "osg::DrawElements") {
			inElementVector = true
			continue
		}
		if strings.HasPrefix(line, "vector ") && inElementVector {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if value, parseErr := strconv.ParseInt(fields[1], 10, 64); parseErr == nil {
					stats.Indices = value
					stats.Faces = value / 3
				}
			}
			inElementVector = false
			continue
		}
		if strings.HasPrefix(line, "vector ") && strings.Contains(line, "Vec3Array") {
			continue
		}
		if strings.HasPrefix(line, "Count ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if value, parseErr := strconv.ParseInt(fields[1], 10, 64); parseErr == nil {
					if value > stats.Vertices {
						stats.Vertices = value
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return stats, fmt.Errorf("scan OSGB text export: %w", err)
	}
	return stats, nil
}

func (m *JobManager) validateOSGBOutput(id, path, sourcePath string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("OSGB not found: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("OSGB is empty: %s", path)
	}
	osgt := path + ".validate.osgt"
	osgconv := m.config.OSGConvBin
	if osgconv == "" {
		return fmt.Errorf("OSGCONV_BIN is not configured")
	}
	defer os.Remove(osgt)
	cmd := exec.Command(osgconv, path, osgt)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("OSGB cannot be read back: %w", err)
	}
	stats, err := parseOSGTStats(osgt)
	if err != nil {
		return fmt.Errorf("inspect OSGB: %w", err)
	}
	if stats.Vertices <= 0 {
		return fmt.Errorf("OSGB contains no vertices")
	}
	if stats.Indices <= 0 || stats.Faces <= 0 {
		return fmt.Errorf("OSGB contains no triangles: indices=%d faces=%d", stats.Indices, stats.Faces)
	}
	if !strings.EqualFold(filepath.Ext(sourcePath), ".ply") {
		header, headerErr := readPlyHeader(sourcePath)
		if headerErr == nil && header.hasFaces && header.faceCount > 0 && stats.Faces != int64(header.faceCount) {
			// OSG may append a sentinel/deduplicated primitive record, so only
			// fail when the output is materially incomplete.
			tolerance := int64(header.faceCount)/1000 + 8
			if stats.Faces < int64(header.faceCount)-tolerance {
				return fmt.Errorf("OSGB face mismatch: got %d want at least %d", stats.Faces, int64(header.faceCount)-tolerance)
			}
		}
		m.updateJobStats(id, sourcePath, &stats)
		return nil
	}
	header, err := readPlyHeader(sourcePath)
	if err != nil {
		return err
	}
	// OpenMVS PLY files can contain a small amount of trailing geometry that
	// osgconv materializes. Reject only materially incomplete outputs.
	vertexTolerance := int64(header.vertexCount)/1000 + 8
	if stats.Vertices < int64(header.vertexCount)-vertexTolerance {
		return fmt.Errorf("OSGB vertex mismatch: got %d want at least %d", stats.Vertices, int64(header.vertexCount)-vertexTolerance)
	}
	faceTolerance := int64(header.faceCount)/1000 + 8
	if stats.Faces < int64(header.faceCount)-faceTolerance {
		return fmt.Errorf("OSGB face mismatch: got %d want at least %d", stats.Faces, int64(header.faceCount)-faceTolerance)
	}
	m.updateJobStats(id, sourcePath, &stats)
	return nil
}

func (m *JobManager) updateJobStats(id, sourcePath string, stats *osgbOutputStats) {
	if stats == nil {
		return
	}
	captured := *stats
	m.update(id, func(job *Job) {
		value := captured
		job.Stats = &value
	})
	m.logLine(id, "system", fmt.Sprintf("OSGB validation passed: vertices=%d indices=%d faces=%d geodes=%d source=%s", stats.Vertices, stats.Indices, stats.Faces, stats.Geodes, sourcePath))
}

// offsets remain valid because the original header length is recorded before
// rewriting and used as the source seek offset when copying the payload.
type plyElement struct {
	name  string
	count int
}

type plyHeader struct {
	format         string
	vertexCount    int
	faceCount      int
	elements       []plyElement
	hasFaces       bool
	vertexHasColor bool
	faceIndexSize  int
	payloadOffset  int64
	raw            string
}

func readPlyHeader(path string) (*plyHeader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	var sb strings.Builder
	offset := 0
	header := &plyHeader{}
	for {
		line, readErr := reader.ReadString('\n')
		sb.WriteString(line)
		offset += len(line)
		text := strings.TrimRight(line, "\r\n")
		fields := strings.Fields(text)
		switch {
		case len(fields) >= 2 && fields[0] == "format":
			header.format = fields[1]
		case len(fields) >= 3 && fields[0] == "element":
			count, _ := strconv.Atoi(fields[2])
			header.elements = append(header.elements, plyElement{name: fields[1], count: count})
			if fields[1] == "vertex" {
				header.vertexCount = count
			} else if fields[1] == "face" {
				header.faceCount = count
				header.hasFaces = count > 0
			}
		case len(fields) >= 3 && fields[0] == "property" && fields[1] != "list" && currentElement(header.elements) == "vertex":
			lower := strings.ToLower(text)
			if strings.Contains(lower, "red") || strings.Contains(lower, "green") || strings.Contains(lower, "blue") {
				header.vertexHasColor = true
			}
		case len(fields) >= 4 && fields[0] == "property" && fields[1] == "list" && currentElement(header.elements) == "face":
			size := sizeOfType(fields[3])
			if size > header.faceIndexSize {
				header.faceIndexSize = size
			}
		case strings.TrimSpace(text) == "end_header":
			header.raw = sb.String()
			header.payloadOffset = int64(offset)
			return header, nil
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil, fmt.Errorf("read PLY header %s: missing end_header", path)
			}
			return nil, fmt.Errorf("read PLY header %s: %w", path, readErr)
		}
	}
}

func currentElement(elements []plyElement) string {
	if len(elements) == 0 {
		return ""
	}
	return elements[len(elements)-1].name
}

func sizeOfType(name string) int {
	switch strings.ToLower(name) {
	case "char", "uchar", "int8", "uint8":
		return 1
	case "short", "ushort", "int16", "uint16":
		return 2
	case "int", "uint", "int32", "uint32", "float", "float32":
		return 4
	case "double", "float64":
		return 8
	default:
		return 0
	}
}

func convertFullmeshFaceIndicesToUnsignedByte(path string) error {
	header, err := readPlyHeader(path)
	if err != nil {
		return err
	}
	if header.format != "binary_little_endian" || !header.hasFaces {
		return fmt.Errorf("unsupported PLY for face-index conversion: %s", path)
	}

	// Find the exact face list declaration. OpenMVS writes a one-byte vertex
	// count followed by four-byte indices, but historically declares the list
	// as "uint8 uint32". OSG 3.6.5 only understands the canonical spellings
	// "uchar int" for this payload, so rewrite the declaration while keeping
	// the binary payload unchanged whenever it is already valid.
	countSize, indexSize, faceProperty, err := plyFaceProperty(header.raw)
	if err != nil {
		return err
	}
	if countSize != 1 && countSize != 4 {
		return fmt.Errorf("unsupported PLY face count size %d: %s", countSize, path)
	}
	if indexSize != 4 {
		return fmt.Errorf("unsupported PLY face index size %d: %s", indexSize, path)
	}
	if countSize == 4 {
		// Legacy OpenMVS files can declare a four-byte count. Normalize those
		// records to the compact one-byte form expected by OSG.
		return rewriteFaceCountsToUint8(path, header, faceProperty)
	}

	target := strings.Replace(faceProperty, "uint8 uint32", "uchar int", 1)
	target = strings.Replace(target, "uchar uint32", "uchar int", 1)
	target = strings.Replace(target, "uint8 int", "uchar int", 1)
	if target == faceProperty {
		return nil
	}
	rewritten := strings.Replace(header.raw, faceProperty, target, 1)
	temp := path + ".osgply"
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		source.Close()
		return err
	}
	if _, err := out.WriteString(rewritten); err != nil {
		out.Close()
		source.Close()
		os.Remove(temp)
		return err
	}
	if _, err := source.Seek(header.payloadOffset, io.SeekStart); err != nil {
		out.Close()
		source.Close()
		os.Remove(temp)
		return err
	}
	if _, err := io.Copy(out, source); err != nil {
		out.Close()
		source.Close()
		os.Remove(temp)
		return fmt.Errorf("copy PLY payload: %w", err)
	}
	if err := out.Close(); err != nil {
		source.Close()
		os.Remove(temp)
		return err
	}
	if err := source.Close(); err != nil {
		os.Remove(temp)
		return err
	}
	info, err := os.Stat(path)
	if err == nil {
		os.Chmod(temp, info.Mode())
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return err
	}
	return nil
}

func plyFaceProperty(raw string) (int, int, string, error) {
	for _, line := range strings.Split(raw, "\n") {
		text := strings.TrimRight(line, "\r\n")
		fields := strings.Fields(text)
		if len(fields) >= 5 && fields[0] == "property" && fields[1] == "list" && fields[4] == "vertex_indices" {
			return sizeOfType(fields[2]), sizeOfType(fields[3]), text, nil
		}
	}
	return 0, 0, "", fmt.Errorf("PLY has no vertex_indices face property")
}

func rewriteFaceCountsToUint8(path string, header *plyHeader, faceProperty string) error {
	temp := path + ".u8ply"
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	if _, err := source.Seek(header.payloadOffset, io.SeekStart); err != nil {
		return err
	}
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	target := strings.Replace(faceProperty, "uint8 uint32", "uchar int", 1)
	target = strings.Replace(target, "uchar uint32", "uchar int", 1)
	target = strings.Replace(target, "uint8 int", "uchar int", 1)
	rewritten := strings.Replace(header.raw, faceProperty, target, 1)
	if _, err := out.WriteString(rewritten); err != nil {
		out.Close()
		os.Remove(temp)
		return err
	}
	writer := bufio.NewWriterSize(out, 1024*1024)
	if _, err := io.CopyN(writer, source, int64(header.vertexCount)*12); err != nil {
		out.Close()
		os.Remove(temp)
		return fmt.Errorf("copy PLY vertex payload: %w", err)
	}
	for i := 0; i < header.faceCount; i++ {
		var count uint32
		if err := binary.Read(source, binary.LittleEndian, &count); err != nil {
			out.Close()
			os.Remove(temp)
			return fmt.Errorf("read face count at face %d: %w", i, err)
		}
		if count > 255 {
			out.Close()
			os.Remove(temp)
			return fmt.Errorf("face %d has %d vertices; cannot encode count as uint8", i, count)
		}
		if err := writer.WriteByte(uint8(count)); err != nil {
			out.Close()
			os.Remove(temp)
			return err
		}
		if _, err := io.CopyN(writer, source, int64(count)*4); err != nil {
			out.Close()
			os.Remove(temp)
			return fmt.Errorf("copy indices at face %d: %w", i, err)
		}
	}
	if err := writer.Flush(); err != nil {
		out.Close()
		os.Remove(temp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(temp)
		return err
	}
	info, err := os.Stat(path)
	if err == nil {
		os.Chmod(temp, info.Mode())
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return err
	}
	return nil
}
func normalizeOpenMVSPly(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	reader := bufio.NewReader(file)
	var lines []string
	var originalHeaderLen int
	changed := false
	for {
		line, readErr := reader.ReadString('\n')
		originalHeaderLen += len(line)
		text := strings.TrimRight(line, "\r\n")
		if fields := strings.Fields(text); len(fields) >= 3 && fields[0] == "property" {
			if fields[1] == "list" {
				if len(fields) >= 4 {
					fields[2] = normalizePlyTypeName(fields[2])
					fields[3] = normalizePlyTypeName(fields[3])
				}
			} else {
				fields[1] = normalizePlyTypeName(fields[1])
			}
			newText := strings.Join(fields, " ")
			changed = changed || newText != text
			text = newText
		}
		lines = append(lines, text)
		if strings.TrimSpace(line) == "end_header" {
			break
		}
		if readErr != nil {
			file.Close()
			return "", fmt.Errorf("read PLY header %s: %w", path, readErr)
		}
	}
	file.Close()
	if !changed {
		return path, nil
	}
	header := []byte(strings.Join(lines, "\n") + "\n")
	temp := path + ".osgcompat"
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := out.Write(header); err != nil {
		out.Close()
		os.Remove(temp)
		return "", err
	}
	source, err := os.Open(path)
	if err != nil {
		out.Close()
		os.Remove(temp)
		return "", err
	}
	if _, err := source.Seek(int64(originalHeaderLen), io.SeekStart); err != nil {
		source.Close()
		out.Close()
		os.Remove(temp)
		return "", err
	}
	if _, err := io.Copy(out, source); err != nil {
		source.Close()
		out.Close()
		os.Remove(temp)
		return "", err
	}
	if err := source.Close(); err != nil {
		out.Close()
		os.Remove(temp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(temp)
		return "", err
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return "", err
	}
	return path, nil
}

func normalizePlyTypeName(name string) string {
	switch name {
	case "uint8":
		return "uchar"
	case "uint16":
		return "ushort"
	case "uint32":
		return "int"
	case "int8":
		return "char"
	case "int16":
		return "short"
	default:
		return name
	}
}

// runNative uses the conventional COLMAP -> OpenMVS -> OpenSceneGraph chain.
// The binaries are intentionally configurable so a production deployment can pin
// versions and GPU-enabled builds without changing the service.
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
	m.logLine(id, "system", fmt.Sprintf("Building Smart3D PagedLOD tree: model=%s outDir=%s grid=16x16", modelPath, outDir))
	logFn := func(msg string) {
		m.logLine(id, "system", msg)
	}
	if err := buildSmart3DOSGB(mesh, texMap, m.config.OSGConvBin, outDir, 16, 16, logFn); err != nil {
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
	if current := m.snapshot(id); current != nil && current.Status == "failed" && current.Error == "job cancelled by user" {
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
		return fmt.Errorf("command %s timed out or was cancelled: %w", binary, ctx.Err())
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

func (m *JobManager) handleHealth(w http.ResponseWriter, r *http.Request) {
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

func main() {
	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		if err := runHealthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("ok")
		return
	}
	loadDotEnv(".env")
	configuredDataDir := envOr("DATA_DIR", "./runtime")
	loadDotEnv(envOr("CONFIG_FILE", filepath.Join(configuredDataDir, "service.env")))
	config := loadConfig()
	if err := os.MkdirAll(filepath.Join(config.DataDir, "jobs"), 0o755); err != nil {
		log.Fatal(err)
	}
	for _, directory := range []string{"uploads", "plans", "deliverables"} {
		if err := os.MkdirAll(filepath.Join(config.DataDir, directory), 0o755); err != nil {
			log.Fatal(err)
		}
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	manager := NewJobManager(config, logger)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", manager.handleHealth)
	mux.HandleFunc("/api/config", manager.handleConfig)
	mux.HandleFunc("/api/pick-directory", manager.handlePickDirectory)
	mux.HandleFunc("/api/uploads", manager.handleUpload)
	mux.HandleFunc("/api/plans", manager.handlePlans)
	mux.HandleFunc("/api/plans/", manager.handlePlans)
	mux.HandleFunc("/api/jobs", manager.jobRoutes)
	mux.HandleFunc("/api/jobs/", manager.jobRoutes)
	mux.HandleFunc("/", manager.serveStatic)

	server := &http.Server{Addr: ":" + config.Port, Handler: withRequestLogging(mux, logger), ReadHeaderTimeout: 15 * time.Second}
	logger.Info("server started", "address", server.Addr, "data_dir", config.DataDir)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// runHealthcheck is used by Docker HEALTHCHECK. It checks the real native
// chain rather than merely confirming that the server binary can start.
func runHealthcheck() error {
	loadDotEnv(".env")
	dataDir := envOr("DATA_DIR", "./runtime")
	loadDotEnv(envOr("CONFIG_FILE", filepath.Join(dataDir, "service.env")))
	config := loadConfig()
	if strings.EqualFold(config.PipelineMode, "native") && !nativePipelineReady(config) {
		return errors.New("native photogrammetry tools are not ready")
	}
	return nil
}

func loadDotEnv(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
}

func withRequestLogging(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("http request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(started).Milliseconds())
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]any{"code": code, "detail": detail})
}

func writeSSE(w io.Writer, eventType string, value any) {
	data, _ := json.Marshal(value)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
}

func methodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", http.MethodPost)
	writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
}

func isSupportedImage(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".tif", ".tiff", ".webp":
		return true
	default:
		return false
	}
}

func validateOutputPath(outputPath, root string) error {
	if outputPath == "" {
		return errors.New("output_path is required")
	}
	clean := filepath.Clean(outputPath)
	if !filepath.IsAbs(clean) {
		return errors.New("output_path must be an absolute local path")
	}
	if clean == "." || clean == string(filepath.Separator) {
		return errors.New("output_path is not a valid file or directory path")
	}
	if root != "" {
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			return fmt.Errorf("invalid output root: %w", err)
		}
		pathAbs, err := filepath.Abs(clean)
		if err != nil {
			return fmt.Errorf("invalid output path: %w", err)
		}
		rel, err := filepath.Rel(rootAbs, pathAbs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("output path must be inside OUTPUT_ROOT (%s)", rootAbs)
		}
	}
	return nil
}

// idCounter makes IDs unique even when two IDs are requested within the same
// clock tick. Job IDs also name directories on disk, so uniqueness is a hard
// requirement rather than a nicety: a collision would make two jobs share a
// workspace and corrupt each other's state.
var idCounter atomic.Uint64

// newID returns a unique, filesystem-safe identifier of the form
// <unix-nano>-<counter><random>. The counter covers same-nanosecond calls and
// the random suffix from crypto/rand keeps IDs unguessable across restarts.
func newID() string {
	seq := idCounter.Add(1)
	return fmt.Sprintf("%d-%s%s", time.Now().UnixNano(), encodeBase36(seq), randomToken(6))
}

// encodeBase36 renders a number in base 36 so the counter stays short in paths.
func encodeBase36(value uint64) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	if value == 0 {
		return "0"
	}
	var buf [13]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = alphabet[value%36]
		value /= 36
	}
	return string(buf[i:])
}

// randomToken returns a cryptographically random token of the requested length.
// It falls back to a time-seeded generator only if the system entropy source is
// unavailable, which would otherwise make every caller fail.
func randomToken(length int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	if length <= 0 {
		return ""
	}
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		seed := uint64(time.Now().UnixNano())
		for i := range buf {
			seed = seed*6364136223846793005 + 1442695040888963407
			buf[i] = byte(seed >> 33)
		}
	}
	var value strings.Builder
	value.Grow(length)
	for _, b := range buf {
		value.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	return value.String()
}

func findModel(projectDir string) (string, error) {
	preferred := []string{
		filepath.Join(projectDir, "odm_texturing", "odm_textured_model_geo.obj"),
		filepath.Join(projectDir, "odm_texturing", "odm_textured_model.obj"),
		filepath.Join(projectDir, "odm_25dtexturing", "odm_textured_model_geo.obj"),
		filepath.Join(projectDir, "odm_25dtexturing", "odm_textured_model.obj"),
	}
	for _, path := range preferred {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return filepath.Abs(path)
		}
	}
	var candidates []string
	err := filepath.WalkDir(projectDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".obj" || ext == ".ply" || ext == ".glb" {
			candidates = append(candidates, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("search reconstructed model: %w", err)
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return "", errors.New("reconstruction finished but no OBJ/PLY/GLB model was found")
	}
	return filepath.Abs(candidates[0])
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func inspectInputPath(inputPath string) (int, int64, error) {
	if inputPath == "" {
		return 0, 0, errors.New("input_path is required")
	}
	if !filepath.IsAbs(inputPath) {
		return 0, 0, errors.New("input_path must be an absolute local directory path")
	}
	info, err := os.Stat(inputPath)
	if err != nil {
		return 0, 0, fmt.Errorf("input path is not accessible: %w", err)
	}
	if !info.IsDir() {
		return 0, 0, errors.New("input_path must be a directory containing flight images")
	}
	scanRoot := inputPath
	if imagesInfo, imagesErr := os.Stat(filepath.Join(inputPath, "images")); imagesErr == nil && imagesInfo.IsDir() {
		scanRoot = filepath.Join(inputPath, "images")
	}
	count := 0
	var bytes int64
	err = filepath.WalkDir(scanRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !isSupportedImage(entry.Name()) {
			return nil
		}
		fileInfo, statErr := entry.Info()
		if statErr != nil {
			return statErr
		}
		count++
		bytes += fileInfo.Size()
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("scan input images: %w", err)
	}
	if count == 0 {
		return 0, 0, errors.New("no supported images found; use JPG, PNG, TIFF or WebP files")
	}
	return count, bytes, nil
}

func pickWindowsPath(kind string) (string, error) {
	// The dialog strings are deliberately ASCII-only: the script is passed to
	// PowerShell through -Command, and non-ASCII text here has previously been
	// corrupted into mojibake by console/GBK code-page translation.
	script := `$ErrorActionPreference = 'Stop'; Add-Type -AssemblyName System.Windows.Forms; `
	if kind == "output" {
		script += `$dialog = New-Object System.Windows.Forms.SaveFileDialog; $dialog.Title = 'Choose the OSGB output file (for example root.osgb)'; $dialog.Filter = 'OSGB files (*.osgb)|*.osgb|All files (*.*)|*.*'; $dialog.DefaultExt = 'osgb'; if ($dialog.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::WriteLine($dialog.FileName) }`
	} else {
		script += `$dialog = New-Object System.Windows.Forms.FolderBrowserDialog; $dialog.Description = 'Choose the folder containing the flight images for reconstruction'; $dialog.UseDescriptionForTitle = $true; if ($dialog.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::WriteLine($dialog.SelectedPath) }`
	}
	for _, binary := range []string{"powershell.exe", "pwsh.exe"} {
		if _, err := exec.LookPath(binary); err != nil {
			continue
		}
		command := exec.Command(binary, "-NoProfile", "-STA", "-Command", script)
		output, err := command.Output()
		if err != nil {
			return "", fmt.Errorf("open native folder picker: %w", err)
		}
		return strings.TrimSpace(string(output)), nil
	}
	return "", errors.New("PowerShell is not available")
}

func expandArgs(template string, vars map[string]string) ([]string, error) {
	tokens, err := tokenize(template)
	if err != nil {
		return nil, err
	}
	for i := range tokens {
		for key, value := range vars {
			tokens[i] = strings.ReplaceAll(tokens[i], key, value)
		}
	}
	return tokens, nil
}

// tokenize splits a command template into argv, honouring single and double
// quotes.
//
// Backslash handling is deliberately conservative because these templates carry
// Windows paths. Historically a backslash was always treated as an escape, which
// silently mangled any Windows path: `-o "C:\data\job\out.mvs"` produced the
// single token `C:datajobout.mvs` and the tool then wrote to a nonsense path.
// Forward-slash paths happened to work, so the defect went unnoticed while the
// shipped defaults all used "/".
//
// Rules now applied:
//   - Inside single quotes, everything is literal (no escapes at all).
//   - A backslash only escapes a character when that character would otherwise
//     be meaningful here, i.e. a quote, a backslash, or whitespace. This keeps
//     `\"` and `\ ` working as escapes while `C:\data` and a trailing
//     `C:\some dir\` survive intact.
//   - Any other backslash is kept verbatim, which is what a Windows path needs.
func tokenize(input string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	var quote rune
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	runes := []rune(input)
	for i := 0; i < len(runes); i++ {
		char := runes[i]
		if quote == '\'' {
			// Single quotes are fully literal.
			if char == '\'' {
				quote = 0
			} else {
				current.WriteRune(char)
			}
			continue
		}
		if char == '\\' {
			// Escape only when the next character would otherwise be special.
			if i+1 < len(runes) {
				next := runes[i+1]
				if next == '"' || next == '\\' || next == ' ' || next == '\t' || next == '\'' {
					current.WriteRune(next)
					i++
					continue
				}
			}
			// A trailing backslash, or one before an ordinary character such as
			// "d" in C:\data, is literal.
			current.WriteRune(char)
			continue
		}
		if quote == '"' {
			if char == '"' {
				quote = 0
			} else {
				current.WriteRune(char)
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if char == ' ' || char == '\t' || char == '\r' || char == '\n' {
			flush()
			continue
		}
		current.WriteRune(char)
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in command arguments")
	}
	flush()
	return tokens, nil
}
