package services

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

// ResolvedIdentity is one deterministic same-as component. Canonical is either the declared
// root entity URI or, for page-only identities, the lexicographically first authored page.
type ResolvedIdentity struct {
	Canonical string            `json:"canonical"`
	Kind      core.IdentityKind `json:"kind,omitempty"`
	Owner     bool              `json:"owner,omitempty"`
	Aliases   []string          `json:"aliases"`
	Names     []string          `json:"names,omitempty"`
	Pages     []string          `json:"pages,omitempty"`
}

// IdentityAlias is one URI-shaped alias suitable for graph same-as provenance. Bare aliases
// remain exact lookup keys but cannot be graph nodes.
type IdentityAlias struct {
	Alias     string
	Canonical string
	Via       string
}

// IdentityResolver is an immutable, base-local exact-identity index.
type IdentityResolver struct {
	byCanonical map[string]ResolvedIdentity
	aliases     map[string]string
	names       map[string][]string
	graph       []IdentityAlias
}

type identityDeclaration struct {
	canonical string
	kind      core.IdentityKind
	owner     bool
	aliases   []string
	names     []string
	pages     []string
	root      string
	via       map[string]string
}

// LoadIdentityResolver merges committed root declarations and authored person/organization
// pages. It performs no command and reads only published Markdown.
func LoadIdentityResolver(ctx context.Context, base *Base) (*IdentityResolver, error) {
	declarations := rootIdentityDeclarations(base)
	pageDeclarations, err := authoredIdentityDeclarations(ctx, base)
	if err != nil {
		return nil, err
	}
	declarations = append(declarations, pageDeclarations...)
	return resolveIdentityDeclarations(declarations)
}

func rootIdentityDeclarations(base *Base) []identityDeclaration {
	names := make([]string, 0, len(base.Config.Identities))
	for name := range base.Config.Identities {
		names = append(names, name)
	}
	sort.Strings(names)
	declarations := make([]identityDeclaration, 0, len(names))
	for _, name := range names {
		identity := base.Config.Identities[name]
		via := map[string]string{}
		for _, alias := range identity.Aliases {
			via[normalizeIdentityKey(alias)] = "identities." + name + ".aliases"
		}
		declarations = append(declarations, identityDeclaration{
			canonical: identity.Canonical, kind: identity.EffectiveKind(), owner: identity.Owner,
			aliases: append([]string(nil), identity.Aliases...), root: name, via: via,
		})
	}
	return declarations
}

func authoredIdentityDeclarations(ctx context.Context, base *Base) ([]identityDeclaration, error) {
	var declarations []identityDeclaration
	for _, layer := range []core.Layer{core.LayerProjects, core.LayerWiki} {
		if !base.Store.Enabled(layer) {
			continue
		}
		pages, _, err := loadMarkdownLayer(ctx, base, layer)
		if err != nil {
			return nil, err
		}
		for _, page := range pages {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
			kind := core.IdentityKind(strings.TrimSpace(page.Type))
			if len(page.Aliases) > 0 && kind != core.IdentityPerson && kind != core.IdentityOrganization {
				return nil, fmt.Errorf("%s: frontmatter aliases require type person or organization", page.URI)
			}
			if kind != core.IdentityPerson && kind != core.IdentityOrganization {
				continue
			}
			via := map[string]string{normalizeIdentityKey(page.URI): "frontmatter:aliases"}
			for _, alias := range page.Aliases {
				if err := core.ValidateIdentityAlias(alias); err != nil {
					return nil, fmt.Errorf("%s: frontmatter alias %q: %w", page.URI, alias, err)
				}
				via[normalizeIdentityKey(alias)] = "frontmatter:aliases"
			}
			declaration := identityDeclaration{
				canonical: page.URI, kind: kind, aliases: append([]string(nil), page.Aliases...),
				pages: []string{page.URI}, via: via,
			}
			if page.Title != "" {
				declaration.names = []string{page.Title}
			}
			declarations = append(declarations, declaration)
		}
	}
	return declarations, nil
}

