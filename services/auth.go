package services

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

type authProbeResult struct {
	ready bool
	err   error
}

type pendingAuthProbe struct {
	done   chan struct{}
	result authProbeResult
}

// authProbeCache coordinates concurrent sync units without weakening the per-source rule: a
// source with several missing days still runs its readiness probe exactly once for this sync.
type authProbeCache struct {
	base *Base
	mu   sync.Mutex
	byID map[string]*pendingAuthProbe
}

func newAuthProbeCache(base *Base) *authProbeCache {
	return &authProbeCache{base: base, byID: make(map[string]*pendingAuthProbe)}
}

func (cache *authProbeCache) ready(ctx context.Context, source *core.Source) (bool, error) {
	if len(source.Auth) == 0 {
		return true, nil
	}
	cache.mu.Lock()
	pending, exists := cache.byID[source.Name]
	if !exists {
		pending = &pendingAuthProbe{done: make(chan struct{})}
		cache.byID[source.Name] = pending
	}
	cache.mu.Unlock()

	if exists {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-pending.done:
			return pending.result.ready, pending.result.err
		}
	}

	pending.result.ready, pending.result.err = runAuthProbe(ctx, cache.base, source)
	close(pending.done)
	return pending.result.ready, pending.result.err
}

func runAuthProbe(ctx context.Context, base *Base, source *core.Source) (bool, error) {
	if len(source.Auth) == 0 {
		return true, nil
	}
	command := sources.BuildAuthCommand(source, base.Env, base.Config.Sync.Timeout)
	_, runErr := base.Runner.Run(ctx, command)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	if runErr == nil {
		return true, nil
	}
	// Only a process exit is the provider CLI answering "not ready". Trust drift, an unsafe
	// path, timeout, signal, missing executable, and runner failures must stop the unit.
	type exitFailure interface {
		error
		ExitCode() (int, bool)
	}
	var failure exitFailure
	if errors.As(runErr, &failure) {
		if _, exited := failure.ExitCode(); exited {
			// Output and diagnostics stay discarded: account state is recoverable and private.
			return false, nil
		}
	}
	return false, runErr
}

// ProbeSourceAuth reports enabled sources whose declared readiness probe currently fails.
// Callers such as MCP pass live=false to preserve their offline/read-only boundary. Identical
// argv are run once because sources commonly share one provider login.
func ProbeSourceAuth(
	ctx context.Context, base *Base, candidates []*core.Source, live bool,
) ([]string, error) {
	if !live {
		return []string{}, nil
	}
	hasProbe := false
	for _, source := range candidates {
		if len(source.Auth) > 0 {
			hasProbe = true
			break
		}
	}
	if !hasProbe {
		return []string{}, nil
	}
	if err := base.RequireTrust(ctx); err != nil {
		return nil, err
	}
	type result struct {
		ready bool
		err   error
	}
	results := make(map[string]result)
	required := make([]string, 0)
	for _, source := range candidates {
		if len(source.Auth) == 0 {
			continue
		}
		encoded, err := json.Marshal(source.Auth)
		if err != nil {
			return nil, err
		}
		key := string(encoded)
		observed, exists := results[key]
		if !exists {
			observed.ready, observed.err = runAuthProbe(ctx, base, source)
			results[key] = observed
		}
		if observed.err != nil {
			return nil, observed.err
		}
		if !observed.ready {
			required = append(required, source.Name)
		}
	}
	sort.Strings(required)
	return required, nil
}
