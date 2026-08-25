package sources_test

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// noSleep makes a retry test instant: the schedule is asserted through call counts and
// recorded waits, never by actually waiting.
func noSleep(waits *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
}

// TestPolicyRunnerRetriesOnlyTheDeclaredFailure is the reason `on:` exists: a source's policy
// must retry the failure it names and nothing else, or "retry" quietly becomes "retry
// everything" and turns a real, permanent failure into a slow one.
func TestPolicyRunnerRetriesOnlyTheDeclaredFailure(t *testing.T) {
	calls := 0
	runner := sources.RunnerFunc(func(context.Context, sources.Command) (string, error) {
		calls++
		if calls < 3 {
			return "", errors.New("boom: rate limit exceeded")
		}
		return "[]", nil
	})
	source := &core.Source{
		Name: "s",
		Retry: core.RetryPolicy{
			Attempts: 3, Backoff: 10 * time.Millisecond, On: []string{"rate limit exceeded"},
		},
	}
	policy := sources.NewPolicyRunner(runner, source)
	var waits []time.Duration
	policy.Sleep = noSleep(&waits)

	stdout, err := policy.Run(t.Context(), sources.Command{})
	if err != nil {
		t.Fatalf("Run() error = %v, want the third attempt to succeed", err)
	}
	if stdout != "[]" {
		t.Fatalf("stdout = %q, want the successful attempt's output", stdout)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want exactly 3", calls)
	}
	if policy.Attempts() != 3 {
		t.Fatalf("Attempts() = %d, want 3", policy.Attempts())
	}
	// Backoff grows linearly with the attempt number, not exponentially: a provider's
	// documented reset window is a wall-clock fact, and doubling would silently turn three
	// attempts into far longer than the source declared.
	if len(waits) != 2 || waits[0] != 10*time.Millisecond || waits[1] != 20*time.Millisecond {
		t.Fatalf("waits = %v, want [10ms 20ms]", waits)
	}
}

