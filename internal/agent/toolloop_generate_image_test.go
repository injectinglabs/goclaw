package agent

import "testing"

// Regression: generate_image must NOT be exempt from the same-result loop
// breaker. Gemini over-eagerly re-calls it with tweaked args; the server-side
// cooldown returns an identical notice each time, and the breaker is the
// deterministic stop that bounds the runaway. Exempting it (a prior attempt)
// let the model call it 14 times.
func TestGenerateImageBoundedBySameResultBreaker(t *testing.T) {
	const tool = "mcp_document_mcp__generate_image"
	const result = "Image generation already completed for this request; the generated image is attached to the chat. No additional image was produced."

	var s toolLoopState
	rh := hashResult(result)

	// Calls 1..2 with DIFFERENT args, identical result → warning, not yet critical.
	for i := 0; i < 2; i++ {
		h := s.record(tool, map[string]any{"prompt": "green elephant", "variant": i})
		s.recordResult(h, result)
	}
	if level, _ := s.detectSameResult(tool, rh); level == "critical" {
		t.Fatalf("should not be critical at 2 identical results, got critical")
	}

	// Third identical result → critical halt.
	h := s.record(tool, map[string]any{"prompt": "green elephant", "variant": 2})
	s.recordResult(h, result)
	level, msg := s.detectSameResult(tool, rh)
	if level != "critical" {
		t.Fatalf("expected critical at 3 identical results for generate_image, got level=%q msg=%q", level, msg)
	}
}

// refresh_page_content legitimately returns the same snapshot; it must remain
// exempt from the same-result breaker.
func TestRefreshPageContentStaysExempt(t *testing.T) {
	const tool = "refresh_page_content"
	const result = "<html>unchanged</html>"

	var s toolLoopState
	for i := 0; i < 5; i++ {
		h := s.record(tool, map[string]any{"selector": i})
		s.recordResult(h, result)
	}
	if level, _ := s.detectSameResult(tool, hashResult(result)); level != "" {
		t.Fatalf("refresh_page_content must stay exempt, got level=%q", level)
	}
}
