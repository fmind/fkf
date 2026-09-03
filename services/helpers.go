package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	fkf "github.com/fmind/fkf"
	"github.com/fmind/fkf/core"
)

// HelperState compares one official helper with the exact bytes embedded in this binary.
type HelperState string

const (
	HelperCurrent HelperState = "current"
	HelperDrifted HelperState = "drifted"
	HelperMissing HelperState = "missing"
)

// HelperStatus is one shipped helper required by this base's enabled execution plan.
type HelperStatus struct {
	Name          string      `json:"name"`
	Path          string      `json:"path"`
	State         HelperState `json:"state"`
	Required      bool        `json:"required"`
	CurrentSHA256 string      `json:"current_sha256,omitempty"`
	ShippedSHA256 string      `json:"shipped_sha256"`
	Refreshed     bool        `json:"refreshed,omitempty"`
}

// HelperReport is the explicit diff/refresh result for official helper scripts. Custom
// executables are deliberately outside it and are never modified.
type HelperReport struct {
	Base      string         `json:"base"`
	Helpers   []HelperStatus `json:"helpers"`
	Current   int            `json:"current"`
	Drifted   int            `json:"drifted"`
	Missing   int            `json:"missing"`
	Refreshed int            `json:"refreshed"`
}

// InspectHelpers compares required official helper names with this binary and, only when refresh is
// explicit, restores drifted installed helpers and missing helpers required by the current
// configuration through per-file atomic replacements. Unknown scripts are user-owned and
// invisible to this operation.
func InspectHelpers(ctx context.Context, base *Base, refresh bool) (*HelperReport, error) {
	shipped, err := shippedHelpers()
	if err != nil {
		return nil, err
	}
	report, err := inspectShippedHelpers(ctx, base, shipped, requiredHelpers(base, shipped))
	if err != nil {
		return nil, err
	}
	// Inspect every official target before changing any of them. A later symlink or special
	// file must not leave earlier helpers silently refreshed by an otherwise failed command.
	if refresh {
		if err := refreshHelpers(ctx, base, report, shipped); err != nil {
			return nil, err
		}
	}
	summarizeHelpers(report)
	return report, nil
}

func requiredHelpers(base *Base, shipped map[string][]byte) map[string]bool {
	return requiredHelpersForConfig(base.Config, shipped)
}

func requiredHelpersForConfig(config *core.Config, shipped map[string][]byte) map[string]bool {
	required := map[string]bool{hookScript: true}
	if config == nil {
		return required
	}
	for _, source := range config.EnabledSources() {
		for _, requirement := range source.Requires {
			if _, official := shipped[requirement]; official {
				required[requirement] = true
			}
		}
	}
	return required
}

// installMissingRequiredHelpers materializes only the official helpers the enabled execution
// plan can call. Disabled examples stay declarative until the owner enables and explicitly
// refreshes their helpers; existing files remain owner-controlled and are never overwritten.
func installMissingRequiredHelpers(root string, config *core.Config) ([]string, error) {
	shipped, err := shippedHelpers()
	if err != nil {
		return nil, err
	}
	required := requiredHelpersForConfig(config, shipped)
	names := make([]string, 0, len(required))
	for name := range required {
		names = append(names, name)
	}
	sort.Strings(names)

	written := make([]string, 0, len(names))
	for _, name := range names {
		target := filepath.Join(root, core.BaseBinDir, name)
		if err := core.ValidatePathConfinement(target); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(target); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect %s: %w", target, err)
		}
		if err := core.WriteFileAtomicMode(target, shipped[name], 0o700); err != nil {
			return nil, fmt.Errorf("write %s: %w", target, err)
		}
		written = append(written, name)
	}
	return written, nil
}

