package core

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// The base is the configuration. `<base>/fkf.yaml` is committed, so a team's sources travel
// with the clone; `<base>/fkf.local.yaml` is gitignored and holds the handful of facts that
// are true of one machine. There is no global file, no profile, and no bundle: what a base
// collects is what its own configuration enables, and that is also the disclosure boundary.
//
// Decoding is strict in both directions. An unknown key fails, and so does a typo inside a
// DISABLED source — the file has to stay trustworthy as documentation, and a source nobody
// has enabled yet is exactly the one whose mistakes go unnoticed.

// ErrConfig marks every load and validation failure, so the CLI can map the whole class to
// exit code 2 without inspecting messages.
var ErrConfig = errors.New("invalid configuration")

// A source declares the layer it files into, and the value IS the layer name: one word for
// one decision, so nothing has to translate between a source kind and a destination.

// OutputFormat is how a source's stdout is decoded.
type OutputFormat string

const (
	// FormatJSON expects one JSON document: an array of records, or an object holding them
	// at `records:`. Empty stdout is an error, because a CLI that prints JSON prints `[]`.
	FormatJSON OutputFormat = "json"
	// FormatNDJSON expects one JSON value per line. Empty stdout is an empty day, because a
	// paginating CLI legitimately prints nothing when a day held nothing.
	FormatNDJSON OutputFormat = "ndjson"
)

// sourceNamePattern keeps a source name usable as a filename and as a URI segment. It is
// enforced rather than escaped: `<provider>-<resource>` is the whole convention, and a name
// needing an escape is a name the owner should change.
var sourceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateSourceName keeps every configured and stored source usable as the same flat JSON
// filename and URI segment. Stored evidence crosses this boundary again because it may have
// been hand-edited after collection.
func ValidateSourceName(name string) error {
	if !sourceNamePattern.MatchString(name) {
		return errors.New("name must be lowercase letters, digits, and hyphens (for example github-pull-requests)")
	}
	if len(name) > MaxSourceNameLength {
		return fmt.Errorf("name is %d bytes; expected at most %d bytes so <source>.json fits in one filename",
			len(name), MaxSourceNameLength)
	}
	return nil
}

// A requirement names an executable resolved through PATH. Paths belong in bin:, while
// base-controlled helpers stay in <base>/bin/ and are named like any other executable.
var requirementPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// MaxSourceNameLength keeps `<source>.json` within the 255-byte filename-component limit
// shared by the supported Linux and macOS filesystems. Source names are ASCII by grammar, so
// the byte count is also the schema's character count.
const MaxSourceNameLength = 255 - len(".json")

// MaxBaseNameLength bounds the MCP server title and every fkf:// resource authority. Keeping
// it at a DNS-label-sized 63 bytes makes the generated connection instructions bounded while
// leaving ordinary personal and team names comfortably readable.
const MaxBaseNameLength = 63

// MaxFreshnessAgeHours is ten years. Larger freshness windows are operationally equivalent
// to disabling refresh/staleness checks, while their conversion to time.Duration can wrap and
// invert the comparison. A finite shared bound keeps configuration and CLI arithmetic honest.
const MaxFreshnessAgeHours = 10 * 365 * 24

// RunPlaceholders are the only substitutions fkf makes into `run:` arguments. Every one is a
// value fkf itself computes; collected data never chooses an argument or executable.
var RunPlaceholders = []string{"date", "next_date", "start", "end", "base", "home"}

// bodyStaticPlaceholders are the body substitutions that do not come from a record. Every
// declared field name is also available, which is safe because body is argv and never shell.
var bodyStaticPlaceholders = []string{"base", "home"}

// datePlaceholders are meaningless for an index source, which collects a point in time
// rather than a day. Naming one is a load error rather than a silent empty substitution.
var datePlaceholders = []string{"date", "next_date", "start", "end"}

var placeholderPattern = regexp.MustCompile(`\{\{([a-z][a-z0-9_-]*)\}\}`)

