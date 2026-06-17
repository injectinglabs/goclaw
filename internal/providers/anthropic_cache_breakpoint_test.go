package providers

import "testing"

func hasCacheControl(block map[string]any) bool {
	_, ok := block["cache_control"]
	return ok
}

func TestAddLastMessageCacheBreakpoint_StringContent(t *testing.T) {
	msgs := []map[string]any{
		{"role": "user", "content": "hello"},
	}
	addLastMessageCacheBreakpoint(msgs)
	blocks, ok := msgs[0]["content"].([]map[string]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("string content not converted to block: %#v", msgs[0]["content"])
	}
	if blocks[0]["type"] != "text" || blocks[0]["text"] != "hello" || !hasCacheControl(blocks[0]) {
		t.Errorf("expected text block with cache_control, got %#v", blocks[0])
	}
}

func TestAddLastMessageCacheBreakpoint_BlockContent(t *testing.T) {
	msgs := []map[string]any{
		{"role": "user", "content": "earlier"}, // must NOT get a breakpoint
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "x", "content": "snapshot"},
		}},
	}
	addLastMessageCacheBreakpoint(msgs)
	// earlier message untouched (still a string)
	if _, isStr := msgs[0]["content"].(string); !isStr {
		t.Errorf("non-last message should be untouched")
	}
	last := msgs[1]["content"].([]map[string]any)
	if !hasCacheControl(last[len(last)-1]) {
		t.Errorf("last block should have cache_control: %#v", last)
	}
}

func TestAddLastMessageCacheBreakpoint_Empty(t *testing.T) {
	addLastMessageCacheBreakpoint(nil)            // must not panic
	addLastMessageCacheBreakpoint([]map[string]any{}) // must not panic
}