func resolveIdentityDeclarations(declarations []identityDeclaration) (*IdentityResolver, error) {
	resolver := &IdentityResolver{
		byCanonical: map[string]ResolvedIdentity{}, aliases: map[string]string{}, names: map[string][]string{},
	}
	if len(declarations) == 0 {
		return resolver, nil
	}
	parents := make([]int, len(declarations))
	for index := range parents {
		parents[index] = index
	}
	var find func(int) int
	find = func(index int) int {
		if parents[index] != index {
			parents[index] = find(parents[index])
		}
		return parents[index]
	}
	union := func(left, right int) {
		left, right = find(left), find(right)
		if left != right {
			parents[right] = left
		}
	}
	claimed := map[string]int{}
	for index, declaration := range declarations {
		for _, value := range declarationIdentityTokens(declaration) {
			key := normalizeIdentityKey(value)
			if prior, found := claimed[key]; found {
				union(index, prior)
			} else {
				claimed[key] = index
			}
		}
	}
	components := map[int][]identityDeclaration{}
	for index, declaration := range declarations {
		root := find(index)
		components[root] = append(components[root], declaration)
	}
	ordered := make([]int, 0, len(components))
	for root := range components {
		ordered = append(ordered, root)
	}
	sort.Ints(ordered)
	for _, root := range ordered {
		if err := resolver.addComponent(components[root]); err != nil {
			return nil, err
		}
	}
	sort.Slice(resolver.graph, func(i, j int) bool {
		if resolver.graph[i].Alias != resolver.graph[j].Alias {
			return resolver.graph[i].Alias < resolver.graph[j].Alias
		}
		return resolver.graph[i].Canonical < resolver.graph[j].Canonical
	})
	return resolver, nil
}

func declarationIdentityTokens(declaration identityDeclaration) []string {
	values := []string{declaration.canonical}
	values = append(values, declaration.aliases...)
	values = append(values, declaration.pages...)
	return values
}

func (resolver *IdentityResolver) addComponent(component []identityDeclaration) error {
	rootCanonical := ""
	kind := core.IdentityKind("")
	owner := false
	aliases, names, pages := map[string]string{}, map[string]string{}, map[string]struct{}{}
	vias := map[string]string{}
	canonicalCandidates := make([]string, 0, len(component))
	for _, declaration := range component {
		if declaration.root != "" {
			if rootCanonical != "" && rootCanonical != declaration.canonical {
				return fmt.Errorf("identity aliases transitively join canonical declarations %q and %q", rootCanonical, declaration.canonical)
			}
			rootCanonical = declaration.canonical
		}
		canonicalCandidates = append(canonicalCandidates, declaration.canonical)
		if kind == "" {
			kind = declaration.kind
		} else if declaration.kind != "" && declaration.kind != kind {
			return fmt.Errorf("identity aliases transitively join kinds %q and %q", kind, declaration.kind)
		}
		owner = owner || declaration.owner
		for _, value := range declarationIdentityTokens(declaration) {
			aliases[normalizeIdentityKey(value)] = value
		}
		for key, via := range declaration.via {
			vias[key] = via
		}
		for _, name := range declaration.names {
			names[normalizeIdentityKey(name)] = name
		}
		for _, page := range declaration.pages {
			pages[page] = struct{}{}
		}
	}
	canonical := rootCanonical
	if canonical == "" {
		sort.Strings(canonicalCandidates)
		canonical = canonicalCandidates[0]
	}
	delete(aliases, normalizeIdentityKey(canonical))
	identity := ResolvedIdentity{
		Canonical: canonical, Kind: kind, Owner: owner,
		Aliases: sortedIdentityValues(aliases), Names: sortedIdentityValues(names), Pages: sortedIdentitySet(pages),
	}
	resolver.byCanonical[canonical] = identity
	resolver.aliases[normalizeIdentityKey(canonical)] = canonical
	for _, alias := range identity.Aliases {
		resolver.aliases[normalizeIdentityKey(alias)] = canonical
		if isGraphIdentityAlias(alias) {
			resolver.graph = append(resolver.graph, IdentityAlias{
				Alias: alias, Canonical: canonical, Via: vias[normalizeIdentityKey(alias)],
			})
		}
	}
	for _, name := range identity.Names {
		key := normalizeIdentityKey(name)
		resolver.names[key] = append(resolver.names[key], canonical)
		sort.Strings(resolver.names[key])
	}
	return nil
}

func sortedIdentityValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := normalizeIdentityKey(result[i]), normalizeIdentityKey(result[j])
		if left != right {
			return left < right
		}
		return result[i] < result[j]
	})
	return result
}

func sortedIdentitySet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func isGraphIdentityAlias(value string) bool {
	if core.ValidateEntityURI(value) == nil {
		return true
	}
	parsed, err := ParseURI(value)
	if err != nil || parsed.Scheme != SchemeFile || parsed.Fragment != "" || parsed.JQ != "" || parsed.Dir {
		return false
	}
	first, _, found := strings.Cut(parsed.Path, "/")
	if !found {
		return false
	}
	return core.Layer(first) == core.LayerWiki || core.Layer(first) == core.LayerProjects
}

func normalizeIdentityKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// Canonical resolves an exact alias or unique authored name. Unknown values pass through,
// except GitHub noreply commit emails whose actor URI is explicitly encoded by the address.
func (resolver *IdentityResolver) Canonical(value string) string {
	key := normalizeIdentityKey(value)
	if canonical, found := resolver.aliases[key]; found {
		return canonical
	}
	if matches := resolver.names[key]; len(matches) == 1 {
		return matches[0]
	}
	if actor, ok := sources.NormalizeGitHubNoreplyActor(value); ok {
		if canonical, found := resolver.aliases[normalizeIdentityKey(actor)]; found {
			return canonical
		}
		return actor
	}
	return value
}

// Exact resolves one unambiguous exact alias or page name.
func (resolver *IdentityResolver) Exact(value string) (ResolvedIdentity, bool) {
	canonical := resolver.Canonical(value)
	identity, found := resolver.byCanonical[canonical]
	return identity, found
}

// Match resolves an exact key first, then returns deterministic substring matches for `who`.
func (resolver *IdentityResolver) Match(value string) []ResolvedIdentity {
	if identity, found := resolver.Exact(value); found {
		return []ResolvedIdentity{identity}
	}
	needle := normalizeIdentityKey(value)
	if needle == "" {
		return nil
	}
	matches := make([]ResolvedIdentity, 0)
	for _, identity := range resolver.Identities() {
		values := append([]string{identity.Canonical}, identity.Aliases...)
		values = append(values, identity.Names...)
		for _, candidate := range values {
			if strings.Contains(normalizeIdentityKey(candidate), needle) {
				matches = append(matches, identity)
				break
			}
		}
	}
	return matches
}

// Identities returns canonical components in stable order.
func (resolver *IdentityResolver) Identities() []ResolvedIdentity {
	identities := make([]ResolvedIdentity, 0, len(resolver.byCanonical))
	for _, identity := range resolver.byCanonical {
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(i, j int) bool { return identities[i].Canonical < identities[j].Canonical })
	return identities
}

// GraphAliases returns URI aliases whose same-as edges graph extraction must record.
func (resolver *IdentityResolver) GraphAliases() []IdentityAlias {
	return append([]IdentityAlias(nil), resolver.graph...)
}

// IsOwner reports whether a value exactly resolves to the one declared owner identity.
func (resolver *IdentityResolver) IsOwner(value string) bool {
	identity, found := resolver.Exact(value)
	return found && identity.Owner
}

// IsCanonical reports whether the value is a resolved component's canonical node.
func (resolver *IdentityResolver) IsCanonical(value string) bool {
	_, found := resolver.byCanonical[value]
	return found
}

// Kind returns the declared kind of an exact identity value.
func (resolver *IdentityResolver) Kind(value string) core.IdentityKind {
	identity, found := resolver.Exact(value)
	if !found {
		return ""
	}
	return identity.Kind
}
