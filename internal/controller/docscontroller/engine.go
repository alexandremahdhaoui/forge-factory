package docscontroller

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/alexandremahdhaoui/forge-factory/internal/types/docstypes"
)

// RenderUsage writes how to call one engine. Every fact comes from its
// forge-dev.yaml, so the doc cannot describe a tool the engine does not have.
func RenderUsage(dir string, e docstypes.Engine) (docstypes.File, error) {
	if strings.TrimSpace(e.Name) == "" {
		return docstypes.File{}, fmt.Errorf("rendering usage: the engine has no name")
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%s\n\n# %s\n\n%s\n", Header, e.Name, e.Description)

	fmt.Fprintf(&b, "\nAn MCP engine. forge resolves it by URI and calls a tool over stdio.\n")
	fmt.Fprintf(&b, "\n```yaml\nengine: go://github.com/alexandremahdhaoui/forge-factory/cmd/%s@v%s\n```\n",
		e.Name, e.Version)

	b.WriteString("\n## Tools\n\n| Tool | Does | Input | Output |\n|---|---|---|---|\n")

	for _, t := range e.Generate.Tools {
		fmt.Fprintf(&b, "| `%s` | %s | `%s` | `%s` |\n",
			t.Name, t.Description, t.Input, t.Output)
	}

	b.WriteString("\n## Running it by hand\n\n```sh\n")
	fmt.Fprintf(&b, "go run ./cmd/%s --mcp\n```\n", e.Name)

	b.WriteString("\nIt speaks JSON-RPC on stdin and stdout. Logs go to stderr, because")
	b.WriteString("\nanything else corrupts the stream.\n")

	return docstypes.File{Path: path.Join(dir, "docs", "usage.md"), Content: b.String()}, nil
}

// RenderSchema writes what each tool takes and returns, read out of the OpenAPI
// document the engine already generates its types from.
func RenderSchema(dir string, e docstypes.Engine, schemas map[string]docstypes.Schema) (docstypes.File, error) {
	var b strings.Builder

	fmt.Fprintf(&b, "%s\n\n# %s configuration\n\n%s\n", Header, e.Name, e.Description)

	wanted := map[string]bool{}

	for _, t := range e.Generate.Tools {
		if t.Input != "" {
			wanted[t.Input] = true
		}

		if t.Output != "" {
			wanted[t.Output] = true
		}
	}

	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		schema, ok := schemas[name]
		if !ok {
			return docstypes.File{}, fmt.Errorf(
				"rendering the schema of %s: the spec declares no %q", e.Name, name)
		}

		fmt.Fprintf(&b, "\n## %s\n", schema.Name)

		if schema.Description != "" {
			fmt.Fprintf(&b, "\n%s\n", schema.Description)
		}

		if len(schema.Properties) == 0 {
			continue
		}

		b.WriteString("\n| Field | Type | Required | Means |\n|---|---|---|---|\n")

		for _, p := range schema.Properties {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n",
				p.Name, p.Type, yesNo(p.Required), oneLine(p.Description))
		}
	}

	return docstypes.File{Path: path.Join(dir, "docs", "schema.md"), Content: b.String()}, nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
}
