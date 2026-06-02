package providers

import "strings"

// collapseToolCallsWithoutSig rewrites tool_call cycles that lack thought_signature
// (required by Gemini 2.5+). Some Gemini variants reject tool_call echo-back unless
// the cycle is at least partially signed; the assistant's tool_calls are stripped
// and the corresponding tool-result messages folded into a single user message with
// the tool output content. This preserves context without using a format that
// triggers tool-call imitation.
//
// Group semantics: Gemini emits the signature once per "thought" — when a single
// thought produces several parallel tool_calls (e.g. three concurrent spawn() calls
// after one reasoning step), only the FIRST tool_call carries the signature; the
// siblings are part of the same signed group. So we collapse only when the assistant
// message has NO signed tool_call at all. The previous "any-empty triggers collapse"
// rule fired on every parallel-spawn turn and stripped the tool_calls — Gemini
// then saw the tool reports arriving as plain user messages out of nowhere and
// re-spawned indefinitely (the infinite-loop bug hit by the 3-subagent research
// prompt; live probe of gemini-3.5-flash confirms the API now accepts mixed-sig
// cycles end-to-end).
func collapseToolCallsWithoutSig(msgs []Message) []Message {
	// Collect tool_call IDs that need collapsing.
	collapseIDs := make(map[string]bool)
	for _, m := range msgs {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		hasAnySig := false
		for _, tc := range m.ToolCalls {
			// Checks both snake_case and camelCase for cross-proxy reliability.
			sig := ""
			if tc.Metadata != nil {
				sig = tc.Metadata["thought_signature"]
				if sig == "" {
					sig = tc.Metadata["thoughtSignature"]
				}
			}
			if strings.TrimSpace(sig) != "" {
				hasAnySig = true
				break
			}
		}
		if !hasAnySig {
			for _, tc := range m.ToolCalls {
				collapseIDs[tc.ID] = true
			}
		}
	}
	if len(collapseIDs) == 0 {
		return msgs
	}

	result := make([]Message, 0, len(msgs))
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]

		// Strip tool_calls from assistant message, keep original content only.
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && collapseIDs[m.ToolCalls[0].ID] {
			if m.Content != "" {
				result = append(result, Message{
					Role:    "assistant",
					Content: m.Content,
				})
			}

			// Collect consecutive tool results → fold into one user message.
			var parts []string
			for i+1 < len(msgs) && msgs[i+1].Role == "tool" && collapseIDs[msgs[i+1].ToolCallID] {
				i++
				if content := strings.TrimSpace(msgs[i].Content); content != "" {
					parts = append(parts, content)
				}
			}
			if len(parts) > 0 {
				result = append(result, Message{
					Role:    "user",
					Content: strings.Join(parts, "\n\n"),
				})
			}
			continue
		}

		// Skip orphaned tool results whose assistant was already collapsed.
		if m.Role == "tool" && collapseIDs[m.ToolCallID] {
			continue
		}

		result = append(result, m)
	}
	return result
}
