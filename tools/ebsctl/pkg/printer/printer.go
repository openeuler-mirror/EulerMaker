package printer

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"ebsctl/pkg/resource"
	"gopkg.in/yaml.v3"
)

type Options struct {
	Format    string
	NoHeaders bool
	Wide      bool
}

type Printer struct {
	writer        io.Writer
	option        Options
	headerPrinted bool
	valuePrinted  bool
}

func New(writer io.Writer, option Options) *Printer {
	if option.Format == "" {
		option.Format = "table"
	}
	if option.Format == "wide" {
		option.Wide = true
	}
	return &Printer{writer: writer, option: option}
}

func (p *Printer) Print(definition resource.Definition, data []byte) error {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return p.PrintValue(definition, value, "")
}

func (p *Printer) PrintValue(definition resource.Definition, value any, event string) error {
	switch p.option.Format {
	case "json":
		encoder := json.NewEncoder(p.writer)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	case "yaml":
		if p.valuePrinted {
			if _, err := fmt.Fprintln(p.writer, "---"); err != nil {
				return err
			}
		}
		data, err := yaml.Marshal(value)
		if err != nil {
			return err
		}
		_, err = p.writer.Write(data)
		p.valuePrinted = true
		return err
	case "name":
		return p.printNames(definition, value)
	case "table", "wide":
		return p.printTable(definition, value, event)
	default:
		return fmt.Errorf("unsupported output format %q", p.option.Format)
	}
}

func (p *Printer) printNames(definition resource.Definition, value any) error {
	for _, object := range objects(value) {
		name := nestedString(object, "metadata", "name")
		if name != "" {
			if _, err := fmt.Fprintf(p.writer, "%s/%s\n", definition.Singular, name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Printer) printTable(definition resource.Definition, value any, event string) error {
	columns := tableColumns(definition, p.option.Wide)
	if event != "" {
		columns = append([]column{{name: "EVENT", value: func(map[string]any) string { return event }}}, columns...)
	}
	table := tabwriter.NewWriter(p.writer, 0, 4, 2, ' ', 0)
	if !p.option.NoHeaders && !p.headerPrinted {
		for index, column := range columns {
			if index > 0 {
				fmt.Fprint(table, "\t")
			}
			fmt.Fprint(table, column.name)
		}
		fmt.Fprintln(table)
		p.headerPrinted = true
	}
	for _, object := range objects(value) {
		for index, column := range columns {
			if index > 0 {
				fmt.Fprint(table, "\t")
			}
			fmt.Fprint(table, column.value(object))
		}
		fmt.Fprintln(table)
	}
	return table.Flush()
}

type column struct {
	name  string
	value func(map[string]any) string
}

func tableColumns(definition resource.Definition, wide bool) []column {
	name := column{name: "NAME", value: func(object map[string]any) string { return nestedString(object, "metadata", "name") }}
	age := column{name: "AGE", value: func(object map[string]any) string { return age(nestedString(object, "metadata", "creationTimestamp")) }}
	phase := func(label string) column {
		return column{name: label, value: func(object map[string]any) string { return nestedString(object, "status", "phase") }}
	}
	var columns []column
	switch definition.Kind {
	case "Project":
		columns = []column{name, {name: "DISPLAY NAME", value: func(object map[string]any) string { return nestedString(object, "spec", "displayName") }}, phase("PHASE"), age}
	case "Snapshot":
		columns = []column{name, phase("PHASE"), age}
	case "Build":
		columns = []column{name, phase("PHASE"), {name: "SNAPSHOT", value: func(object map[string]any) string { return nestedString(object, "spec", "snapshotName") }}, age}
	case "Job":
		columns = []column{name, phase("PHASE"), {name: "STAGE", value: func(object map[string]any) string { return nestedString(object, "status", "stage") }}, {name: "RUNNER", value: func(object map[string]any) string { return nestedString(object, "status", "runner") }}, age}
	case "BuildInfo", "RpmRepo":
		columns = []column{name, phase("STATUS"), age}
	default:
		columns = []column{name, age}
	}
	if wide && definition.Namespaced {
		columns = append(columns, column{name: "PROJECT", value: func(object map[string]any) string { return nestedString(object, "metadata", "namespace") }})
	}
	return columns
}

func objects(value any) []map[string]any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	items, list := object["items"].([]any)
	if !list {
		return []map[string]any{object}
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if mapped, ok := item.(map[string]any); ok {
			result = append(result, mapped)
		}
	}
	return result
}

func nestedString(object map[string]any, path ...string) string {
	var current any = object
	for _, segment := range path {
		mapped, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = mapped[segment]
	}
	if current == nil {
		return ""
	}
	return fmt.Sprint(current)
}

func age(value string) string {
	created, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "<unknown>"
	}
	duration := time.Since(created)
	if duration < 0 {
		duration = 0
	}
	switch {
	case duration < time.Minute:
		return fmt.Sprintf("%ds", int(duration.Seconds()))
	case duration < time.Hour:
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	case duration < 24*time.Hour:
		return fmt.Sprintf("%dh", int(duration.Hours()))
	default:
		return fmt.Sprintf("%dd", int(duration.Hours()/24))
	}
}

func Describe(writer io.Writer, definition resource.Definition, object map[string]any) error {
	sections := []struct {
		name string
		key  string
	}{{"Metadata", "metadata"}, {"Spec", "spec"}, {"Status", "status"}}
	fmt.Fprintf(writer, "Name:\t%s\nKind:\t%s\n", nestedString(object, "metadata", "name"), definition.Kind)
	for _, section := range sections {
		value, exists := object[section.key]
		if !exists {
			continue
		}
		data, err := yaml.Marshal(value)
		if err != nil {
			return err
		}
		fmt.Fprintf(writer, "\n%s:\n", section.name)
		for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			fmt.Fprintf(writer, "  %s\n", line)
		}
	}
	return nil
}

func SortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
