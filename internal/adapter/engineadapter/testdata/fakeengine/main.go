// Command fakeengine is an MCP engine that exists so the caller can be tested
// over the wire it actually uses.
//
// A mocked session would prove that the parser reads what the test wrote. The
// things that break here break in the transport: a tool that answers an error
// rather than failing, a tool that answers no structured content, and a tool
// whose structured content does not fit the shape the caller decodes into.
// None of those are reachable without a real server on the other end.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/alexandremahdhaoui/forge/pkg/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type input struct {
	Word string `json:"word,omitempty"`
}

func main() {
	s := mcpserver.New("fakeengine", "test")

	register(s, "echo", func(_ context.Context, _ *mcp.CallToolRequest, in input) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"word": in.Word}, nil
	})

	// Answers an error the MCP way, with IsError set and the reason in the
	// content. The caller must surface that text and not the transport's.
	register(s, "refuse", func(_ context.Context, _ *mcp.CallToolRequest, _ input) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "the engine refused"}},
		}, nil, nil
	})

	// IsError with nothing to say. A caller that renders an empty string
	// gives the operator a failure with no reason in it.
	register(s, "refuse-silently", func(_ context.Context, _ *mcp.CallToolRequest, _ input) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{IsError: true}, nil, nil
	})

	// A tool that returns nothing structured at all.
	register(s, "say-nothing", func(_ context.Context, _ *mcp.CallToolRequest, _ input) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hi"}}}, nil, nil
	})

	// Structured content of the wrong shape for what the caller decodes into.
	register(s, "wrong-shape", func(_ context.Context, _ *mcp.CallToolRequest, _ input) (*mcp.CallToolResult, any, error) {
		return nil, map[string]any{"word": []int{1, 2}}, nil
	})

	// Fails outright, so the transport reports it rather than the tool.
	register(s, "explode", func(_ context.Context, _ *mcp.CallToolRequest, _ input) (*mcp.CallToolResult, any, error) {
		return nil, nil, errors.New("boom")
	})

	if err := s.RunDefault(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func register(
	s *mcpserver.Server,
	name string,
	fn func(context.Context, *mcp.CallToolRequest, input) (*mcp.CallToolResult, any, error),
) {
	mcpserver.RegisterTool(s, &mcp.Tool{Name: name, Description: name}, fn)
}
