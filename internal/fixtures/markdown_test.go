package fixtures

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMarkdownBlocks(t *testing.T) {
	md := "Intro text.\n\n| Code | Meaning |\n|---|:--|\n| `401` | No key \\| bad key |\n| 404 | Not found |\n\nAfter.\n\n```\n| not | a table |\n|---|---|\n```\n"
	got := markdownBlocks(md)
	var types []string
	for _, b := range got {
		types = append(types, b["type"].(string))
	}
	if strings.Join(types, ",") != "rich-text,table,rich-text" {
		t.Fatalf("blocks: %v", types)
	}
	rows := got[1]["data"].(map[string]any)["rows"].([]any)
	if len(rows) != 3 || rows[1].(map[string]any)["cells"] != "`401`\nNo key | bad key" {
		t.Errorf("rows: %v", rows)
	}
	b, _ := json.Marshal(got[2])
	if !strings.Contains(string(b), "| not | a table |") || !strings.Contains(string(b), "codeBlock") {
		t.Errorf("a table in a code fence was converted: %s", b)
	}
}
