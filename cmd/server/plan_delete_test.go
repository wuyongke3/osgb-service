package main

// plan_delete_test.go covers deleting plans together with their artifacts.
//
// The two behaviors worth guarding are the ones with real consequences: an
// upload shared by another plan must survive, and a plan with a running job must
// not be deleted out from under its subprocess.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// prepareDataDir creates a temporary data directory with the four
// subdirectories present, mirroring what main() creates at startup, without
// constructing a manager.
func prepareDataDir(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()
	for _, dir := range []string{"jobs", "uploads", "plans", "deliverables"} {
		if err := os.MkdirAll(filepath.Join(dataDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dataDir
}

// newDeleteTestManager builds a manager over a temporary data directory with the
// four data subdirectories present, mirroring what main() creates at startup.
func newDeleteTestManager(t *testing.T) (*JobManager, string) {
	t.Helper()
	dataDir := t.TempDir()
	for _, dir := range []string{"jobs", "uploads", "plans", "deliverables"} {
		if err := os.MkdirAll(filepath.Join(dataDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	config := Config{DataDir: dataDir, PipelineMode: "native", Port: "0"}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewJobManager(config, logger), dataDir
}

// writeFile creates a file with the given content, creating parent directories.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedPlan creates a plan plus one job, deliverable and upload on disk, and
// returns the plan id and the paths it created.
func seedPlan(t *testing.T, manager *JobManager, dataDir, name, uploadID string) (string, map[string]string) {
	t.Helper()
	plan := manager.createPlan(name, uploadID, "", nil)

	jobID := "job-" + plan.ID
	manager.create(jobID, "/in", "/out/root.osgb", 3, 100)
	manager.update(jobID, func(j *Job) { j.PlanID = plan.ID; j.Status = "completed" })

	paths := map[string]string{
		"plan":        filepath.Join(dataDir, "plans", plan.ID+".json"),
		"job":         filepath.Join(dataDir, "jobs", jobID),
		"deliverable": filepath.Join(dataDir, "deliverables", plan.ID),
		"upload":      filepath.Join(dataDir, "uploads", uploadID),
	}
	writeFile(t, filepath.Join(paths["job"], "database.db"), "job workspace data")
	writeFile(t, filepath.Join(paths["deliverable"], "root.osgb"), "delivered osgb")
	writeFile(t, filepath.Join(paths["upload"], "IMG_0001.jpg"), "imagery")
	return plan.ID, paths
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A full delete removes the plan record, the job workspace, the deliverables and
// the upload that only this plan used.
func TestDeletePlanRemovesEverything(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)
	planID, paths := seedPlan(t, manager, dataDir, "survey", "upload-solo")

	outcome, err := manager.deletePlan(planID)
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range paths {
		if pathExists(path) {
			t.Errorf("%s still exists: %s", name, path)
		}
	}
	if manager.planSnapshot(planID) != nil {
		t.Error("plan is still in memory after deletion")
	}
	if outcome.UploadDeleted != "upload-solo" {
		t.Errorf("UploadDeleted = %q, want upload-solo", outcome.UploadDeleted)
	}
	if len(outcome.JobsDeleted) != 1 {
		t.Errorf("JobsDeleted = %v, want one job", outcome.JobsDeleted)
	}
	if outcome.FreedBytes <= 0 {
		t.Errorf("FreedBytes = %d, want a positive figure", outcome.FreedBytes)
	}
}

// The regression test that matters most: an upload referenced by a surviving
// plan must not be deleted, or that plan loses its imagery.
func TestDeletePlanKeepsSharedUpload(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)

	// Two plans deliberately pointing at the same upload directory.
	firstID, _ := seedPlan(t, manager, dataDir, "first", "upload-shared")
	second := manager.createPlan("second", "upload-shared", "", nil)

	_, err := manager.deletePlan(firstID)
	if err != nil {
		t.Fatal(err)
	}

	shared := filepath.Join(dataDir, "uploads", "upload-shared")
	if !pathExists(filepath.Join(shared, "IMG_0001.jpg")) {
		t.Fatal("shared upload was deleted while another plan still references it")
	}
	// And the surviving plan must still be present and still point at it.
	survivor := manager.planSnapshot(second.ID)
	if survivor == nil {
		t.Fatal("the other plan was removed")
	}
	if survivor.UploadID != "upload-shared" {
		t.Errorf("surviving plan upload_id = %q", survivor.UploadID)
	}
}

// Deleting the last plan referencing an upload must then remove it.
func TestDeletePlanRemovesUploadOnceUnreferenced(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)
	firstID, _ := seedPlan(t, manager, dataDir, "first", "upload-shared")
	second := manager.createPlan("second", "upload-shared", "", nil)

	if _, err := manager.deletePlan(firstID); err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(dataDir, "uploads", "upload-shared")) {
		t.Fatal("upload removed too early")
	}

	outcome, err := manager.deletePlan(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.UploadDeleted != "upload-shared" {
		t.Errorf("UploadDeleted = %q, want upload-shared", outcome.UploadDeleted)
	}
	if pathExists(filepath.Join(dataDir, "uploads", "upload-shared")) {
		t.Error("upload still on disk after the last referencing plan was deleted")
	}
}

// A plan whose job is still running must be refused, so the workspace is not
// yanked from under a live subprocess.
func TestDeletePlanRefusesWhileJobRunning(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)
	planID, paths := seedPlan(t, manager, dataDir, "busy", "upload-busy")

	// Mark the plan's job as running.
	for _, job := range manager.snapshots() {
		if job.PlanID == planID {
			manager.update(job.ID, func(j *Job) { j.Status = "running" })
		}
	}

	if _, err := manager.deletePlan(planID); err == nil {
		t.Fatal("expected deletion of a running plan to be refused")
	}
	if !pathExists(paths["job"]) {
		t.Error("job workspace was removed despite the refusal")
	}
	if manager.planSnapshot(planID) == nil {
		t.Error("plan was removed despite the refusal")
	}
}

// A queued job is equally unsafe to delete.
func TestDeletePlanRefusesWhileJobQueued(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)
	planID, _ := seedPlan(t, manager, dataDir, "queued", "upload-queued")
	for _, job := range manager.snapshots() {
		if job.PlanID == planID {
			manager.update(job.ID, func(j *Job) { j.Status = "queued" })
		}
	}
	if _, err := manager.deletePlan(planID); err == nil {
		t.Fatal("expected deletion of a queued plan to be refused")
	}
}

// A failed or completed job must not block deletion.
func TestDeletePlanAllowsFinishedJobs(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)
	for _, status := range []string{"completed", "failed"} {
		planID, _ := seedPlan(t, manager, dataDir, "done-"+status, "upload-"+status)
		for _, job := range manager.snapshots() {
			if job.PlanID == planID {
				manager.update(job.ID, func(j *Job) { j.Status = status })
			}
		}
		if _, err := manager.deletePlan(planID); err != nil {
			t.Fatalf("status %q should be deletable: %v", status, err)
		}
	}
}

// An unknown plan reports os.ErrNotExist so the handler can answer 404.
func TestDeletePlanUnknownID(t *testing.T) {
	manager, _ := newDeleteTestManager(t)
	if _, err := manager.deletePlan("no-such-plan"); err == nil {
		t.Fatal("expected an error for an unknown plan")
	}
}

// A plan with an input path rather than an upload must delete cleanly and must
// not touch anything under uploads/.
func TestDeletePlanWithInputPathLeavesUploadsAlone(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)
	plan := manager.createPlan("host-mounted", "", "/mnt/input/site-a/images", nil)
	jobID := "job-" + plan.ID
	manager.create(jobID, "/mnt/input/site-a/images", "/out/root.osgb", 1, 1)
	manager.update(jobID, func(j *Job) { j.PlanID = plan.ID; j.Status = "completed" })

	unrelated := filepath.Join(dataDir, "uploads", "some-other-upload", "IMG.jpg")
	writeFile(t, unrelated, "keep me")

	outcome, err := manager.deletePlan(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.UploadDeleted != "" {
		t.Errorf("UploadDeleted = %q, want empty for an input-path plan", outcome.UploadDeleted)
	}
	if !pathExists(unrelated) {
		t.Error("an unrelated upload was removed")
	}
}

// Deleting a plan that produced no job or deliverables at all must still work.
func TestDeletePlanWithNoArtifacts(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)
	plan := manager.createPlan("empty", "", "/mnt/in", nil)
	outcome, err := manager.deletePlan(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.JobsDeleted) != 0 {
		t.Errorf("JobsDeleted = %v, want none", outcome.JobsDeleted)
	}
	if pathExists(filepath.Join(dataDir, "plans", plan.ID+".json")) {
		t.Error("plan record survived deletion")
	}
}

