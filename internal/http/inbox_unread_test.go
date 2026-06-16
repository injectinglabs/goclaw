package http

import "testing"

func TestExtractGmailCount(t *testing.T) {
	cases := map[string]int{
		`{"resultSizeEstimate":7,"messages":[{"id":"a"}]}`:        7,           // prefer estimate
		`{"data":{"resultSizeEstimate":3}}`:                       3,           // nested under data
		`{"messages":[{"id":"a"},{"id":"b"}]}`:                    2,           // fallback to length
		`{"data":{"messages":[{"id":"a"},{"id":"b"},{"id":"c"}]}}`: 3,          // nested messages
		`not json`:                                                0,           // garbage → 0
		`{"foo":"bar"}`:                                           0,           // no count keys → 0
	}
	for in, want := range cases {
		if got := extractGmailCount(in); got != want {
			t.Errorf("extractGmailCount(%q) = %d, want %d", in, got, want)
		}
	}
}
