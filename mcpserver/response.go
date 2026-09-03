package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

// maxResponseBytes bounds what one MCP call may return. It reuses core.MaxNarrativeBytes — the
// same "this is meant for a model to read" ceiling `fkf read` already enforces on disk — because
// a response heading into a connected agent's context window is bound by exactly that concern,
// not by how it was produced. Reading a busy day's whole document had no bound at all before
// this: one call could return several megabytes and, per the go-sdk's own fallback behaviour
// below, send it twice.
var maxResponseBytes = int(core.MaxNarrativeBytes)

// boundToolResponses is the final size gate for tools/call. The typed SDK may reject an input
// before wrap runs, and it turns handler errors into TextContent after wrap returns; measuring
// here is therefore the only place that covers successful, validation, and handler-error paths.
func boundToolResponses(base *services.Base) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, request)
			if _, ok := request.(*mcp.CallToolRequest); !ok {
				return result, err
			}
			if err != nil {
				// Protocol errors can also quote caller-controlled tool names. Four KiB is ample
				// diagnostic space and leaves the surrounding JSON-RPC envelope far below the cap.
				if len(err.Error()) <= maxInstructionBytes {
					return nil, err
				}
				return boundedToolError(base, "tool call failed with an error too large to return safely; retry with smaller arguments"), nil
			}
			call, ok := result.(*mcp.CallToolResult)
			if !ok || call == nil {
				return result, nil
			}
			size, encodeErr := encodedToolResultSize(call)
			if encodeErr != nil {
				return boundedToolError(base, "tool result could not be encoded safely"), nil
			}
			if size <= maxResponseBytes {
				return result, nil
			}
			return boundedToolError(base, fmt.Sprintf(
				"tool response exceeded the %d-byte limit; retry with smaller arguments or narrower filters",
				maxResponseBytes,
			)), nil
		}
	}
}

func boundedToolError(base *services.Base, message string) *mcp.CallToolResult {
	result := &mcp.CallToolResult{Meta: mcp.Meta{
		mcp.MetaKeyServerInfo: serverImplementation(base),
	}}
	result.SetError(fmt.Errorf("%w: %s", core.ErrFileTooLarge, message))
	return result
}

// wrap adds the one structured log line every call emits and bounds the complete dual-channel
// result: structured-output clients and TextContent-only clients receive the same JSON.
//
// It records what was asked and how much came back — never the evidence itself, which is why
// the input is reduced to a digest rather than logged: a server log must not become a second,
// unmanaged copy of the base.
func wrap[In any](base *services.Base, tool string, handler func(context.Context, In) (any, int, error)) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input In) (*mcp.CallToolResult, any, error) {
		started := base.Now()
		result, items, err := handler(ctx, input)
		attributes := []any{
			"tool", tool, "base", base.Config.Name, "items", items,
			"elapsed_ms", base.Now().Sub(started).Milliseconds(), "input_digest", digestInput(input),
		}
		if err != nil {
			// The CLASS, never the text. The text was assumed to be fkf's own diagnostic, and
			// most of it is — but a `?jq=` failure carries the value it failed on, straight out
			// of gojq: `tonumber cannot be applied to "Review quiet source watc ..."` is a
			// collected record's title, and on a real base that is a mail subject or a page
			// title. The expression is chosen by the connected agent, so slicing the field
			// walks any record into the log twenty-four characters at a time. The caller still
			// receives an actionable error with base-local paths rewritten as fkf URIs, but the
			// log must not become a second, unmanaged copy of the base.
			slog.Info("fkf mcp call failed", append(attributes, "error", errorClass(err))...)
			return nil, nil, safeClientError(base, err)
		}
		result, graphGeneration := unwrapToolResult(result)
		payload, err := json.Marshal(result)
		if err != nil {
			failure := fmt.Errorf("encode the %s result: %w", tool, err)
			slog.Info("fkf mcp call failed", append(attributes, "error", errorClass(failure))...)
			return nil, nil, safeClientError(base, failure)
		}
		envelope, size, err := dualToolResult(base, payload, items, graphGeneration)
		if err != nil {
			failure := fmt.Errorf("encode the %s MCP result: %w", tool, err)
			slog.Info("fkf mcp call failed", append(attributes, "error", errorClass(failure))...)
			return nil, nil, safeClientError(base, failure)
		}
		if size > maxResponseBytes {
			refusal := fmt.Errorf("%w: %s returned %d bytes, over the %d-byte limit for one call; %s",
				core.ErrFileTooLarge, tool, size, maxResponseBytes, narrowingHint(tool))
			slog.Info("fkf mcp call failed", append(attributes, "error", errorClass(refusal))...)
			return nil, nil, refusal
		}
		attributes = append(attributes, "bytes", size)
		slog.Info("fkf mcp call", attributes...)
		// Leaving Content and StructuredContent unset asks ToolHandlerFor to populate both from
		// this one compact encoding. The preflight above measured that exact duplicated shape.
		return envelope, json.RawMessage(payload), nil
	}
}

