package main

// colmap_args.go builds the COLMAP argv for feature extraction and matching.
//
// Two problems are solved here.
//
// 1. CPU-only operation. The service runs in a Linux container without an
//    NVIDIA runtime, so every GPU switch must be forced off. COLMAP enables the
//    GPU by default, so relying on defaults would fail at runtime.
//
// 2. COLMAP renamed its SIFT option groups. Up to and including 3.9 the options
//    live under --SiftExtraction.* / --SiftMatching.*; from 4.x the processing
//    switches moved to --FeatureExtraction.* / --FeatureMatching.*, and passing
//    the old spelling is a hard error ("unrecognized option"), not a warning.
//    The two spellings are mutually exclusive, so no single fixed string works
//    on both. The deployment image ships COLMAP 3.9.1 while a developer machine
//    may have 4.2, so the correct group is detected once per binary and cached.

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// colmapOptionStyle records which option-group spelling a COLMAP binary accepts.
type colmapOptionStyle int

const (
	// colmapStyleLegacy is COLMAP <= 3.9: --SiftExtraction.* / --SiftMatching.*
	colmapStyleLegacy colmapOptionStyle = iota
	// colmapStyleModern is COLMAP >= 4.0: --FeatureExtraction.* / --FeatureMatching.*
	colmapStyleModern
)

// colmapStyleCache memoises detection per binary path. Detection spawns a
// subprocess, and the pipeline builds these argument lists on every job.
var colmapStyleCache sync.Map

// detectColmapOptionStyle probes a COLMAP binary to learn which option group it
// accepts. It prefers the modern spelling and falls back to the legacy one.
//
// Detection is deliberately done by asking the binary rather than by parsing a
// version string: `colmap --help` output has changed shape between releases, and
// a wrong guess would turn into a failed job hours into a run. If the probe
// cannot run at all (binary missing, help unsupported) the legacy spelling is
// returned, because that is what the pinned deployment image uses.
func detectColmapOptionStyle(binary string) colmapOptionStyle {
	if cached, ok := colmapStyleCache.Load(binary); ok {
		if style, ok := cached.(colmapOptionStyle); ok {
			return style
		}
	}
	style := probeColmapOptionStyle(binary)
	colmapStyleCache.Store(binary, style)
	return style
}

// probeColmapOptionStyle runs the detection probe and falls back on any error.
func probeColmapOptionStyle(binary string) colmapOptionStyle {
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return colmapStyleLegacy
	}
	output, err := exec.Command(resolved, "feature_extractor", "--help").CombinedOutput()
	if err != nil && len(output) == 0 {
		return colmapStyleLegacy
	}
	text := string(output)
	// A modern binary lists the new group; a legacy one lists the old group.
	if strings.Contains(text, "--FeatureExtraction.") {
		return colmapStyleModern
	}
	if strings.Contains(text, "--SiftExtraction.") {
		return colmapStyleLegacy
	}
	return colmapStyleLegacy
}

// optionGroup returns the extraction/matching option prefix for this style.
func (s colmapOptionStyle) extractionGroup() string {
	if s == colmapStyleModern {
		return "FeatureExtraction"
	}
	return "SiftExtraction"
}

func (s colmapOptionStyle) matchingGroup() string {
	if s == colmapStyleModern {
		return "FeatureMatching"
	}
	return "SiftMatching"
}

// featureExtractorArgs builds the argv for `colmap feature_extractor`.
//
// Everything here is CPU-only: use_gpu is explicitly 0 rather than left at
// COLMAP's default of 1, so the command cannot accidentally attempt CUDA
// initialisation in a container that has no GPU.
func featureExtractorArgs(config Config, database, imageDir string) []string {
	style := detectColmapOptionStyle(config.ColmapBin)
	group := style.extractionGroup()
	args := []string{
		"feature_extractor",
		"--database_path", database,
		"--image_path", imageDir,
		"--ImageReader.single_camera", "1",
	}
	// CPU-only, with an explicit worker count. num_threads=-1 would mean
	// "use every core", which competes with the HTTP server and log readers.
	args = append(args,
		"--"+group+".use_gpu", "0",
		"--"+group+".num_threads", fmt.Sprint(config.resolveThreads()),
	)
	if config.MaxImageSize > 0 {
		args = append(args, "--"+group+".max_image_size", fmt.Sprint(config.MaxImageSize))
	}
	return args
}

// matcherCommand returns the COLMAP subcommand name for the configured strategy.
// Unknown values fall back to the exhaustive matcher, which is always correct
// even when it is the slowest option.
func matcherCommand(matcher string) string {
	switch strings.ToLower(strings.TrimSpace(matcher)) {
	case "sequential", "sequential_matcher":
		return "sequential_matcher"
	case "spatial", "spatial_matcher":
		return "spatial_matcher"
	case "vocab_tree", "vocab", "vocab_tree_matcher":
		return "vocab_tree_matcher"
	case "transitive", "transitive_matcher":
		return "transitive_matcher"
	default:
		return "exhaustive_matcher"
	}
}

// matcherArgs builds the argv for the COLMAP feature matcher.
//
// The matcher choice dominates total runtime. `exhaustive_matcher` compares
// every image against every other image, which is O(n^2) in the image count: a
// 428-image survey produces 91,378 image pairs. Aerial imagery is captured along
// flight lines, so only nearby frames can overlap; `sequential_matcher` with an
// overlap of 10 reduces that to roughly 10n pairs and was measured to be the
// single largest saving available in the pipeline.
func matcherArgs(config Config, database string) []string {
	style := detectColmapOptionStyle(config.ColmapBin)
	args := []string{
		matcherCommand(config.Matcher),
		"--database_path", database,
		// CPU-only, as everywhere else in the pipeline.
		"--" + style.matchingGroup() + ".use_gpu", "0",
		"--" + style.matchingGroup() + ".num_threads", fmt.Sprint(config.resolveThreads()),
	}
	if matcherCommand(config.Matcher) == "sequential_matcher" {
		overlap := config.MatcherOverlap
		if overlap <= 0 {
			overlap = 10
		}
		args = append(args, "--SequentialMatching.overlap", fmt.Sprint(overlap))
	}
	return args
}

// describeMatcherStrategy returns a human-readable summary for the job log, so
// the chosen strategy and its expected cost are visible during a long run.
func describeMatcherStrategy(config Config, imageCount int) string {
	command := matcherCommand(config.Matcher)
	if command == "exhaustive_matcher" {
		pairs := 0
		if imageCount > 1 {
			pairs = imageCount * (imageCount - 1) / 2
		}
		return fmt.Sprintf("matcher=exhaustive_matcher, candidate image pairs=%d (all-pairs)", pairs)
	}
	if command == "sequential_matcher" {
		overlap := config.MatcherOverlap
		if overlap <= 0 {
			overlap = 10
		}
		pairs := imageCount * overlap
		return fmt.Sprintf("matcher=sequential_matcher overlap=%d, candidate image pairs=%d", overlap, pairs)
	}
	return "matcher=" + command
}

// colmapCPUNote documents the CPU-only guarantee for logs and diagnostics.
const colmapCPUNote = "CPU-only: GPU switches are forced off for every COLMAP and OpenMVS stage"
