package sources

import (
	"regexp"
	"strings"
)

var githubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

// NormalizeGitHubNoreplyActor derives the stable actor URI GitHub's documented commit-email
// forms encode. It is a pure boundary helper: callers decide whether to index the derived URI,
// while stored provider evidence remains byte-for-byte unchanged.
func NormalizeGitHubNoreplyActor(value string) (string, bool) {
	local, domain, found := strings.Cut(strings.TrimSpace(value), "@")
	if !found || !strings.EqualFold(domain, "users.noreply.github.com") {
		return "", false
	}
	if _, login, hasID := strings.Cut(local, "+"); hasID {
		prefix, _, _ := strings.Cut(local, "+")
		if prefix == "" || strings.Trim(prefix, "0123456789") != "" {
			return "", false
		}
		local = login
	}
	if !githubLoginPattern.MatchString(local) {
		return "", false
	}
	return "actor:github.com/" + strings.ToLower(local), true
}
