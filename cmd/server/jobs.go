package main

// jobs.go covers the job and plan lifecycle: creating them, persisting and
// reloading them, the schedule ticker that starts due plans, and the restart
// reconciliation that repairs state left behind by an unclean shutdown.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func NewJobManager(config Config, logger *slog.Logger) *JobManager {
	manager := &JobManager{jobs: make(map[string]*jobRecord), plans: make(map[string]*Plan), cancel: make(map[string]context.CancelFunc), config: config, logger: logger}
	manager.loadPersisted()
	manager.loadPlans()
	// Must run after both loads: reconciliation compares each plan against the
	// jobs that reference it, so neither side may still be empty. Calling it
	// from loadPersisted (as the first version did) ran before plans existed and
	// silently repaired nothing.
	manager.reconcilePlanStatuses()
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
		job.Error = errJobcanceled
	})
	m.logLine(id, "system", errJobcanceled)
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

// reconcilePlanStatuses repairs plans left claiming "running" after a restart.
//
// loadPersisted demotes interrupted jobs to failed, but the plan that started
// them kept whatever status it had. A plan stuck at "running" is not merely
// cosmetic: the delete path refuses to remove a plan with a live job, and the UI
// disables its checkbox, so the plan can never be cleaned up. Recovery derives
// the plan status from its jobs instead of trusting the stored value.
func (m *JobManager) reconcilePlanStatuses() {
	// Collect the repairs first, then persist them after releasing the lock,
	// so the lock is held only for the in-memory update.
	type repair struct {
		id   string
		from string
		to   string
	}
	var repairs []repair

	m.mu.Lock()
	for _, plan := range m.plans {
		if plan.Status != "running" {
			continue
		}
		hasActiveJob := false
		hasAnyJob := false
		allCompleted := true
		for _, record := range m.jobs {
			if record.job.PlanID != plan.ID {
				continue
			}
			hasAnyJob = true
			if record.job.Status == "running" || record.job.Status == "queued" {
				hasActiveJob = true
			}
			if record.job.Status != "completed" {
				allCompleted = false
			}
		}
		if hasActiveJob {
			// A job really is live; leave the plan alone.
			continue
		}
		target := "failed"
		if hasAnyJob && allCompleted {
			target = "completed"
		}
		previous := plan.Status
		plan.Status = target
		plan.UpdatedAt = time.Now()
		repairs = append(repairs, repair{id: plan.ID, from: previous, to: target})
	}
	m.mu.Unlock()

	for _, item := range repairs {
		m.logger.Info("reconciled plan status after restart",
			"plan_id", item.id, "from", item.from, "to", item.to)
		if plan := m.planSnapshot(item.id); plan != nil {
			m.persistPlan(plan)
		}
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
