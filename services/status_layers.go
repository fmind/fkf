package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
)

func populateLayerOverviews(
	ctx context.Context, base *Base, status *Status, documents *statusDocuments, now time.Time,
) error {
	for _, layer := range core.Layers {
		if err := checkContext(ctx); err != nil {
			return err
		}
		summary, err := summarizeLayer(ctx, base, layer, documents, now)
		if err != nil {
			return err
		}
		status.Layers = append(status.Layers, summary)
	}

	if _, staleDays := collectionFreshness(base, now); staleDays > 0 {
		status.StaleDays = staleDays
	}
	graph, err := summarizeGraphWithOptions(ctx, base, graphValidationOptions{knownInputs: documents.known})
	switch {
	case err == nil:
		status.Graph = graph
	case errors.Is(err, ErrDerivedMissing):
	default:
		status.Graph = graph
		status.addFinding("derived", SeverityError,
			fmt.Sprintf("the graph cache is invalid: %v", err), baseCommand(base.Root(), "build graph"),
			core.GraphFile, core.GraphDstFile, core.GraphOffsetsFile, core.GraphMetaFile)
	}

	if base.Store.Enabled(core.LayerTasks) {
		learned, err := ListLearned(ctx, base, Window{}, true)
		if err != nil {
			return err
		}
		status.Unharvested = learned.Unharvested
	}
	return nil
}

func summarizeLayer(
	ctx context.Context, base *Base, layer core.Layer, documents *statusDocuments, now time.Time,
) (LayerOverview, error) {
	summary := LayerOverview{Layer: layer, Enabled: base.Store.Enabled(layer), URI: string(layer) + "/"}
	if !summary.Enabled {
		return summary, nil
	}
	switch layer {
	case core.LayerEvents:
		dates, err := base.EventDates()
		if err != nil {
			return summary, err
		}
		summary.Count, summary.Unit = len(dates), "day"
		if len(dates) > 0 {
			summary.Since, summary.Until = dates[0], dates[len(dates)-1]
		}
	case core.LayerIndex:
		listing, err := documents.indexListing(base, now)
		if err != nil {
			return summary, err
		}
		summary.Count, summary.Unit = len(listing.Entries), "document"
		summary.Note = oldestIndexNote(listing)
	case core.LayerTasks:
		listing, err := ListTasks(ctx, base, Window{}, 0)
		if err != nil {
			return summary, err
		}
		summary.Count, summary.Unit = len(listing.Traces), "trace"
		if len(listing.Traces) > 0 {
			summary.Until = listing.Traces[0].Date
		}
	case core.LayerProjects, core.LayerWiki:
		listing, err := ListPages(ctx, base, layer, PageFilter{})
		if err != nil {
			return summary, err
		}
		summary.Count, summary.Unit = listing.Total, "page"
		summary.Note = pageNote(ctx, base, layer, listing)
	}
	return summary, nil
}

func oldestIndexNote(listing *IndexListing) string {
	oldest := 0
	for _, entry := range listing.Entries {
		oldest = max(oldest, entry.AgeHours)
	}
	if oldest == 0 {
		return ""
	}
	return fmt.Sprintf("oldest refreshed %dh ago", oldest)
}

func pageNote(ctx context.Context, base *Base, layer core.Layer, listing *PageListing) string {
	if layer == core.LayerProjects {
		byStatus := map[string]int{}
		for _, page := range listing.Pages {
			if page.Status != "" {
				byStatus[page.Status]++
			}
		}
		return joinCounts(byStatus)
	}
	if listing.Total == 0 {
		return ""
	}
	vocabulary, err := BuildTagVocabulary(ctx, base, layer)
	if err != nil {
		return ""
	}
	note := fmt.Sprintf("%d tags", len(vocabulary.Tags))
	if untagged := len(vocabulary.Untagged); untagged > 0 {
		note += fmt.Sprintf(", %d untagged", untagged)
	}
	return note
}

func joinCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[key], key))
	}
	return strings.Join(parts, ", ")
}
