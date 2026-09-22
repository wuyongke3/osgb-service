package main

// orchestration_test.go covers the argument-expansion, path-safety and
// job-lifecycle logic in main.go. These paths were previously untested even
// though they guard the two boundaries that matter most for a long-running
// unattended service: what gets written to disk, and what a browser request is
// allowed to reach.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// expandArgs / tokenize: these build the argv passed to COLMAP, OpenMVS and
// osgconv, so a quoting bug here breaks every job.
// ---------------------------------------------------------------------------

func TestExpandArgsSubstitutesPlaceholders(t *testing.T) {
	vars := map[string]string{
		"{job_dir}":   "/data/jobs/abc",
		"{input_dir}": "/data/uploads/img",
	}
	got, err := expandArgs(`feature_extractor --database_path "{job_dir}/database.db" --image_path "{input_dir}"`, vars)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"feature_extractor",
		"--database_path", "/data/jobs/abc/database.db",
		"--image_path", "/data/uploads/img",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d args %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A path containing spaces must survive as a single argument. OpenMVS and COLMAP
// receive Windows paths with spaces in ordinary use, so this is not theoretical.
func TestExpandArgsKeepsQuotedPathsWithSpacesTogether(t *testing.T) {
	vars := map[string]string{"{job_dir}": `C:\Program Files\survey run`}
	got, err := expandArgs(`tool -i "{job_dir}" -o "{job_dir}\out.mvs"`, vars)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tool", "-i", `C:\Program Files\survey run`, "-o", `C:\Program Files\survey run\out.mvs`}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExpandArgsEmptyTemplate(t *testing.T) {
	got, err := expandArgs("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty template produced %q", got)
	}
}

func TestTokenizeRejectsUnterminatedQuote(t *testing.T) {
	if _, err := tokenize(`tool "unclosed`); err == nil {
		t.Fatal("expected an error for an unterminated quote")
	}
	if _, err := tokenize(`tool 'unclosed`); err == nil {
		t.Fatal("expected an error for an unterminated single quote")
	}
}