// completeResultField is added by go-sdk for the current protocol after the tool handler
// returns. The candidate already includes the exact server-info metadata it will carry, so this
// is the only result-level byte overhead not representable through exported SDK fields.
const completeResultField = `,"resultType":"complete"`

func dualToolResult(
	base *services.Base, payload json.RawMessage, items int, graphGeneration string,
) (*mcp.CallToolResult, int, error) {
	hint := map[string]any{"bytes": 0, "items": items, "maxBytes": maxResponseBytes}
	meta := mcp.Meta{
		mcp.MetaKeyServerInfo: serverImplementation(base),
		ResultSizeMetaKey:     hint,
	}
	if graphGeneration != "" {
		meta[GraphGenerationMetaKey] = graphGeneration
	}
	envelope := &mcp.CallToolResult{Meta: meta}
	for range 4 {
		candidate := *envelope
		candidate.Content = []mcp.Content{&mcp.TextContent{Text: string(payload)}}
		candidate.StructuredContent = payload
		size, err := encodedToolResultSize(&candidate)
		if err != nil {
			return nil, 0, err
		}
		if hint["bytes"] == size {
			return envelope, size, nil
		}
		hint["bytes"] = size
	}
	return nil, 0, errors.New("stabilize MCP tool result size hint")
}

func encodedToolResultSize(result *mcp.CallToolResult) (int, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return 0, err
	}
	return len(encoded) + len(completeResultField), nil
}

type clientError struct {
	message string
	cause   error
}

func (e clientError) Error() string { return e.message }
func (e clientError) Unwrap() error { return e.cause }

// safeClientError preserves the sentinel error for MCP classification while removing local
// filesystem layout from the text delivered to a connected model. Paths inside the base become
// the relative fkf addresses the client can actually use; a home path outside it is anonymized.
func safeClientError(base *services.Base, err error) error {
	return clientError{message: safeClientText(base, err.Error()), cause: err}
}

func safeClientText(base *services.Base, value string) string {
	root := filepath.Clean(base.Root())
	separator := string(filepath.Separator)
	value = strings.ReplaceAll(value, root+separator, "")
	value = strings.ReplaceAll(value, root, ".")
	state := core.StateDir()
	if state != "" {
		state = filepath.Clean(state)
	}
	if state != "" && state != root {
		value = strings.ReplaceAll(value, state+separator, "state/")
		value = strings.ReplaceAll(value, state, "state")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != root {
		home = filepath.Clean(home)
		value = strings.ReplaceAll(value, home+separator, "~/")
		value = strings.ReplaceAll(value, home, "~")
	}
	return value
}

func safeClientPath(base *services.Base, value string) string {
	if !filepath.IsAbs(value) {
		return filepath.ToSlash(value)
	}
	relative, err := filepath.Rel(base.Root(), value)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(relative)
	}
	return filepath.Base(value)
}

// narrowingHint names how to shrink an oversized response, tool by tool: read/jq/id addresses
// exactly one thing, and every other tool already has a --limit or a --budget to lower.
func narrowingHint(tool string) string {
	if tool == "read" {
		return "add `?jq=` or `#id` to the uri to select part of it"
	}
	return "lower limit or budget, or narrow since/until"
}

// errorClass names why a call failed without quoting anything the base holds. The sentinels
// are the ones the read path returns; anything else is reported as its kind alone, because an
// unrecognised message is exactly the one whose contents cannot be vouched for.
func errorClass(err error) string {
	for _, known := range []struct {
		sentinel error
		name     string
	}{
		{core.ErrUntrusted, "untrusted-base"},
		{core.ErrNotAddressable, "not-addressable"},
		{core.ErrPathEscapes, "path-escapes"},
		{core.ErrUnsafePath, "unsafe-path"},
		{core.ErrFileTooLarge, "too-large"},
		{services.ErrContextBudgetTooSmall, "budget-too-small"},
		{fs.ErrNotExist, "not-found"},
		{context.Canceled, "cancelled"},
		{context.DeadlineExceeded, "timeout"},
	} {
		if errors.Is(err, known.sentinel) {
			return known.name
		}
	}
	var disabled core.ErrLayerDisabled
	if errors.As(err, &disabled) {
		return "layer-disabled"
	}
	return "error"
}

func digestInput(input any) string {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "unencodable"
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])[:12]
}