//----------------------------------------------------------------------------
// HTTP layer
//----------------------------------------------------------------------------

func TestHandleDeletePlansSingle(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)
	planID, paths := seedPlan(t, manager, dataDir, "single", "upload-single")

	request := httptest.NewRequest(http.MethodDelete, "/api/plans/"+planID, nil)
	recorder := httptest.NewRecorder()
	manager.handlePlans(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if pathExists(paths["deliverable"]) {
		t.Error("deliverables survived a single delete")
	}
}

// Deleting a plan that is already gone must succeed, not fail. A repeat click,
// or a batch whose entries another operator just removed, is a normal race and
// reporting it as an error made the feature look broken.
func TestHandleDeletePlansMissingPlanIsNotAnError(t *testing.T) {
	manager, _ := newDeleteTestManager(t)
	body := strings.NewReader(`{"ids":["already-gone"]}`)
	request := httptest.NewRequest(http.MethodDelete, "/api/plans", body)
	recorder := httptest.NewRecorder()
	manager.handlePlans(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an absent plan; body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Missing []string          `json:"missing"`
		Failed  map[string]string `json:"failed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Missing) != 1 || response.Missing[0] != "already-gone" {
		t.Errorf("missing = %v, want [already-gone]", response.Missing)
	}
	if len(response.Failed) != 0 {
		t.Errorf("failed = %v, want empty for an absent plan", response.Failed)
	}
}

// A single addressed plan that does not exist is the one case where 404 is the
// useful answer, because the caller named a specific resource.
func TestHandleDeletePlansUnknownSingleIs404(t *testing.T) {
	manager, _ := newDeleteTestManager(t)
	request := httptest.NewRequest(http.MethodDelete, "/api/plans/missing", nil)
	recorder := httptest.NewRecorder()
	manager.handlePlans(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

// A batch delete removes every requested plan and reports the tally.
func TestHandleDeletePlansBatch(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)
	firstID, firstPaths := seedPlan(t, manager, dataDir, "one", "upload-one")
	secondID, secondPaths := seedPlan(t, manager, dataDir, "two", "upload-two")

	body := strings.NewReader(`{"ids":["` + firstID + `","` + secondID + `"]}`)
	request := httptest.NewRequest(http.MethodDelete, "/api/plans", body)
	recorder := httptest.NewRecorder()
	manager.handlePlans(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Total     int `json:"total"`
		Succeeded int `json:"succeeded"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 2 || response.Succeeded != 2 {
		t.Errorf("total=%d succeeded=%d, want 2 and 2", response.Total, response.Succeeded)
	}
	for name, path := range map[string]string{"first deliverable": firstPaths["deliverable"], "second deliverable": secondPaths["deliverable"]} {
		if pathExists(path) {
			t.Errorf("%s survived the batch delete", name)
		}
	}
}

// A batch that mixes a deletable plan with an already-absent one must delete
// what it can and report the rest. Because the absent entry is not an error, the
// overall result is a success and the client can read both lists from the body.
func TestHandleDeletePlansBatchWithMissingEntry(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)
	goodID, goodPaths := seedPlan(t, manager, dataDir, "good", "upload-good")

	body := strings.NewReader(`{"ids":["` + goodID + `","does-not-exist"]}`)
	request := httptest.NewRequest(http.MethodDelete, "/api/plans", body)
	recorder := httptest.NewRecorder()
	manager.handlePlans(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if pathExists(goodPaths["deliverable"]) {
		t.Error("the valid plan was not deleted when another entry was absent")
	}
	var response struct {
		Succeeded int               `json:"succeeded"`
		Missing   []string          `json:"missing"`
		Failed    map[string]string `json:"failed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Succeeded != 1 {
		t.Errorf("succeeded = %d, want 1", response.Succeeded)
	}
	if len(response.Missing) != 1 {
		t.Errorf("missing = %v, want one entry", response.Missing)
	}
	if len(response.Failed) != 0 {
		t.Errorf("failed = %v, want none", response.Failed)
	}
}

// A batch that mixes a deletable plan with a blocked one is a genuine partial
// success and must answer 207 so the client can separate the two lists.
func TestHandleDeletePlansBatchPartialBlocked(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)

	// One plan with a running job, one free to delete.
	busyID, busyPaths := seedPlan(t, manager, dataDir, "busy", "upload-busy")
	for _, job := range manager.snapshots() {
		if job.PlanID == busyID {
			manager.update(job.ID, func(j *Job) { j.Status = "running" })
		}
	}
	freeID, freePaths := seedPlan(t, manager, dataDir, "free", "upload-free")

	body := strings.NewReader(`{"ids":["` + busyID + `","` + freeID + `"]}`)
	request := httptest.NewRequest(http.MethodDelete, "/api/plans", body)
	recorder := httptest.NewRecorder()
	manager.handlePlans(recorder, request)

	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207; body=%s", recorder.Code, recorder.Body.String())
	}
	if !pathExists(busyPaths["job"]) {
		t.Error("a running plan's workspace was removed")
	}
	if pathExists(freePaths["deliverable"]) {
		t.Error("the deletable plan was not removed")
	}
	var response struct {
		Succeeded int               `json:"succeeded"`
		Failed    map[string]string `json:"failed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Succeeded != 1 || len(response.Failed) != 1 {
		t.Errorf("succeeded=%d failed=%v, want 1 and one failure", response.Succeeded, response.Failed)
	}
}

// A batch where every plan has a running job must be a conflict, not a 404.
func TestHandleDeletePlansBatchAllRunningIs409(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)
	planID, _ := seedPlan(t, manager, dataDir, "busy", "upload-busy")
	for _, job := range manager.snapshots() {
		if job.PlanID == planID {
			manager.update(job.ID, func(j *Job) { j.Status = "running" })
		}
	}
	body := strings.NewReader(`{"ids":["` + planID + `"]}`)
	request := httptest.NewRequest(http.MethodDelete, "/api/plans", body)
	recorder := httptest.NewRecorder()
	manager.handlePlans(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHandleDeletePlansRejectsEmptyList(t *testing.T) {
	for _, body := range []string{`{"ids":[]}`, `{"ids":["  "]}`, `{}`} {
		manager, _ := newDeleteTestManager(t)
		request := httptest.NewRequest(http.MethodDelete, "/api/plans", strings.NewReader(body))
		recorder := httptest.NewRecorder()
		manager.handlePlans(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, recorder.Code)
		}
	}
}

// A non-DELETE method on the collection must not delete anything.
func TestHandleDeletePlansRejectsWrongMethod(t *testing.T) {
	manager, dataDir := newDeleteTestManager(t)
	planID, paths := seedPlan(t, manager, dataDir, "keep", "upload-keep")
	request := httptest.NewRequest(http.MethodDelete, "/api/plans/"+planID, nil)
	request.Method = http.MethodGet
	recorder := httptest.NewRecorder()
	manager.handlePlans(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
	if !pathExists(paths["deliverable"]) {
		t.Error("a non-DELETE request deleted data")
	}
}

// directorySize must report a real figure and must not fail on a missing path.
func TestDirectorySize(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.bin"), strings.Repeat("x", 1000))
	writeFile(t, filepath.Join(dir, "nested", "b.bin"), strings.Repeat("y", 500))

	size, err := directorySize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if size != 1500 {
		t.Errorf("size = %d, want 1500", size)
	}

	if _, err := directorySize(filepath.Join(dir, "missing")); err == nil {
		t.Error("expected an error for a missing directory")
	}
}

//----------------------------------------------------------------------------
// Plan status reconciliation after a restart
//----------------------------------------------------------------------------

// writePersistedPlan lays down a plan record plus jobs with the given statuses,
// simulating the on-disk state left behind by an unclean shutdown.
func writePersistedPlan(t *testing.T, dataDir, planID, planStatus string, jobStatuses []string) {
	t.Helper()
	now := time.Now()
	plan := Plan{ID: planID, Name: "persisted", Status: planStatus, CreatedAt: now, UpdatedAt: now}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dataDir, "plans", planID+".json"), string(data))

	for i, status := range jobStatuses {
		jobID := fmt.Sprintf("%s-job-%d", planID, i)
		job := Job{ID: jobID, PlanID: planID, Status: status, Phase: phaseMatching, Logs: []LogEntry{}}
		jobData, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dataDir, "jobs", jobID, "job.json"), string(jobData))
	}
}