func TestTokenizeHandlesEscapesAndRepeatedSpaces(t *testing.T) {
	got, err := tokenize(`a   b\ c   "d e"`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "b c", "d e"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestTokenizePreservesWindowsBackslashPaths is the regression test for a real
// defect: a backslash used to be consumed as an escape character everywhere, so
// `-o "C:\data\job\out.mvs"` was mangled into the single token
// `C:datajobout.mvs`. Forward-slash templates worked, which is why the shipped
// defaults hid the bug, but any user pasting a native Windows path silently got
// nonsense output paths.
func TestTokenizePreservesWindowsBackslashPaths(t *testing.T) {
	got, err := tokenize(`osgconv -i "C:\data\job\model.obj" -o "C:\data\job\root.osgb"`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"osgconv",
		"-i", `C:\data\job\model.obj`,
		"-o", `C:\data\job\root.osgb`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// UNC paths without a trailing separator are the realistic case and must work.
// Note on escaping: inside the argument a doubled backslash is a literal
// backslash escape, exactly as in a shell, so the template below yields a single
// leading backslash pair from the four typed characters.
func TestTokenizePreservesUNCPath(t *testing.T) {
	got, err := tokenize(`tool -i "\\server\share\flight"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %q, want 3 tokens", got)
	}
	// `\\` collapses to one literal backslash, matching shell semantics.
	want := `\server\share\flight`
	if got[2] != want {
		t.Errorf("UNC token = %q, want %q", got[2], want)
	}
}

// A literal pair of leading backslashes is expressed as four in the template,
// which is the standard way to pass a UNC prefix to a shell-style parser.
func TestTokenizeUNCPrefixWithEscapedBackslashes(t *testing.T) {
	got, err := tokenize(`tool -i "\\\\server\share\flight"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %q, want 3 tokens", got)
	}
	want := `\\server\share\flight`
	if got[2] != want {
		t.Errorf("UNC token = %q, want %q", got[2], want)
	}
}

// A quoted Windows path containing a space must stay one argument.
func TestTokenizePreservesWindowsPathWithSpace(t *testing.T) {
	got, err := tokenize(`tool -i "C:\Program Files\survey"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %q, want 3 tokens", got)
	}
	if got[2] != `C:\Program Files\survey` {
		t.Errorf("token = %q, want %q", got[2], `C:\Program Files\survey`)
	}
}

// Documented limitation (not a regression): a quoted argument that ends with a
// backslash immediately before the closing quote is inherently ambiguous with an
// escaped quote, e.g. `"C:\dir\"`. Every shell-style parser resolves it the same
// way, and the previous implementation also consumed the closing quote and
// reported an unterminated quote. Trailing separators are therefore rejected
// with a clear error rather than silently producing a wrong path.
func TestTokenizeTrailingBackslashBeforeQuoteIsAmbiguous(t *testing.T) {
	_, err := tokenize(`tool -i "C:\dir\"`)
	if err == nil {
		t.Fatal("expected the ambiguous trailing backslash to be reported as an error")
	}
	if !strings.Contains(err.Error(), "unterminated quote") {
		t.Errorf("error = %v, want an unterminated-quote report", err)
	}
}

// Escaped quotes must still work, so the fix does not remove real escaping.
func TestTokenizeStillSupportsEscapedQuotes(t *testing.T) {
	got, err := tokenize(`tool -name "say \"hi\""`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %q, want 3 tokens", got)
	}
	if got[2] != `say "hi"` {
		t.Errorf("escaped-quote token = %q, want %q", got[2], `say "hi"`)
	}
}

// Mixed separators appear in real .env files; both forms must pass through.
func TestTokenizeMixedSeparators(t *testing.T) {
	got, err := tokenize(`tool "C:\data/mixed\path/file.obj"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != `C:\data/mixed\path/file.obj` {
		t.Fatalf("got %q, want the mixed path preserved", got)
	}
}

// ---------------------------------------------------------------------------
// safeUploadRelativePath: the upload endpoint writes browser-supplied names to
// disk, so traversal must be impossible.
// ---------------------------------------------------------------------------

func TestSafeUploadRelativePathRejectsTraversal(t *testing.T) {
	rejected := []string{
		"../escape.jpg",
		"..\\escape.jpg",
		"a/../../escape.jpg",
		"..",
		".",
		"",
	}
	for _, name := range rejected {
		if got, err := safeUploadRelativePath(name); err == nil {
			t.Errorf("safeUploadRelativePath(%q) = %q, want error", name, got)
		}
	}
}

// A leading slash is stripped rather than rejected: browsers can legitimately
// send "/folder/file.jpg" for a directory upload, and trimming it keeps the file
// inside DATA_DIR, which is the property that actually matters.
func TestSafeUploadRelativePathStripsLeadingSlashSafely(t *testing.T) {
	got, err := safeUploadRelativePath("/flight/IMG_1.jpg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filepath.IsAbs(got) {
		t.Fatalf("result %q is still absolute", got)
	}
	if strings.Contains(got, "..") {
		t.Fatalf("result %q contains a traversal sequence", got)
	}
}

func TestSafeUploadRelativePathKeepsNestedDirectories(t *testing.T) {
	cases := map[string]string{
		"flight/IMG_0001.jpg": filepath.FromSlash("flight/IMG_0001.jpg"),
		"a/b/c/IMG_2.JPG":     filepath.FromSlash("a/b/c/IMG_2.JPG"),
		"./IMG_3.jpg":         "IMG_3.jpg",
		"nested/./IMG_4.jpg":  filepath.FromSlash("nested/IMG_4.jpg"),
	}
	for in, want := range cases {
		got, err := safeUploadRelativePath(in)
		if err != nil {
			t.Errorf("safeUploadRelativePath(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("safeUploadRelativePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// validateOutputPath / pathWithin: OUTPUT_ROOT containment. A bug here would let
// a request write outside the managed deliverables tree.
// ---------------------------------------------------------------------------

func TestValidateOutputPathRequiresAbsolutePath(t *testing.T) {
	if err := validateOutputPath("relative/out.osgb", ""); err == nil {
		t.Fatal("expected relative path to be rejected")
	}
	if err := validateOutputPath("", ""); err == nil {
		t.Fatal("expected empty path to be rejected")
	}
}

func TestValidateOutputPathEnforcesOutputRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "job", "root.osgb")
	if err := validateOutputPath(inside, root); err != nil {
		t.Fatalf("path inside OUTPUT_ROOT rejected: %v", err)
	}

	outside := filepath.Join(filepath.Dir(root), "elsewhere", "root.osgb")
	if err := validateOutputPath(outside, root); err == nil {
		t.Fatal("expected a path outside OUTPUT_ROOT to be rejected")
	}

	// A sibling directory whose name merely shares the root prefix must not
	// pass: naive strings.HasPrefix checks fail this case.
	sibling := root + "-sibling"
	if err := validateOutputPath(filepath.Join(sibling, "root.osgb"), root); err == nil {
		t.Fatal("expected a prefix-sharing sibling directory to be rejected")
	}
}

func TestValidateOutputPathAllowsAnyPathWithoutRoot(t *testing.T) {
	dir := t.TempDir()
	if err := validateOutputPath(filepath.Join(dir, "a.osgb"), ""); err != nil {
		t.Fatalf("unexpected rejection without OUTPUT_ROOT: %v", err)
	}
}

func TestPathWithin(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name      string
		candidate string
		want      bool
	}{
		{"same dir", root, true},
		{"child", filepath.Join(root, "a", "b.osgb"), true},
		{"sibling", filepath.Dir(root), false},
		{"prefix sibling", root + "-x", false},
		{"parent", filepath.Join(root, ".."), false},
	}
	for _, c := range cases {
		if got := pathWithin(root, c.candidate); got != c.want {
			t.Errorf("%s: pathWithin(%q) = %v, want %v", c.name, c.candidate, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// normalizeOutputPath / isSupportedImage / inspectInputPath
// ---------------------------------------------------------------------------

func TestNormalizeOutputPath(t *testing.T) {
	if got := normalizeOutputPath(`C:\out\model.osgb`); !strings.HasSuffix(strings.ToLower(got), ".osgb") {
		t.Errorf("normalizeOutputPath kept a non-osgb name: %q", got)
	}
	dir := filepath.Join(t.TempDir(), "deliver")
	got := normalizeOutputPath(dir)
	if filepath.Base(got) != "model.osgb" {
		t.Errorf("normalizeOutputPath(%q) = %q, want it to append model.osgb", dir, got)
	}
}

func TestIsSupportedImage(t *testing.T) {
	supported := []string{"a.jpg", "a.JPEG", "a.png", "a.tif", "a.TIFF", "a.webp"}
	for _, name := range supported {
		if !isSupportedImage(name) {
			t.Errorf("isSupportedImage(%q) = false, want true", name)
		}
	}
	unsupported := []string{"a.txt", "a.mvs", "a.osgb", "a", "a.jpg.exe"}
	for _, name := range unsupported {
		if isSupportedImage(name) {
			t.Errorf("isSupportedImage(%q) = true, want false", name)
		}
	}
}

func TestInspectInputPathCountsImages(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.jpg", "b.JPG", "c.png", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	count, bytes, err := inspectInputPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	if bytes != 3 {
		t.Errorf("bytes = %d, want 3", bytes)
	}
}

// A directory with no images must be rejected before a job is created, rather
// than failing hours later inside COLMAP.
func TestInspectInputPathRejectsEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := inspectInputPath(dir); err == nil {
		t.Fatal("expected an error for a directory with no supported images")
	}
}

func TestInspectInputPathPrefersNestedImagesDir(t *testing.T) {
	root := t.TempDir()
	images := filepath.Join(root, "images")
	if err := os.MkdirAll(images, 0o755); err != nil {
		t.Fatal(err)
	}
	// Decoy at the root level, real images in images/.
	if err := os.WriteFile(filepath.Join(root, "decoy.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.jpg", "b.jpg"} {
		if err := os.WriteFile(filepath.Join(images, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	count, _, err := inspectInputPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2 (only images/ should be scanned)", count)
	}
}

// ---------------------------------------------------------------------------
// humanizeJobError: operators rely on these hints when a long job dies.
// ---------------------------------------------------------------------------

func TestHumanizeJobError(t *testing.T) {
	if got := humanizeJobError(nil); got != "" {
		t.Errorf("humanizeJobError(nil) = %q, want empty", got)
	}
	killed := humanizeJobError(context.DeadlineExceeded)
	if !strings.Contains(killed, "超时") {
		t.Errorf("deadline error hint = %q, want it to mention the timeout", killed)
	}
	oom := humanizeJobError(errStub("signal: killed"))
	if !strings.Contains(oom, "内存") {
		t.Errorf("signal:killed hint = %q, want it to mention memory", oom)
	}
	plain := humanizeJobError(errStub("something else"))
	if plain != "something else" {
		t.Errorf("plain error = %q, want it unchanged", plain)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }

// mustTime parses an RFC3339 timestamp or fails the test.
func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

// ---------------------------------------------------------------------------
// safeCheckpointName: checkpoint filenames are derived from step names.
// ---------------------------------------------------------------------------

func TestSafeCheckpointNameIsFilesystemSafe(t *testing.T) {
	got := safeCheckpointName("Mesh reconstruction (OpenMVS)")
	if strings.ContainsAny(got, ` /\:*?"<>|()`) {
		t.Fatalf("safeCheckpointName produced unsafe name %q", got)
	}
	if got == "" {
		t.Fatal("safeCheckpointName returned an empty name")
	}
	// Distinct steps must not collapse onto the same checkpoint file.
	a := safeCheckpointName("Dense fusion (OpenMVS)")
	b := safeCheckpointName("Mesh reconstruction (OpenMVS)")
	if a == b {
		t.Fatalf("distinct step names collided: %q", a)
	}
}

// ---------------------------------------------------------------------------
// Job lifecycle persistence: the service must recover a consistent state after
// an unclean restart instead of leaving jobs stuck as "running" forever.
// ---------------------------------------------------------------------------

func newTestManager(t *testing.T, dataDir string) *JobManager {
	t.Helper()
	config := Config{DataDir: dataDir, PipelineMode: "native", Port: "0"}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewJobManager(config, logger)
}

func TestLoadPersistedMarksInterruptedJobsResumable(t *testing.T) {
	dataDir := t.TempDir()
	jobDir := filepath.Join(dataDir, "jobs", "job-running")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A job left as "running" by a crash.
	persisted := Job{
		ID: "job-running", Status: "running", Phase: phaseTexture,
		Logs: []LogEntry{},
	}
	data, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "job.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	manager := newTestManager(t, dataDir)
	got := manager.snapshot("job-running")
	if got == nil {
		t.Fatal("persisted job was not loaded")
	}
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed after an unclean restart", got.Status)
	}
	if !got.Resumable {
		t.Error("interrupted job should be marked resumable")
	}
	if got.Error == "" {
		t.Error("interrupted job should explain why it failed")
	}
}

func TestLoadPersistedKeepsCompletedJobs(t *testing.T) {
	dataDir := t.TempDir()
	jobDir := filepath.Join(dataDir, "jobs", "job-done")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	persisted := Job{ID: "job-done", Status: "completed", Phase: phaseCompleted, Logs: []LogEntry{}}
	data, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "job.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	manager := newTestManager(t, dataDir)
	got := manager.snapshot("job-done")
	if got == nil {
		t.Fatal("completed job was not loaded")
	}
	if got.Status != "completed" {
		t.Errorf("status = %q, want completed", got.Status)
	}
	if got.Resumable {
		t.Error("a completed job must not be offered for resume")
	}
}

// A corrupt job.json must be skipped rather than aborting startup, otherwise a
// single bad file would make the whole service fail to boot.
func TestLoadPersistedSkipsCorruptRecords(t *testing.T) {
	dataDir := t.TempDir()
	jobDir := filepath.Join(dataDir, "jobs", "job-corrupt")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "job.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := newTestManager(t, dataDir)
	if got := manager.snapshot("job-corrupt"); got != nil {
		t.Errorf("corrupt job should have been skipped, got %+v", got)
	}
}

func TestPersistAndReloadJob(t *testing.T) {
	dataDir := t.TempDir()
	manager := newTestManager(t, dataDir)
	manager.create("job-1", "/in", "/out/root.osgb", 3, 100)
	manager.update("job-1", func(j *Job) { j.Phase = phaseMesh; j.Progress = 74 })
	manager.logLine("job-1", "system", "hello")

	// A second manager over the same directory simulates a restart.
	reloaded := newTestManager(t, dataDir)
	got := reloaded.snapshot("job-1")
	if got == nil {
		t.Fatal("job was not persisted")
	}
	if got.Progress != 74 || got.Phase != phaseMesh {
		t.Errorf("progress/phase = %d/%s, want 74/%s", got.Progress, got.Phase, phaseMesh)
	}
	if got.InputCount != 3 {
		t.Errorf("input count = %d, want 3", got.InputCount)
	}
	if len(got.Logs) == 0 {
		t.Error("logs were not persisted")
	}
}

// Log growth must be bounded, because a long COLMAP run emits thousands of
// lines and job.json is rewritten on every update.
func TestLogLinesAreCapped(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	manager.create("job-log", "/in", "/out.osgb", 1, 1)
	for i := 0; i < 5100; i++ {
		manager.logLine("job-log", "stdout", "line")
	}
	got := manager.snapshot("job-log")
	if len(got.Logs) > 5000 {
		t.Fatalf("log grew to %d entries, want <= 5000", len(got.Logs))
	}
}

// ---------------------------------------------------------------------------
// Plan lifecycle
// ---------------------------------------------------------------------------

func TestPlanLifecycleAndPersistence(t *testing.T) {
	dataDir := t.TempDir()
	manager := newTestManager(t, dataDir)

	plan := manager.createPlan("survey", "", filepath.Join(dataDir, "in"), nil)
	if plan == nil || plan.ID == "" {
		t.Fatal("createPlan returned no plan")
	}
	if plan.Status != "draft" {
		t.Errorf("new plan status = %q, want draft", plan.Status)
	}

	reloaded := newTestManager(t, dataDir)
	if got := reloaded.planSnapshot(plan.ID); got == nil {
		t.Fatal("plan was not persisted")
	} else if got.Name != "survey" {
		t.Errorf("plan name = %q, want survey", got.Name)
	}
}

func TestCreatePlanWithFutureScheduleBecomesScheduled(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	future := mustTime(t, "2999-01-01T00:00:00Z")
	plan := manager.createPlan("later", "", "/tmp/in", &future)
	if plan.Status != "scheduled" {
		t.Errorf("status = %q, want scheduled", plan.Status)
	}
}

func TestCreatePlanWithPastScheduleStaysDraft(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	past := mustTime(t, "2000-01-01T00:00:00Z")
	plan := manager.createPlan("past", "", "/tmp/in", &past)
	if plan.Status != "draft" {
		t.Errorf("status = %q, want draft", plan.Status)
	}
}

// ---------------------------------------------------------------------------
// Config: tool-path edits must be refused while work is in flight, otherwise a
// running job could switch binaries midway.
// ---------------------------------------------------------------------------

func TestUpdateToolPathsWritesConfigFile(t *testing.T) {
	dataDir := t.TempDir()
	configFile := filepath.Join(dataDir, "service.env")
	config := Config{DataDir: dataDir, ConfigFile: configFile, PipelineMode: "native", Port: "0"}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	manager := NewJobManager(config, logger)

	if err := manager.updateToolPaths(map[string]string{"COLMAP_BIN": "/opt/colmap"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("config file was not written: %v", err)
	}
	if !strings.Contains(string(data), "COLMAP_BIN=/opt/colmap") {
		t.Errorf("config file missing the new value:\n%s", data)
	}
	if got := manager.configSnapshot().ColmapBin; got != "/opt/colmap" {
		t.Errorf("in-memory ColmapBin = %q, want /opt/colmap", got)
	}
}

func TestUpdateToolPathsRejectsUnknownKey(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	if err := manager.updateToolPaths(map[string]string{"NOT_A_TOOL": "x"}); err == nil {
		t.Fatal("expected an error for an unsupported tool key")
	}
}

func TestUpdateToolPathsBlockedWhileJobActive(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	manager.create("busy", "/in", "/out.osgb", 1, 1)
	manager.update("busy", func(j *Job) { j.Status = "running" })

	if err := manager.updateToolPaths(map[string]string{"COLMAP_BIN": "/x"}); err == nil {
		t.Fatal("expected tool changes to be refused while a job is running")
	}
}

// writeToolConfig must preserve unrelated keys instead of truncating the file.
func TestWriteToolConfigPreservesExistingKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.env")
	if err := os.WriteFile(path, []byte("KEEP_ME=1\nCOLMAP_BIN=old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeToolConfig(path, map[string]string{"COLMAP_BIN": "new"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "KEEP_ME=1") {
		t.Errorf("unrelated key was dropped:\n%s", text)
	}
	if !strings.Contains(text, "COLMAP_BIN=new") {
		t.Errorf("value was not updated:\n%s", text)
	}
	if strings.Contains(text, "COLMAP_BIN=old") {
		t.Errorf("stale value survived:\n%s", text)
	}
}

// ---------------------------------------------------------------------------
// checkTileFailureTolerance: the policy deciding whether a partly damaged tree
// is still delivered. Previously a single bad tile discarded an entire
// multi-hour run.
// ---------------------------------------------------------------------------

func TestCheckTileFailureTolerance(t *testing.T) {
	cases := []struct {
		name    string
		failed  int
		total   int
		wantErr bool
	}{
		{"no failures", 0, 256, false},
		{"one bad tile out of 256 is tolerated", 1, 256, false},
		{"exactly at the 10 percent limit", 10, 100, false},
		{"just above the limit", 11, 100, true},
		{"all tiles failed", 256, 256, true},
		{"single tile failed of one", 1, 1, true},
		{"no tiles at all", 0, 0, false},
		{"no tiles but failures reported", 1, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkTileFailureTolerance(c.failed, c.total)
			if c.wantErr && err == nil {
				t.Fatalf("checkTileFailureTolerance(%d,%d) = nil, want an error", c.failed, c.total)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("checkTileFailureTolerance(%d,%d) = %v, want nil", c.failed, c.total, err)
			}
		})
	}
}

// The error must explain how bad the damage was, since it is what an operator
// sees when an overnight run is rejected.
func TestCheckTileFailureToleranceErrorIsDescriptive(t *testing.T) {
	err := checkTileFailureTolerance(50, 256)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"50", "256"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

// ---------------------------------------------------------------------------
// nativePipelineReady / commandAvailable
// ---------------------------------------------------------------------------

func TestCommandAvailable(t *testing.T) {
	if commandAvailable("") {
		t.Error("empty binary name should not be available")
	}
	if commandAvailable("definitely-not-a-real-binary-xyz") {
		t.Error("bogus binary reported as available")
	}
}

func TestNativePipelineReadyRequiresNativeMode(t *testing.T) {
	config := Config{PipelineMode: "external"}
	if nativePipelineReady(config) {
		t.Error("external mode must not report the native pipeline as ready")
	}
}
