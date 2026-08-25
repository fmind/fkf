package services

import (
	"fmt"
	"strings"

	"github.com/fmind/fkf/core"
)

// Silent-empty is the worst failure a memory tool can have for an agent: a typo and a
// genuinely empty base look identical from the caller's side, "0 of 0 record(s)" or "0
// pages", exit 0 either way — and the honest report an agent then writes from that is a
// confident negative claim about the user's own past. A selector whose valid values are a
// closed, known set is refused instead, naming what the base actually declares, so the
// refusal is the discovery surface rather than a second search.

// maxVocabularyListed bounds how many valid values one refusal names. A base can declare
// dozens of sources; the point is to be useful, not to print the whole file back.
const maxVocabularyListed = 20

// unknownValueError refuses value for field, naming the vocabulary it should have come from.
// vocabulary is expected already sorted, the order every such listing in fkf uses.
func unknownValueError(field, value string, vocabulary []string) error {
	return fmt.Errorf("%w: unknown %s %q; this base declares %s",
		core.ErrConfig, field, value, joinVocabulary(vocabulary))
}

func joinVocabulary(vocabulary []string) string {
	if len(vocabulary) == 0 {
		return "none"
	}
	if len(vocabulary) <= maxVocabularyListed {
		return strings.Join(vocabulary, ", ")
	}
	shown := vocabulary[:maxVocabularyListed]
	return fmt.Sprintf("%s, … and %d more (see `fkf config`)", strings.Join(shown, ", "), len(vocabulary)-maxVocabularyListed)
}

// requireKnown refuses the first value in values that vocabulary does not contain. It is used
// wherever a selector's whole valid range is a small, closed, known set — a typo caught here
// is caught before a scan runs, not after it silently returns nothing.
func requireKnown(field string, values, vocabulary []string) error {
	known := make(map[string]bool, len(vocabulary))
	for _, value := range vocabulary {
		known[value] = true
	}
	for _, value := range values {
		if !known[value] {
			return unknownValueError(field, value, vocabulary)
		}
	}
	return nil
}
