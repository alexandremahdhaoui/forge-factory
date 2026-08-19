package engineadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Caller interface {
	Call(ctx context.Context, uri, tool string, in, out any) error
}

type MCPCaller struct {
	resolver *Resolver
	version  string
	stderr   io.Writer
}

var _ Caller = (*MCPCaller)(nil)

func NewMCPCaller(sourceDir, version string, stderr io.Writer) *MCPCaller {
	if stderr == nil {
		stderr = io.Discard
	}

	return &MCPCaller{resolver: NewResolver(sourceDir), version: version, stderr: stderr}
}

func (c *MCPCaller) Call(ctx context.Context, uri, tool string, in, out any) error {
	resolved, err := c.resolver.Resolve(uri)
	if err != nil {
		return err
	}

	args := append(append([]string{}, resolved.Args...), "--mcp")

	cmd := exec.CommandContext(ctx, resolved.Path, args...)
	cmd.Env = os.Environ()
	cmd.Stderr = c.stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "forge-factory", Version: c.version}, nil)

	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("connecting to engine %s: %w", uri, err)
	}

	defer func() { _ = session.Close() }()

	args2, err := toArguments(in)
	if err != nil {
		return fmt.Errorf("encoding arguments for %s %s: %w", uri, tool, err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args2})
	if err != nil {
		return fmt.Errorf("calling %s on %s: %w", tool, uri, err)
	}

	if result.IsError {
		return fmt.Errorf("calling %s on %s: %s", tool, uri, firstText(result))
	}

	if out == nil {
		return nil
	}

	if result.StructuredContent == nil {
		return fmt.Errorf("calling %s on %s: engine returned no structured content", tool, uri)
	}

	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return fmt.Errorf("re-encoding result of %s on %s: %w", tool, uri, err)
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding result of %s on %s: %w", tool, uri, err)
	}

	return nil
}

func toArguments(in any) (map[string]any, error) {
	if in == nil {
		return map[string]any{}, nil
	}

	if m, ok := in.(map[string]any); ok {
		return m, nil
	}

	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}

	return m, nil
}

func firstText(result *mcp.CallToolResult) string {
	for _, c := range result.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			return t.Text
		}
	}

	return "engine reported an error with no message"
}
