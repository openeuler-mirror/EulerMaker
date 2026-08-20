package cli

import "testing"

func TestParseWaitCondition(t *testing.T) {
	condition, err := parseWaitCondition("jsonpath='{.status.phase}'=Completed")
	if err != nil {
		t.Fatal(err)
	}
	object := map[string]any{"status": map[string]any{"phase": "Completed"}}
	if !condition.matches(object) {
		t.Fatal("JSONPath condition did not match")
	}
	condition, err = parseWaitCondition("condition=Ready")
	if err != nil || !condition.matches(map[string]any{"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}) {
		t.Fatal("condition did not match")
	}
	if _, err := parseWaitCondition("jsonpath='{.items[*]}'=x"); err == nil {
		t.Fatal("expected complex JSONPath to be rejected")
	}
}
