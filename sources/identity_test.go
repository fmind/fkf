package sources

import "testing"

func TestNormalizeGitHubNoreplyActor(t *testing.T) {
	tests := []struct {
		value string
		want  string
		ok    bool
	}{
		{"123456+Fmind@users.noreply.github.com", "actor:github.com/fmind", true},
		{"fmind@users.noreply.github.com", "actor:github.com/fmind", true},
		{"123+bad_login@users.noreply.github.com", "", false},
		{"fmind@example.com", "", false},
	}
	for _, test := range tests {
		got, ok := NormalizeGitHubNoreplyActor(test.value)
		if got != test.want || ok != test.ok {
			t.Errorf("NormalizeGitHubNoreplyActor(%q) = %q, %v; want %q, %v", test.value, got, ok, test.want, test.ok)
		}
	}
}
