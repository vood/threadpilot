package threadpilot

import "testing"

func TestExtractLinkFlairTemplates(t *testing.T) {
	t.Parallel()

	raw := []interface{}{
		map[string]interface{}{"id": "flair_a", "text": "Discussion"},
		map[string]interface{}{"flair_template_id": "flair_b", "flair_text": "Help", "mod_only": true},
		map[string]interface{}{
			"id": "flair_c",
			"richtext": []interface{}{
				map[string]interface{}{"t": "Show"},
				map[string]interface{}{"t": "case"},
			},
		},
		// Duplicate entry should be deduplicated.
		map[string]interface{}{"id": "flair_a", "text": "Discussion"},
	}

	got := extractLinkFlairTemplates(raw)
	if len(got) != 3 {
		t.Fatalf("len(templates) = %d, want 3", len(got))
	}
	if got[0].ID != "flair_a" || got[0].Text != "Discussion" || got[0].ModOnly {
		t.Fatalf("unexpected first template: %#v", got[0])
	}
	if got[1].ID != "flair_b" || got[1].Text != "Help" || !got[1].ModOnly {
		t.Fatalf("unexpected second template: %#v", got[1])
	}
	if got[2].ID != "flair_c" || got[2].Text != "Showcase" {
		t.Fatalf("unexpected richtext template: %#v", got[2])
	}
}

func TestScoreLinkFlairMatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		actual  string
		desired string
		min     int
		max     int
	}{
		{name: "exact_case_insensitive", actual: "Discussion", desired: "discussion", min: 100, max: 100},
		{name: "normalized_punctuation", actual: "Tutorial/Guide", desired: "tutorial guide", min: 90, max: 90},
		{name: "contains_match", actual: "Product Discussion", desired: "discussion", min: 70, max: 89},
		{name: "empty_desired", actual: "Discussion", desired: "", min: -1, max: -1},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			score := scoreLinkFlairMatch(tc.actual, tc.desired)
			if score < tc.min || score > tc.max {
				t.Fatalf("scoreLinkFlairMatch(%q, %q) = %d, expected in [%d, %d]", tc.actual, tc.desired, score, tc.min, tc.max)
			}
		})
	}
}

func TestNormalizeFlairLabel(t *testing.T) {
	t.Parallel()

	if got := normalizeFlairLabel("  Tutorial/Guide  "); got != "tutorial guide" {
		t.Fatalf("normalizeFlairLabel returned %q", got)
	}
}
