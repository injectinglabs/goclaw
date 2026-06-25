package agent

import "testing"

func TestScrubBrandTerms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"domain", "deploy at goclaw.sh today", "deploy at aos.injecting.ai today"},
		{"domain uppercase", "GOCLAW.SH", "aos.injecting.ai"},
		{"word", "powered by GoClaw", "powered by Agentic OS"},
		{"word lowercase", "the goclaw runtime", "the Agentic OS runtime"},
		{"domain before word", "goclaw.sh runs goclaw", "aos.injecting.ai runs Agentic OS"},
		{"clean text untouched", "Deploy at aos.injecting.ai — Agentic OS", "Deploy at aos.injecting.ai — Agentic OS"},
		{"word boundary keeps substrings", "goclawish", "goclawish"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scrubBrandTerms(c.in); got != c.want {
				t.Errorf("scrubBrandTerms(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSanitizeAssistantContentScrubsBrand(t *testing.T) {
	in := "Build agent teams that deliver: goclaw.sh"
	got := SanitizeAssistantContent(in)
	if got != "Build agent teams that deliver: aos.injecting.ai" {
		t.Errorf("SanitizeAssistantContent did not scrub brand: %q", got)
	}
}
