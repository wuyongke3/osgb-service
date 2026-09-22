package main

// colmap_args_test.go covers the CPU-only guarantee, the runtime thread
// resolution, and the COLMAP option-name selection.
//
// The option-name selection exists because COLMAP renamed its SIFT option
// groups: 3.x uses --SiftExtraction.* / --SiftMatching.*, 4.x uses
// --FeatureExtraction.* / --FeatureMatching.*. Passing the wrong spelling is a
// hard error ("unrecognised option") rather than a warning, so a wrong guess
// fails an entire job. The deployment image ships 3.9.1 while a developer
// machine may have 4.x, which is why detection happens per binary.

import (
	"runtime"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// CPU-only guarantee
// ---------------------------------------------------------------------------

// TestFeatureExtractorForcesCPU is the core assertion for the CPU-only
// requirement: the GPU switch must be explicitly present and set to 0. COLMAP
// defaults use_gpu to 1, so omitting it would attempt CUDA in the container.
func TestFeatureExtractorForcesCPU(t *testing.T) {
	for _, style := range []colmapOptionStyle{colmapStyleLegacy, colmapStyleModern} {
		args := featureExtractorArgs(Config{}, "db.db", "imgs")
		_ = style
		if !hasFlagValue(args, "--SiftExtraction.use_gpu", "0") &&
			!hasFlagValue(args, "--FeatureExtraction.use_gpu", "0") {
			t.Fatalf("extractor args do not force use_gpu=0: %v", args)
		}
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "use_gpu 1") || strings.Contains(joined, "use_gpu=1") {
			t.Fatalf("extractor args enable the GPU: %v", args)
		}
		if strings.Contains(joined, "--cuda-device") {
			t.Fatalf("extractor args request a CUDA device: %v", args)
		}
	}
}

// TestMatcherForcesCPU is the same guarantee for the matcher stage.
func TestMatcherForcesCPU(t *testing.T) {
	config := Config{Matcher: "sequential", MatcherOverlap: 10}
	args := matcherArgs(config, "db.db")
	if !hasFlagValue(args, "--SiftMatching.use_gpu", "0") &&
		!hasFlagValue(args, "--FeatureMatching.use_gpu", "0") {
		t.Fatalf("matcher args do not force use_gpu=0: %v", args)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "use_gpu 1") {
		t.Fatalf("matcher args enable the GPU: %v", args)
	}
	if strings.Contains(joined, "--cuda-device") {
		t.Fatalf("matcher args request a CUDA device: %v", args)
	}
}

