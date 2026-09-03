package services_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

func TestDemoDayMatchesGoldenAndBudget(t *testing.T) {
	base := demoDigestBase(t)
	report, err := services.Day(t.Context(), base, services.DayRequest{
		Date: "yesterday", Budget: 600, DeliveryFormat: services.DigestDeliveryJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) > 600*4 {
		t.Fatalf("delivered JSON = %d bytes, want at most 2400", len(encoded))
	}
	assertGolden(t, "demo-day.json", encoded)

	textReport, err := services.Day(t.Context(), base, services.DayRequest{
		Date: "yesterday", Budget: 600, DeliveryFormat: services.DigestDeliveryText,
	})
	if err != nil {
		t.Fatal(err)
	}
	text := []byte(services.RenderTimelineText(textReport))
	if len(text) > 600*4 {
		t.Fatalf("delivered text = %d bytes, want at most 2400", len(text))
	}
	assertGolden(t, "demo-day.txt", text)
	if textReport.Receipt.Selected <= report.Receipt.Selected {
		t.Fatalf("text selected %d records and JSON selected %d; want the compact delivery to retain more",
			textReport.Receipt.Selected, report.Receipt.Selected)
	}

	repeat, err := services.Day(t.Context(), base, services.DayRequest{
		Date: "2026-05-09", Budget: 600, DeliveryFormat: services.DigestDeliveryJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.Groups, repeat.Groups) ||
		!reflect.DeepEqual(report.People, repeat.People) ||
		!reflect.DeepEqual(report.Repositories, repeat.Repositories) {
		t.Fatalf("relative and absolute day select different evidence:\nrelative=%+v\nabsolute=%+v", report, repeat)
	}
	if report.Receipt.Records != 36 || report.Receipt.Selected+report.Receipt.Dropped != 36 {
		t.Fatalf("receipt = %+v, want an accounted 36-record demo day", report.Receipt)
	}
	foundSummary := false
	for _, group := range report.Groups {
		if group.Source == "shell-commands" {
			foundSummary = group.Summarized && group.Count == 6 && len(group.Items) == 0
		}
	}
	if !foundSummary {
		t.Fatalf("groups = %+v, want shell commands represented by one truthful summary", report.Groups)
	}
	expanded, err := services.Day(t.Context(), base, services.DayRequest{Date: "yesterday", Budget: 5000, All: true})
	if err != nil {
		t.Fatal(err)
	}
	foundExpanded := false
	for _, group := range expanded.Groups {
		if group.Source == "shell-commands" {
			foundExpanded = !group.Summarized && len(group.Items) == 6
		}
	}
	if !foundExpanded {
		t.Fatalf("--all groups = %+v, want all six shell command lines", expanded.Groups)
	}
}

func TestTimelineFiltersAndAroundOneRecord(t *testing.T) {
	base := demoDigestBase(t)
	all, err := services.Timeline(t.Context(), base, services.TimelineRequest{
		Window: services.Window{Since: "2026-05-08", Until: "2026-05-09"}, Budget: 2000, All: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.People) == 0 || len(all.Repositories) == 0 {
		t.Fatalf("timeline entities = people %v repositories %v, want both", all.People, all.Repositories)
	}

	filtered, err := services.Timeline(t.Context(), base, services.TimelineRequest{
		Window:  services.Window{Since: "2026-05-08", Until: "2026-05-09"},
		Sources: []string{"git-commits"}, Repository: all.Repositories[0], Budget: 1000, All: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range filtered.Groups {
		if group.Source != "git-commits" {
			t.Fatalf("filtered source = %q, want git-commits", group.Source)
		}
	}
	if filtered.Receipt.Repository != all.Repositories[0] {
		t.Fatalf("filter receipt = %+v", filtered.Receipt)
	}

	var aroundURI string
	for _, group := range all.Groups {
		if len(group.Items) > 0 {
			aroundURI = group.Items[0].URI
			break
		}
	}
	if aroundURI == "" {
		t.Fatal("demo timeline returned no URI for --around")
	}
	around, err := services.Timeline(t.Context(), base, services.TimelineRequest{
		AroundURI: aroundURI, Around: 2 * time.Hour, Budget: 2000, All: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if around.Receipt.Around != aroundURI || around.Receipt.AroundWindow != "2h0m0s" || around.Receipt.Records == 0 {
		t.Fatalf("around receipt = %+v", around.Receipt)
	}
}

func TestTimelineAroundUsesTheBaseLocalDocumentDay(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	location := time.FixedZone("CEST", 2*60*60)
	base.Now = func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, location) }
	source, err := base.Source("synthetic")
	if err != nil {
		t.Fatal(err)
	}
	document, err := sources.Collect(t.Context(), &fakeRunner{responses: map[string]string{
		"": `[{"id":"center","t":"2026-05-04T23:30:00Z","subject":"Cross-zone event"}]`,
	}}, source, base.Env, sources.Window{
		Date: "2026-05-05", Next: "2026-05-06",
		Start: "2026-05-04T22:00:00Z", End: "2026-05-05T22:00:00Z",
	}, time.Minute, base.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := base.WriteDocument(document); err != nil {
		t.Fatal(err)
	}
	uri := "events/2026-05-05/synthetic.json#center"
	report, err := services.Timeline(t.Context(), base, services.TimelineRequest{
		AroundURI: uri, Around: 20 * time.Minute, Budget: 1000, All: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Receipt.Records != 1 || report.Receipt.Around != uri {
		t.Fatalf("cross-zone around report = %+v, want its center record", report)
	}
}

func TestDigestBudgetFailsWithTheExactFloor(t *testing.T) {
	base := demoDigestBase(t)
	_, err := services.Day(t.Context(), base, services.DayRequest{Date: "yesterday", Budget: 1})
	var budgetErr *services.DigestBudgetError
	if !errors.As(err, &budgetErr) || budgetErr.Minimum <= 1 {
		t.Fatalf("Day() error = %v, want DigestBudgetError naming a larger minimum", err)
	}
	report, err := services.Day(t.Context(), base, services.DayRequest{
		Date: "yesterday", Budget: budgetErr.Minimum,
	})
	if err != nil {
		t.Fatalf("minimum budget %d failed: %v", budgetErr.Minimum, err)
	}
	if report.Receipt.UsedTokens > budgetErr.Minimum {
		t.Fatalf("receipt = %+v, want used_tokens within the admitted floor", report.Receipt)
	}
}

func TestDayOrdersItemsChronologicallyAndCollapsesIdenticalTitles(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	collect(t, base, "2026-05-04", `[
  {"id":"repeat-2","t":"2026-05-04T11:00:00Z","subject":"Repeated work"},
  {"id":"later","t":"2026-05-04T10:00:00Z","subject":"Later work"},
  {"id":"repeat-1","t":"2026-05-04T09:00:00Z","subject":"Repeated work"}
]`)
	report, err := services.Day(t.Context(), base, services.DayRequest{Date: "2026-05-04", Budget: 1000, All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Groups) != 1 || len(report.Groups[0].Items) != 2 {
		t.Fatalf("groups = %+v, want two collapsed lines", report.Groups)
	}
	items := report.Groups[0].Items
	if items[0].Title != "Repeated work" || items[0].Count != 2 ||
		items[1].Title != "Later work" || items[0].Time >= items[1].Time {
		t.Fatalf("items = %+v, want chronological lines and a repeated-title count", items)
	}
}

func TestDayRejectsUnknownSourcesWithoutExecuting(t *testing.T) {
	base := demoDigestBase(t)
	base.Runner = &fakeRunner{err: errors.New("stored read executed a command")}
	_, err := services.Timeline(t.Context(), base, services.TimelineRequest{
		Window: services.Window{Since: "yesterday", Until: "yesterday"}, Sources: []string{"absent"},
	})
	if err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("Timeline() error = %v, want unknown source named", err)
	}
	runner, ok := base.Runner.(*fakeRunner)
	if !ok {
		t.Fatalf("runner = %T, want the offline fake", base.Runner)
	}
	if len(runner.calls) != 0 {
		t.Fatal("timeline executed a provider command")
	}
}

func TestDayRejectsAnUnknownDeliveryFormat(t *testing.T) {
	base := demoDigestBase(t)
	_, err := services.Day(t.Context(), base, services.DayRequest{
		Date: "yesterday", DeliveryFormat: "yaml",
	})
	if err == nil || !strings.Contains(err.Error(), "digest delivery format") {
		t.Fatalf("Day() error = %v, want the closed delivery vocabulary", err)
	}
}

func TestDayResolvesItsWindowAndReceiptFromOneClockRead(t *testing.T) {
	base := newBase(t, baseConfig, nil)
	calls := 0
	base.Now = func() time.Time {
		calls++
		return time.Date(2026, 5, 10+calls-1, 23, 59, 59, 0, time.UTC)
	}
	report, err := services.Day(t.Context(), base, services.DayRequest{Budget: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || report.Receipt.Window.Since != "2026-05-10" ||
		report.Receipt.AsOf != "2026-05-10" {
		t.Fatalf("clock calls=%d receipt=%+v; want one instant for date and as_of", calls, report.Receipt)
	}
}

func demoDigestBase(t *testing.T) *services.Base {
	t.Helper()
	isolate(t)
	root := filepath.Join(t.TempDir(), "brain")
	if _, err := services.Init(t.Context(), services.InitRequest{Path: root, Demo: 2, SkipGit: true}, clock); err != nil {
		t.Fatal(err)
	}
	return openBase(t, root, &fakeRunner{err: errors.New("stored read executed a command")})
}

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", "day", name))
	if err != nil {
		t.Fatalf("%v\ngolden content:\n%s", err, got)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s differs from golden:\n%s", name, got)
	}
}
