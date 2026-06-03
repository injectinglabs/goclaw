package tools

import (
	"errors"
	"strings"
	"testing"
)

// The raw Docker error that leaked to a Telegram user — it named the container
// and told them to run `docker rm -f` on the host. The sanitized result the LLM
// sees must contain NONE of that.
func TestSandboxInfraErrorResultStripsHostDetails(t *testing.T) {
	raw := errors.New("docker run failed: exit status 125\nstderr: " +
		"Conflict. The container name \"/goclaw-sbx-agent-default-telegram-aggiii_bot-direct-544442097\" " +
		"is already in use by container \"abc123\". You have to remove (or rename) that container " +
		"to be able to reuse that name. docker rm -f goclaw-sbx-agent-default-telegram-aggiii_bot-direct-544442097")

	res := sandboxInfraErrorResult("test", raw)
	if res == nil || !res.IsError {
		t.Fatalf("expected an error Result, got %+v", res)
	}

	leaks := []string{
		"docker", "rm -f", "container", "goclaw-sbx", "exit status", "125",
		"544442097", "Conflict", "stderr",
	}
	low := strings.ToLower(res.ForLLM)
	for _, tok := range leaks {
		if strings.Contains(low, strings.ToLower(tok)) {
			t.Errorf("sanitized message leaks host detail %q: %s", tok, res.ForLLM)
		}
	}

	// It should still tell the model the action didn't run (so it can respond).
	if !strings.Contains(low, "did not run") && !strings.Contains(low, "unavailable") {
		t.Errorf("sanitized message should signal the action failed: %s", res.ForLLM)
	}

	// And it must not embed an imperative command the model would parrot.
	for _, imp := range []string{"do not", "you must", "run `", "run the"} {
		if strings.Contains(low, imp) {
			t.Errorf("sanitized message contains imperative phrasing %q: %s", imp, res.ForLLM)
		}
	}
}