// Source is one declared collection command.
type Source struct {
	Name     string        `json:"name"`
	Enabled  bool          `json:"enabled"`
	Layer    Layer         `json:"layer"`
	Run      []string      `json:"run"`
	Format   OutputFormat  `json:"format"`
	Records  FieldPath     `json:"records,omitzero"`
	Fields   FieldMap      `json:"fields,omitempty"`
	Schema   FieldSchema   `json:"-"`
	Body     []string      `json:"body,omitempty"`
	Requires []string      `json:"requires,omitempty"`
	Install  string        `json:"install,omitempty"`
	Timeout  time.Duration `json:"timeout,omitempty"`
	// Retry and MinInterval declare HOW fkf invokes the command, never what it is, which is
	// exactly the relationship `timeout:` already has to `run:`. They exist because the
	// alternative is shell: a rate-limited provider drove a real base to wrap `gh search` in a
	// hand-written script that sleeps until the limit resets, which moved a retry loop out of
	// tested Go and into the one surface a human has to re-read on every trust.
	Retry       RetryPolicy   `json:"retry,omitzero"`
	MinInterval time.Duration `json:"min_interval,omitempty"`
	// Window asks `fkf sync` to render this source's `run:` ONCE for the whole requested
	// range — {{start}}/{{end}} span every day being collected, not one — and bucket the
	// records it returns into one document per day by each record's declared `fields.time`.
	//
	// It exists because a day's worth of work is not what most sources actually cost: a local
	// script's fixed overhead (a filesystem scan, a process start) repeats on every day a
	// day-at-a-time sync asks for, and a paginating search API charges one call PER DAY
	// against a rate limit that counts calls, not days. `window:` collects what `run:`
	// already returns for a wider range in one call instead of many.
	Window bool `json:"window,omitempty"`
}

// RetryPolicy is a source's declared back-pressure. Attempts counts the total, so 1 is the
// default "run it once" and needs no key.
type RetryPolicy struct {
	Attempts int           `json:"attempts,omitempty"`
	Backoff  time.Duration `json:"backoff,omitempty"`
	// On is what may be retried, and it is required whenever Attempts exceeds one. Retrying
	// anything is how a source that is failing for a real reason turns into a source that
	// hammers a provider quietly: a declared list makes the reviewer say which failure is
	// transient. An entry is `exit:<n>` or a substring matched against the command's stderr.
	On []string `json:"on,omitempty"`
}

// IsZero lets the policy be omitted from JSON when nothing is declared.
func (r RetryPolicy) IsZero() bool { return r.Attempts == 0 && r.Backoff == 0 && len(r.On) == 0 }

// RetryAttempts is the total number of runs this source allows, never fewer than one.
func (s Source) RetryAttempts() int { return max(1, s.Retry.Attempts) }

// MaxRetryAttempts bounds the declared attempt count. A source that may run ten times is not
// declaring back-pressure, it is declaring a loop.
const MaxRetryAttempts = 5

// MaxSyncConcurrency bounds simultaneous provider processes. Each process owns separately
// bounded stdout and stderr buffers, so a small ceiling contains aggregate memory use.
const MaxSyncConcurrency = 4

// MaxRetryBackoff bounds one wait between attempts, and MaxMinInterval bounds the pacing gap.
// Both sit under the collection timeout's own ceiling for the same reason it has one.
const (
	MaxRetryBackoff = 10 * time.Minute
	MaxMinInterval  = 10 * time.Minute
)

// HasBody reports whether this source can fetch one record's body on demand.
func (s Source) HasBody() bool { return len(s.Body) > 0 }

// BodyFieldNames returns the declared record fields that select body argv values. Static base
// and home placeholders keep their execution meaning even when the open field map uses the
// same name, so they are deliberately absent here.
func (s Source) BodyFieldNames() []string {
	var names []string
	for _, name := range s.Fields.Names() {
		if contains(bodyStaticPlaceholders, name) {
			continue
		}
		placeholder := "{{" + name + "}}"
		for _, argument := range s.Body {
			if strings.Contains(argument, placeholder) {
				names = append(names, name)
				break
			}
		}
	}
	return names
}

// SyncConfig tunes collection. Every key has a visible default in the file `fkf init` writes.
type SyncConfig struct {
	Days             int           `json:"days"`
	IndexMaxAgeHours int           `json:"index_max_age_hours"`
	Timeout          time.Duration `json:"timeout"`
	Concurrency      int           `json:"concurrency"`
}

// DefaultSync is what an omitted `sync:` block means, and what `fkf init` writes verbatim.
func DefaultSync() SyncConfig {
	return SyncConfig{Days: 30, IndexMaxAgeHours: 168, Timeout: 120 * time.Second, Concurrency: 4}
}

