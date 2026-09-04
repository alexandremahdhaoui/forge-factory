package docscontroller

import (
	"fmt"
	"path"
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
	fmt.Fprintf(&b, "\n```yaml\nengine: forge://github.com/alexandremahdhaoui/forge-factory/cmd/%s@v%s\n```\n",
		e.Name, e.Version)

	b.WriteString("\n## Tools\n\n| Tool | Does | Input | Output |\n|---|---|---|---|\n")

	for _, t := range e.Layout.Tools {
		fmt.Fprintf(&b, "| `%s` | %s | `%s` | `%s` |\n",
			t.Name, t.Description, t.Input, t.Output)
	}

	b.WriteString("\n## Running it by hand\n\n```sh\n")
	fmt.Fprintf(&b, "go run ./cmd/%s --mcp\n```\n", e.Name)

	b.WriteString("\nIt speaks JSON-RPC on stdin and stdout. Logs go to stderr, because")
	b.WriteString("\nanything else corrupts the stream.\n")

	return docstypes.File{Path: path.Join(dir, "docs", "usage.md"), Content: b.String()}, nil
}
