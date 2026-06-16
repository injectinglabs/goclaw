package http

import "testing"

func TestExtractCount(t *testing.T) {
	type want struct {
		n  int
		ok bool
	}
	cases := map[string]want{
		`{"resultSizeEstimate":7,"messages":[{"id":"a"}]}`:        {7, true},  // gmail: prefer estimate
		`{"messages":[{"id":"a"},{"id":"b"}]}`:                    {2, true},  // gmail: fallback to len
		`{"@odata.count":4,"value":[{"id":"x"}]}`:                 {4, true},  // outlook: odata.count
		`{"value":[{"id":"x"},{"id":"y"},{"id":"z"}]}`:            {3, true},  // outlook: value len
		`{"data":{"@odata.count":1}}`:                             {1, true},  // nested under data
		`{"response_data":{"resultSizeEstimate":5}}`:              {5, true},  // nested wrapper
		`not json`:                                                {0, false}, // garbage
		`{"foo":"bar"}`:                                           {0, false}, // no count keys
	}
	for in, exp := range cases {
		n, ok := extractCount(in)
		if n != exp.n || ok != exp.ok {
			t.Errorf("extractCount(%q) = (%d,%v), want (%d,%v)", in, n, ok, exp.n, exp.ok)
		}
	}
}

func TestCountUnreadOutlook(t *testing.T) {
	type want struct {
		n  int
		ok bool
	}
	cases := map[string]want{
		// one unread among several read (the user's real case)
		`{"value":[{"isRead":true},{"isRead":false},{"isRead":true}]}`: {1, true},
		// nested under data
		`{"data":{"value":[{"isRead":false},{"isRead":false}]}}`: {2, true},
		// snake_case passthrough
		`{"value":[{"is_read":false},{"is_read":true}]}`: {1, true},
		// no isRead field anywhere → can't determine (must NOT return array length)
		`{"value":[{"id":"a"},{"id":"b"}]}`: {0, false},
		`not json`:                          {0, false},
	}
	for in, exp := range cases {
		n, ok := countUnreadOutlook(in)
		if n != exp.n || ok != exp.ok {
			t.Errorf("countUnreadOutlook(%q) = (%d,%v), want (%d,%v)", in, n, ok, exp.n, exp.ok)
		}
	}
}
