package agent

import (
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

func TestStuckResultDetail_SurfacesRealError(t *testing.T) {
	// X "not permitted" error body — the detail should be surfaced.
	r := &tools.Result{IsError: true, ForLLM: `Request failed error: {"detail":"You are not permitted to perform this action.","type":"about:blank"}`}
	got := stuckResultDetail(r)
	if !strings.Contains(got, "You are not permitted to perform this action.") {
		t.Errorf("expected real X error in message, got: %q", got)
	}
	if !strings.Contains(got, "reported this error") {
		t.Errorf("expected error framing, got: %q", got)
	}
}

func TestCleanToolResultForUser(t *testing.T) {
	cases := map[string]string{
		`{"detail":"You are not permitted to perform this action."}`: "You are not permitted to perform this action.",
		"<<<EXTERNAL_UNTRUSTED_CONTENT>>> Source: X --- {\"message\":\"Rate limit exceeded\"}": "Rate limit exceeded",
		"  plain   multi\nline   text  ": "plain multi line text",
		"": "",
	}
	for in, want := range cases {
		if got := cleanToolResultForUser(in); got != want {
			t.Errorf("cleanToolResultForUser(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStuckResultDetail_Nil(t *testing.T) {
	if stuckResultDetail(nil) != "" {
		t.Error("nil result should yield empty detail")
	}
	if stuckResultDetail(&tools.Result{}) != "" {
		t.Error("empty result should yield empty detail")
	}
}
