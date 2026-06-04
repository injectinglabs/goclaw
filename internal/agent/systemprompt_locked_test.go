package agent

import (
	"strings"
	"testing"
)

func TestBuildSystemPrompt_LockedAgentPreamble(t *testing.T) {
	t.Run("locked agent receives preamble", func(t *testing.T) {
		out := BuildSystemPrompt(SystemPromptConfig{
			IsLocked:           true,
			CustomInstructions: "Custom role text.",
		})
		if !strings.Contains(out, "Your name is Agentic OS") {
			t.Fatalf("locked agent missing preamble: %q", out)
		}
		pIdx := strings.Index(out, "Your name is Agentic OS")
		cIdx := strings.Index(out, "Custom role text.")
		if pIdx == -1 || cIdx == -1 || pIdx >= cIdx {
			t.Fatalf("preamble must precede CustomInstructions; pIdx=%d cIdx=%d", pIdx, cIdx)
		}
	})

	t.Run("unlocked agent does NOT receive preamble", func(t *testing.T) {
		out := BuildSystemPrompt(SystemPromptConfig{
			IsLocked:           false,
			CustomInstructions: "You are Bob, a financial analyst.",
		})
		if strings.Contains(out, "Your name is Agentic OS") {
			t.Fatalf("unlocked agent leaked preamble: %q", out)
		}
		if !strings.Contains(out, "You are Bob") {
			t.Fatalf("unlocked agent missing CustomInstructions: %q", out)
		}
	})

	t.Run("locked agent without CustomInstructions still gets preamble", func(t *testing.T) {
		out := BuildSystemPrompt(SystemPromptConfig{
			IsLocked: true,
		})
		if !strings.Contains(out, "Your name is Agentic OS") {
			t.Fatalf("locked agent missing preamble: %q", out)
		}
	})
}
