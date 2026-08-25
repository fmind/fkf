// Package fkf embeds the assets a base needs at initialization time.
//
// The embed lives at the repository root because go:embed cannot reach outside its own
// package directory, and skills/ and presets/ are the two installable surfaces `fkf init`
// must be able to write on a machine that has only the binary. Embedding rather than copying
// from disk is also what makes the binary the provenance: there is no lock file to drift,
// because the bytes travel with the build.
package fkf

import "embed"

// Skills is the canonical skill set installed into a base at <base>/.agents/skills/.
//
//go:embed skills
var Skills embed.FS

// Presets are the source declarations `fkf init --preset` composes into a base's fkf.yaml,
// and the helper scripts some of them need under <base>/bin/.
//
//go:embed presets
var Presets embed.FS
