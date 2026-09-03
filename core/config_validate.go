package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func validateConfig(config *Config) error {
	path := config.Path
	if config.FKF != ConfigVersion {
		return fmt.Errorf("%w: %s: fkf must be %d; got %d", ErrConfig, path, ConfigVersion, config.FKF)
	}
	if config.Name == "" {
		return fmt.Errorf("%w: %s: name is required; it is the MCP server name and the resource authority", ErrConfig, path)
	}
	if !sourceNamePattern.MatchString(config.Name) {
		return fmt.Errorf("%w: %s: name %q must be lowercase letters, digits, and hyphens", ErrConfig, path, config.Name)
	}
	if len(config.Name) > MaxBaseNameLength {
		return fmt.Errorf("%w: %s: name is %d bytes; expected at most %d", ErrConfig, path, len(config.Name), MaxBaseNameLength)
	}
	if err := ValidateFieldSchema(config.Schema); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrConfig, path, err)
	}
	if err := validateIdentities(config); err != nil {
		return err
	}
	if err := validateCommandBin(config); err != nil {
		return err
	}
	if err := validateSync(config.Sync, path); err != nil {
		return err
	}
	for _, name := range config.SourceNames() {
		if err := validateSource(config, config.Sources[name]); err != nil {
			return err
		}
	}
	return nil
}

var bareIdentityAliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+@-]*$`)

func validateIdentities(config *Config) error {
	claimed := map[string]string{}
	owners := 0
	for _, name := range sortedIdentityNames(config.Identities) {
		owner, err := validateIdentityDeclaration(config, name, claimed)
		if err != nil {
			return err
		}
		if owner {
			owners++
		}
	}
	if owners > 1 {
		return fmt.Errorf("%w: %s: at most one identity may be the owner; found %d", ErrConfig, config.Path, owners)
	}
	return nil
}

func sortedIdentityNames(identities map[string]*Identity) []string {
	names := make([]string, 0, len(identities))
	for name := range identities {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func validateIdentityDeclaration(config *Config, name string, claimed map[string]string) (bool, error) {
	identity := config.Identities[name]
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s: identities.%s: %s", ErrConfig, config.Path, name, fmt.Sprintf(format, args...))
	}
	if !sourceNamePattern.MatchString(name) || len(name) > MaxBaseNameLength {
		return false, fail("name must be lowercase letters, digits, and hyphens and at most %d bytes", MaxBaseNameLength)
	}
	if identity == nil || identity.Canonical == "" {
		return false, fail("canonical is required")
	}
	if err := ValidateEntityURI(identity.Canonical); err != nil {
		return false, fail("canonical %q: %v", identity.Canonical, err)
	}
	if len(identity.Aliases) == 0 {
		return false, fail("aliases must contain at least one exact entity URI, email, or login")
	}
	if err := validateIdentityKind(*identity); err != nil {
		return false, fail("%v", err)
	}
	if err := claimIdentityValues(name, identity, claimed); err != nil {
		return false, fail("%v", err)
	}
	return identity.Owner, nil
}

func validateIdentityKind(identity Identity) error {
	switch identity.Kind {
	case "", IdentityPerson, IdentityOrganization, IdentityRepository:
	default:
		return fmt.Errorf("kind %q must be person, organization, or repository", identity.Kind)
	}
	if identity.Owner && identity.EffectiveKind() != IdentityPerson {
		return errors.New("owner may only mark a person identity")
	}
	return nil
}

func claimIdentityValues(name string, identity *Identity, claimed map[string]string) error {
	values := append([]string{identity.Canonical}, identity.Aliases...)
	for index, value := range values {
		value = strings.TrimSpace(value)
		if index > 0 {
			identity.Aliases[index-1] = value
			if err := ValidateIdentityAlias(value); err != nil {
				return fmt.Errorf("alias %q: %w", value, err)
			}
		}
		key := strings.ToLower(value)
		if owner, exists := claimed[key]; exists {
			return fmt.Errorf("value %q also belongs to identities.%s", value, owner)
		}
		claimed[key] = name
	}
	return nil
}

// ValidateIdentityAlias validates the shared root and authored-page alias grammar.
func ValidateIdentityAlias(value string) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 320 {
		return errors.New("must be non-empty, unpadded, and at most 320 bytes")
	}
	if strings.Contains(value, ":") {
		return ValidateEntityURI(value)
	}
	if !bareIdentityAliasPattern.MatchString(value) || strings.Count(value, "@") > 1 {
		return errors.New("must be an entity URI, email, or login without whitespace")
	}
	if local, domain, found := strings.Cut(value, "@"); found && (local == "" || domain == "") {
		return errors.New("email aliases need non-empty local and domain parts")
	}
	return nil
}

func validateSync(sync SyncConfig, path string) error {
	switch {
	case sync.Days < 1 || sync.Days > 366:
		return fmt.Errorf("%w: %s: sync.days is %d; expected 1..366", ErrConfig, path, sync.Days)
	case sync.IndexMaxAgeHours < 1 || sync.IndexMaxAgeHours > MaxFreshnessAgeHours:
		return fmt.Errorf("%w: %s: sync.index_max_age_hours is %d; expected 1..%d",
			ErrConfig, path, sync.IndexMaxAgeHours, MaxFreshnessAgeHours)
	case sync.Concurrency < 1 || sync.Concurrency > MaxSyncConcurrency:
		return fmt.Errorf("%w: %s: sync.concurrency is %d; expected 1..%d", ErrConfig, path, sync.Concurrency, MaxSyncConcurrency)
	case sync.Timeout < time.Second || sync.Timeout > time.Hour:
		return fmt.Errorf("%w: %s: sync.timeout is %s; expected 1s..1h", ErrConfig, path, sync.Timeout)
	}
	return nil
}

// validateSource applies the completeness rules to every declared source, enabled or not. A
// disabled source with a typo is still a lie in a file people read to learn what a base can
// collect, so it fails the load.
func validateSource(config *Config, source *Source) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s: sources.%s: %s", ErrConfig, config.Path, source.Name, fmt.Sprintf(format, args...))
	}
	if err := validateRun(config, source, fail); err != nil {
		return err
	}
	if err := validateAuth(config, source, fail); err != nil {
		return err
	}
	if err := validateTest(config, source, fail); err != nil {
		return err
	}
	if err := validateRequirements(source, fail); err != nil {
		return err
	}
	if err := validateSourceFields(config, source, fail); err != nil {
		return err
	}
	if err := validateSourcePolicy(config, source, fail); err != nil {
		return err
	}
	if err := validateBody(config, source, fail); err != nil {
		return err
	}
	if source.Layer != LayerTasks && source.Fields.Path(FieldTitle).IsZero() {
		return fail("fields.title is required: every collected record needs a meaningful subject line")
	}
	return nil
}

// validateAuth keeps the readiness probe a literal, reviewable argv. Unlike run and body,
// authentication readiness needs no date, base, home, or collected value, so accepting a
// placeholder would add execution variability without adding capability.
func validateAuth(config *Config, source *Source, fail func(string, ...any) error) error {
	for index, argument := range source.Auth {
		if problem := executionTextProblem(argument); problem != "" {
			return fail("auth[%d] %s", index, problem)
		}
		names, err := placeholderNames(argument)
		if err != nil {
			return fail("auth[%d]: %v", index, err)
		}
		if len(names) > 0 {
			return fail("auth[%d] must be literal; auth probes accept no placeholders", index)
		}
		if index == 0 {
			if strings.TrimSpace(argument) == "" {
				return fail("auth[0] must be the executable to run")
			}
			if err := validateArgvExecutable(config, "auth", argument, fail); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateRun(config *Config, source *Source, fail func(string, ...any) error) error {
	if len(source.Run) == 0 {
		return fail("run is required and must contain an executable")
	}
	for index, argument := range source.Run {
		if problem := executionTextProblem(argument); problem != "" {
			return fail("run[%d] %s", index, problem)
		}
		names, err := placeholderNames(argument)
		if err != nil {
			return fail("run[%d]: %v", index, err)
		}
		if index == 0 {
			if strings.TrimSpace(argument) == "" {
				return fail("run[0] must be the executable to run")
			}
			if len(names) > 0 {
				return fail("run[0] must be a literal executable; placeholders are allowed only in arguments")
			}
			if err := validateArgvExecutable(config, "run", argument, fail); err != nil {
				return err
			}
		}
		for _, name := range names {
			if !contains(RunPlaceholders, name) {
				return fail("run[%d]: unknown placeholder {{%s}}; known placeholders are %s",
					index, name, strings.Join(RunPlaceholders, ", "))
			}
		}
	}
	return nil
}

func validateTest(config *Config, source *Source, fail func(string, ...any) error) error {
	for index, argument := range source.Test {
		if problem := executionTextProblem(argument); problem != "" {
			return fail("test[%d] %s", index, problem)
		}
		names, err := placeholderNames(argument)
		if err != nil {
			return fail("test[%d]: %v", index, err)
		}
		if index == 0 {
			if strings.TrimSpace(argument) == "" {
				return fail("test[0] must be the executable to run")
			}
			if len(names) > 0 {
				return fail("test[0] must be a literal executable; placeholders are allowed only in arguments")
			}
			if err := validateArgvExecutable(config, "test", argument, fail); err != nil {
				return err
			}
		}
		for _, name := range names {
			if !contains(TestPlaceholders, name) {
				return fail("test[%d]: unknown placeholder {{%s}}; known placeholders are %s",
					index, name, strings.Join(TestPlaceholders, ", "))
			}
		}
	}
	return nil
}

func validateRequirements(source *Source, fail func(string, ...any) error) error {
	seenRequirements := make(map[string]struct{}, len(source.Requires))
	for index, requirement := range source.Requires {
		if !requirementPattern.MatchString(requirement) {
			return fail("requires[%d] is %q; expected a bare executable name resolved through PATH", index, requirement)
		}
		if _, duplicate := seenRequirements[requirement]; duplicate {
			return fail("requires names %q more than once", requirement)
		}
		seenRequirements[requirement] = struct{}{}
	}
	return nil
}

func validateSourceFields(config *Config, source *Source, fail func(string, ...any) error) error {
	if source.Layer == LayerTasks {
		return nil
	}
	if err := ValidateFieldMap(source.Fields, source.Layer == LayerEvents); err != nil {
		return fail("%v", err)
	}
	for _, name := range source.Fields.Names() {
		if _, declared := config.Schema[name]; !declared {
			return fail("fields.%s is not declared in schema", name)
		}
	}
	source.Schema = config.Schema.Select(source.Fields)
	for _, name := range source.BodyFieldNames() {
		if !source.Schema[name].Cardinality.MaxOne() {
			return fail("body placeholder {{%s}} needs schema.%s.cardinality one or optional", name, name)
		}
	}
	return nil
}

func validateSourcePolicy(config *Config, source *Source, fail func(string, ...any) error) error {
	if source.Bodies == "" {
		source.Bodies = BodiesNone
	}
	if source.Bodies != BodiesNone && source.Bodies != BodiesCache && source.Bodies != BodiesSync {
		return fail("bodies is %q; expected none, cache, or sync", source.Bodies)
	}
	if source.Bodies != BodiesNone && !source.HasBody() {
		return fail("bodies is %s but body is not declared; caching cannot fetch this source", source.Bodies)
	}
	if source.Recency.HalfLifeDays < 0 || source.Recency.HalfLifeDays > MaxRecencyHalfLifeDays {
		return fail("recency.half_life_days is %d; expected 1..%d when declared",
			source.Recency.HalfLifeDays, MaxRecencyHalfLifeDays)
	}
	if source.Layer == LayerTasks && !source.Window {
		return fail("window must be true for a tasks source so one command selects completed sessions across the requested range")
	}
	if source.Window && source.Layer != LayerEvents && source.Layer != LayerTasks {
		return fail("window is true but layer is %s; a whole-range collection buckets by day only for events, "+
			"while tasks imports one completed-session range", source.Layer)
	}
	if source.Layer == LayerTasks && source.Format != FormatJSON {
		return fail("format is %s; a tasks source must emit one json array", source.Format)
	}
	if source.Timeout < 0 || source.Timeout > time.Hour {
		return fail("timeout is %s; expected 0 (inherit sync.timeout) up to 1h", source.Timeout)
	}
	layer := source.Layer
	if source.Enabled && !config.Layers[layer] {
		return fail("is enabled but layers.%s is false; enable the layer or disable the source", layer)
	}
	return nil
}

// validateCommandBin keeps mutable repository content inside the hashed <base>/bin boundary.
// Extra PATH entries are a machine-local execution boundary, like the inherited PATH: they
// must therefore be explicit absolute directories outside the repository, never a relative
// spelling or a symlink back into content a pull can replace without changing the trust digest.
func validateCommandBin(config *Config) error {
	for index, declared := range config.Bin {
		if problem := executionTextProblem(declared); problem != "" {
			return fmt.Errorf("%w: %s: bin[%d] %s", ErrConfig, config.Path, index, problem)
		}
		problem, err := machineLocalDirectoryProblem(config, declared)
		if err != nil {
			return fmt.Errorf("%w: %s: bin[%d]: %w", ErrConfig, config.Path, index, err)
		}
		if problem != "" {
			return fmt.Errorf("%w: %s: bin[%d] %s", ErrConfig, config.Path, index, problem)
		}
	}
	return nil
}

func machineLocalDirectoryProblem(config *Config, declared string) (string, error) {
	expanded := filepath.Clean(ExpandHome(strings.TrimSpace(declared)))
	if strings.TrimSpace(declared) == "" || !filepath.IsAbs(expanded) {
		return "must be an absolute or ~-relative directory outside the base", nil
	}
	root, err := filepath.Abs(filepath.Dir(config.Path))
	if err != nil {
		return "", fmt.Errorf("cannot resolve its base: %w", err)
	}
	absolute, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q: %w", declared, err)
	}
	if pathIsWithin(root, absolute) {
		return fmt.Sprintf("value %q resolves inside the base; put base-controlled executables in bin/ and keep extra PATH directories outside the base", declared), nil
	}
	resolvedRoot, err := resolveExistingPath(root)
	if err != nil {
		return "", fmt.Errorf("cannot inspect the base path: %w", err)
	}
	resolved, err := resolveExistingPath(absolute)
	if err != nil {
		return "", fmt.Errorf("cannot inspect %q: %w", declared, err)
	}
	if pathIsWithin(resolvedRoot, resolved) {
		return fmt.Sprintf("value %q resolves inside the base through a symlink; put base-controlled executables in bin/ and keep extra PATH directories outside the base", declared), nil
	}
	return "", nil
}

// executionTextProblem rejects bytes that cannot cross an exec boundary or can make a trust
// disclosure display different text from the execution definition it approves. Every argv,
// path, and retry matcher is one value and therefore admits no controls or invisible formats.
func executionTextProblem(value string) string {
	if !utf8.ValidString(value) {
		return "is not valid UTF-8"
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return fmt.Sprintf("contains control character U+%04X", char)
		}
		if unicode.Is(unicode.Cf, char) {
			return fmt.Sprintf("contains invisible format character U+%04X", char)
		}
	}
	return ""
}

// resolveExistingPath resolves every existing symlink in a path even when its final suffix
// does not exist yet. EvalSymlinks alone stops at the missing leaf, which would let a declared
// path pass through a symlinked parent into the base and become executable after a later pull.
func resolveExistingPath(path string) (string, error) {
	current := filepath.Clean(path)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func pathIsWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func validateBody(config *Config, source *Source, fail func(string, ...any) error) error {
	if len(source.Body) == 0 {
		return nil
	}
	if len(source.Body) < 2 {
		return fail("body must contain an executable and at least one argument")
	}
	sawID := false
	for index, element := range source.Body {
		elementHasID, err := validateBodyElement(config, source, index, element, fail)
		if err != nil {
			return err
		}
		sawID = sawID || elementHasID
	}
	if !sawID {
		return fail("body must name {{id}}: it fetches exactly one record")
	}
	if strings.TrimSpace(source.Body[0]) == "" {
		return fail("body[0] must be the executable to run")
	}
	return nil
}

func validateBodyElement(
	config *Config, source *Source, index int, element string, fail func(string, ...any) error,
) (bool, error) {
	if problem := executionTextProblem(element); problem != "" {
		return false, fail("body[%d] %s", index, problem)
	}
	names, err := placeholderNames(element)
	if err != nil {
		return false, fail("body[%d]: %v", index, err)
	}
	sawID := false
	for _, name := range names {
		placeholderIsID, err := validateBodyPlaceholder(source, index, name, fail)
		if err != nil {
			return false, err
		}
		sawID = sawID || placeholderIsID
	}
	if index == 0 {
		if err := validateArgvExecutable(config, "body", element, fail); err != nil {
			return false, err
		}
	}
	return sawID, nil
}

func validateBodyPlaceholder(
	source *Source, index int, name string, fail func(string, ...any) error,
) (bool, error) {
	_, field := source.Fields[name]
	static := contains(bodyStaticPlaceholders, name)
	if !field && !static {
		return false, fail("body[%d]: unknown placeholder {{%s}}; declare fields.%s or use base or home",
			index, name, name)
	}
	if index == 0 {
		if field && !static {
			return false, fail("body[0] must not use collected placeholder {{%s}}; the executable is trusted configuration", name)
		}
		return false, fail("body[0] must be a literal executable; placeholders are allowed only in arguments")
	}
	return name == FieldID, nil
}

func validateArgvExecutable(
	config *Config, label, executable string, fail func(string, ...any) error,
) error {
	if !strings.ContainsRune(executable, filepath.Separator) {
		if !requirementPattern.MatchString(executable) {
			return fail("%s[0] must be a bare executable name or an absolute machine-local path outside the base", label)
		}
		return nil
	}
	if !filepath.IsAbs(executable) {
		return fail("%s[0] must be a bare executable resolved on PATH or an absolute machine-local path outside the base", label)
	}
	problem, err := machineLocalDirectoryProblem(config, executable)
	if err != nil {
		return fail("%s[0] cannot be validated: %v", label, err)
	}
	if problem != "" {
		tree := BaseBinDir
		if label == "test" {
			tree = BaseTestsDir
		}
		return fail("%s[0] must resolve outside the base; put base-controlled code in %s/ and name it without a path", label, tree)
	}
	return nil
}

func placeholderNames(value string) ([]string, error) {
	// Catch every unmatched brace pair before the well-formed matcher runs, so `{{ Date }}`,
	// `{{date`, and `date}}` are mistakes rather than literals passed to a command.
	stripped := placeholderPattern.ReplaceAllString(value, "")
	if strings.Contains(stripped, "{{") || strings.Contains(stripped, "}}") {
		return nil, fmt.Errorf("malformed placeholder near %q; use {{name}} in lowercase", firstBraces(stripped))
	}
	matches := placeholderPattern.FindAllStringSubmatch(value, -1)
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, match[1])
	}
	return names, nil
}

func firstBraces(value string) string {
	index := strings.Index(value, "{{")
	closing := strings.Index(value, "}}")
	if index < 0 || (closing >= 0 && closing < index) {
		index = closing
	}
	end := min(index+24, len(value))
	return value[index:end]
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// ValidBodyValue reports whether collected data is safe to pass as one opaque argv value.
// Shell punctuation is ordinary data because body commands never use a shell. The boundary
// rejects options and response-file selectors, invalid UTF-8, controls and invisible format
// characters instead: those can change CLI interpretation or make reviewed text differ from
// the bytes that execute.
func ValidBodyValue(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	first, _ := utf8.DecodeRuneInString(value)
	if first == '-' || first == '@' {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return false
		}
	}
	return true
}
