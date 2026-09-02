package printer

import (
	"bytes"
	"strings"
	"testing"

	"ebsctl/pkg/resource"
)

func TestJobTableAndNameOutput(t *testing.T) {
	definition, _ := resource.Resolve("job")
	data := []byte(`{"items":[{"metadata":{"name":"job-a","creationTimestamp":"2026-08-19T00:00:00Z"},"status":{"phase":"Running","stage":"build","runner":"runner-a"}}]}`)
	var table bytes.Buffer
	if err := New(&table, Options{Format: "table"}).Print(definition, data); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"NAME", "PHASE", "job-a", "Running", "runner-a"} {
		if !strings.Contains(table.String(), value) {
			t.Fatalf("table missing %q: %s", value, table.String())
		}
	}
	var name bytes.Buffer
	if err := New(&name, Options{Format: "name"}).Print(definition, data); err != nil {
		t.Fatal(err)
	}
	if name.String() != "job/job-a\n" {
		t.Fatalf("unexpected name output %q", name.String())
	}
}

func TestWatchTablePrintsHeaderOnce(t *testing.T) {
	definition, _ := resource.Resolve("job")
	var output bytes.Buffer
	p := New(&output, Options{Format: "table"})
	object := map[string]any{"metadata": map[string]any{"name": "job-a"}}
	_ = p.PrintValue(definition, object, "ADDED")
	_ = p.PrintValue(definition, object, "MODIFIED")
	if strings.Count(output.String(), "EVENT") != 1 {
		t.Fatalf("header repeated: %s", output.String())
	}
}

func TestBuildResourceTable(t *testing.T) {
	definition, _ := resource.Resolve("buildresource")
	data := []byte(`{"metadata":{"name":"project-a"},"spec":{"default":{"requests":{"cpu":"4","memory":"8Gi"}},"packages":{"gcc":{},"llvm":{}}}}`)
	var output bytes.Buffer
	if err := New(&output, Options{Format: "table"}).Print(definition, data); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"CPU", "MEMORY", "PACKAGES", "project-a", "4", "8Gi", "2"} {
		if !strings.Contains(output.String(), value) {
			t.Fatalf("table missing %q: %s", value, output.String())
		}
	}
}
