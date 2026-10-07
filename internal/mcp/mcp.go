// Package mcp lists and calls Model Context Protocol tools over streamable
// HTTP or child-process stdio, using JSON-RPC and the standard library.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// protocolVersion is the revision asked for. A server answers with the one
// it speaks; tools/list and tools/call have not changed shape across them.
const protocolVersion = "2025-06-18"

// maxMessage bounds one message from a server. A tool's output goes into a
// model's context, so anything near this is already useless.
const maxMessage = 8 << 20

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

type Client interface {
	ListTools(ctx context.Context) ([]Tool, error)
	// CallTool runs a tool and returns its output as text. A tool that ran
	// and reported failure is an error carrying what it said.
	CallTool(ctx context.Context, name string, args json.RawMessage) (string, error)
	Close() error
}

type transport interface {
	// roundTrip sends one message. For a request (wantReply) it returns the
	// reply with that id.
	roundTrip(ctx context.Context, id int64, msg []byte, wantReply bool) (json.RawMessage, error)
	close() error
}

type client struct {
	t    transport
	next atomic.Int64
}

type rpcReply struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *client) call(ctx context.Context, method string, params any, out any) error {
	id := c.next.Add(1)
	msg, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	raw, err := c.t.roundTrip(ctx, id, msg, true)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	var r rpcReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("%s: unreadable reply: %w", method, err)
	}
	if r.Error != nil {
		return fmt.Errorf("%s: %s", method, r.Error.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Result, out)
}

func (c *client) initialize(ctx context.Context) error {
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "modelfabric", "version": "1"},
	}, nil)
	if err != nil {
		return err
	}
	note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	_, err = c.t.roundTrip(ctx, 0, note, false)
	return err
}

func (c *client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	// Bounded: a server that never ends its pagination must not hold a request.
	for page := 0; page < 20; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var out struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Tools...)
		if cursor = out.NextCursor; cursor == "" {
			break
		}
	}
	return all, nil
}

func (c *client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
		IsError    bool            `json:"isError"`
	}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &out); err != nil {
		return "", err
	}
	var parts []string
	for _, p := range out.Content {
		switch p.Type {
		case "text":
			parts = append(parts, p.Text)
		default:
			// An image or a resource cannot go into a text tool result;
			// saying so beats dropping it silently.
			parts = append(parts, "["+p.Type+" content omitted]")
		}
	}
	text := strings.Join(parts, "\n")
	if text == "" && len(out.Structured) > 0 {
		text = string(out.Structured)
	}
	if out.IsError {
		return "", errors.New(or(text, "the tool reported an error"))
	}
	return text, nil
}

func (c *client) Close() error { return c.t.close() }

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// DialHTTP opens a session with a streamable-HTTP server. headers are sent on
// every request: they are how a caller authenticates to it.
func DialHTTP(ctx context.Context, url string, headers map[string]string) (Client, error) {
	c := &client{t: &httpTransport{url: url, headers: headers, hc: &http.Client{Timeout: 5 * time.Minute}}}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

type httpTransport struct {
	url     string
	headers map[string]string
	hc      *http.Client
	mu      sync.Mutex
	session string
}

func (t *httpTransport) roundTrip(ctx context.Context, id int64, msg []byte, wantReply bool) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	t.mu.Lock()
	if t.session != "" {
		req.Header.Set("Mcp-Session-Id", t.session)
	}
	t.mu.Unlock()
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		t.mu.Lock()
		t.session = s
		t.mu.Unlock()
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if !wantReply {
		return nil, nil
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return io.ReadAll(io.LimitReader(resp.Body, maxMessage))
	}
	// A server may answer on an event stream, with its own requests and
	// notifications before the reply. Only the reply to this id is wanted.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimPrefix(v, " "))
			continue
		}
		if line != "" || len(data) == 0 {
			continue
		}
		raw := []byte(strings.Join(data, "\n"))
		data = data[:0]
		if isReply(raw, id) {
			return raw, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("the server closed its stream without answering")
}

func isReply(raw []byte, id int64) bool {
	var r struct {
		ID     *int64 `json:"id"`
		Method string `json:"method"`
	}
	return json.Unmarshal(raw, &r) == nil && r.Method == "" && r.ID != nil && *r.ID == id
}

func (t *httpTransport) close() error {
	t.mu.Lock()
	s := t.session
	t.mu.Unlock()
	if s == "" {
		return nil
	}
	// Ending the session is a courtesy; a server that refuses loses nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.url, nil)
	if err != nil {
		return nil
	}
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Mcp-Session-Id", s)
	if resp, err := t.hc.Do(req); err == nil {
		resp.Body.Close()
	}
	return nil
}

// DialStdio starts a server as a child process and speaks to it over its
// stdin and stdout, one JSON message per line. The process is killed when
// ctx ends or the client is closed.
func DialStdio(ctx context.Context, command string, args []string, env map[string]string) (Client, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", command, err)
	}
	t := &stdioTransport{cmd: cmd, in: in, lines: make(chan []byte)}
	go func() {
		defer close(t.lines)
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), maxMessage)
		for sc.Scan() {
			t.lines <- append([]byte(nil), sc.Bytes()...)
		}
	}()
	c := &client{t: t}
	if err := c.initialize(ctx); err != nil {
		t.close()
		return nil, err
	}
	return c, nil
}

type stdioTransport struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan []byte
	mu    sync.Mutex // one request at a time: replies are read in order
}

func (t *stdioTransport) roundTrip(ctx context.Context, id int64, msg []byte, wantReply bool) (json.RawMessage, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.in.Write(append(msg, '\n')); err != nil {
		return nil, err
	}
	if !wantReply {
		return nil, nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case line, ok := <-t.lines:
			if !ok {
				return nil, errors.New("the server process exited without answering")
			}
			// Anything that is not the reply (a log line a server wrongly
			// prints to stdout, a notification) is skipped.
			if isReply(line, id) {
				return line, nil
			}
		}
	}
}

func (t *stdioTransport) close() error {
	t.in.Close()
	if t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}
	go func() {
		for range t.lines {
		}
	}()
	t.cmd.Wait()
	return nil
}
