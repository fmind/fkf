package core

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
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

// BodyPolicy controls the rebuildable local copy of a body without changing the durable
// evidence envelope. Fetching remains possible only when the source declares body argv.
type BodyPolicy string

const (
	BodiesNone  BodyPolicy = "none"
	BodiesCache BodyPolicy = "cache"
	BodiesSync  BodyPolicy = "sync"
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

// A requirement names an ordinary collection/body executable or external test dependency
// resolved through PATH. Base-controlled collection helpers stay in <base>/bin/; the test[0]
// entrypoint has its own readiness check against <base>/tests/.
var requirementPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// MaxSourceNameLength keeps `<source>.json` within the 255-byte filename-component limit
// shared by the supported Linux and macOS filesystems. Source names are ASCII by grammar, so
// the byte count is also the schema's character count.
const MaxSourceNameLength = 255 - len(".json")

// MaxBaseNameLength bounds the MCP server title and every fkf:// resource authority. Keeping
// it at a DNS-label-sized 63 bytes makes the generated connection instructions bounded while
// leaving ordinary personal and team names comfortably readable.
const MaxBaseNameLength = 63

// IdentityKind is the optional human classification of a declared canonical entity.
type IdentityKind string

const (
	IdentityPerson       IdentityKind = "person"
	IdentityOrganization IdentityKind = "organization"
	IdentityRepository   IdentityKind = "repository"
)

// Identity declares exact spellings that refer to one canonical graph entity. Alias values
// are data, never commands: they may be canonical entity URIs, emails, or provider logins.
type Identity struct {
	Canonical string       `json:"canonical" yaml:"canonical"`
	Aliases   []string     `json:"aliases" yaml:"aliases"`
	Kind      IdentityKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	Owner     bool         `json:"owner,omitempty" yaml:"owner,omitempty"`
}

// EffectiveKind returns an explicit kind, or the conventional kind encoded by a canonical
// URI scheme. Open entity schemes remain valid and simply have no inferred classification.
func (identity Identity) EffectiveKind() IdentityKind {
	if identity.Kind != "" {
		return identity.Kind
	}
	scheme, _, _ := strings.Cut(identity.Canonical, ":")
	switch scheme {
	case "person", "actor":
		return IdentityPerson
	case "organization", "org":
		return IdentityOrganization
	case "repository", "repo":
		return IdentityRepository
	default:
		return ""
	}
}

// MaxFreshnessAgeHours is ten years. Larger freshness windows are operationally equivalent
// to disabling refresh/staleness checks, while their conversion to time.Duration can wrap and
// invert the comparison. A finite shared bound keeps configuration and CLI arithmetic honest.
const MaxFreshnessAgeHours = 10 * 365 * 24

// RunPlaceholders are the only substitutions fkf makes into `run:` arguments. Every one is a
// value fkf itself computes; collected data never chooses an argument or executable.
var RunPlaceholders = []string{"date", "next_date", "start", "end", "base", "home"}

// TestPlaceholders are stable machine/base paths only. A source verification hook is independent
// of collection windows and stored values, so dates and record fields never enter its argv.
var TestPlaceholders = []string{"base", "home"}

// bodyStaticPlaceholders are the body substitutions that do not come from a record. Every
// declared field name is also available, which is safe because body is argv and never shell.
var bodyStaticPlaceholders = []string{"base", "home"}

var placeholderPattern = regexp.MustCompile(`\{\{([a-z][a-z0-9_-]*)\}\}`)

// Source is one declared collection command.
type Source struct {
	Name     string        `json:"name"`
	Enabled  bool          `json:"enabled"`
	Layer    Layer         `json:"layer"`
	Auth     []string      `json:"auth,omitempty"`
	Run      []string      `json:"run"`
	Test     []string      `json:"test,omitempty"`
	Format   OutputFormat  `json:"format"`
	Records  FieldPath     `json:"records,omitzero"`
	Fields   FieldMap      `json:"fields,omitempty"`
	Schema   FieldSchema   `json:"-"`
	Body     []string      `json:"body,omitempty"`
	Bodies   BodyPolicy    `json:"bodies,omitempty"`
	Recency  RecencyPolicy `json:"recency,omitzero"`
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

// RecencyPolicy controls the optional source-local exponential freshness modifier used by
// lexical retrieval. Undated records receive no bonus even when their source declares it.
type RecencyPolicy struct {
	HalfLifeDays int `json:"half_life_days,omitempty"`
}

// IsZero lets an omitted policy stay absent from resolved configuration JSON.
func (r RecencyPolicy) IsZero() bool { return r.HalfLifeDays == 0 }

// MaxRecencyHalfLifeDays keeps ranking arithmetic finite while allowing long-lived sources.
const MaxRecencyHalfLifeDays = 10 * 365

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

// CachesBodies reports whether this source may write the rebuildable body cache. The empty
// zero value is the same as the configured default none for synthetic in-memory sources.
func (s Source) CachesBodies() bool { return s.Bodies == BodiesCache || s.Bodies == BodiesSync }

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
	FKF        int                  `json:"fkf"`
	Name       string               `json:"name"`
	Schema     FieldSchema          `json:"schema"`
	Layers     map[Layer]bool       `json:"layers"`
	Identities map[string]*Identity `json:"identities,omitempty"`
	Sources    map[string]*Source   `json:"sources"`
	Sync       SyncConfig           `json:"sync"`
	Bin        []string             `json:"bin,omitempty"`

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
