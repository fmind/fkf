package services

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

func populateSourceStatuses(
	ctx context.Context, base *Base, status *Status, request StatusRequest, documents *statusDocuments,
) error {
	history, err := documents.volumeHistory(ctx)
	if err != nil {
		return err
	}
	undeclared := map[string][]dayVolume{}
	for name, days := range history {
		if _, declared := base.Config.Sources[name]; !declared {
			undeclared[name] = days
		}
	}
	missing := map[string]struct{}{}
	if err := populateDeclaredSourceStatuses(
		ctx, base, status, history, undeclared, missing, request, documents,
	); err != nil {
		return err
	}
	if err := populateUndeclaredSourceStatuses(ctx, base, status, undeclared, documents, request.evaluationTime); err != nil {
		return err
	}
	status.Missing = len(missing)
	return nil
}

func populateDeclaredSourceStatuses(
	ctx context.Context,
	base *Base,
	status *Status,
	history, undeclared map[string][]dayVolume,
	missing map[string]struct{},
	request StatusRequest,
	documents *statusDocuments,
) error {
	for _, name := range base.Config.SourceNames() {
		if err := checkContext(ctx); err != nil {
			return err
		}
		source := base.Config.Sources[name]
		delete(undeclared, name)
		entry := sourceStatusOf(base, source, history[name], documents)
		if source.Enabled && source.Layer == core.LayerTasks {
			boundary, err := taskTraceFreshnessBoundary(base, source.Name, request.evaluationTime)
			if err != nil {
				return fmt.Errorf("inspect tasks freshness for %s: %w", source.Name, err)
			}
			entry.lastCollected = boundary
		}
		if err := checkContext(ctx); err != nil {
			return err
		}
		if source.Enabled {
			status.Enabled++
			if entry.Test != nil && !entry.Test.OnPath {
				status.MissingTests++
			}
			for _, requirement := range entry.Requires {
				if !requirement.OnPath {
					missing[requirement.Name] = struct{}{}
				}
			}
		}
		if entry.Quiet {
			status.Quiet++
		}
		observeSourceFreshness(status, &entry, request.evaluationTime, request.MaxAgeHours)
		status.Sources = append(status.Sources, entry)
	}
	return nil
}

func taskTraceFreshnessBoundary(base *Base, source string, now time.Time) (time.Time, error) {
	completed, err := previousCompletedDays(now, 1)
	if err != nil {
		return time.Time{}, err
	}
	date := completed[0].Format(time.DateOnly)
	due, err := taskTraceRangeDueAt(base, source, []string{date}, now.Location())
	if err != nil || due {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, sources.DayWindow(completed[0]).End)
}

func populateUndeclaredSourceStatuses(
	ctx context.Context,
	base *Base,
	status *Status,
	undeclared map[string][]dayVolume,
	documents *statusDocuments,
	now time.Time,
) error {
	for _, name := range sortedNames(undeclared) {
		if err := checkContext(ctx); err != nil {
			return err
		}
		entry := SourceStatus{Name: name, Kind: core.LayerEvents, Undeclared: true}
		applyVolume(&entry, undeclared[name])
		observeSourceFreshness(status, &entry, now, 0)
		status.Sources = append(status.Sources, entry)
	}
	if base.Store.Enabled(core.LayerIndex) {
		for _, name := range documents.indexNames() {
			if err := checkContext(ctx); err != nil {
				return err
			}
			if _, declared := base.Config.Sources[name]; declared {
				continue
			}
			entry := SourceStatus{Name: name, Kind: core.LayerIndex, Undeclared: true}
			documents.applyIndex(&entry)
			observeSourceFreshness(status, &entry, now, 0)
			status.Sources = append(status.Sources, entry)
		}
	}
	return nil
}

