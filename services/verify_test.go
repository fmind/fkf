package services_test

import (
	"strings"
	"testing"

	"github.com/fmind/fkf/services"
)

func TestVerifyReportsACleanBaseOK(t *testing.T) {
	base := documentDayBase(t) // two valid documents for 2026-05-04: alpha (2 records), beta (1)
	report, err := services.Verify(t.Context(), base)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !report.OK || report.Documents != 2 || report.Records != 3 || len(report.Findings) != 0 {
		t.Fatalf("report = %+v, want a clean base with nothing to find", report)
	}
}

func documentDayBase(t *testing.T) *services.Base {
	t.Helper()
	base := newBase(t, baseConfig, nil)
	write(t, base, "events/2026-05-04/beta.json", `{
  "fkf": 1, "source": "beta", "layer": "events", "date": "2026-05-04",
  "collected_at": "2026-05-04T09:00:00Z",
  "schema": {"id": {"description": "Stable record identity.", "cardinality": "one"}, "time": {"description": "Event time.", "cardinality": "one"}},
  "fields": {"id": ".id", "time": ".t"}, "body": false,
  "count": 1, "records": [{"id": "b1", "t": "2026-05-04T09:00:00Z"}]
}`)
	write(t, base, "events/2026-05-04/alpha.json", `{
  "fkf": 1, "source": "alpha", "layer": "events", "date": "2026-05-04",
  "collected_at": "2026-05-04T09:00:00Z",
  "schema": {"id": {"description": "Stable record identity.", "cardinality": "one"}, "time": {"description": "Event time.", "cardinality": "one"}},
  "fields": {"id": ".id", "time": ".t"}, "body": false,
  "count": 2, "records": [
    {"id": "a1", "t": "2026-05-04T09:00:00Z"},
    {"id": "a2", "t": "2026-05-04T10:00:00Z"}
  ]
}`)
	return base
}

// TestVerifyCatchesADuplicateIdThatPredatesTheCheck is the regression test for the exact gap
// this command exists to close: sources.VerifyRecords refuses a duplicate id at collection
// time, but a document written before that check existed, or hand-edited since, sits on disk
// unexamined until something re-applies the same rule to what is already stored.
func TestVerifyCatchesADuplicateIdThatPredatesTheCheck(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "events/2026-05-04/legacy.json", `{
  "fkf": 1, "source": "legacy", "layer": "events", "date": "2026-05-04",
  "collected_at": "2026-05-04T09:00:00Z", "command": "echo",
  "schema": {"id": {"description": "Stable record identity.", "cardinality": "one"}, "time": {"description": "Event time.", "cardinality": "one"}},
  "fields": {"id": ".id", "time": ".t"}, "body": false,
  "count": 2, "records": [
    {"id": "same", "t": "2026-05-04T09:00:00Z"},
    {"id": "same", "t": "2026-05-04T10:00:00Z"}
  ]
}`)
	report, err := services.Verify(t.Context(), base)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if report.OK || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, want the duplicate id caught", report)
	}
	if !strings.Contains(report.Findings[0].Problem, `share the id "same"`) {
		t.Fatalf("finding = %+v, want it to name the shared id", report.Findings[0])
	}
}

func TestVerifyCatchesACountMismatch(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "events/2026-05-04/miscounted.json", `{
  "fkf": 1, "source": "miscounted", "layer": "events", "date": "2026-05-04",
  "collected_at": "2026-05-04T09:00:00Z", "command": "echo",
  "schema": {"id": {"description": "Stable record identity.", "cardinality": "one"}, "time": {"description": "Event time.", "cardinality": "one"}},
  "fields": {"id": ".id", "time": ".t"}, "body": false,
  "count": 5, "records": [{"id": "a1", "t": "2026-05-04T09:00:00Z"}]
}`)
	report, err := services.Verify(t.Context(), base)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if report.OK || len(report.Findings) != 1 {
		t.Fatalf("report = %+v, want the count mismatch caught", report)
	}
	if !strings.Contains(report.Findings[0].Problem, "declares count 5 but holds 1 record(s)") {
		t.Fatalf("finding = %+v, want the declared and actual counts named", report.Findings[0])
	}
}

