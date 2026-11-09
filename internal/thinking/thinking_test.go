package thinking

import (
	"strings"
	"testing"
)

func TestCleanMarkdownLine(t *testing.T) {
	cases := map[string]string{
		"## Heading":         "Heading",
		"- bullet point":     "bullet point",
		"1. numbered":        "numbered",
		"`code` note":        "code note",
		"[link](http://x)":   "link",
		"  spaced  ":         "spaced",
		"":                   "",
		"###":                "",
		"plain text":         "plain text",
		"see [docs](/d) now": "see docs now",
	}
	for in, want := range cases {
		if got := cleanMarkdownLine(in); got != want {
			t.Errorf("cleanMarkdownLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstMeaningfulLineSkipsDecorations(t *testing.T) {
	text := "```\ncode fence\n```\n\n**Real thought**"
	if got := FirstMeaningfulLine(text); got != "code fence" {
		t.Errorf("FirstMeaningfulLine = %q, want the first readable line", got)
	}
}

func TestSummaryTruncation(t *testing.T) {
	long := strings.Repeat("а", 300)
	got := Truncate(long, MaxChars)
	if len([]rune(got)) != MaxChars+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("Truncate produced %d runes, want %d plus the ellipsis", len([]rune(got)), MaxChars)
	}
}
