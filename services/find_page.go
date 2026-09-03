package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"sort"

	"github.com/fmind/fkf/core"
)

// FindPosition is the last primary item returned by a bounded find page. It is deliberately
// semantic rather than an offset: an MCP continuation can resume without retaining every prior
// match in memory, while the independent snapshot digest still refuses a changed result.
type FindPosition struct {
	Phase string `json:"phase"`
	Score int    `json:"score,omitempty"`
	Time  string `json:"time,omitempty"`
	URI   string `json:"uri,omitempty"`
	Date  string `json:"date,omitempty"`
}

const (
	FindPhasePage   = "page"
	FindPhaseRecord = "record"
	FindPhaseVolume = "volume"

	// MaxFindPageLimit keeps the bounded API bounded even when called outside MCP.
	MaxFindPageLimit = 100
)

// BoundedFindResult is one reconnectable page of an exhaustive find scan. Result contains at
// most limit primary items; SnapshotSHA256 covers the complete semantic result, not just this
// page; and Next is nil only when the result is exhausted.
type BoundedFindResult struct {
	Result         *FindResult
	SnapshotSHA256 string
	Next           *FindPosition
}

// FindBounded scans the complete admitted evidence while retaining only limit+1 candidates per
// result phase. A stored document is therefore the largest evidence allocation, regardless of
// the number of matches in the base. The exhaustive scan keeps Scanned, Matched, and the cursor
// snapshot honest; keyset positions keep continuation memory independent of the page number.
func FindBounded(
	ctx context.Context,
	base *Base,
	filter FindFilter,
	counting bool,
	limit int,
	after FindPosition,
) (*BoundedFindResult, error) {
	if limit <= 0 || limit > MaxFindPageLimit {
		return nil, fmt.Errorf("bounded find limit must be between 1 and %d", MaxFindPageLimit)
	}
	if err := validateFindPosition(after, counting); err != nil {
		return nil, err
	}
	selected, err := prepareFindScan(ctx, base, &filter)
	if err != nil {
		return nil, err
	}

	scan := &boundedFindScan{
		counting: counting, after: after, capacity: limit + 1,
		result: &FindResult{Window: filter.Window, Index: filter.index}, digest: sha256.New(),
	}
	engine := &findScanEngine{
		ctx: ctx, base: base, filter: filter, counting: counting, result: scan.result,
		onPage: scan.addPage, onRecord: scan.addRecord, onVolume: scan.addVolume,
	}
	_, _ = scan.digest.Write([]byte("fkf-bounded-find-v1\x00"))
	if err := scan.hashValue("window", filter.Window); err != nil {
		return nil, err
	}
	scanErr := engine.scan(selected)
	if filter.afterScan != nil {
		filter.afterScan()
	}
	current, generationErr := findIndexInputsCurrent(ctx, base, filter)
	if generationErr != nil {
		return nil, generationErr
	}
	if !current {
		if filter.generationRetries >= 2 {
			return nil, fmt.Errorf("find inputs kept changing while they were read; retry after the writer finishes")
		}
		return FindBounded(
			ctx, base, findScanRetry(filter, LexicalIndexFallbackStale), counting, limit, after,
		)
	}
	if scanErr != nil {
		return nil, scanErr
	}
	if err := scan.hashValue("counters", struct {
		Scanned int `json:"scanned"`
		Matched int `json:"matched"`
	}{Scanned: scan.result.Scanned, Matched: scan.result.Matched}); err != nil {
		return nil, err
	}

	next := scan.compose(limit)
	if !counting && scan.result.Records == nil {
		scan.result.Records = []FindRecord{}
	}
	if counting && scan.result.Volumes == nil {
		scan.result.Volumes = []DayVolume{}
	}
	scan.result.Truncated = next != nil
	return &BoundedFindResult{
		Result: scan.result, SnapshotSHA256: hex.EncodeToString(scan.digest.Sum(nil)), Next: next,
	}, nil
}

func validateFindPosition(position FindPosition, counting bool) error {
	if position == (FindPosition{}) {
		return nil
	}
	invalid := func() error {
		return fmt.Errorf("%w: invalid find continuation position", core.ErrConfig)
	}
	if counting {
		if position.Phase != FindPhaseVolume || position.Date == "" || position.Score != 0 ||
			position.Time != "" || position.URI != "" {
			return invalid()
		}
		if err := core.ValidateDate(position.Date); err != nil {
			return invalid()
		}
		return nil
	}
	switch position.Phase {
	case FindPhasePage:
		if position.Score <= 0 || position.URI == "" || position.Time != "" || position.Date != "" {
			return invalid()
		}
	case FindPhaseRecord:
		if position.Score != 0 || position.URI == "" || position.Date != "" {
			return invalid()
		}
	default:
		return invalid()
	}
	return nil
}