func observeSourceFreshness(
	status *Status,
	entry *SourceStatus,
	now time.Time,
	maxAgeHours int,
) {
	if entry.LastDate > status.LastSync {
		status.LastSync = entry.LastDate
	}
	if !entry.lastCollected.IsZero() {
		entry.LastCollectedAt = entry.lastCollected.UTC().Format(time.RFC3339)
		if age := now.Sub(entry.lastCollected); age > 0 {
			entry.LagHours = int(age / time.Hour)
		}
	}
	age := now.Sub(entry.lastCollected)
	entry.Stale = entry.Enabled && maxAgeHours > 0 &&
		(entry.lastCollected.IsZero() || age < 0 || age > time.Duration(maxAgeHours)*time.Hour)
	status.Stale = status.Stale || entry.Stale
}

func sourceStatusOf(
	base *Base, source *core.Source, days []dayVolume,
	documents *statusDocuments,
) SourceStatus {
	entry := SourceStatus{
		Name: source.Name, Enabled: source.Enabled, Kind: source.Layer,
		Install: source.Install, Body: source.HasBody(), Auth: len(source.Auth) > 0,
		Requires: []RequirementStatus{},
	}
	if len(source.Test) > 0 {
		_, found := base.Env.LookTestPath(source.Test[0])
		entry.Test = &RequirementStatus{Name: source.Test[0], OnPath: found}
	}
	for _, name := range source.Requires {
		_, found := base.Env.LookPath(name)
		entry.Requires = append(entry.Requires, RequirementStatus{Name: name, OnPath: found})
	}
	if source.Layer == core.LayerIndex {
		documents.applyIndex(&entry)
	} else {
		applyVolume(&entry, days)
	}
	return entry
}

func markAuthRequired(status *Status) {
	required := make(map[string]struct{}, len(status.AuthRequired))
	for _, name := range status.AuthRequired {
		required[name] = struct{}{}
	}
	for index := range status.Sources {
		_, status.Sources[index].AuthRequired = required[status.Sources[index].Name]
	}
}

func sortedNames(values map[string][]dayVolume) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type dayVolume struct {
	date          string
	count         int
	lastCollected time.Time
}

func volumeHistory(ctx context.Context, base *Base) (map[string][]dayVolume, error) {
	documents, err := loadStatusDocuments(ctx, base, base.ReadFileContext)
	if err != nil {
		return nil, err
	}
	return documents.volumeHistory(ctx)
}

func eventCollectionBoundary(document *sources.Document) (time.Time, error) {
	if document.WindowEnd != "" {
		return time.Parse(time.RFC3339, document.WindowEnd)
	}
	day, err := time.ParseInLocation(time.DateOnly, document.Date, time.Local)
	if err != nil {
		return time.Time{}, err
	}
	return day.AddDate(0, 0, 1), nil
}

func applyVolume(entry *SourceStatus, days []dayVolume) {
	if len(days) == 0 {
		return
	}
	latest := days[len(days)-1]
	entry.LastDate, entry.LastCount, entry.Days = latest.date, latest.count, len(days)
	entry.lastCollected = latest.lastCollected
	if len(days) < quietArmingDays {
		return
	}
	baseline := make([]int, 0, quietWindow)
	latestDay, err := time.Parse(time.DateOnly, latest.date)
	weekend := err == nil && (latestDay.Weekday() == time.Saturday || latestDay.Weekday() == time.Sunday)
	for index := len(days) - 2; index >= 0 && len(baseline) < quietWindow; index-- {
		if weekend {
			day, parseErr := time.Parse(time.DateOnly, days[index].date)
			if parseErr != nil || day.Weekday() != latestDay.Weekday() {
				continue
			}
			baseline = append(baseline, days[index].count)
			continue
		}
		if days[index].count > 0 {
			baseline = append(baseline, days[index].count)
		}
	}
	if len(baseline) < quietArmingDays {
		return
	}
	sort.Ints(baseline)
	entry.Median = baseline[len(baseline)/2]
	if entry.Median == 0 {
		return
	}
	switch {
	case latest.count == 0:
		entry.Quiet = true
		entry.QuietReason = fmt.Sprintf("%s returned nothing while its recent median is %d", latest.date, entry.Median)
	case latest.count*100 < entry.Median*quietRatioPercent:
		entry.Quiet = true
		entry.QuietReason = fmt.Sprintf("%s returned %d, under %d%% of its recent median %d",
			latest.date, latest.count, quietRatioPercent, entry.Median)
	}
}
