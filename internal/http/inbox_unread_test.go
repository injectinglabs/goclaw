package http

import "testing"

func TestExtractCount(t *testing.T) {
	type want struct {
		n  int
		ok bool
	}
	cases := map[string]want{
		`{"resultSizeEstimate":7,"messages":[{"id":"a"}]}`: {7, true},
		`{"messages":[{"id":"a"},{"id":"b"}]}`:             {2, true},
		`{"@odata.count":4,"value":[{"id":"x"}]}`:          {4, true},
		`{"data":{"@odata.count":1}}`:                      {1, true},
		`not json`:                                         {0, false},
		`{"foo":"bar"}`:                                    {0, false},
	}
	for in, exp := range cases {
		n, ok := extractCount(in)
		if n != exp.n || ok != exp.ok {
			t.Errorf("extractCount(%q) = (%d,%v), want (%d,%v)", in, n, ok, exp.n, exp.ok)
		}
	}
}

func TestExtractOutlookMessages(t *testing.T) {
	// one unread (with nested from) among reads → exactly one message
	in := `{"value":[
		{"isRead":true,"subject":"read one"},
		{"isRead":false,"subject":"Hi there","from":{"emailAddress":{"name":"Alice","address":"a@x.com"}},"receivedDateTime":"2026-06-16T12:00:00Z"},
		{"isRead":true,"subject":"read two"}
	]}`
	msgs := extractOutlookMessages(in)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Provider != "outlook" || m.From != "Alice" || m.Subject != "Hi there" || m.Date == "" {
		t.Errorf("unexpected message: %+v", m)
	}
	// no isRead anywhere → no messages (don't fabricate)
	if got := extractOutlookMessages(`{"value":[{"id":"a"}]}`); len(got) != 0 {
		t.Errorf("expected 0 without isRead, got %d", len(got))
	}
}

func TestExtractGmailMessages(t *testing.T) {
	// flat shape
	flat := `{"messages":[{"sender":"bob@x.com","subject":"Flat subject","messageTimestamp":"123"}]}`
	if msgs := extractGmailMessages(flat); len(msgs) != 1 || msgs[0].From != "bob@x.com" || msgs[0].Subject != "Flat subject" || msgs[0].Provider != "gmail" {
		t.Errorf("flat gmail parse failed: %+v", msgs)
	}
	// raw payload.headers shape
	raw := `{"messages":[{"payload":{"headers":[{"name":"From","value":"Carol <c@x.com>"},{"name":"Subject","value":"Hdr subject"}]}}]}`
	if msgs := extractGmailMessages(raw); len(msgs) != 1 || msgs[0].From != "Carol <c@x.com>" || msgs[0].Subject != "Hdr subject" {
		t.Errorf("header gmail parse failed: %+v", msgs)
	}
}
