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
	"strings"
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
	// UseGPU enables CUDA for the COLMAP feature extraction and matching stages.
	//
	// It is off by default and stays off for the CPU-only Docker image. Enabling
	// it requires a CUDA-enabled COLMAP binary and, inside a container, the
	// NVIDIA runtime with the GPU passed through; without those, COLMAP fails at
	// startup with a CUDA error rather than silently falling back to CPU.
	UseGPU bool
	// GPUIndex selects which CUDA device to use, mirroring COLMAP's gpu_index
	// option. The default of -1 lets COLMAP pick automatically.
	GPUIndex int
}

// Memory model for the automatically derived worker count.
//
// COLMAP's SIFT extraction is the memory peak of the whole pipeline: each worker
// holds a full-size image, its pyramid and its keypoints. The figures below were
// measured in the deployment image at a 12 GB container limit with 3200px images:
//
//	threads=1   peak 2.37 GB
//	threads=2   peak 4.55 GB
//	threads=3   peak 6.74 GB
//	threads=4   peak 8.90 GB
//	threads=6   peak 12 GB  (saturated the limit)
//	threads=8   peak 12 GB  killed (exit 137)
//
// The slope is about 2.2 GB per worker at this image size, far above an earlier
// 350 MB estimate that let the automatic setting choose a parallelism that was
// OOM-killed on real survey imagery.
const (
	// bytesPerMegapixelPerWorker is SIFT peak memory per worker per megapixel of
	// the image it is processing.
	//
	// Calibrated: 3200x2400 is 7.68 MP and cost ~2.2 GB per worker, i.e. about
	// 290 MB per megapixel. Rounded up to 320 for cameras that are not 4:3 and to
	// stay safely below the measured kill point.
	bytesPerMegapixelPerWorker = 320 << 20 // 320 MB per megapixel
	// siftBaseBytes is the fixed cost of the process plus one resident image
	// buffer before any worker parallelism is added, from the 1-thread run above
	// minus one worker's share.
	siftBaseBytes = 256 << 20 // 256 MB
	// reservedBytes stays free for the Go service, the log and SSE goroutines,
	// page cache and whatever else the host runs.
	reservedBytes = 1 << 30 // 1 GB
	// fallbackMegapixels is used when MaxImageSize is disabled (0), so the
	// calculation still has a defensible figure rather than assuming zero.
	fallbackMegapixels = 8.0
)

// estimateWorkerBytes returns the peak memory assumed for one SIFT worker.
func (c Config) estimateWorkerBytes() int64 {
	megapixels := fallbackMegapixels
	if c.MaxImageSize > 0 {
		// The cap applies to the long edge; assume a 4:3 frame, which is what
		// survey cameras overwhelmingly produce.
		longEdge := float64(c.MaxImageSize)
		megapixels = (longEdge * longEdge * 3 / 4) / 1_000_000
	}
	if megapixels < 0.1 {
		megapixels = 0.1
	}
	return int64(megapixels*float64(bytesPerMegapixelPerWorker)) + siftBaseBytes
}

// resolveThreads returns the worker count to pass to subprocesses.
//
// The service runs CPU-only by design, so thread count is the main performance
// lever. It previously passed a hard-coded 4, which left most of a modern server
// idle. An intermediate version counted CPUs alone; on a 32-core host with a
// 12 GB container limit that chose 16 workers and COLMAP was OOM-killed during
// feature extraction (cgroup reported oom_kill). Memory, not core count, is what
// bounds this pipeline.
//
// An explicit Threads setting always wins. Otherwise the count is the smallest
// of:
//   - GOMAXPROCS minus one, leaving a core for the HTTP server and log readers
//   - what the memory budget allows, after reserving room for everything else
//   - maxAutoThreads
//
// The result is at least 1, so the pipeline still runs on a small machine.
func (c Config) resolveThreads() int {
	if c.Threads > 0 {
		return c.Threads
	}

	byCPU := runtime.GOMAXPROCS(0)
	if byCPU > 1 {
		byCPU--
	}

	byMemory := maxAutoThreads
	if limit, ok := availableMemoryBytes(); ok {
		budget := int64(limit) - reservedBytes
		// estimateWorkerBytes already includes the fixed base cost, so the
		// memory needed for n workers is n * perWorker. Divide directly; the
		// base is therefore only counted once, matching the linear fit.
		perWorker := c.estimateWorkerBytes()
		if budget < perWorker {
			// Not enough headroom for even one full-size worker. Fall back to a
			// single worker and let the operator lower SIFT_MAX_IMAGE_SIZE.
			byMemory = 1
		} else {
			byMemory = int(budget / perWorker)
		}
	}

	threads := byCPU
	if byMemory < threads {
		threads = byMemory
	}
	if threads < 1 {
		threads = 1
	}
	if threads > maxAutoThreads {
		threads = maxAutoThreads
	}
	return threads
}

// maxAutoThreads caps the automatically derived worker count.
const maxAutoThreads = 16

// envBool reads a boolean environment variable, treating 1, true, yes and on
// (case-insensitive) as true and anything else, including absence, as false.
func envBool(name string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "":
		return fallback
	default:
		return false
	}
}

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
		UseGPU:         envBool("USE_GPU", false),
		GPUIndex:       int(envInt64AllowZero("COLMAP_GPU_INDEX", -1)),
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
