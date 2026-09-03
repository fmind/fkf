package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/services"
)

// Instructions describe the base that is actually open — its public name, enabled layers, and
// source count — plus the trust sentence and reading chain. They use only the already-loaded
// configuration: a full status audit is available as a resource and must not make MCP startup
// grow with the corpus. The local filesystem root is deliberately absent: every model-visible
// address is an fkf URI, so revealing a username or customer/workspace path adds exposure
// without helping a client use the server.
func Instructions(ctx context.Context, base *services.Base) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// header is the part that varies with the base: its name, enabled layers, and sync
	// status. trailer is fixed text — the trust notice and the URI grammar — that every base
	// needs regardless of size. A blind tail truncation at maxInstructionBytes could cut the
	// trailer off entirely on a base with a long name or many quiet sources, silently dropping
	// the one sentence (UntrustedEvidenceNotice) that must reach every session. Truncating the
	// header alone, and always appending the full trailer, keeps that guarantee regardless of
	// how long the variable part grows.
	var header strings.Builder
	fmt.Fprintf(&header, "This server exposes the fkf base %q, read-only.\n\n", base.Config.Name)
	fmt.Fprintf(&header, "Enabled layers: %s.\n", strings.Join(layerNames(base), ", "))
	fmt.Fprintf(&header, "%d source(s) enabled. Read fkf://%s/status for collection health and freshness.\n",
		len(base.Config.EnabledSources()), base.Config.Name)

	var trailer strings.Builder
	trailer.WriteString("\n" + UntrustedEvidenceNotice + "\n\n")
	trailer.WriteString("Start with context for a ranked, budgeted pack, or find for every match in the base. ")
	if base.Store.Enabled(core.LayerWiki) {
		fmt.Fprintf(&trailer,
			"Then read the fkf://%s/wiki/index and fkf://%s/wiki/tags resources, and read the wiki/<slug>.md pages that matter. ",
			base.Config.Name, base.Config.Name)
	}
	trailer.WriteString(
		"Every result carries a uri you can pass to read or graph; cite it. " +
			"Use graph with direction \"in\" to find what points at a page or entity.\n\n",
	)
	trailer.WriteString(
		"URIs: events/<date>/<source>.json#<id> is one record by its declared id; " +
			"<path>?jq=<expr> selects with jq; wiki/<slug>.md#<anchor> is a heading; " +
			"any non-reserved lowercase <scheme>:<identity> names an entity with no file of its own.\n",
	)

	return truncateBytes(header.String(), maxInstructionBytes-trailer.Len()) + trailer.String(), nil
}

// truncateBytes shortens value to at most limit bytes, snapping back to the nearest rune
// boundary so a multi-byte character is never split in half.
func truncateBytes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

func layerNames(base *services.Base) []string {
	names := make([]string, 0, len(core.Layers))
	for _, layer := range base.Store.EnabledLayers() {
		names = append(names, string(layer))
	}
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}
