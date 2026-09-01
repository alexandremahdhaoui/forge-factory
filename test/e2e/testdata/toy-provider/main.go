// toy-provider is the e2e suite's fixture runtime provider: an MCP engine
// whose describe tool answers whatever JSON document the TOY_DESCRIPTION
// environment variable points at. The suite writes a description naming its
// own httptest server and fixture hashes, which no real provider could pin.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/alexandremahdhaoui/forge/pkg/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type describeInput struct {
	Runtime string            `json:"runtime"`
	Version string            `json:"version"`
	OS      string            `json:"os"`
	Arch    string            `json:"arch"`
	Params  map[string]string `json:"params,omitempty"`
	Spec    map[string]any    `json:"spec,omitempty"`
}

func main() {
	server := mcpserver.New("toy-provider", "dev")

	mcpserver.RegisterTool(server, &mcp.Tool{
		Name:        "describe",
		Description: "Answer the description TOY_DESCRIPTION points at.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ describeInput) (*mcp.CallToolResult, any, error) {
		path := os.Getenv("TOY_DESCRIPTION")
		if path == "" {
			return nil, nil, fmt.Errorf("TOY_DESCRIPTION names no description file")
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, err
		}

		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, nil, err
		}

		return nil, out, nil
	})

	if err := server.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "toy-provider: %v\n", err)
		os.Exit(1)
	}
}
