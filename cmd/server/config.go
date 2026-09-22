package main

// config.go holds the service configuration: loading it from the environment
// and the persisted tool-path file, and the helpers that read individual
// values. Everything here is concerned with turning process state into the
// Config struct the rest of the service reads.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

// errJobcanceled marks a job stopped by the operator. It is stored in Job.Error
// and compared against by fail(), so it must be a single shared constant: two
// copies of the literal would silently drift apart and the guard that preserves
// this message would stop matching.
const errJobcanceled = "job canceled by user"

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
	// MatcherOverlap is the number of neighboring images each image is matched
	// against when Matcher is "sequential".
	MatcherOverlap int
	// Threads is the worker count handed to COLMAP and OpenMVS. Zero or less
	// means "derive from the number of available CPUs".
	Threads int
	// MaxImageSize caps the long edge fed to SIFT extraction. Lower values are
	// faster but find fewer features.
	MaxImageSize int
	// TileGrid is the PagedLOD tile grid dimension per axis. Zero means choose
	// it from the model's triangle count.
	TileGrid int
	// LODLevels is the per-tile LOD chain depth. Zero means choose it from the
	// per-tile face count.
	LODLevels int
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
		TileGrid:       int(envInt64AllowZero("TILE_GRID", 0)),
		LODLevels:      int(envInt64AllowZero("LOD_LEVELS", 0)),
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
