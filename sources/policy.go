package sources

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fmind/fkf/core"
)

// A source declares WHAT it runs; this file declares HOW fkf invokes it. The distinction is
// the same one `timeout:` already draws: the command text is unchanged, so `fkf trust` still
// prints the line a reviewer approves, and a retried day that still fails still fails.
//
// It exists because the alternative is shell. A rate-limited provider drove a real base to
// wrap `gh search` in a hand-written script that sleeps until the limit resets — moving a
// retry loop out of tested Go and into the one surface a human must re-read on every trust,
// where it is neither tested nor visible as policy.

// PolicyRunner applies one source's declared back-pressure around another Runner. It is
// created per unit of work, so Attempts reports what that unit actually cost.
type PolicyRunner struct {
	inner  Runner
	source *core.Source
	// Sleep is the wait between attempts. It is a field so a test can assert the schedule
	// without spending it, and it takes a context so a cancelled sync stops waiting.
	Sleep func(ctx context.Context, d time.Duration) error

	mu       sync.Mutex
	attempts int
}

// NewPolicyRunner wraps inner with source's declared retry policy.
func NewPolicyRunner(inner Runner, source *core.Source) *PolicyRunner {
	return &PolicyRunner{inner: inner, source: source, Sleep: sleepContext}
}

// Attempts is how many times the command was actually run. A retried failure must never be
// quieter than a first-try one, so the sync unit reports this even when the run succeeded.
func (p *PolicyRunner) Attempts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts
}

// Run executes the command, retrying only the failures the source named.
func (p *PolicyRunner) Run(ctx context.Context, cmd Command) (string, error) {
	allowed := 1
	if p.source != nil {
		allowed = p.source.RetryAttempts()
	}
	var lastErr error
	for attempt := 1; attempt <= allowed; attempt++ {
		p.mu.Lock()
		p.attempts = attempt
		p.mu.Unlock()
		stdout, err := p.inner.Run(ctx, cmd)
		if err == nil {
			return stdout, nil
		}
		lastErr = err
		// A cancelled or timed-out run is never retried: the operator asked it to stop, and a
		// timeout is the bound saying this command does not finish, not a transient failure.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		if attempt == allowed || !p.retryable(err) {
			return "", err
		}
		if wait := p.backoff(attempt); wait > 0 {
			if sleepErr := p.Sleep(ctx, wait); sleepErr != nil {
				return "", sleepErr
			}
		}
	}
	return "", lastErr
}

// backoff grows linearly with the attempt number rather than exponentially. A provider's
// documented reset window is a wall-clock fact, and a source declaring `backoff: 30s` means
// thirty seconds — doubling silently turns three attempts into two minutes of nothing.
func (p *PolicyRunner) backoff(attempt int) time.Duration {
	if p.source == nil || p.source.Retry.Backoff <= 0 {
		return 0
	}
	return time.Duration(attempt) * p.source.Retry.Backoff
}

// retryable matches the failure against the conditions the source declared. An empty list
// never matches, which is what makes retry-anything unreachable: the loader already refuses a
// policy with more than one attempt and no `on:`.
//
// The matched text is never logged, stored, or returned. Provider stderr is exactly where a
// credential surfaces, and a retry decision is not worth a second copy of one.
func (p *PolicyRunner) retryable(err error) bool {
	if p.source == nil {
		return false
	}
	// The real runner returns an opaque command failure: its Error is deliberately safe, while
	// these two methods expose only the exact decisions retry needs. Keeping the matcher on the
	// error means wrapping it with source/day context never loses the private retry evidence.
	type privateCommandFailure interface {
		error
		ExitCode() (int, bool)
		MatchesCommandStderr(string) bool
	}
	var commandFailure privateCommandFailure
	if !errors.As(err, &commandFailure) {
		return false
	}
	code, hasCode := commandFailure.ExitCode()
	for _, condition := range p.source.Retry.On {
		if wanted, isExit := strings.CutPrefix(condition, "exit:"); isExit {
			if parsed, parseErr := strconv.Atoi(strings.TrimSpace(wanted)); parseErr == nil &&
				hasCode && parsed == code {
				return true
			}
			continue
		}
		if commandFailure.MatchesCommandStderr(condition) {
			return true
		}
	}
	return false
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Pacer enforces a source's `min_interval:` across the whole sync, which retry cannot: retry
// spaces the attempts of one failing call, while a provider's rate limit counts every call.
// A source collecting thirty days at concurrency four makes thirty calls that retry never sees
// as related.
//
// It is a value on the sync run rather than a package global so two syncs in one process — a
// test suite — never pace each other.
type Pacer struct {
	mu   sync.Mutex
	next map[string]time.Time
	// Now and Sleep are injected for the same reason PolicyRunner's Sleep is.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

type pacingRunner struct {
	inner  Runner
	pacer  *Pacer
	source *core.Source
}

// NewPacingRunner wraps the actual invocation boundary. It must sit inside PolicyRunner so
// the first try and every retry reserve separate provider-rate-limit slots shared with all
// concurrent units for the same source.
func NewPacingRunner(inner Runner, pacer *Pacer, source *core.Source) Runner {
	return &pacingRunner{inner: inner, pacer: pacer, source: source}
}

func (p *pacingRunner) Run(ctx context.Context, cmd Command) (string, error) {
	if err := p.pacer.Wait(ctx, p.source); err != nil {
		return "", err
	}
	return p.inner.Run(ctx, cmd)
}

// NewPacer returns a Pacer using the wall clock.
func NewPacer(now func() time.Time) *Pacer {
	if now == nil {
		now = time.Now
	}
	return &Pacer{next: map[string]time.Time{}, Now: now, Sleep: sleepContext}
}

// Wait blocks until this source may be invoked again, then reserves the following slot. A
// source declaring no interval never waits and never takes the lock for longer than a map read.
func (p *Pacer) Wait(ctx context.Context, source *core.Source) error {
	if p == nil || source == nil || source.MinInterval <= 0 {
		return nil
	}
	p.mu.Lock()
	now := p.Now()
	wait := time.Duration(0)
	if ready, seen := p.next[source.Name]; seen && ready.After(now) {
		wait = ready.Sub(now)
	}
	// Reserved before unlocking, so two goroutines entering together are spaced rather than
	// both waiting the same amount and then colliding.
	p.next[source.Name] = now.Add(wait + source.MinInterval)
	p.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	return p.Sleep(ctx, wait)
}

// DescribePolicy renders a source's invocation policy for the trust listing, or "" when it
// declares none. The review has to say how a command will be invoked, not only what it is.
func DescribePolicy(source *core.Source) string {
	if source == nil {
		return ""
	}
	parts := make([]string, 0, 4)
	if source.Window {
		parts = append(parts, "window: one command for the whole requested range")
	}
	if source.Retry.Attempts > 1 {
		detail := fmt.Sprintf("retry %d attempts on %s", source.Retry.Attempts,
			strings.Join(source.Retry.On, ", "))
		if source.Retry.Backoff > 0 {
			detail += fmt.Sprintf(", backoff %s", source.Retry.Backoff)
		}
		parts = append(parts, detail)
	}
	if source.MinInterval > 0 {
		parts = append(parts, "min interval "+source.MinInterval.String())
	}
	if source.Timeout > 0 {
		parts = append(parts, "timeout "+source.Timeout.String())
	}
	return strings.Join(parts, "; ")
}