type boundedFindScan struct {
	counting bool
	after    FindPosition
	capacity int
	result   *FindResult
	digest   hash.Hash

	pages   []SearchHit
	records []FindRecord
	volumes []DayVolume
}

func (scan *boundedFindScan) hashValue(kind string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("hash bounded find %s: %w", kind, err)
	}
	writeDigestValue(scan.digest, []byte(kind))
	writeDigestValue(scan.digest, encoded)
	return nil
}

func (scan *boundedFindScan) addPage(hit SearchHit) error {
	if err := scan.hashValue(FindPhasePage, hit); err != nil {
		return err
	}
	if scan.after.Phase == FindPhaseRecord ||
		(scan.after.Phase == FindPhasePage && !searchHitAfter(hit, scan.after)) {
		return nil
	}
	scan.pages = retainBounded(scan.pages, hit, scan.capacity, searchHitBefore)
	return nil
}

func (scan *boundedFindScan) addRecord(record FindRecord) error {
	if err := scan.hashValue(FindPhaseRecord, record); err != nil {
		return err
	}
	if scan.after.Phase == FindPhaseRecord && !findRecordAfter(record, scan.after) {
		return nil
	}
	scan.records = retainBounded(scan.records, record, scan.capacity, findRecordBefore)
	return nil
}

func (scan *boundedFindScan) addVolume(volume DayVolume) error {
	if err := scan.hashValue(FindPhaseVolume, volume); err != nil {
		return err
	}
	if scan.after.Phase == FindPhaseVolume && !dayVolumeAfter(volume, scan.after) {
		return nil
	}
	scan.volumes = retainBounded(scan.volumes, volume, scan.capacity, dayVolumeBefore)
	return nil
}

func retainBounded[T any](values []T, value T, capacity int, before func(T, T) bool) []T {
	values = append(values, value)
	sort.SliceStable(values, func(i, j int) bool { return before(values[i], values[j]) })
	if len(values) > capacity {
		values = values[:capacity]
	}
	return values
}

func searchHitBefore(left, right SearchHit) bool {
	if left.Score != right.Score {
		return left.Score > right.Score
	}
	return left.URI < right.URI
}

func searchHitAfter(hit SearchHit, after FindPosition) bool {
	return hit.Score < after.Score || (hit.Score == after.Score && hit.URI > after.URI)
}

func findRecordBefore(left, right FindRecord) bool {
	if left.Time != right.Time {
		return left.Time > right.Time
	}
	return left.URI < right.URI
}

func findRecordAfter(record FindRecord, after FindPosition) bool {
	return record.Time < after.Time || (record.Time == after.Time && record.URI > after.URI)
}

func dayVolumeBefore(left, right DayVolume) bool {
	// The undated index volume is the final row after every ISO event date.
	if left.Date == "" {
		return false
	}
	if right.Date == "" {
		return true
	}
	return left.Date > right.Date
}

func dayVolumeAfter(volume DayVolume, after FindPosition) bool {
	return volume.Date == "" || volume.Date < after.Date
}

func (scan *boundedFindScan) compose(limit int) *FindPosition {
	if scan.counting {
		take := min(limit, len(scan.volumes))
		scan.result.Volumes = append([]DayVolume(nil), scan.volumes[:take]...)
		if len(scan.volumes) <= take {
			return nil
		}
		last := scan.result.Volumes[len(scan.result.Volumes)-1]
		return &FindPosition{Phase: FindPhaseVolume, Date: last.Date}
	}

	remaining := limit
	pageTake := min(remaining, len(scan.pages))
	scan.result.Pages = append([]SearchHit(nil), scan.pages[:pageTake]...)
	remaining -= pageTake
	recordTake := min(remaining, len(scan.records))
	scan.result.Records = append([]FindRecord(nil), scan.records[:recordTake]...)
	more := len(scan.pages) > pageTake || len(scan.records) > recordTake
	if !more {
		return nil
	}
	if recordTake > 0 {
		last := scan.result.Records[len(scan.result.Records)-1]
		return &FindPosition{Phase: FindPhaseRecord, Time: last.Time, URI: last.URI}
	}
	last := scan.result.Pages[len(scan.result.Pages)-1]
	return &FindPosition{Phase: FindPhasePage, Score: last.Score, URI: last.URI}
}