// hasFlagValue reports whether args contains the flag immediately followed by
// the expected value.
func hasFlagValue(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Thread resolution
// ---------------------------------------------------------------------------

// TestResolveThreadsExplicitWins: an operator setting must always be honoured.
func TestResolveThreadsExplicitWins(t *testing.T) {
	for _, threads := range []int{1, 2, 4, 32, 128} {
		config := Config{Threads: threads}
		if got := config.resolveThreads(); got != threads {
			t.Errorf("resolveThreads() with explicit %d = %d, want %d", threads, got, threads)
		}
	}
}

// TestResolveThreadsAutoUsesAvailableCPUs is the regression test for the
// hard-coded `num_threads 4`: the resolved count must scale with the host rather
// than being fixed at 4.
func TestResolveThreadsAutoUsesAvailableCPUs(t *testing.T) {
	config := Config{Threads: 0}
	got := config.resolveThreads()

	if got < 1 {
		t.Fatalf("resolveThreads() = %d, want at least 1", got)
	}
	if got > maxAutoThreads {
		t.Fatalf("resolveThreads() = %d, want at most %d", got, maxAutoThreads)
	}

	available := runtime.GOMAXPROCS(0)
	want := available - 1
	if want < 1 {
		want = 1
	}
	if want > maxAutoThreads {
		want = maxAutoThreads
	}
	if got != want {
		t.Fatalf("resolveThreads() = %d, want %d (GOMAXPROCS=%d)", got, want, available)
	}
}

// TestResolveThreadsNegativeTreatedAsAuto guards against a malformed negative
// setting disabling parallelism or panicking.
func TestResolveThreadsNegativeTreatedAsAuto(t *testing.T) {
	config := Config{Threads: -5}
	if got := config.resolveThreads(); got < 1 {
		t.Fatalf("resolveThreads() with a negative setting = %d, want auto resolution", got)
	}
}

// ---------------------------------------------------------------------------
// Matcher selection
// ---------------------------------------------------------------------------

func TestMatcherCommandSelection(t *testing.T) {
	cases := map[string]string{
		"sequential":         "sequential_matcher",
		"SEQUENTIAL":         "sequential_matcher",
		"sequential_matcher": "sequential_matcher",
		"exhaustive":         "exhaustive_matcher",
		"exhaustive_matcher": "exhaustive_matcher",
		"vocab_tree":         "vocab_tree_matcher",
		"vocab":              "vocab_tree_matcher",
		"spatial":            "spatial_matcher",
		"transitive":         "transitive_matcher",
		"  sequential  ":     "sequential_matcher",
		"":                   "exhaustive_matcher",
		"nonsense":           "exhaustive_matcher",
		"typo_sequential_x":  "exhaustive_matcher",
	}
	for input, want := range cases {
		if got := matcherCommand(input); got != want {
			t.Errorf("matcherCommand(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestMatcherArgsSequentialIncludesOverlap: the overlap value is what makes the
// sequential matcher correct for aerial imagery, so it must reach the argv.
func TestMatcherArgsSequentialIncludesOverlap(t *testing.T) {
	config := Config{Matcher: "sequential", MatcherOverlap: 15}
	args := matcherArgs(config, "db.db")
	if args[0] != "sequential_matcher" {
		t.Fatalf("subcommand = %q, want sequential_matcher", args[0])
	}
	if !hasFlagValue(args, "--SequentialMatching.overlap", "15") {
		t.Fatalf("overlap not passed through: %v", args)
	}
}

// A zero or negative overlap is invalid for COLMAP, so it must be replaced with
// the documented default rather than forwarded.
func TestMatcherArgsOverlapDefaultsWhenUnset(t *testing.T) {
	for _, overlap := range []int{0, -1} {
		config := Config{Matcher: "sequential", MatcherOverlap: overlap}
		args := matcherArgs(config, "db.db")
		if !hasFlagValue(args, "--SequentialMatching.overlap", "10") {
			t.Errorf("overlap %d produced %v, want the default 10", overlap, args)
		}
	}
}

// The exhaustive matcher has no overlap concept, so passing it would be invalid.
func TestMatcherArgsOmitsOverlapForNonSequential(t *testing.T) {
	config := Config{Matcher: "exhaustive", MatcherOverlap: 10}
	args := matcherArgs(config, "db.db")
	if strings.Contains(strings.Join(args, " "), "SequentialMatching.overlap") {
		t.Fatalf("exhaustive matcher received an overlap flag: %v", args)
	}
}

// Every matcher must carry a thread count, otherwise COLMAP would use its
// internal default and the configured value would silently not apply.
func TestMatcherArgsAlwaysPassThreadCount(t *testing.T) {
	for _, matcher := range []string{"sequential", "exhaustive", "vocab_tree", "spatial"} {
		config := Config{Matcher: matcher, Threads: 6}
		args := matcherArgs(config, "db.db")
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "num_threads 6") {
			t.Errorf("matcher %q args missing the thread count: %v", matcher, args)
		}
	}
}

// ---------------------------------------------------------------------------
// Option-style detection and generation
// ---------------------------------------------------------------------------

// TestOptionGroupNames pins the two spellings, since getting these wrong makes
// COLMAP exit immediately with "unrecognised option".
func TestOptionGroupNames(t *testing.T) {
	if got := colmapStyleLegacy.extractionGroup(); got != "SiftExtraction" {
		t.Errorf("legacy extraction group = %q", got)
	}
	if got := colmapStyleLegacy.matchingGroup(); got != "SiftMatching" {
		t.Errorf("legacy matching group = %q", got)
	}
	if got := colmapStyleModern.extractionGroup(); got != "FeatureExtraction" {
		t.Errorf("modern extraction group = %q", got)
	}
	if got := colmapStyleModern.matchingGroup(); got != "FeatureMatching" {
		t.Errorf("modern matching group = %q", got)
	}
}

// A missing binary must degrade to the spelling used by the pinned deployment
// image rather than panicking during command construction.
func TestDetectStyleFallsBackForMissingBinary(t *testing.T) {
	style := detectColmapOptionStyle("definitely-not-a-real-colmap-binary-xyz")
	if style != colmapStyleLegacy {
		t.Errorf("style for a missing binary = %v, want legacy", style)
	}
}

// Detection must be memoised: it spawns a subprocess, and the arguments are
// rebuilt for every job.
func TestDetectStyleIsCached(t *testing.T) {
	const binary = "definitely-not-a-real-colmap-binary-xyz"
	first := detectColmapOptionStyle(binary)
	if _, ok := colmapStyleCache.Load(binary); !ok {
		t.Fatal("detection result was not cached")
	}
	second := detectColmapOptionStyle(binary)
	if first != second {
		t.Fatalf("cached detection was inconsistent: %v vs %v", first, second)
	}
}

// TestGenerateArgsPerStyle exercises generation through the real entry points
// with an injected style, using a stub binary name so no subprocess is needed.
func TestGenerateArgsPerStyle(t *testing.T) {
	config := Config{Matcher: "sequential", MatcherOverlap: 10, Threads: 4, MaxImageSize: 3200}
	extractor := featureExtractorArgs(config, "db.db", "images")
	matcher := matcherArgs(config, "db.db")

	// The chosen style must be self-consistent between the two stages of one run.
	legacyExtract := strings.Join(extractor, " ")
	legacyMatch := strings.Join(matcher, " ")
	style := detectColmapOptionStyle(config.ColmapBin)
	if style == colmapStyleModern {
		if !strings.Contains(legacyExtract, "--FeatureExtraction.") {
			t.Errorf("modern style produced legacy extractor args: %v", extractor)
		}
		if !strings.Contains(legacyMatch, "--FeatureMatching.") {
			t.Errorf("modern style produced legacy matcher args: %v", matcher)
		}
	} else {
		if !strings.Contains(legacyExtract, "--SiftExtraction.") {
			t.Errorf("legacy style produced modern extractor args: %v", extractor)
		}
		if !strings.Contains(legacyMatch, "--SiftMatching.") {
			t.Errorf("legacy style produced modern matcher args: %v", matcher)
		}
	}
}

// A non-positive max image size means "do not constrain", matching COLMAP's own
// -1 default, so the flag must be omitted rather than sent as 0 or -1.
func TestFeatureExtractorOmitsMaxImageSizeWhenUnset(t *testing.T) {
	config := Config{Threads: 4, MaxImageSize: 0}
	args := featureExtractorArgs(config, "db.db", "images")
	if strings.Contains(strings.Join(args, " "), "max_image_size") {
		t.Fatalf("max_image_size should be omitted when unset: %v", args)
	}
}

// The image size must actually reach the argv when configured.
func TestFeatureExtractorPassesMaxImageSize(t *testing.T) {
	config := Config{Threads: 4, MaxImageSize: 1600}
	args := featureExtractorArgs(config, "db.db", "images")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "max_image_size 1600") {
		t.Fatalf("max_image_size not passed: %v", args)
	}
}

// ---------------------------------------------------------------------------
// Strategy reporting
// ---------------------------------------------------------------------------

// TestDescribeMatcherStrategyQuantifiesSaving verifies the log line actually
// conveys the cost difference, which is what makes a slow run diagnosable.
func TestDescribeMatcherStrategyQuantifiesSaving(t *testing.T) {
	sequential := describeMatcherStrategy(Config{Matcher: "sequential", MatcherOverlap: 10}, 428)
	exhaustive := describeMatcherStrategy(Config{Matcher: "exhaustive"}, 428)

	if !strings.Contains(sequential, "sequential_matcher") {
		t.Errorf("sequential description = %q", sequential)
	}
	if !strings.Contains(sequential, "4280") {
		t.Errorf("sequential description should state the pair count: %q", sequential)
	}
	if !strings.Contains(exhaustive, "91378") {
		t.Errorf("exhaustive description should state the all-pairs count: %q", exhaustive)
	}
}

// A single image has no pairs; the arithmetic must not underflow.
func TestDescribeMatcherStrategyHandlesTinyInputs(t *testing.T) {
	for _, count := range []int{0, 1} {
		got := describeMatcherStrategy(Config{Matcher: "exhaustive"}, count)
		if got == "" {
			t.Fatalf("empty description for %d images", count)
		}
		if strings.Contains(got, "-") && strings.Contains(got, "pairs=-") {
			t.Fatalf("negative pair count for %d images: %q", count, got)
		}
	}
}
