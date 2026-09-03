package services

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/fmind/fkf/core"
)

func auditHealth(
	ctx context.Context, base *Base, status *Status, request StatusRequest,
	trackCollected bool, documents *statusDocuments,
) error {
	if !status.Trust.Trusted {
		status.addFinding("trust", SeverityWarning,
			"this base's configuration is not trusted on this machine, so `fkf sync` will refuse to run its commands",
			baseCommand(base.Root(), "trust"))
	}
	if trackCollected {
		status.addFinding("history", SeverityWarning,
			"this base commits events/ and index/; git history is append-only, so anything collected is permanent",
			"start a new base if that was not intended")
	}
	if !request.SkipGitAudit {
		if err := checkGit(ctx, base, status); err != nil {
			return err
		}
	}
	if err := checkSkills(ctx, base, status); err != nil {
		return err
	}
	if err := checkConflictMarkers(ctx, base, status, documents); err != nil {
		return err
	}
	if err := checkPermissions(ctx, base, status); err != nil {
		return err
	}
	checkDerived(base, status)
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := checkLearnedBacklog(ctx, base, status); err != nil {
		return err
	}
	if err := checkDocuments(ctx, base, status, documents); err != nil {
		return err
	}

	sort.SliceStable(status.Findings, func(i, j int) bool { return status.Findings[i].Check < status.Findings[j].Check })
	for _, finding := range status.Findings {
		if finding.Severity == SeverityError {
			status.Errors++
		} else {
			status.Warnings++
		}
	}
	status.OK = status.Errors == 0
	return nil
}

// --- Health / Audit checks ---

func checkSkills(ctx context.Context, base *Base, status *Status) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	states, err := SkillDrift(base.Root())
	if err != nil {
		return err
	}
	var drifted, missing []string
	for _, state := range states {
		if err := checkContext(ctx); err != nil {
			return err
		}
		switch {
		case !state.Present:
			missing = append(missing, state.URI)
		case !state.Current:
			drifted = append(drifted, state.URI)
		}
	}
	if len(missing) > 0 {
		status.addFinding("skills", SeverityWarning, "fkf-owned skills are missing from this base",
			"fkf init "+shellArg(base.Root()), missing...)
	}
	if len(drifted) > 0 {
		status.addFinding("skills", SeverityWarning,
			"fkf-owned skills differ from this binary's copy; they are rewritten by init, so local edits are lost",
			"fkf init "+shellArg(base.Root()), drifted...)
	}
	return checkHelpers(ctx, base, status)
}

func checkHelpers(ctx context.Context, base *Base, status *Status) error {
	report, err := InspectHelpers(ctx, base, false)
	if err != nil {
		return err
	}
	var missing, drifted []string
	for _, helper := range report.Helpers {
		switch helper.State {
		case HelperMissing:
			missing = append(missing, helper.Path)
		case HelperDrifted:
			drifted = append(drifted, helper.Path)
		}
	}
	if len(missing) > 0 {
		status.addFinding("helpers", SeverityWarning, "official helpers required by this base are missing",
			baseCommand(base.Root(), "config helpers --refresh"), missing...)
	}
	if len(drifted) > 0 {
		status.addFinding("helpers", SeverityWarning, "official helpers differ from this binary's copy",
			baseCommand(base.Root(), "config helpers --refresh"), drifted...)
	}
	return nil
}

var conflictMarkerBytes = [][]byte{
	[]byte("<<<<<<< "),
	[]byte("=======\n"),
	[]byte(">>>>>>> "),
}

func checkConflictMarkers(
	ctx context.Context, base *Base, status *Status, documents *statusDocuments,
) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	conflicted := documents.conflicted()
	if len(conflicted) > 0 {
		status.addFinding("conflict-markers", SeverityError,
			"collected JSON documents contain unresolved git merge conflicts",
			"resolve the conflict or re-collect the day with `"+baseCommand(base.Root(), "sync --force")+"`", conflicted...)
	}
	return nil
}

func containsConflictMarker(data []byte) bool {
	for _, marker := range conflictMarkerBytes {
		if bytes.Contains(data, marker) {
			return true
		}
	}
	return false
}

func checkDerived(base *Base, status *Status) {
	graphURI := core.GraphFile
	if !base.Exists(graphURI) {
		status.addFinding("derived", SeverityWarning,
			"the graph cache is absent, so `graph <uri>` and `context --expand` have nothing to read",
			baseCommand(base.Root(), "build graph"), graphURI)
	}
}

func checkLearnedBacklog(ctx context.Context, base *Base, status *Status) error {
	if !base.Store.Enabled(core.LayerTasks) {
		return nil
	}
	learned, err := ListLearned(ctx, base, Window{}, true)
	if err != nil {
		return err
	}
	if learned.Unharvested > 0 {
		status.addFinding("learned", SeverityWarning,
			fmt.Sprintf("%d \"## Learned\" bullet(s) across your task traces have not been promoted "+
				"into a wiki or projects page yet", learned.Unharvested),
			baseCommand(base.Root(), "list tasks learned --unharvested"))
	}
	return nil
}

func checkDocuments(
	ctx context.Context, base *Base, status *Status, documents *statusDocuments,
) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	verifyReport := documents.verifyReport(base)
	for _, finding := range verifyReport.Findings {
		status.addFinding("documents", SeverityError,
			fmt.Sprintf("%s: %s", finding.URI, finding.Problem),
			"re-collect the day or fix the document JSON", finding.URI)
	}
	return nil
}
