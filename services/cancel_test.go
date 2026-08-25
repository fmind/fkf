package services_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
	"github.com/fmind/fkf/sources"
)

type cancelAfterErrChecks struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (ctx *cancelAfterErrChecks) Err() error {
	ctx.remaining--
	if ctx.remaining == 0 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestLongOfflineServicesRefuseAPreCancelledContext(t *testing.T) {
	base := queryBase(t)
	write(t, base, "wiki/retrieval-boundary.md", "# Retrieval boundary\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name string
		run  func() error
	}{
		{name: "find", run: func() error {
			_, err := services.Find(ctx, base, services.FindFilter{Grep: []string{"retrieval"}}, false)
			return err
		}},
		{name: "context", run: func() error {
			_, err := services.BuildContext(ctx, base, services.ContextRequest{Query: "retrieval"})
			return err
		}},
		{name: "read document", run: func() error {
			_, err := services.Read(ctx, base, "events/2026-05-04/synthetic.json", services.ReadOptions{})
			return err
		}},
		{name: "read page", run: func() error {
			_, err := services.Read(ctx, base, "wiki/retrieval-boundary.md", services.ReadOptions{})
			return err
		}},
		{name: "events", run: func() error {
			_, err := services.ListEvents(ctx, base, services.Window{}, "", 0)
			return err
		}},
		{name: "pages", run: func() error {
			_, err := services.ListPages(ctx, base, core.LayerWiki, services.PageFilter{})
			return err
		}},
		{name: "page search", run: func() error {
			_, err := services.SearchPages(ctx, base, core.LayerWiki, []string{"retrieval"}, services.PageFilter{})
			return err
		}},
		{name: "graph summary", run: func() error {
			_, err := services.SummarizeGraph(ctx, base)
			return err
		}},
		{name: "graph nodes", run: func() error {
			_, err := services.ListNodes(ctx, base, "", 0)
			return err
		}},
		{name: "graph walk", run: func() error {
			_, err := services.Neighbours(ctx, base, services.GraphQuery{URI: "repo:fmind/fkf"})
			return err
		}},
		{name: "validation", run: func() error {
			_, err := services.ValidateMarkdownLayer(ctx, base, core.LayerWiki, false, true)
			return err
		}},
		{name: "document verification", run: func() error {
			_, err := services.Verify(ctx, base)
			return err
		}},
		{name: "derived build", run: func() error {
			_, err := services.BuildGraph(ctx, base)
			return err
		}},
		{name: "trust disclosure", run: func() error {
			_, err := services.Trust(ctx, base, false, false)
			return err
		}},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.run(); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled service error = %v, want context.Canceled", err)
			}
		})
	}
}

// TestDocumentDecodeCanBeCancelledAfterParsingStarts characterizes the lower seam Read relies
// on. The synthetic context cancels after the decoder has accepted more than one input buffer,
// so this is not merely another pre-cancelled call and remains deterministic under the race
// detector without a timer or scheduler assumption.
func TestDocumentDecodeCanBeCancelledAfterParsingStarts(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	ctx := &cancelAfterErrChecks{Context: parent, cancel: cancel, remaining: 4}
	t.Cleanup(cancel)
	data := []byte(`{"fkf":1,"source":"synthetic","layer":"index","payload":"` +
		strings.Repeat("x", 1<<20) + `"}`)

	if _, err := sources.DecodeDocumentContext(ctx, data, "index/synthetic.json"); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-decode error = %v, want context.Canceled", err)
	}
	if ctx.remaining > 0 {
		t.Fatalf("decoder stopped with %d checks remaining, want cancellation reached during parsing", ctx.remaining)
	}
}