// TestVerifyContinuesPastAnUndecodableDocument is the other half of the same design: one
// document from a schema this build no longer recognises must not hide every other document's
// findings, the way eachDocument's rebuild callers are right to abort on
// exactly this — verify's whole point is surfacing every problem in one pass, not stopping at
// the first.
func TestVerifyContinuesPastAnUndecodableDocument(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "events/2026-05-04/stale.json", `{
	"fkf": 99, "source": "stale", "layer": "events", "date": "2026-05-04",
  "collected_at": "2026-05-04T09:00:00Z", "command": "echo",
  "fields": {"id": ".id", "time": ".t"}, "body": false,
  "count": 1, "records": [{"id": "a1", "t": "2026-05-04T09:00:00Z"}]
}`)
	write(t, base, "events/2026-05-04/fine.json", `{
  "fkf": 1, "source": "fine", "layer": "events", "date": "2026-05-04",
  "collected_at": "2026-05-04T09:00:00Z", "command": "echo",
  "schema": {"id": {"description": "Stable record identity.", "cardinality": "one"}, "time": {"description": "Event time.", "cardinality": "one"}},
  "fields": {"id": ".id", "time": ".t"}, "body": false,
  "count": 1, "records": [{"id": "b1", "t": "2026-05-04T09:00:00Z"}]
}`)
	report, err := services.Verify(t.Context(), base)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if report.Documents != 2 {
		t.Fatalf("report.Documents = %d, want both walked despite one failing to decode", report.Documents)
	}
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].URI, "stale.json") {
		t.Fatalf("findings = %+v, want exactly the undecodable document named", report.Findings)
	}
	// fine.json's own record was still counted: the walk did not stop at stale.json.
	if report.Records != 1 {
		t.Fatalf("report.Records = %d, want the decodable document's record still counted", report.Records)
	}
}

func TestVerifyWalksTheIndexLayerToo(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "index/repos.json", `{
  "fkf": 1, "source": "repos", "layer": "index",
  "collected_at": "2026-05-04T09:00:00Z", "command": "echo",
  "schema": {"id": {"description": "Stable record identity.", "cardinality": "one"}},
  "fields": {"id": ".id"}, "body": false,
  "count": 1, "records": [{"id": "fmind/fkf"}]
}`)
	report, err := services.Verify(t.Context(), base)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !report.OK || report.Documents != 1 || report.Records != 1 {
		t.Fatalf("report = %+v, want the one clean index document counted", report)
	}
}

func TestVerifyRejectsDocumentMetadataThatDoesNotMintItsStoredURI(t *testing.T) {
	cases := []struct {
		name, uri, document, want string
	}{
		{
			name: "event source", uri: "events/2026-05-04/filed.json", want: "mints events/2026-05-04/other.json",
			document: `{"fkf":1,"source":"other","layer":"events","date":"2026-05-04","collected_at":"2026-05-04T09:00:00Z","command":"echo","schema":{"id":{"description":"Stable record identity.","cardinality":"one"},"time":{"description":"Event time.","cardinality":"one"}},"fields":{"id":".id","time":".t"},"body":false,"count":0,"records":[]}`,
		},
		{
			name: "event date", uri: "events/2026-05-04/filed.json", want: "mints events/2026-05-03/filed.json",
			document: `{"fkf":1,"source":"filed","layer":"events","date":"2026-05-03","collected_at":"2026-05-04T09:00:00Z","command":"echo","schema":{"id":{"description":"Stable record identity.","cardinality":"one"},"time":{"description":"Event time.","cardinality":"one"}},"fields":{"id":".id","time":".t"},"body":false,"count":0,"records":[]}`,
		},
		{
			name: "index source", uri: "index/filed.json", want: "mints index/other.json",
			document: `{"fkf":1,"source":"other","layer":"index","collected_at":"2026-05-04T09:00:00Z","command":"echo","schema":{"id":{"description":"Stable record identity.","cardinality":"one"}},"fields":{"id":".id"},"body":false,"count":0,"records":[]}`,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			base := newBase(t, baseConfig, nil)
			write(t, base, test.uri, test.document)
			report, err := services.Verify(t.Context(), base)
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			if report.OK || len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Problem, test.want) {
				t.Fatalf("report = %+v, want one URI-metadata mismatch containing %q", report, test.want)
			}
		})
	}
}

func TestVerifyRejectsMissingFieldPathsEvenWhenTheDocumentIsEmpty(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	write(t, base, "events/2026-05-04/empty.json", `{
  "fkf": 1, "source": "empty", "layer": "events", "date": "2026-05-04",
  "collected_at": "2026-05-04T09:00:00Z", "command": "echo",
  "fields": {}, "body": false, "count": 0, "records": []
}`)
	report, err := services.Verify(t.Context(), base)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if report.OK || len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Problem, "fields.id is required") {
		t.Fatalf("report = %+v, want the empty document's missing field map rejected", report)
	}
}