// newPersistedManager builds a manager over a prepared data directory, which
// triggers the same load-and-reconcile path the real service runs at startup.
func newPersistedManager(t *testing.T, dataDir string) *JobManager {
	t.Helper()
	config := Config{DataDir: dataDir, PipelineMode: "native", Port: "0"}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewJobManager(config, logger)
}

// The regression test for a plan stuck at "running" after an unclean restart.
// Such a plan could never be deleted: the delete path refuses plans with live
// jobs and the UI disabled its checkbox.
func TestReconcilePlanStatusAfterRestart(t *testing.T) {
	cases := []struct {
		name        string
		jobStatuses []string
		want        string
	}{
		{"all jobs failed", []string{"failed", "failed"}, "failed"},
		{"all jobs completed", []string{"completed", "completed"}, "completed"},
		{"mixed finished", []string{"completed", "failed"}, "failed"},
		{"no jobs at all", nil, "failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Prepare the on-disk state first, then construct the manager so its
			// startup load-and-reconcile path sees it. Constructing first would
			// test nothing, because NewJobManager only reconciles once.
			dataDir := prepareDataDir(t)
			writePersistedPlan(t, dataDir, "stuck-plan", "running", c.jobStatuses)
			manager := newPersistedManager(t, dataDir)

			plan := manager.planSnapshot("stuck-plan")
			if plan == nil {
				t.Fatal("plan was not loaded")
			}
			if plan.Status != c.want {
				t.Fatalf("status = %q, want %q", plan.Status, c.want)
			}
			// A repaired plan must now be deletable, which is the whole point.
			if _, err := manager.deletePlan("stuck-plan"); err != nil {
				t.Fatalf("reconciled plan is still not deletable: %v", err)
			}
		})
	}
}

// A plan already in a terminal or waiting state must not be rewritten.
func TestReconcileLeavesOtherPlanStatesUntouched(t *testing.T) {
	for _, status := range []string{"draft", "failed", "completed", "scheduled"} {
		t.Run(status, func(t *testing.T) {
			_, dataDir := newDeleteTestManager(t)
			writePersistedPlan(t, dataDir, "terminal", status, []string{"failed"})
			manager := newPersistedManager(t, dataDir)

			plan := manager.planSnapshot("terminal")
			if plan == nil {
				t.Fatal("plan was not loaded")
			}
			if plan.Status != status {
				t.Errorf("status %q was changed to %q", status, plan.Status)
			}
		})
	}
}

// Reconciliation must not prevent a normal plan from being started.
func TestReconcileDoesNotDisturbStartablePlan(t *testing.T) {
	manager, _ := newDeleteTestManager(t)
	plan := manager.createPlan("fresh", "", t.TempDir(), nil)
	if got := manager.planSnapshot(plan.ID); got == nil || got.Status != "draft" {
		t.Fatalf("fresh plan status = %v, want draft", got)
	}
}
