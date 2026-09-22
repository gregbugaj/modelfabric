package mcp

import (
	"context"
	"os"
	"testing"
)

// Not run by default: it calls a real server. MCP_LIVE_URL=https://... go test -run Live
func TestLiveServer(t *testing.T) {
	url := os.Getenv("MCP_LIVE_URL")
	if url == "" {
		t.Skip("set MCP_LIVE_URL to list a real server's tools")
	}
	c, err := DialHTTP(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		t.Logf("%s", tl.Name)
	}
}
