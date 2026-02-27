package threadpilot

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeSubredditName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain", input: "SaaS", want: "SaaS"},
		{name: "with_r_prefix", input: "r/SaaS", want: "SaaS"},
		{name: "with_slash_prefix", input: "/r/SaaS", want: "SaaS"},
		{name: "with_trailing_slash", input: "/r/SaaS/", want: "SaaS"},
		{name: "with_whitespace", input: "  /r/Entrepreneur/  ", want: "Entrepreneur"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeSubredditName(tc.input)
			if got != tc.want {
				t.Fatalf("normalizeSubredditName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestNormalizePermalink(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: "/"},
		{name: "already_path", input: "/r/SaaS/comments/abc/title/", want: "/r/SaaS/comments/abc/title/"},
		{name: "missing_leading_slash", input: "r/SaaS/comments/abc/title/", want: "/r/SaaS/comments/abc/title/"},
		{name: "full_url", input: "https://www.reddit.com/r/SaaS/comments/abc/title/?utm_source=test", want: "/r/SaaS/comments/abc/title/"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizePermalink(tc.input)
			if got != tc.want {
				t.Fatalf("normalizePermalink(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestSubredditRulesPayloadUnmarshal(t *testing.T) {
	t.Parallel()

	raw := `{
		"rules": [{"short_name":"No spam","description":"No promo","kind":"all","violation_reason":"No spam"}],
		"site_rules": ["Spam","Violence"],
		"site_rules_flow": [{"reasonText":"This is spam","reasonTextToShow":"This is spam"}]
	}`

	var payload subredditRulesPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if len(payload.Rules) != 1 {
		t.Fatalf("len(payload.Rules) = %d, want 1", len(payload.Rules))
	}
	if len(payload.SiteRules) != 2 {
		t.Fatalf("len(payload.SiteRules) = %d, want 2", len(payload.SiteRules))
	}
	if len(payload.SiteRulesFlow) != 1 {
		t.Fatalf("len(payload.SiteRulesFlow) = %d, want 1", len(payload.SiteRulesFlow))
	}
}

func TestRunPostRejectsAPIDryRun(t *testing.T) {
	t.Parallel()

	a := &app{}
	err := a.runPost([]string{
		"--transport", "api",
		"--kind", "self",
		"--subreddit", "openclaw",
		"--title", "test",
		"--text", "body",
		"--dry-run",
	})
	if err == nil {
		t.Fatal("expected error for --transport api with --dry-run, got nil")
	}
	if !strings.Contains(err.Error(), "--dry-run is supported only for --transport browser") {
		t.Fatalf("unexpected error: %v", err)
	}
}
