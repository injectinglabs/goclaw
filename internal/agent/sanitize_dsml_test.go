package agent

import (
	"strings"
	"testing"
)

// TestSanitizeDSML covers DeepSeek-V3.2 / V4 DSML markup leaking into content.
// Regression for the empty-reply chain firing when the model emits the
// tool-call format as text instead of proper structured tool_calls.
func TestSanitizeDSML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // expected to be empty (stripped) or sanitized text
	}{
		{
			name: "single-pipe DSML wrapper",
			in:   "Some preamble.\n<｜DSML｜tool_calls>\n<｜DSML｜invoke name=\"web_fetch\">\n<｜DSML｜parameter name=\"url\">https://x</｜DSML｜parameter>\n</｜DSML｜invoke>\n</｜DSML｜tool_calls>\nAnd a trailing remark.",
			want: "Some preamble.\n\n\n\n\n\nAnd a trailing remark.",
		},
		{
			name: "double-pipe DSML (mojibake)",
			in:   "<｜｜DSML｜｜tool_calls>\n<｜｜DSML｜｜invoke name=\"x\">\n</｜｜DSML｜｜invoke>\n</｜｜DSML｜｜tool_calls>",
			want: "",
		},
		{
			name: "DSML only — nothing else",
			in:   "<｜DSML｜tool_calls></｜DSML｜tool_calls>",
			want: "",
		},
		{
			name: "no DSML, plain text untouched",
			in:   "Plain reply with no markup at all.",
			want: "Plain reply with no markup at all.",
		},
		{
			name: "DSML + real prose around it",
			in:   "Here's what I found:\n\n<｜DSML｜tool_calls><｜DSML｜invoke name=\"search\"><｜DSML｜parameter name=\"q\">AI</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>\n\nThe answer is 42.",
			want: "Here's what I found:\n\nThe answer is 42.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeAssistantContent(tc.in)
			// Collapse whitespace runs for comparison robustness — the
			// regex leaves empty lines where tags used to be.
			gotN := strings.Join(strings.Fields(got), " ")
			wantN := strings.Join(strings.Fields(tc.want), " ")
			if gotN != wantN {
				t.Errorf("SanitizeAssistantContent mismatch\nIN:   %q\nGOT:  %q\nWANT: %q", tc.in, got, tc.want)
			}
		})
	}
}
