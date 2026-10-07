package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func answer(msg []byte) []byte {
	var in struct {
		ID     *int64 `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	json.Unmarshal(msg, &in)
	if in.ID == nil {
		return nil
	}
	var result any
	switch in.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}}
	case "tools/list":
		result = map[string]any{"tools": []Tool{{Name: "echo", Description: "says it back", InputSchema: json.RawMessage(`{"type":"object"}`)}, {Name: "fail"}}}
	case "tools/call":
		if in.Params.Name == "fail" {
			result = map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": "no such city"}}}
		} else {
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": fmt.Sprint("echo: ", in.Params.Arguments["text"])}}}
		}
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *in.ID, "result": result})
	return b
}

// The test binary doubles as a stdio MCP server, so DialStdio is tested
// against a real child process and real pipes.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_TEST_SERVER") == "1" {
		fmt.Println("a log line a careless server prints to stdout")
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if out := answer(sc.Bytes()); out != nil {
				fmt.Println(string(out))
			}
		}
		return
	}
	os.Exit(m.Run())
}

func httpServer(t *testing.T, sse bool, seen *http.Header) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			return
		}
		if seen != nil {
			*seen = r.Header.Clone()
		}
		var msg json.RawMessage
		json.NewDecoder(r.Body).Decode(&msg)
		out := answer(msg)
		w.Header().Set("Mcp-Session-Id", "s1")
		if out == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", out)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestClientListsAndCallsTools(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var seen http.Header
	tests := []struct {
		name string
		dial func() (Client, error)
	}{
		{name: "over HTTP, answered as JSON", dial: func() (Client, error) {
			return DialHTTP(ctx, httpServer(t, false, &seen), map[string]string{"Authorization": "Bearer t"})
		}},
		{name: "over HTTP, answered on an event stream", dial: func() (Client, error) {
			return DialHTTP(ctx, httpServer(t, true, nil), nil)
		}},
		{name: "over a child process's stdio", dial: func() (Client, error) {
			return DialStdio(ctx, os.Args[0], nil, map[string]string{"MCP_TEST_SERVER": "1"})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := tc.dial()
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			tools, err := c.ListTools(ctx)
			if err != nil || len(tools) != 2 || tools[0].Name != "echo" {
				t.Fatalf("tools %+v, %v", tools, err)
			}
			out, err := c.CallTool(ctx, "echo", json.RawMessage(`{"text":"hi"}`))
			if err != nil || out != "echo: hi" {
				t.Fatalf("echo: %q, %v", out, err)
			}
			// A tool that ran and failed is an error carrying what it said,
			// so the model can be told why.
			if _, err := c.CallTool(ctx, "fail", nil); err == nil || !strings.Contains(err.Error(), "no such city") {
				t.Fatalf("fail: %v", err)
			}
		})
	}
	if seen.Get("Authorization") != "Bearer t" || seen.Get("Mcp-Session-Id") != "s1" {
		t.Fatalf("the caller's header or the session id was not sent: %v", seen)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name, file, wantErr string
		want                int
	}{
		{name: "a missing file is no servers"},
		{name: "remote and local servers", file: `{"mcpServers":{"hf":{"url":"https://example.com/mcp"},"pw":{"command":"npx","args":["x"]}}}`, want: 2},
		{name: "a server with neither is refused by name", file: `{"mcpServers":{"bad":{}}}`, wantErr: `"bad" needs either a url or a command`},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, fmt.Sprint(i, ".json"))
			if tc.file != "" {
				os.WriteFile(p, []byte(tc.file), 0o600)
			}
			got, err := Load(p)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v", err)
				}
				return
			}
			if err != nil || len(got) != tc.want {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}
