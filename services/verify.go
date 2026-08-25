package services

import (
	"context"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// `fkf verify` re-checks the rules collection enforces at write time against every document
// already on disk. Those rules run exactly once, the moment a day is filed — a document
// hand-edited afterward or written before a rule existed is never checked again. Nothing else
// in fkf walks every collected document this way: `status` audits the base as a whole, and
// `validate wiki`/`validate projects` re-check the two Markdown layers on demand — verify is
// their events/index equivalent, over collected data instead of authored prose.

// VerifyFinding is one document that fails a rule collection would have refused it for today.
type VerifyFinding struct {
	URI     string `json:"uri"`
	Problem string `json:"problem"`
}

// VerifyReport is what `fkf verify` returns.
type VerifyReport struct {
	Base      string          `json:"base"`
	Documents int             `json:"documents"`
	Records   int             `json:"records"`
	Findings  []VerifyFinding `json:"findings"`
	OK        bool            `json:"ok"`
}

// Verify walks every stored document, events first then index, and re-applies what collection
// checks at write time: a current schema marker and a recognised layer (both enforced by
// decoding the document at all), a count field that still matches its records, unique record
// identities, and — for a dated document — a parseable time inside that document's civil day.
// A document that fails even to decode is reported rather than aborting the walk, so one bad
// document never hides the rest.
func Verify(ctx context.Context, base *Base) (*VerifyReport, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	report := &VerifyReport{Base: base.Root(), Findings: []VerifyFinding{}}
	uris, err := documentURIs(ctx, base)
	if err != nil {
		return nil, err
	}
	for _, uri := range uris {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		report.Documents++
		document, err := base.ReadDocumentContext(ctx, uri)
		if err != nil {
			if ctxErr := checkContext(ctx); ctxErr != nil {
				return nil, ctxErr
			}
			report.Findings = append(report.Findings, VerifyFinding{URI: uri, Problem: err.Error()})
			continue
		}
		report.Records += document.Count
	}
	report.OK = len(report.Findings) == 0
	return report, nil
}

// documentURIs lists every stored document's URI, in the same events-then-index, sorted order
// eachDocument walks — but returns the list up front rather than visiting as it goes, so Verify
// can keep going past a document that will not even decode. eachDocument's rebuild callers
// are correct to abort on that; a rebuild over a base whose data is
// known-good has nothing to gain by continuing, and everything to lose by silently omitting a
// day from the derived files it is about to write.
func documentURIs(ctx context.Context, base *Base) ([]string, error) {
	var uris []string
	if base.Store.Enabled(core.LayerEvents) {
		dates, err := base.EventDates()
		if err != nil {
			return nil, err
		}
		for _, date := range dates {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
			names, err := base.DayDocuments(date)
			if err != nil {
				return nil, err
			}
			for _, name := range names {
				if err := checkContext(ctx); err != nil {
					return nil, err
				}
				uris = append(uris, sources.EventDocumentURI(date, name))
			}
		}
	}
	if base.Store.Enabled(core.LayerIndex) {
		names, err := base.IndexDocuments()
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
			uris = append(uris, sources.IndexDocumentURI(name))
		}
	}
	return uris, nil
}
