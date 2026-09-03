package core

import (
	"strings"
	"testing"
)

func TestLoadConfigRejectsMachineLocalIdentities(t *testing.T) {
	configText := strings.Replace(minimalConfig, "name: brain\n", `name: brain
identities:
  fmind:
    canonical: person:email/fmind@example.com
    aliases: [fmind, actor:github.com/fmind]
    kind: person
    owner: true
`, 1)
	_, err := LoadConfig(writeBase(t, configText, map[string]string{
		LocalConfigName: "identities:\n  fmind:\n    aliases: [fmind@work.example]\n",
	}))
	if err == nil || !strings.Contains(err.Error(), "field identities not found") {
		t.Fatalf("LoadConfig() error = %v, want the local identity map rejected", err)
	}
}

func TestLoadConfigRejectsInvalidIdentityDeclarations(t *testing.T) {
	base := strings.Replace(minimalConfig, "name: brain\n", `name: brain
identities:
  fmind:
    canonical: person:email/fmind@example.com
    aliases: [fmind]
    kind: person
`, 1)
	tests := []struct {
		name    string
		config  string
		extra   map[string]string
		wantErr string
	}{
		{"missing canonical", strings.Replace(base, "    canonical: person:email/fmind@example.com\n", "", 1), nil, "canonical is required"},
		{"missing aliases", strings.Replace(base, "    aliases: [fmind]\n", "", 1), nil, "aliases must contain at least one"},
		{"bad bare alias", strings.Replace(base, "aliases: [fmind]", "aliases: [\"Maxime Cordy\"]", 1), nil, "alias"},
		{"non-person owner", strings.Replace(base, "    kind: person\n", "    kind: organization\n    owner: true\n", 1), nil, "owner may only mark a person"},
		{"multiple owners", strings.Replace(strings.Replace(base, "    kind: person\n", "    kind: person\n    owner: true\n", 1), "layers:\n", `  other:
    canonical: person:email/other@example.com
    aliases: [other]
    kind: person
    owner: true
layers:
`, 1), nil, "at most one identity may be the owner"},
		{"local identity map", base, map[string]string{LocalConfigName: "identities:\n  other:\n    aliases: [other]\n"}, "field identities not found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadConfig(writeBase(t, test.config, test.extra))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("LoadConfig() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadConfigRejectsIdentityAliasCollisions(t *testing.T) {
	configText := strings.Replace(minimalConfig, "name: brain\n", `name: brain
identities:
  first:
    canonical: person:email/first@example.com
    aliases: [shared]
  second:
    canonical: person:email/second@example.com
    aliases: [SHARED]
`, 1)
	_, err := LoadConfig(writeBase(t, configText, nil))
	if err == nil || !strings.Contains(err.Error(), "also belongs to identities.first") {
		t.Fatalf("LoadConfig() error = %v, want a deterministic alias collision", err)
	}
}
