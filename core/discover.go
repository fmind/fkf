package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// BaseEnvVar is the only process-wide input fkf reads. There is deliberately no global
// configuration file: a base carries its own definition, so the environment only has to
// answer "which base", never "configured how".
const BaseEnvVar = "FKF_BASE"

// ErrNoBase reports that none of the three discovery rules found a base.
var ErrNoBase = errors.New("no fkf base found")

// BaseOrigin records which rule selected the base, so `fkf config` and every "no base"
// diagnostic can say where the answer came from rather than leaving the user to guess.
type BaseOrigin string

const (
	BaseFromFlag        BaseOrigin = "flag"
	BaseFromEnvironment BaseOrigin = "environment"
	BaseFromDiscovery   BaseOrigin = "discovery"
)

// DiscoverBase resolves the base root from an explicit path, then the environment, then by
// walking up from the working directory to the nearest directory holding fkf.yaml — the way
// git finds its repository. A base is never created implicitly, so a miss is an error with
// all three rules named rather than a silent fallback to the working directory.
func DiscoverBase(explicit string) (string, BaseOrigin, error) {
	if expanded := ExpandHome(explicit); expanded != "" {
		absolute, err := ResolveAbsolutePath(expanded)
		if err != nil {
			return "", "", fmt.Errorf("%w: resolve --base %q: %w", ErrNoBase, explicit, err)
		}
		return absolute, BaseFromFlag, nil
	}
	if expanded := ExpandHome(os.Getenv(BaseEnvVar)); expanded != "" {
		absolute, err := ResolveAbsolutePath(expanded)
		if err != nil {
			return "", "", fmt.Errorf("%w: resolve %s=%q: %w", ErrNoBase, BaseEnvVar, os.Getenv(BaseEnvVar), err)
		}
		return absolute, BaseFromEnvironment, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve the working directory: %w", ErrNoBase, err)
	}
	for dir := filepath.Clean(wd); ; {
		if info, statErr := os.Stat(filepath.Join(dir, ConfigFileName)); statErr == nil && info.Mode().IsRegular() {
			return dir, BaseFromDiscovery, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", "", fmt.Errorf(
		"%w: pass --base <path>, export %s, or run from inside a base (a directory holding %s, found by walking up from %s)",
		ErrNoBase, BaseEnvVar, ConfigFileName, wd,
	)
}
