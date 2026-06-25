package tools

import "testing"

func TestIsTelegramChatID(t *testing.T) {
	cases := map[string]bool{
		"544442097":   true,  // private chat id
		"-1001234567": true,  // supergroup id (negative)
		"x0nick":      false, // bare username (the bug)
		"@x0nick":     false, // handle with @
		"":            false,
		"123abc":      false,
		"  789  ":     true, // trimmed
	}
	for in, want := range cases {
		if got := isTelegramChatID(in); got != want {
			t.Errorf("isTelegramChatID(%q) = %v, want %v", in, got, want)
		}
	}
}
