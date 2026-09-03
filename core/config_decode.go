package core

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// --- file shapes -------------------------------------------------------------------------

type fileConfig struct {
	FKF        int                     `yaml:"fkf"`
	Name       string                  `yaml:"name"`
	Schema     FieldSchema             `yaml:"schema"`
	Layers     map[string]bool         `yaml:"layers"`
	Identities map[string]fileIdentity `yaml:"identities"`
	Bin        []string                `yaml:"bin"`
	Sources    map[string]fileSource   `yaml:"sources"`
	Sync       *fileSync               `yaml:"sync"`
}

type fileIdentity struct {
	Canonical string       `yaml:"canonical"`
	Aliases   *[]string    `yaml:"aliases"`
	Kind      IdentityKind `yaml:"kind"`
	Owner     bool         `yaml:"owner"`
}

type fileSource struct {
	Enabled     bool         `yaml:"enabled"`
	Layer       string       `yaml:"layer"`
	Auth        *[]string    `yaml:"auth"`
	Run         *[]string    `yaml:"run"`
	Test        *[]string    `yaml:"test"`
	Format      string       `yaml:"format"`
	Records     string       `yaml:"records"`
	Fields      FieldMap     `yaml:"fields"`
	Body        []string     `yaml:"body"`
	Bodies      string       `yaml:"bodies"`
	Recency     *fileRecency `yaml:"recency"`
	Requires    []string     `yaml:"requires"`
	Install     string       `yaml:"install"`
	Timeout     string       `yaml:"timeout"`
	Retry       *fileRetry   `yaml:"retry"`
	MinInterval string       `yaml:"min_interval"`
	Window      bool         `yaml:"window"`
}

type fileRecency struct {
	HalfLifeDays int `yaml:"half_life_days"`
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
		FKF: file.FKF, Name: strings.TrimSpace(file.Name), Schema: file.Schema,
		Layers: make(map[Layer]bool, len(Layers)), Identities: make(map[string]*Identity, len(file.Identities)),
		Sources: make(map[string]*Source, len(file.Sources)), Sync: DefaultSync(),
		Bin: file.Bin, Origins: map[string]string{},
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
	for name, declaration := range file.Identities {
		aliases := []string(nil)
		if declaration.Aliases != nil {
			aliases = append(aliases, (*declaration.Aliases)...)
		}
		config.Identities[name] = &Identity{
			Canonical: strings.TrimSpace(declaration.Canonical), Aliases: aliases,
			Kind: declaration.Kind, Owner: declaration.Owner,
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
