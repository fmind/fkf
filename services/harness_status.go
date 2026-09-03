package services

import (
	"context"
	"fmt"
)

// HarnessRegistration is the read-only status of one complete managed integration for this
// base. Registered means its MCP, hook, and skills fragments all match exactly.
type HarnessRegistration struct {
	Name       string `json:"name"`
	Registered bool   `json:"registered"`
	Changes    int    `json:"changes,omitempty"`
	Error      string `json:"error,omitempty"`
}

// InspectHarnesses reads user-scope harness files without writing or requiring the base-owned
// assets to exist. A conflict is data in the report rather than a failure of `fkf status`.
func InspectHarnesses(ctx context.Context, baseRoot, home, executable string) ([]HarnessRegistration, error) {
	root, err := validateHarnessBase(baseRoot)
	if err != nil {
		return nil, err
	}
	home, err = harnessHome(home)
	if err != nil {
		return nil, err
	}
	executable, err = validateHarnessExecutable(executable)
	if err != nil {
		return nil, err
	}
	registrations := make([]HarnessRegistration, 0, len(harnessOrder))
	for _, name := range HarnessNames() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		files, links, inspectErr := preflightHarnessPlans(ctx, home, []*HarnessPlan{buildHarnessPlan(root, name, executable)})
		entry := HarnessRegistration{Name: name}
		if inspectErr != nil {
			entry.Error = fmt.Sprintf("%v", inspectErr)
			registrations = append(registrations, entry)
			continue
		}
		for _, file := range files {
			if file.changed {
				entry.Changes++
			}
		}
		for _, link := range links {
			if link.changed {
				entry.Changes++
			}
		}
		entry.Registered = entry.Changes == 0
		registrations = append(registrations, entry)
	}
	return registrations, nil
}
