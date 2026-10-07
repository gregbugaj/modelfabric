package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/chatapi"
	"github.com/gregbugaj/modelfabric/internal/mcp"
	"github.com/gregbugaj/modelfabric/internal/router"
	"github.com/gregbugaj/modelfabric/internal/supervisor"
)

// POST /api/v1/chat is LM Studio's native chat endpoint (internal/chatapi):
// stored conversations continued by response_id, and MCP servers' tools run
// here for the model.
//
// It is the one route under /api/v1 an app calls, so it is treated as
// inference everywhere a listener decides: a key protects it, and the public
// listener serves it. On the tailnet listener it stays with the rest of
// /api/v1, for the owner's own devices only.

// chatConversations bounds what the node remembers: memory only, gone on
// restart, never written to disk.
const (
	chatConversations = 1000
	chatLifetime      = 24 * time.Hour
)

func (s *Server) chatRunner() *chatapi.Runner {
	s.chatOnce.Do(func() {
		s.chat = &chatapi.Runner{
			Infer:           s.chatInfer,
			InferResponses:  s.responsesInfer,
			Store:           chatapi.NewStore(chatConversations, chatLifetime),
			ResponseStore:   chatapi.NewStore(chatConversations, chatLifetime),
			AllowEphemeral:  s.mcpEphemeral.Load,
			AllowConfigured: s.mcpConfigured.Load,
			Servers: func() (map[string]mcp.Server, error) {
				if s.mcpFile == "" {
					return map[string]mcp.Server{}, nil
				}
				return mcp.Load(s.mcpFile)
			},
			Dial: func(ctx context.Context, srv mcp.Server) (mcp.Client, error) { return srv.Dial(ctx) },
		}
		if s.sup != nil && s.sup.JITEnabled() {
			s.chat.Load = s.chatLoad
		}
	})
	return s.chat
}

// chatLoad loads a model for /api/v1/chat when nothing in the mesh serves it,
// so the load can take the request's context_length and be reported as
// model_load events. Left to the router, the load would happen inside the
// first model call, at the default context and unseen. A request that may
// not load on demand (one from the tailnet) is left to the router to refuse.
func (s *Server) chatLoad(ctx context.Context, model string, contextLength int, started func()) (bool, error) {
	if !router.JITAllowed(ctx) || len(s.m.Candidates(model, false)) > 0 {
		return false, nil
	}
	if _, err := s.sup.Catalog().Resolve(model); err != nil {
		return false, nil // not downloaded here: the front door's 404 says so
	}
	started()
	err := s.sup.EnsureLoadedWith(ctx, model, 0, contextLength)
	if errors.Is(err, supervisor.ErrJITDisabled) || errors.Is(err, supervisor.ErrNotInCatalog) {
		return false, nil
	}
	return err == nil, err
}

// chatInfer sends one model call through this node's own front door, so it is
// routed, scheduled, counted and logged like any app's request.
func (s *Server) chatInfer(ctx context.Context, body []byte) (io.ReadCloser, string, error) {
	base, err := s.frontBase()
	if err != nil {
		return nil, "", &chatapi.Error{Status: http.StatusServiceUnavailable, Msg: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.requireKey.Load() && s.apiKey != nil {
		key, err := s.apiKey()
		if err != nil {
			return nil, "", &chatapi.Error{Status: http.StatusInternalServerError, Msg: "the front door asks for a key and this node's cannot be read: " + err.Error()}
		}
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", &chatapi.Error{Status: http.StatusBadGateway, Msg: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// The front door's own error, with its status: "no node serves this
		// model" is the caller's 404, not a failure of this endpoint.
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := strings.TrimSpace(string(b))
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		return nil, "", &chatapi.Error{Status: resp.StatusCode, Msg: msg}
	}
	return resp.Body, resp.Header.Get("X-Fabric-Node"), nil
}

// innerHeader marks Responses-layer calls for routing without recursion.
// Only requests authenticated with the node key may set it.
const innerHeader = "X-Fabric-Inner"

func (s *Server) layered(r *http.Request) bool {
	// Without a front door of its own to call there is nothing to layer on,
	// and the request is routed as it always was.
	if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || s.frontListen == "" {
		return false
	}
	return !(r.Header.Get(innerHeader) != "" && s.authorized(r))
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	s.chatRunner().Responses(r.Context(), raw, w)
}

// responsesInfer sends one /v1/responses call through the front door, marked
// so it is routed rather than layered again.
func (s *Server) responsesInfer(ctx context.Context, body []byte) (*chatapi.Upstream, error) {
	base, err := s.frontBase()
	if err != nil {
		return nil, &chatapi.Error{Status: http.StatusServiceUnavailable, Msg: err.Error()}
	}
	if s.apiKey == nil {
		return nil, &chatapi.Error{Status: http.StatusInternalServerError, Msg: "this node has no key to call its own front door with"}
	}
	key, err := s.apiKey()
	if err != nil {
		return nil, &chatapi.Error{Status: http.StatusInternalServerError, Msg: "this node's key cannot be read: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set(innerHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, &chatapi.Error{Status: http.StatusBadGateway, Msg: err.Error()}
	}
	return &chatapi.Upstream{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatapi.Request
	// Images arrive inline as data URLs, so the limit is an image's, not a
	// prompt's.
	if err := decodeBody(w, r, &req, 64<<20); err != nil {
		writeError(w, http.StatusBadRequest, `send {"model": ..., "input": ...}: `+err.Error())
		return
	}
	run := s.chatRunner()
	if !req.Stream {
		resp, node, err := run.Run(r.Context(), req, nil)
		if err != nil {
			chatError(w, err)
			return
		}
		if node != "" {
			w.Header().Set("X-Fabric-Node", node)
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Streaming: headers go out with the first event, so an error after that
	// is an event too.
	started := false
	flusher, _ := w.(http.Flusher)
	emit := func(event string, data any) {
		if !started {
			started = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			// Tells nginx not to hold the stream back; without it tokens
			// arrive all at once at the end.
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
		}
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if _, _, err := run.Run(r.Context(), req, emit); err != nil {
		if !started {
			chatError(w, err)
			return
		}
		emit("error", map[string]any{"type": "error", "error": map[string]string{"type": "modelfabric_error", "message": err.Error()}})
	}
}

func chatError(w http.ResponseWriter, err error) {
	var e *chatapi.Error
	if errors.As(err, &e) {
		writeError(w, e.Status, e.Msg)
		return
	}
	writeError(w, http.StatusBadGateway, err.Error())
}
