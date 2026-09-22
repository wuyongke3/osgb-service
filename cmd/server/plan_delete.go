package main

// plan_delete.go implements deleting production plans together with every
// artifact they produced.
//
// A plan owns four kinds of on-disk state:
//
//	plans/<plan-id>.json          the plan record itself
//	jobs/<job-id>/                each job's workspace (databases, MVS scenes,
//	                              logs, checkpoints) for jobs whose PlanID matches
//	deliverables/<plan-id>/       the delivered OSGB tree for each run
//	uploads/<upload-id>/          the uploaded imagery, but only when no other
//	                              plan still refers to it
//
// The upload case needs care: the API accepts any existing upload_id, so two
// plans can legitimately share one upload directory. Deleting a shared upload
// would destroy imagery a surviving plan still needs, so an upload directory is
// removed only after confirming no remaining plan references it.
//
// Running jobs are never deleted. Their subprocesses hold open files inside
// jobs/<job-id>/, and removing that directory underneath them would produce
// confusing tool failures rather than a clean cancellation.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// deletePlanOutcome reports what one plan deletion actually removed.
type deletePlanOutcome struct {
	PlanID          string   `json:"plan_id"`
	Name            string   `json:"name"`
	JobsDeleted     []string `json:"jobs_deleted"`
	UploadDeleted   string   `json:"upload_deleted,omitempty"`
	UploadRetained  string   `json:"upload_retained,omitempty"`
	DeliverableGone bool     `json:"deliverables_deleted"`
	FreedBytes      int64    `json:"freed_bytes"`
	Warnings        []string `json:"warnings,omitempty"`
}

