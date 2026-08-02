package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// blindSpotPhrase is what all three documents call the failure, and the marker
// this test looks for. The caveat may be worded however its document words it;
// what it may not do is go missing.
const blindSpotPhrase = "long turn"

// counterGuidance is every document that tells a reader to use the counter.
var counterGuidance = []string{"README.md", "docs/json.md", "skills/coordinator/SKILL.md"}

// The counter guidance lives in three documents, and all three shipped
// recommending `agent_status_seq` as the way to spot a worker that stopped
// without the sentence that makes the recommendation safe (#112).
//
// The field counts state *changes*, so a worker that stays in one state does
// not move it, and a worker thinking is exactly that. Beside `idle` a frozen
// counter is the answer it was built for; beside `working` it is a long turn or
// a wedge and the field cannot say which. Whoever reads any one of these three
// documents and builds the obvious detector pages themselves about their
// healthiest worker — so the caveat is required wherever the recommendation is
// made. One claim in three places drifts the moment somebody updates one.
func TestCounterGuidanceNamesTheBlindSpot(t *testing.T) {
	for _, path := range counterGuidance {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(raw)

		// A file that stopped recommending the counter needs no caveat, but it
		// also does not belong in the list — say which of the two happened.
		if !strings.Contains(text, "counter") {
			t.Errorf("%s no longer mentions the counter; drop it from counterGuidance", path)
			continue
		}
		if !strings.Contains(text, blindSpotPhrase) {
			t.Errorf("%s recommends the counter without naming its blind spot: a frozen "+
				"counter on a `working` row is a long turn or a wedge, and %q is the "+
				"phrase the other documents name that with", path, blindSpotPhrase)
		}
	}
}

// The README hands the reader off to the full measurement rather than repeating
// it, so the anchor is load-bearing: a heading rename leaves a link that scrolls
// to the top of a long document and reads as though nothing more was written.
func TestTheReadmeCounterLinkResolves(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.ReadFile("docs/json.md")
	if err != nil {
		t.Fatal(err)
	}

	links := regexp.MustCompile(`docs/json\.md#([a-z0-9-]+)`).FindAllStringSubmatch(string(readme), -1)
	if len(links) == 0 {
		t.Fatal("README.md no longer links into docs/json.md by anchor")
	}

	headings := map[string]bool{}
	for line := range strings.SplitSeq(string(target), "\n") {
		if _, title, ok := strings.Cut(line, "# "); ok && strings.HasPrefix(line, "#") {
			headings[githubAnchor(title)] = true
		}
	}
	for _, link := range links {
		if !headings[link[1]] {
			t.Errorf("README.md links to docs/json.md#%s, which is not a heading there", link[1])
		}
	}
}

// githubAnchor is the fragment GitHub derives from a heading: lowercased, every
// character that is not a letter, a digit, a space or a hyphen dropped, spaces
// hyphenated. Backticks and colons are the ones that matter here.
func githubAnchor(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}