// TestPolicyRunnerMatchesPrivateProviderStderr proves privacy did not weaken an explicitly
// declared retry. The error text is safe and therefore does not contain the provider phrase;
// PolicyRunner must ask the opaque command failure to match it without ever extracting or
// returning the captured stderr.
func TestPolicyRunnerMatchesPrivateProviderStderr(t *testing.T) {
	const privateStderr = "synthetic-private-rate-limit-marker"
	calls := 0
	runner := sources.RunnerFunc(func(context.Context, sources.Command) (string, error) {
		calls++
		if calls == 1 {
			return "", core.NewCommandFailure(errors.New("synthetic provider failure"), privateStderr)
		}
		return "[]", nil
	})
	source := &core.Source{
		Name: "s", Retry: core.RetryPolicy{Attempts: 2, On: []string{privateStderr}},
	}
	policy := sources.NewPolicyRunner(runner, source)
	policy.Sleep = noSleep(&[]time.Duration{})

	stdout, err := policy.Run(t.Context(), sources.Command{})
	if err != nil || stdout != "[]" {
		t.Fatalf("Run() = %q, %v; want the private stderr condition retried to success", stdout, err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want the matching private stderr to permit one retry", calls)
	}
}

// TestPolicyRunnerNeverRetriesAnUndeclaredFailure is the other half: a failure the source did
// not name fails on the first attempt, exactly as if no policy had been declared.
func TestPolicyRunnerNeverRetriesAnUndeclaredFailure(t *testing.T) {
	calls := 0
	runner := sources.RunnerFunc(func(context.Context, sources.Command) (string, error) {
		calls++
		return "", errors.New("permission denied")
	})
	source := &core.Source{Name: "s", Retry: core.RetryPolicy{Attempts: 3, On: []string{"rate limit exceeded"}}}
	policy := sources.NewPolicyRunner(runner, source)
	policy.Sleep = noSleep(&[]time.Duration{})

	if _, err := policy.Run(t.Context(), sources.Command{}); err == nil {
		t.Fatal("Run() succeeded, want the undeclared failure to propagate")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want exactly 1 (no retry for an undeclared failure)", calls)
	}
}

// TestPolicyRunnerMatchesExitCode covers the other half of `on:` — a code, not a substring —
// through the opaque command failure ExecRunner returns. The underlying process status must
// remain matchable even though neither its error nor stderr is exposed.
func TestPolicyRunnerMatchesExitCode(t *testing.T) {
	calls := 0
	runner := sources.RunnerFunc(func(context.Context, sources.Command) (string, error) {
		calls++
		if calls == 1 {
			cmd := exec.Command("sh", "-c", "exit 7")
			runErr := cmd.Run()
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) {
				t.Fatalf("could not construct a real ExitError: %v", runErr)
			}
			return "", core.NewCommandFailure(exitErr, "synthetic private exit detail")
		}
		return "[]", nil
	})
	source := &core.Source{Name: "s", Retry: core.RetryPolicy{Attempts: 2, On: []string{"exit:7"}}}
	policy := sources.NewPolicyRunner(runner, source)
	policy.Sleep = noSleep(&[]time.Duration{})

	if _, err := policy.Run(t.Context(), sources.Command{}); err != nil {
		t.Fatalf("Run() error = %v, want exit:7 retried to success", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

// TestPolicyRunnerNeverRetriesCancellation is what keeps an operator's Ctrl-C fast. A
// cancelled or timed-out run is not a transient provider failure, and retrying it would make
// cancellation slower than doing nothing.
func TestPolicyRunnerNeverRetriesCancellation(t *testing.T) {
	calls := 0
	runner := sources.RunnerFunc(func(context.Context, sources.Command) (string, error) {
		calls++
		return "", context.Canceled
	})
	source := &core.Source{Name: "s", Retry: core.RetryPolicy{Attempts: 5, On: []string{"canceled"}}}
	policy := sources.NewPolicyRunner(runner, source)
	policy.Sleep = noSleep(&[]time.Duration{})

	if _, err := policy.Run(t.Context(), sources.Command{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled surfaced unchanged", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want exactly 1; cancellation must never be retried", calls)
	}
}

// TestPolicyRunnerWithNoPolicyRunsOnce is the default: a source declaring no retry: key
// behaves exactly as it did before this feature existed.
func TestPolicyRunnerWithNoPolicyRunsOnce(t *testing.T) {
	calls := 0
	runner := sources.RunnerFunc(func(context.Context, sources.Command) (string, error) {
		calls++
		return "", errors.New("fails every time")
	})
	policy := sources.NewPolicyRunner(runner, &core.Source{Name: "s"})
	if _, err := policy.Run(t.Context(), sources.Command{}); err == nil {
		t.Fatal("Run() succeeded, want the failure to propagate")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want exactly 1 with no retry declared", calls)
	}
}

// TestPacerSpacesCallsToTheDeclaredInterval is what retry cannot do on its own: retry spaces
// the attempts of ONE failing call, while a provider's rate limit counts EVERY call a source
// makes across a sync. A source collecting thirty days at concurrency four makes thirty calls
// retry never sees as related.
func TestPacerSpacesCallsToTheDeclaredInterval(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	pacer := sources.NewPacer(func() time.Time { return now })
	var waits []time.Duration
	pacer.Sleep = noSleep(&waits)
	source := &core.Source{Name: "s", MinInterval: 30 * time.Second}

	if err := pacer.Wait(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 0 {
		t.Fatalf("first call waited %v, want none", waits)
	}
	if err := pacer.Wait(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || waits[0] != 30*time.Second {
		t.Fatalf("second call waited %v, want [30s]", waits)
	}
	// The second call reserved the slot at T0+60s (T0+30s + the 30s interval), so the clock
	// has to pass THAT reservation, not merely the original interval, before a third call
	// waits nothing.
	now = now.Add(61 * time.Second)
	if err := pacer.Wait(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 {
		t.Fatalf("waits = %v, want no third wait once the reserved slot has passed", waits)
	}
}

// TestPacerNeverConflatesTwoSources keeps the pacer keyed correctly: one source's declared
// interval must never throttle a different source's calls.
func TestPacerNeverConflatesTwoSources(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	pacer := sources.NewPacer(func() time.Time { return now })
	var waits []time.Duration
	pacer.Sleep = noSleep(&waits)
	a := &core.Source{Name: "a", MinInterval: time.Minute}
	b := &core.Source{Name: "b", MinInterval: time.Minute}

	if err := pacer.Wait(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if err := pacer.Wait(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 0 {
		t.Fatalf("waits = %v, want none: b has never been called before", waits)
	}
}

// TestPacerWithNoIntervalNeverWaits is the default a source declaring no min_interval: gets.
func TestPacerWithNoIntervalNeverWaits(t *testing.T) {
	pacer := sources.NewPacer(nil)
	var waits []time.Duration
	pacer.Sleep = noSleep(&waits)
	source := &core.Source{Name: "s"}
	for range 3 {
		if err := pacer.Wait(t.Context(), source); err != nil {
			t.Fatal(err)
		}
	}
	if len(waits) != 0 {
		t.Fatalf("waits = %v, want none with no interval declared", waits)
	}
}

func TestPacerSpacesRetriesAndConcurrentUnitsAtTheInvocationBoundary(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	pacer := sources.NewPacer(func() time.Time { return now })
	var waitMu sync.Mutex
	var waits []time.Duration
	pacer.Sleep = func(_ context.Context, duration time.Duration) error {
		waitMu.Lock()
		waits = append(waits, duration)
		waitMu.Unlock()
		return nil
	}
	source := &core.Source{
		Name: "shared", MinInterval: 10 * time.Second,
		Retry: core.RetryPolicy{Attempts: 2, On: []string{"transient"}},
	}
	var callMu sync.Mutex
	calls, retryCalls := 0, 0
	inner := sources.RunnerFunc(func(_ context.Context, command sources.Command) (string, error) {
		callMu.Lock()
		defer callMu.Unlock()
		calls++
		if command.Argv[0] == "retry" && retryCalls == 0 {
			retryCalls++
			return "", errors.New("transient provider failure")
		}
		return "[]", nil
	})

	var group sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for _, name := range []string{"retry", "other"} {
		group.Add(1)
		go func() {
			defer group.Done()
			policy := sources.NewPolicyRunner(sources.NewPacingRunner(inner, pacer, source), source)
			policy.Sleep = noSleep(&[]time.Duration{})
			_, err := policy.Run(t.Context(), sources.Command{Argv: []string{name}})
			errorsSeen <- err
		}()
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatalf("provider calls = %d, want two units plus one retry", calls)
	}
	waitMu.Lock()
	slices.Sort(waits)
	gotWaits := append([]time.Duration(nil), waits...)
	waitMu.Unlock()
	if !slices.Equal(gotWaits, []time.Duration{10 * time.Second, 20 * time.Second}) {
		t.Fatalf("pacer waits = %v, want every invocation after the first to reserve its own slot", gotWaits)
	}
}

// TestDescribePolicyNamesWhatAppliesForTheTrustReview is the disclosure this feature exists
// to make: a review that says WHAT runs but not HOW MANY TIMES, HOW OFTEN, or FOR HOW LONG is
// not the whole review.
func TestDescribePolicyNamesWhatAppliesForTheTrustReview(t *testing.T) {
	quiet := sources.DescribePolicy(&core.Source{Name: "s"})
	if quiet != "" {
		t.Fatalf("DescribePolicy() = %q, want empty for a source declaring no policy", quiet)
	}
	full := sources.DescribePolicy(&core.Source{
		Name:        "s",
		Retry:       core.RetryPolicy{Attempts: 3, Backoff: 30 * time.Second, On: []string{"exit:7"}},
		MinInterval: 5 * time.Second,
		Timeout:     2 * time.Minute,
	})
	for _, want := range []string{"retry 3 attempts", "exit:7", "backoff 30s", "min interval 5s", "timeout 2m0s"} {
		if !strings.Contains(full, want) {
			t.Fatalf("DescribePolicy() = %q, want it to contain %q", full, want)
		}
	}
	// A windowed source's disclosure has to say so: it runs the command once for the WHOLE
	// requested range rather than once per day, which changes what a reviewer is approving.
	windowed := sources.DescribePolicy(&core.Source{Name: "s", Window: true})
	if !strings.Contains(windowed, "window") || !strings.Contains(windowed, "whole requested range") {
		t.Fatalf("DescribePolicy() = %q, want it to disclose the windowed collection", windowed)
	}
}