func inspectShippedHelpers(
	ctx context.Context, base *Base, shipped map[string][]byte, required map[string]bool,
) (*HelperReport, error) {
	// Validate the directory once before Lstat or reads. Lstat refuses a symlink leaf but follows
	// symlinked parents; without this preflight a base/bin alias could expose outside helpers.
	if err := core.ValidateWithinRoot(base.Root(), base.Store.BinDir()); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(shipped))
	for name := range shipped {
		names = append(names, name)
	}
	sort.Strings(names)
	report := &HelperReport{Base: base.Root(), Helpers: []HelperStatus{}}
	for _, name := range names {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		status, include, err := inspectHelper(ctx, base, name, shipped[name], required[name])
		if err != nil {
			return nil, err
		}
		if include {
			report.Helpers = append(report.Helpers, status)
		}
	}
	return report, nil
}

func refreshHelpers(
	ctx context.Context, base *Base, report *HelperReport, shipped map[string][]byte,
) error {
	for index := range report.Helpers {
		if err := checkContext(ctx); err != nil {
			return err
		}
		status := &report.Helpers[index]
		if status.State == HelperCurrent {
			continue
		}
		if err := refreshHelper(base, status, shipped[status.Name]); err != nil {
			return err
		}
	}
	return nil
}

func summarizeHelpers(report *HelperReport) {
	for _, status := range report.Helpers {
		if status.Refreshed {
			report.Refreshed++
		}
		switch status.State {
		case HelperCurrent:
			report.Current++
		case HelperDrifted:
			report.Drifted++
		case HelperMissing:
			report.Missing++
		}
	}
}

func shippedHelpers() (map[string][]byte, error) {
	entries, err := fs.ReadDir(fkf.Presets, path.Join("presets", core.BaseBinDir))
	if err != nil {
		return nil, fmt.Errorf("list embedded helpers: %w", err)
	}
	shipped := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := fs.ReadFile(fkf.Presets, path.Join("presets", core.BaseBinDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read embedded helper %s: %w", entry.Name(), err)
		}
		shipped[entry.Name()] = body
	}
	if _, ok := shipped[hookScript]; !ok {
		return nil, fmt.Errorf("embedded helper %s is missing", hookScript)
	}
	return shipped, nil
}

func inspectHelper(
	ctx context.Context, base *Base, name string, shipped []byte, required bool,
) (HelperStatus, bool, error) {
	relative := path.Join(core.BaseBinDir, name)
	absolute := filepath.Join(base.Root(), filepath.FromSlash(relative))
	status := HelperStatus{
		Name: name, Path: relative, Required: required, State: HelperMissing,
		ShippedSHA256: helperDigest(shipped),
	}
	if !required {
		return status, false, nil
	}
	info, err := os.Lstat(absolute)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return status, false, fmt.Errorf("inspect %s: %w", relative, err)
	case info.Mode()&os.ModeSymlink != 0:
		return status, false, fmt.Errorf("refusing helper symlink %s", relative)
	case !info.Mode().IsRegular():
		return status, false, fmt.Errorf("helper %s is not a regular file", relative)
	default:
		current, err := core.ReadFileLimitContext(ctx, absolute, core.MaxControlFileBytes)
		if err != nil {
			return status, false, fmt.Errorf("read %s: %w", relative, err)
		}
		status.CurrentSHA256 = helperDigest(current)
		// Helper drift is a byte contract. Repository permissions have their own status
		// finding and repair recipe, so a chmod must not masquerade as authored content drift.
		if bytes.Equal(current, shipped) {
			status.State = HelperCurrent
		} else {
			status.State = HelperDrifted
		}
	}
	return status, true, nil
}

func refreshHelper(base *Base, status *HelperStatus, shipped []byte) error {
	absolute := filepath.Join(base.Root(), filepath.FromSlash(status.Path))
	if err := core.ValidateWithinRoot(base.Root(), absolute); err != nil {
		return err
	}
	if err := core.WriteFileAtomicMode(absolute, shipped, 0o700); err != nil {
		return fmt.Errorf("refresh %s: %w", status.Path, err)
	}
	status.State, status.CurrentSHA256, status.Refreshed = HelperCurrent, status.ShippedSHA256, true
	return nil
}

func helperDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
