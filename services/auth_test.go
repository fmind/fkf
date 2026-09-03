package services_test

import (
	"slices"
	"testing"

	"github.com/fmind/fkf/services"
)

const sharedAuthConfig = `name: shared-auth
layers: {events: true}
sources:
  calendar:
    enabled: true
    layer: events
    auth: [gws, auth, status]
    run: [calendar-cli]
    fields: {id: .id, time: .time, title: .id}
  mail:
    enabled: true
    layer: events
    auth: [gws, auth, status]
    run: [mail-cli]
    fields: {id: .id, time: .time, title: .id}
  github:
    enabled: true
    layer: events
    auth: [gh, auth, status]
    run: [github-cli]
    fields: {id: .id, time: .time, title: .id}
`

func TestProbeSourceAuthCanStayOfflineAndDeduplicatesIdenticalArgv(t *testing.T) {
	runner := &authSyncRunner{}
	base := newBase(t, sharedAuthConfig, nil)
	base.Runner = runner
	trust(t, base)
	candidates := base.Config.EnabledSources()

	required, err := services.ProbeSourceAuth(t.Context(), base, candidates, false)
	if err != nil || len(required) != 0 || len(runner.calls) != 0 {
		t.Fatalf("offline auth probe = %v, %v with %d calls; want no live execution", required, err, len(runner.calls))
	}
	required, err = services.ProbeSourceAuth(t.Context(), base, candidates, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(required, []string{"calendar", "mail"}) {
		t.Fatalf("auth required = %v, want both sources sharing the failed gws login", required)
	}
	if runner.countExecutable("gws") != 1 || runner.countExecutable("gh") != 1 {
		t.Fatalf("probe calls: gws=%d gh=%d, want one per distinct auth argv",
			runner.countExecutable("gws"), runner.countExecutable("gh"))
	}
}
