package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Server is one entry of mcp.json: a remote server (url) or a local one
// started as a process (command). The format is the one LM Studio, Cursor and
// Claude Desktop share, so a file written for those works here.
type Server struct {
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Load reads mcp.json. A file that is not there is no servers, not an error.
func Load(path string) (map[string]Server, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]Server{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Servers map[string]Server `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, s := range f.Servers {
		if (s.URL == "") == (s.Command == "") {
			return nil, fmt.Errorf("%s: server %q needs either a url or a command", path, name)
		}
	}
	if f.Servers == nil {
		f.Servers = map[string]Server{}
	}
	return f.Servers, nil
}

// Names lists the servers in order, for messages.
func Names(m map[string]Server) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Dial opens a session with the server.
func (s Server) Dial(ctx context.Context) (Client, error) {
	if s.URL != "" {
		return DialHTTP(ctx, s.URL, s.Headers)
	}
	return DialStdio(ctx, s.Command, s.Args, s.Env)
}
