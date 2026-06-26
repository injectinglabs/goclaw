package agent

import "testing"

func TestIsEmptyToolResult(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// The real TWITTER_RECENT_SEARCH "no matches" body (the false-positive case)
		{"twitter empty search", `{"data": [], "meta": {"has_more": false, "result_count": 0}}`, true},
		{"twitter empty wrapped", "<<<EXTERNAL_UNTRUSTED_CONTENT>>>\nSource: composio-mcp / TWITTER_RECENT_SEARCH\n---\n{\"data\": [], \"meta\": {\"result_count\": 0}}", true},
		{"bare empty array", "[]", true},
		{"bare empty object", "{}", true},
		{"empty data array", `{"data":[]}`, true},
		{"empty results array", `{"results": [ ]}`, true},
		{"total zero", `{"total_results": 0, "hits": []}`, true},
		{"whitespace only", "   \n  ", true},
		// Non-empty: must NOT be treated as empty (guard still applies)
		{"has data", `{"data": [{"id": "2070332430607864242", "text": "hi"}], "meta": {"result_count": 1}}`, false},
		{"non-empty wrapped", "<<<EXTERNAL_UNTRUSTED_CONTENT>>>\n{\"data\": [{\"id\": \"1\"}], \"result_count\": 1}", false},
		{"plain text", "Operation completed successfully", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isEmptyToolResult(c.in); got != c.want {
				t.Errorf("isEmptyToolResult(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
