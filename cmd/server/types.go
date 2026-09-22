package main

// types.go defines the data the service moves around: the jobs and production
// plans it tracks, the uploads it stores, and the manager that owns them. The
// JSON tags are part of the HTTP and persisted-file contract.

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

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
