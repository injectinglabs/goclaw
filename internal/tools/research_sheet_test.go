package tools

import "testing"

func TestBestFirmSite(t *testing.T) {
	cases := []struct {
		firm    string
		results []searchResult
		want    string // expected host ("" = none)
	}{
		{
			firm: "Sequoia Capital",
			results: []searchResult{
				{URL: "https://www.vestbee.com/vc-list"},   // SEO/directory, not blocklisted
				{URL: "https://www.sequoiacap.com/"},        // the real one
				{URL: "https://www.crunchbase.com/org/sequoia"},
			},
			want: "sequoiacap.com",
		},
		{
			firm: "Andreessen Horowitz", // rebranded; no shared token with a16z
			results: []searchResult{
				{URL: "https://a16z.com/"},
				{URL: "https://www.crunchbase.com/org/a16z"},
			},
			want: "a16z.com", // falls back to first non-aggregator
		},
		{
			firm: "Benchmark",
			results: []searchResult{
				{URL: "https://waveup.com/blog/top-vcs"}, // consultancy spam
				{URL: "https://www.benchmark.com/"},
			},
			want: "benchmark.com",
		},
		{
			firm: "Lux Capital",
			results: []searchResult{
				{URL: "https://www.linkedin.com/company/lux-capital"},
				{URL: "https://www.luxcapital.com/contact"},
			},
			want: "luxcapital.com",
		},
	}
	for _, c := range cases {
		_, host := bestFirmSite(c.firm, c.results)
		if host != c.want {
			t.Errorf("bestFirmSite(%q) host = %q, want %q", c.firm, host, c.want)
		}
	}
}

func TestCleanEmail(t *testing.T) {
	cases := map[string]string{
		"388-9310info@costanoa.vc":     "info@costanoa.vc", // phone glued to local part
		"kv@khoslaventures.commedia":   "kv@khoslaventures.com", // TLD bleed
		"  Hello@Neo.com ":             "hello@neo.com",
		"contact@angelspartners.com":   "contact@angelspartners.com",
		"notanemail":                   "",
		"@nolocal.com":                 "",
	}
	for in, want := range cases {
		if got := cleanEmail(in); got != want {
			t.Errorf("cleanEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassifyColumns(t *testing.T) {
	email, site, soft := classifyColumns([]string{"Website", "Email", "HQ", "Stage"})
	if len(email) != 1 || email[0] != "Email" {
		t.Errorf("email cols = %v", email)
	}
	if len(site) != 1 || site[0] != "Website" {
		t.Errorf("site cols = %v", site)
	}
	if len(soft) != 2 {
		t.Errorf("soft cols = %v", soft)
	}
}