// deletePlan removes one plan and everything derived from it.
//
// The plan is removed from memory and disk first so that a failure partway
// through cleanup cannot leave a plan visible in the UI pointing at
// half-deleted artifacts. Cleanup problems are collected as warnings rather
// than aborting, because a stray file is far less harmful than a plan that
// cannot be dismissed.
func (m *JobManager) deletePlan(planID string) (*deletePlanOutcome, error) {
	plan := m.planSnapshot(planID)
	if plan == nil {
		return nil, os.ErrNotExist
	}

	// Collect the plan's jobs before touching anything.
	jobIDs := make([]string, 0, 4)
	for _, job := range m.snapshots() {
		if job.PlanID == planID {
			jobIDs = append(jobIDs, job.ID)
		}
	}

	// Refuse while any of this plan's jobs is still executing.
	for _, id := range jobIDs {
		job := m.snapshot(id)
		if job != nil && (job.Status == "running" || job.Status == "queued") {
			return nil, fmt.Errorf("plan %q still has a %s job (%s); stop it before deleting",
				plan.Name, job.Status, id)
		}
	}

	outcome := &deletePlanOutcome{PlanID: planID, Name: plan.Name, JobsDeleted: jobIDs}

	// Remove from memory and persist that removal first.
	m.mu.Lock()
	delete(m.plans, planID)
	for _, id := range jobIDs {
		delete(m.jobs, id)
	}
	m.mu.Unlock()

	// Plan record.
	if err := os.Remove(filepath.Join(m.config.DataDir, "plans", planID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		outcome.Warnings = append(outcome.Warnings, "remove plan record: "+err.Error())
	}

	// Job workspaces, which hold the bulk of the intermediate data.
	for _, id := range jobIDs {
		jobDir := filepath.Join(m.config.DataDir, "jobs", id)
		size, sizeErr := directorySize(jobDir)
		if sizeErr == nil {
			outcome.FreedBytes += size
		}
		if err := os.RemoveAll(jobDir); err != nil {
			outcome.Warnings = append(outcome.Warnings, "remove job workspace "+id+": "+err.Error())
		}
	}

	// Delivered OSGB trees.
	deliverables := filepath.Join(m.config.DataDir, "deliverables", planID)
	if _, err := os.Stat(deliverables); err == nil {
		if size, sizeErr := directorySize(deliverables); sizeErr == nil {
			outcome.FreedBytes += size
		}
		if err := os.RemoveAll(deliverables); err != nil {
			outcome.Warnings = append(outcome.Warnings, "remove deliverables: "+err.Error())
		} else {
			outcome.DeliverableGone = true
		}
	}

	// Uploaded imagery, but only if this plan used an upload and no surviving
	// plan still points at the same directory.
	if plan.UploadID != "" {
		if m.uploadStillReferenced(plan.UploadID) {
			outcome.UploadRetained = plan.UploadID
		} else {
			uploadDir := filepath.Join(m.config.DataDir, "uploads", plan.UploadID)
			if size, sizeErr := directorySize(uploadDir); sizeErr == nil {
				outcome.FreedBytes += size
			}
			if err := os.RemoveAll(uploadDir); err != nil {
				outcome.Warnings = append(outcome.Warnings, "remove upload: "+err.Error())
			} else {
				outcome.UploadDeleted = plan.UploadID
			}
		}
	}

	m.logger.Info("plan deleted",
		"plan_id", planID, "name", plan.Name,
		"jobs", len(jobIDs), "freed_bytes", outcome.FreedBytes,
		"warnings", len(outcome.Warnings))
	return outcome, nil
}

// uploadStillReferenced reports whether any remaining plan uses uploadID.
//
// It must be called after the deleted plan has been removed from m.plans, so
// that the plan being deleted is not counted as its own referrer.
func (m *JobManager) uploadStillReferenced(uploadID string) bool {
	if uploadID == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, plan := range m.plans {
		if plan.UploadID == uploadID {
			return true
		}
	}
	return false
}

// directorySize sums the size of every regular file under root.
//
// It is best effort: a directory that disappears mid-walk or a file that cannot
// be stat'ed is skipped, because this only feeds a "freed" figure reported to
// the operator and must never fail a deletion. A missing root reports an error
// so the caller can skip adding to the total.
func directorySize(root string) (int64, error) {
	if _, err := os.Stat(root); err != nil {
		return 0, err
	}
	var total int64
	// This walk only produces a "freed" figure for the operator, so an entry it
	// cannot read is skipped rather than failing the whole measurement. Each
	// skip is an explicit continue, which is why a nil error is returned.
	_ = filepath.WalkDir(root, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			//nolint:nilerr // measurement is best effort; skip unreadable entries
			return nil
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			//nolint:nilerr // ditto: a file that vanished mid-walk is not fatal
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, nil
}

// handleDeletePlans handles DELETE /api/plans and DELETE /api/plans/:id.
//
// The single-plan form is DELETE /api/plans/:id. The batch form is
// DELETE /api/plans with a JSON body {"ids": ["...", "..."]}. Batch is a
// separate call rather than a loop of single deletes so that the operator gets
// one summary, and so that a partially failing batch still reports per-plan
// outcomes instead of stopping at the first error.
func (m *JobManager) handleDeletePlans(w http.ResponseWriter, r *http.Request, planID string) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}

	// addressedByPath records whether the caller named one plan in the URL, as
	// opposed to listing ids in a body. Only the former can meaningfully answer
	// 404, because it explicitly asked about that one resource.
	addressedByPath := planID != ""

	ids := []string{}
	if addressedByPath {
		ids = append(ids, planID)
	} else {
		var request struct {
			IDs []string `json:"ids"`
		}
		if err := decodeLimitedJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_JSON", "expected {\"ids\": [...]}")
			return
		}
		for _, id := range request.IDs {
			if trimmed := strings.TrimSpace(id); trimmed != "" {
				ids = append(ids, trimmed)
			}
		}
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "NO_PLAN_IDS", "at least one plan id is required")
		return
	}

	results := make([]*deletePlanOutcome, 0, len(ids))
	failures := make(map[string]string)
	missing := make([]string, 0)
	blocked := false
	for _, id := range ids {
		outcome, err := m.deletePlan(id)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Deleting something already gone is a success for the caller's
				// intent. It is reported separately rather than as a failure:
				// returning an error here made a repeat click, or a batch whose
				// entries another operator had just removed, look broken.
				missing = append(missing, id)
				continue
			}
			// Anything else means the plan exists but must not be removed right
			// now, typically because a job is still running.
			failures[id] = err.Error()
			blocked = true
			continue
		}
		// Only a real deletion contributes an outcome; appending on the error
		// paths would put a nil entry in the response's "deleted" array.
		results = append(results, outcome)
	}

	// Status codes are chosen so that a client can read the body in every case:
	//  200 - at least one plan removed, or every requested id was already absent
	//  207 - some removed, some blocked (the body carries both lists)
	//  409 - nothing removed because a plan is in use
	//  404 - only when a single plan was addressed by path and it does not
	//        exist, which is the one situation where naming a missing resource
	//        is the useful answer. A batch body listing absent ids is not an
	//        error: the caller asked to make them gone and they are gone.
	status := http.StatusOK
	switch {
	case len(results) == 0 && blocked:
		status = http.StatusConflict
	case len(results) == 0 && addressedByPath && len(missing) == 1:
		status = http.StatusNotFound
	case len(failures) > 0:
		status = http.StatusMultiStatus
	}

	writeJSON(w, status, map[string]any{
		"deleted":   results,
		"missing":   missing,
		"failed":    failures,
		"total":     len(ids),
		"succeeded": len(results),
	})
}
