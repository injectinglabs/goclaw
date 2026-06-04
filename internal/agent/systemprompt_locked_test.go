package agent

import (
	"strings"
	"testing"
)

func TestBuildSystemPrompt_LockedAgentPreamble(t *testing.T) {
	preamble := "Your name is Agentic OS — the AI assistant.\nUse skills via skill_search."

	t.Run("locked agent receives preamble", func(t *testing.T) {
		out := BuildSystemPrompt(SystemPromptConfig{
			IsLocked:            true,
			LockedAgentPreamble: preamble,
			CustomInstructions:  "Custom role text.",
		})
		if !strings.Contains(out, "Your name is Agentic OS") {
			t.Fatalf("locked agent missing preamble: %q", out)
		}
		// preamble must appear BEFORE Custom Instructions
		pIdx := strings.Index(out, "Your name is Agentic OS")
		cIdx := strings.Index(out, "Custom role text.")
		if pIdx == -1 || cIdx == -1 || pIdx >= cIdx {
			t.Fatalf("preamble must precede CustomInstructions; got pIdx=%d cIdx=%d\n%s", pIdx, cIdx, out)
		}
	})

	t.Run("unlocked agent does NOT receive preamble", func(t *testing.T) {
		out := BuildSystemPrompt(SystemPromptConfig{
			IsLocked:            false,
			LockedAgentPreamble: preamble,
			CustomInstructions:  "You are Bob, a financial analyst.",
		})
		if strings.Contains(out, "Your name is Agentic OS") {
			t.Fatalf("unlocked agent leaked preamble: %q", out)
		}
		if !strings.Contains(out, "You are Bob") {
			t.Fatalf("unlocked agent missing CustomInstructions: %q", out)
		}
	})

	t.Run("locked agent with empty preamble injects nothing", func(t *testing.T) {
		out := BuildSystemPrompt(SystemPromptConfig{
			IsLocked:            true,
			LockedAgentPreamble: "",
			CustomInstructions:  "X.",
		})
		// no panic, no preamble; CustomInstructions still present
		if !strings.Contains(out, "X.") {
			t.Fatalf("missing CustomInstructions: %q", out)
		}
	})
}