// Config is one base's complete, resolved definition.
type Config struct {
	FKF     int                `json:"fkf"`
	Name    string             `json:"name"`
	Schema  FieldSchema        `json:"schema"`
	Layers  map[Layer]bool     `json:"layers"`
	Sources map[string]*Source `json:"sources"`
	Sync    SyncConfig         `json:"sync"`
	Bin     []string           `json:"bin,omitempty"`

	Path      string            `json:"path"`
	LocalPath string            `json:"local_path,omitempty"`
	Origins   map[string]string `json:"origins,omitempty"`
}

// SourceNames returns the declared source names in stable order, which is the order every
// report, `status` table, and `--dry-run` listing uses.
func (c *Config) SourceNames() []string {
	names := make([]string, 0, len(c.Sources))
	for name := range c.Sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// EnabledSources returns the enabled sources in stable order.
func (c *Config) EnabledSources() []*Source {
	enabled := make([]*Source, 0, len(c.Sources))
	for _, name := range c.SourceNames() {
		if c.Sources[name].Enabled {
			enabled = append(enabled, c.Sources[name])
		}
	}
	return enabled
}

// Store returns the layout this configuration describes.
func (c *Config) Store() Store {
	return NewStore(filepath.Dir(c.Path), c.Layers)
}

// --- file shapes -------------------------------------------------------------------------

type fileConfig struct {
	FKF     int                   `yaml:"fkf"`
	Name    string                `yaml:"name"`
	Schema  FieldSchema           `yaml:"schema"`
	Layers  map[string]bool       `yaml:"layers"`
	Bin     []string              `yaml:"bin"`
	Sources map[string]fileSource `yaml:"sources"`
	Sync    *fileSync             `yaml:"sync"`
}

type fileSource struct {
	Enabled     bool       `yaml:"enabled"`
	Layer       string     `yaml:"layer"`
	Run         *[]string  `yaml:"run"`
	Format      string     `yaml:"format"`
	Records     string     `yaml:"records"`
	Fields      FieldMap   `yaml:"fields"`
	Body        []string   `yaml:"body"`
	Requires    []string   `yaml:"requires"`
	Install     string     `yaml:"install"`
	Timeout     string     `yaml:"timeout"`
	Retry       *fileRetry `yaml:"retry"`
	MinInterval string     `yaml:"min_interval"`
	Window      bool       `yaml:"window"`
}

type fileRetry struct {
	Attempts int      `yaml:"attempts"`
	Backoff  string   `yaml:"backoff"`
	On       []string `yaml:"on"`
}

type fileSync struct {
	Days             *int    `yaml:"days"`
	IndexMaxAgeHours *int    `yaml:"index_max_age_hours"`
	Timeout          *string `yaml:"timeout"`
	Concurrency      *int    `yaml:"concurrency"`
}

type fileLocal struct {
	Bin     []string                   `yaml:"bin"`
	Sources map[string]fileLocalSource `yaml:"sources"`
}

type fileLocalSource struct {
	Enabled *bool     `yaml:"enabled"`
	Run     *[]string `yaml:"run"`
	Timeout *string   `yaml:"timeout"`
}

// --- loading -----------------------------------------------------------------------------

// LoadConfig reads a base's committed configuration and, when present, its machine-local
// overlay. Both are decoded strictly; the resolved value of every overridden key records
// which file it came from, so `fkf config` can show the merge rather than just its result.
func LoadConfig(root string) (*Config, error) {
	store, err := configStore(root)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve base path %q: %w", ErrConfig, root, err)
	}
	config, err := decodeConfigFile(store)
	if err != nil {
		return nil, err
	}
	config.Path = store.ConfigPath()
	if err := applyLocalOverlay(config, store); err != nil {
		return nil, err
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return config, nil
}

func decodeConfigFile(store Store) (*Config, error) {
	path := store.ConfigPath()
	data, exists, err := readConfigLeaf(store, ConfigFileName)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s does not exist; run `fkf init %s` to create a base",
			ErrConfig, path, filepath.Dir(path))
	}
	var file fileConfig
	if err := decodeStrict(data, &file, path); err != nil {
		return nil, err
	}
	return buildConfig(&file, path)
}

// decodeStrict rejects an unknown key rather than ignoring it, because a misspelled key in a
// file that declares what runs is a command that silently does not run.
func decodeStrict(data []byte, into any, path string) error {
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrConfig, path, err)
	}
	// A second Decode returning anything but io.EOF means the file holds a second YAML
	// document, which would otherwise be silently ignored along with everything in it.
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("%w: %s holds more than one YAML document", ErrConfig, path)
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: %s has invalid trailing YAML: %w", ErrConfig, path, err)
	}
	return nil
}

