package secret

import "testing"

func TestIsRef(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"${ssm:/foo/bar}", true},
		{"${ssm:/x}", true},
		{"plain", false},
		{"${env:FOO}", false},
		{"$ssm:/foo}", false},
		{"${ssm:/foo", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsRef(c.in); got != c.want {
			t.Errorf("IsRef(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestExtractName(t *testing.T) {
	if got := extractName("${ssm:/injecting-ai/staging/goclaw/gateway-token}"); got != "/injecting-ai/staging/goclaw/gateway-token" {
		t.Errorf("extractName: got %q", got)
	}
	if got := extractName("plain"); got != "" {
		t.Errorf("extractName(plain) should be empty, got %q", got)
	}
}
