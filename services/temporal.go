package services

import (
	"fmt"
	"strings"
	"time"
)

// TemporalQuery is a lexical query with an optional date expression removed from exactly one
// boundary. Newest represents the closed `last` operator; it changes ordering only after the
// ordinary relevance floor has established that an item actually matches.
type TemporalQuery struct {
	Query  string
	Window Window
	Newest bool
}

type temporalExpression struct {
	words  int
	phrase string
	window Window
	newest bool
}

var temporalWeekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday,
	"wednesday": time.Wednesday, "thursday": time.Thursday, "friday": time.Friday,
	"saturday": time.Saturday,
}

// ParseTemporalQuery recognizes one closed date expression at the start or end of a query.
// Keeping the grammar at the boundary prevents prose such as "compare today with yesterday"
// from silently changing the corpus, while making natural requests like "last meeting notes"
// deterministic and inspectable in receipt.window.derived_from.
func ParseTemporalQuery(query string, now time.Time) (TemporalQuery, error) {
	words := strings.Fields(query)
	if len(words) == 0 {
		return TemporalQuery{Query: strings.TrimSpace(query)}, nil
	}
	prefix, prefixFound, err := parseTemporalBoundary(words, true, now)
	if err != nil {
		return TemporalQuery{}, err
	}
	remaining := words
	if prefixFound {
		remaining = words[prefix.words:]
	}
	suffix, suffixFound, err := parseTemporalBoundary(remaining, false, now)
	if err != nil {
		return TemporalQuery{}, err
	}
	if prefixFound && suffixFound {
		return TemporalQuery{}, fmt.Errorf("ambiguous temporal query: use one date expression at the start or end, not %q and %q",
			prefix.phrase, suffix.phrase)
	}
	expression := prefix
	if suffixFound {
		expression = suffix
		remaining = remaining[:len(remaining)-suffix.words]
	}
	if !prefixFound && !suffixFound {
		return TemporalQuery{Query: strings.TrimSpace(query)}, nil
	}
	for _, atStart := range []bool{true, false} {
		extra, found, extraErr := parseTemporalBoundary(remaining, atStart, now)
		if extraErr != nil {
			return TemporalQuery{}, extraErr
		}
		if found {
			return TemporalQuery{}, fmt.Errorf("ambiguous temporal query: use one date expression at the start or end, not %q and %q",
				expression.phrase, extra.phrase)
		}
	}
	clean := strings.Join(remaining, " ")
	if clean == "" {
		return TemporalQuery{}, fmt.Errorf("temporal expression %q leaves no query terms", expression.phrase)
	}
	expression.window.DerivedFrom = expression.phrase
	return TemporalQuery{Query: clean, Window: expression.window, Newest: expression.newest}, nil
}

func parseTemporalBoundary(words []string, atStart bool, now time.Time) (temporalExpression, bool, error) {
	if len(words) == 0 {
		return temporalExpression{}, false, nil
	}
	today := dateAt(now, 0)
	if len(words) >= 2 {
		selected := temporalBoundaryWords(words, atStart, 2)
		expression, found, err := parseTwoWordTemporal(selected[0], selected[1], now, today)
		if found || err != nil {
			return expression, found, err
		}
	}
	return parseSingleWordTemporal(temporalBoundaryWords(words, atStart, 1)[0], now, today)
}

func temporalBoundaryWords(words []string, atStart bool, count int) []string {
	selected := words[:count]
	if !atStart {
		selected = words[len(words)-count:]
	}
	canonical := make([]string, len(selected))
	for index, word := range selected {
		canonical[index] = normalizeTemporalWord(word)
	}
	return canonical
}

func parseTwoWordTemporal(first, second string, now time.Time, today string) (temporalExpression, bool, error) {
	phrase := first + " " + second
	switch {
	case first == "last" && second == "week":
		start := startOfWeek(now).AddDate(0, 0, -7)
		return temporalExpression{words: 2, phrase: phrase, window: Window{
			Since: start.Format(time.DateOnly), Until: start.AddDate(0, 0, 6).Format(time.DateOnly),
		}}, true, nil
	case first == "this" && second == "week":
		return temporalExpression{words: 2, phrase: phrase, window: Window{
			Since: startOfWeek(now).Format(time.DateOnly), Until: today,
		}}, true, nil
	case first == "since":
		date, err := parseTemporalDate(second)
		if err != nil {
			return temporalExpression{}, false, fmt.Errorf("temporal expression %q: %w", phrase, err)
		}
		return temporalExpression{words: 2, phrase: phrase, window: Window{Since: date, Until: today}}, true, nil
	case first == "last":
		if weekday, ok := temporalWeekdays[second]; ok {
			date := previousWeekday(now, weekday, false).Format(time.DateOnly)
			return temporalExpression{words: 2, phrase: phrase, window: Window{Since: date, Until: date}}, true, nil
		}
	}
	return temporalExpression{}, false, nil
}

func parseSingleWordTemporal(value string, now time.Time, today string) (temporalExpression, bool, error) {
	switch value {
	case "today":
		return exactTemporalExpression(value, today), true, nil
	case "yesterday":
		return exactTemporalExpression(value, dateAt(now, -1)), true, nil
	case "last":
		return temporalExpression{words: 1, phrase: value, newest: true}, true, nil
	}
	if weekday, ok := temporalWeekdays[value]; ok {
		date := previousWeekday(now, weekday, true).Format(time.DateOnly)
		return exactTemporalExpression(value, date), true, nil
	}
	if len(value) == len("2006-01") {
		month, err := time.Parse("2006-01", value)
		if err == nil {
			last := month.AddDate(0, 1, -1)
			return temporalExpression{words: 1, phrase: value, window: Window{
				Since: month.Format(time.DateOnly), Until: last.Format(time.DateOnly),
			}}, true, nil
		}
		if looksLikeTemporalMonth(value) {
			return temporalExpression{}, false, fmt.Errorf("temporal expression %q: invalid YYYY-MM month", value)
		}
	}
	if isoDatePattern.MatchString(value) {
		date, err := parseTemporalDate(value)
		if err != nil {
			return temporalExpression{}, false, fmt.Errorf("temporal expression %q: %w", value, err)
		}
		return exactTemporalExpression(value, date), true, nil
	}
	return temporalExpression{}, false, nil
}

func normalizeTemporalWord(value string) string {
	return strings.Trim(strings.ToLower(value), "?!,.")
}

func looksLikeTemporalMonth(value string) bool {
	if len(value) != len("2006-01") || value[4] != '-' {
		return false
	}
	for index, char := range value {
		if index == 4 {
			continue
		}
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func exactTemporalExpression(phrase, date string) temporalExpression {
	return temporalExpression{words: 1, phrase: phrase, window: Window{Since: date, Until: date}}
}

func parseTemporalDate(value string) (string, error) {
	date, err := time.Parse(time.DateOnly, value)
	if err != nil || date.Format(time.DateOnly) != value {
		return "", fmt.Errorf("%q is not a valid YYYY-MM-DD date", value)
	}
	return value, nil
}

func dateAt(now time.Time, offset int) string {
	return now.AddDate(0, 0, offset).Format(time.DateOnly)
}

func startOfWeek(now time.Time) time.Time {
	days := (int(now.Weekday()) + 6) % 7 // Monday is zero.
	return now.AddDate(0, 0, -days)
}

func previousWeekday(now time.Time, wanted time.Weekday, includeToday bool) time.Time {
	days := (int(now.Weekday()) - int(wanted) + 7) % 7
	if days == 0 && !includeToday {
		days = 7
	}
	return now.AddDate(0, 0, -days)
}