func buildConfig(file *fileConfig, path string) (*Config, error) {
	config := &Config{
		FKF:     file.FKF,
		Name:    strings.TrimSpace(file.Name),
		Schema:  file.Schema,
		Layers:  make(map[Layer]bool, len(Layers)),
		Sources: make(map[string]*Source, len(file.Sources)),
		Sync:    DefaultSync(),
		Bin:     file.Bin,
		Origins: map[string]string{},
	}
	for name, enabled := range file.Layers {
		layer, err := ParseLayer(name)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: layers: %w", ErrConfig, path, err)
		}
		config.Layers[layer] = enabled
	}
	if file.Sync != nil {
		if err := applySync(&config.Sync, file.Sync, path); err != nil {
			return nil, err
		}
	}
	for name, entry := range file.Sources {
		source, err := buildSource(name, entry, path)
		if err != nil {
			return nil, err
		}
		config.Sources[name] = source
	}
	return config, nil
}

// applyRetry decodes a source's declared back-pressure. `on:` is required whenever more than
// one attempt is allowed, because retry-anything is how a source failing for a real reason
// becomes a source hammering a provider quietly — the reviewer has to say which failure is
// transient, and `fkf trust` prints the answer beside the command it modifies.
func applyRetry(into *RetryPolicy, file *fileRetry, fail func(string, ...any) (*Source, error)) error {
	into.Attempts = file.Attempts
	into.On = file.On
	if file.Backoff != "" {
		backoff, err := time.ParseDuration(strings.TrimSpace(file.Backoff))
		if err != nil {
			_, wrapped := fail("retry.backoff: %v", err)
			return wrapped
		}
		if backoff < 0 || backoff > MaxRetryBackoff {
			_, wrapped := fail("retry.backoff is %s; expected a duration up to %s", backoff, MaxRetryBackoff)
			return wrapped
		}
		into.Backoff = backoff
	}
	switch {
	case into.Attempts == 0 && (into.Backoff != 0 || len(into.On) > 0):
		_, wrapped := fail("retry declares backoff or on but no attempts, so nothing would ever be retried")
		return wrapped
	case into.Attempts < 0 || into.Attempts > MaxRetryAttempts:
		_, wrapped := fail("retry.attempts is %d; expected 1..%d", into.Attempts, MaxRetryAttempts)
		return wrapped
	case into.Attempts > 1 && len(into.On) == 0:
		_, wrapped := fail("retry.attempts is %d but retry.on is empty; name the failures that may be "+
			"retried (`exit:<n>` or a stderr substring) rather than retrying every failure", into.Attempts)
		return wrapped
	}
	for _, condition := range into.On {
		if strings.TrimSpace(condition) == "" {
			_, wrapped := fail("retry.on holds an empty condition")
			return wrapped
		}
		if problem := executionTextProblem(condition); problem != "" {
			_, wrapped := fail("retry.on %q %s", condition, problem)
			return wrapped
		}
		if code, found := strings.CutPrefix(condition, "exit:"); found {
			if _, err := strconv.Atoi(strings.TrimSpace(code)); err != nil {
				_, wrapped := fail("retry.on %q: %v", condition, err)
				return wrapped
			}
		}
	}
	return nil
}

func applySync(into *SyncConfig, file *fileSync, path string) error {
	if file.Days != nil {
		into.Days = *file.Days
	}
	if file.IndexMaxAgeHours != nil {
		into.IndexMaxAgeHours = *file.IndexMaxAgeHours
	}
	if file.Concurrency != nil {
		into.Concurrency = *file.Concurrency
	}
	if file.Timeout != nil {
		timeout, err := time.ParseDuration(strings.TrimSpace(*file.Timeout))
		if err != nil {
			return fmt.Errorf("%w: %s: sync.timeout: %w", ErrConfig, path, err)
		}
		into.Timeout = timeout
	}
	return nil
}

func buildSource(name string, file fileSource, path string) (*Source, error) {
	fail := func(format string, args ...any) (*Source, error) {
		return nil, fmt.Errorf("%w: %s: sources.%s: %s", ErrConfig, path, name, fmt.Sprintf(format, args...))
	}
	if err := ValidateSourceName(name); err != nil {
		return fail("%v", err)
	}
	if file.Run == nil || len(*file.Run) == 0 {
		return fail("run is required and must contain an executable")
	}
	source := &Source{
		Name: name, Enabled: file.Enabled, Layer: LayerEvents, Format: FormatJSON,
		Run: append([]string(nil), (*file.Run)...), Body: file.Body, Requires: append([]string(nil), file.Requires...),
		Install: strings.TrimSpace(file.Install), Window: file.Window, Fields: file.Fields,
	}
	if layer := strings.TrimSpace(file.Layer); layer != "" {
		if Layer(layer) != LayerEvents && Layer(layer) != LayerIndex {
			return fail("layer is %q; expected %s or %s", layer, LayerEvents, LayerIndex)
		}
		source.Layer = Layer(layer)
	}
	if format := strings.TrimSpace(file.Format); format != "" {
		if OutputFormat(format) != FormatJSON && OutputFormat(format) != FormatNDJSON {
			return fail("format is %q; expected json or ndjson", format)
		}
		source.Format = OutputFormat(format)
	}
	if file.Timeout != "" {
		timeout, err := time.ParseDuration(strings.TrimSpace(file.Timeout))
		if err != nil {
			return fail("timeout: %v", err)
		}
		source.Timeout = timeout
	}
	if file.MinInterval != "" {
		interval, err := time.ParseDuration(strings.TrimSpace(file.MinInterval))
		if err != nil {
			return fail("min_interval: %v", err)
		}
		if interval <= 0 || interval > MaxMinInterval {
			return fail("min_interval is %s; expected a positive duration up to %s", interval, MaxMinInterval)
		}
		source.MinInterval = interval
	}
	if file.Retry != nil {
		if err := applyRetry(&source.Retry, file.Retry, fail); err != nil {
			return nil, err
		}
	}
	if err := applySourcePaths(source, file, fail); err != nil {
		return nil, err
	}
	return source, nil
}

func applySourcePaths(source *Source, file fileSource, fail func(string, ...any) (*Source, error)) error {
	if strings.TrimSpace(file.Records) != "" {
		parsed, err := ParseFieldPath(file.Records)
		if err != nil {
			_, wrapped := fail("records: %v", err)
			return wrapped
		}
		source.Records = parsed
	}
	return nil
}

func applyLocalOverlay(config *Config, store Store) error {
	path := store.LocalConfigPath()
	data, exists, err := readConfigLeaf(store, LocalConfigName)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrConfig, err)
	}
	if !exists {
		return nil
	}
	var local fileLocal
	if err := decodeStrict(data, &local, path); err != nil {
		return err
	}
	config.LocalPath = path
	if len(local.Bin) > 0 {
		config.Bin = append(config.Bin, local.Bin...)
		config.Origins["bin"] = path
	}
	for name, override := range local.Sources {
		source, declared := config.Sources[name]
		if !declared {
			return fmt.Errorf("%w: %s: sources.%s is not declared in %s; the local overlay may only override a declared source",
				ErrConfig, path, name, ConfigFileName)
		}
		if override.Enabled != nil {
			source.Enabled = *override.Enabled
			config.Origins["sources."+name+".enabled"] = path
		}
		if override.Run != nil {
			source.Run = append([]string(nil), (*override.Run)...)
			config.Origins["sources."+name+".run"] = path
		}
		if override.Timeout != nil {
			timeout, err := time.ParseDuration(strings.TrimSpace(*override.Timeout))
			if err != nil {
				return fmt.Errorf("%w: %s: sources.%s.timeout: %w", ErrConfig, path, name, err)
			}
			source.Timeout = timeout
			config.Origins["sources."+name+".timeout"] = path
		}
	}
	return nil
}

// --- validation --------------------------------------------------------------------------

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
	if err := validateRequirements(source, fail); err != nil {
		return err
	}
	if err := validateSourceFields(config, source, fail); err != nil {
		return err
	}
	if err := validateSourcePolicy(config, source, fail); err != nil {
		return err
	}
	return validateBody(config, source, fail)
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
			if source.Layer == LayerIndex && contains(datePlaceholders, name) {
				return fail("run[%d]: {{%s}} is a date placeholder and an index source collects a point in time, not a day", index, name)
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
	if source.Window && source.Layer != LayerEvents {
		return fail("window is true but layer is %s; a whole-range collection buckets by day, "+
			"which only an events source has", source.Layer)
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
		return fail("%s[0] must resolve outside the base; put base-controlled helpers in bin/ and name them without a path", label)
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
// rejects options, invalid UTF-8, controls and invisible format characters instead: those can
// change CLI interpretation or make reviewed text differ from the bytes that execute.
func ValidBodyValue(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	first, _ := utf8.DecodeRuneInString(value)
	if first == '-' {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return false
		}
	}
	return true
}
