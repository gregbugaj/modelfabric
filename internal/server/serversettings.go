package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gregbugaj/modelfabric/internal/config"
)

// The dashboard's Server settings: the node's own listeners, authentication,
// CORS and on-demand loading, as LM Studio's Server Settings shows them.
//
// They live in config.json, which a person also edits by hand, so saving
// writes only the keys that changed (config.Update) and keeps the rest. Two
// apply at once — whether loopback asks for a key, and CORS — because the
// node reads them per request. Everything else is read once at startup, and
// the page says so rather than pretend: a saved value the running node is not
// using is reported as waiting for a restart.

// ServerSettings is what the dialog shows and edits. MeshPort is read-only:
// every node must agree on it, so it is not one machine's to change.
type ServerSettings struct {
	Listen        string `json:"listen"`
	RequireAPIKey bool   `json:"require_api_key"`
	// The two MCP switches for /api/v1/chat (chat.go).
	MCPAllowEphemeral  bool     `json:"mcp_allow_ephemeral"`
	MCPAllowConfigured bool     `json:"mcp_allow_configured"`
	PublicListen       string   `json:"public_listen"`
	CORSOrigins        []string `json:"cors_origins"`
	JITLoad            bool     `json:"jit_load"`
	JITTTL             string   `json:"jit_ttl"`
	JITAutoEvict       bool     `json:"jit_auto_evict"`
	MeshAdmin          string   `json:"mesh_admin"`
	WebUI              bool     `json:"web_ui"`
	EngineBind         string   `json:"engine_bind"`
	MeshPort           int      `json:"mesh_port"`
	// Role is "entrypoint" for a node that runs no models. Read-only here:
	// it decides what the node is, not how it is set up.
	Role string `json:"role"`

	// The router: how ModelFabric places every request llm-d does not
	// schedule. Shown on the Routing page, beside llm-d's own controls.
	RateWeightedRouting bool    `json:"rate_weighted_routing"`
	PrefixAffinity      bool    `json:"prefix_affinity"`
	LocalBias           float64 `json:"local_bias"`
	MaxOutputTokens     int     `json:"max_output_tokens"`
	// CacheDiskMiB is the disk tier of the prompt cache; 0 is off.
	CacheDiskMiB int    `json:"cache_disk_mib"`
	CacheDiskDir string `json:"cache_disk_dir"`
}

// liveSettings apply without a restart.
var liveSettings = map[string]bool{"require_api_key": true, "cors_origins": true,
	"mcp_allow_ephemeral": true, "mcp_allow_configured": true}

// engineBindShown is engine_bind as the page shows it: a tailnet address
// written out is the tailnet (config.BindsTailnet), never an address of its
// own to keep or edit.
func engineBindShown(c config.Config) string {
	if c.BindsTailnet() {
		return config.EngineBindTailnet
	}
	return c.EngineBind
}

func settingsOf(c config.Config) ServerSettings {
	cors := c.CORSOrigins
	if cors == nil {
		cors = []string{}
	}
	return ServerSettings{
		Listen: c.Listen, RequireAPIKey: c.RequireAPIKey, PublicListen: c.PublicListen,
		MCPAllowEphemeral: c.MCPAllowEphemeral, MCPAllowConfigured: c.MCPAllowConfigured,
		CORSOrigins: cors, JITLoad: c.JITLoad, JITTTL: c.JITTTL,
		JITAutoEvict: c.JITAutoEvict == nil || *c.JITAutoEvict,
		MeshAdmin:    c.MeshAdmin, WebUI: c.WebUI == nil || *c.WebUI,
		EngineBind: engineBindShown(c), MeshPort: c.MeshPort, Role: c.Role,
		RateWeightedRouting: c.RateWeightedRouting == nil || *c.RateWeightedRouting,
		PrefixAffinity:      c.PrefixAffinity == nil || *c.PrefixAffinity,
		LocalBias:           c.LocalBias, MaxOutputTokens: c.MaxOutputTokens,
		CacheDiskMiB: c.CacheDiskMiB, CacheDiskDir: c.CacheDiskDir,
	}
}

type serverConfig struct {
	mu      sync.Mutex
	path    string
	running ServerSettings
}

// SetConfig tells the server which file it was started from, and with what.
// Without it the settings routes answer 404: a node started from no file has
// nowhere to save to.
func (s *Server) SetConfig(path string, running config.Config) {
	s.conf.mu.Lock()
	defer s.conf.mu.Unlock()
	s.conf.path = path
	s.conf.running = settingsOf(running)
}

type settingsView struct {
	ConfigFile string `json:"config_file"`
	// ConfigShown is ConfigFile with the home directory as ~, for display.
	ConfigShown string         `json:"config_shown"`
	Saved       ServerSettings `json:"saved"`
	Running     ServerSettings `json:"running"`
	// Live names the settings that apply when saved; the rest wait for a
	// restart, which the page shows by comparing Saved with Running.
	Live []string `json:"live"`
	// Tailnet is what Tailscale decides, shown and never edited: this node's
	// tailnet address, and the listener its tailnet devices and peers use.
	Tailnet tailnetView `json:"tailnet"`
}

type tailnetView struct {
	Addr   string `json:"addr,omitempty"`   // empty: Tailscale gave this node none
	Listen string `json:"listen,omitempty"` // tailnet-IP:mesh_port
}

func (s *Server) settingsView() (settingsView, error) {
	s.conf.mu.Lock()
	path, running := s.conf.path, s.conf.running
	s.conf.mu.Unlock()
	saved, err := config.Load(path)
	if err != nil {
		return settingsView{}, err
	}
	live := make([]string, 0, len(liveSettings))
	for k := range liveSettings {
		live = append(live, k)
	}
	sort.Strings(live)
	v := settingsView{ConfigFile: path, ConfigShown: tildePath(path), Saved: settingsOf(saved), Running: running, Live: live}
	if s.m != nil {
		v.Tailnet.Addr = s.m.State().Addr
	}
	v.Tailnet.Listen = s.meshListen
	return v, nil
}

func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if rest, ok := strings.CutPrefix(p, home); ok && (rest == "" || strings.HasPrefix(rest, string(filepath.Separator))) {
		return "~" + rest
	}
	return p
}

func (s *Server) handleGetServerSettings(w http.ResponseWriter, _ *http.Request) {
	if !s.hasConfigFile(w) {
		return
	}
	v, err := s.settingsView()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) hasConfigFile(w http.ResponseWriter) bool {
	s.conf.mu.Lock()
	ok := s.conf.path != ""
	s.conf.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "this node was started without a config file, so there is nowhere to save settings; start it with -config")
		return false
	}
	return true
}

// handlePutServerSettings saves the keys the body names and nothing else.
func (s *Server) handlePutServerSettings(w http.ResponseWriter, r *http.Request) {
	if !s.hasConfigFile(w) {
		return
	}
	var body map[string]json.RawMessage
	if err := decodeBody(w, r, &body, 64<<10); err != nil {
		writeError(w, http.StatusBadRequest, "send a JSON object of the settings to change")
		return
	}
	set, err := settingsToConfig(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.conf.mu.Lock()
	path := s.conf.path
	s.conf.mu.Unlock()
	saved, err := config.Update(path, set)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Applied only after the file took them, so the node never runs a value
	// it could not save.
	if _, ok := body["require_api_key"]; ok {
		s.requireKey.Store(saved.RequireAPIKey)
	}
	if _, ok := body["cors_origins"]; ok {
		s.SetCORS(saved.CORSOrigins)
	}
	if _, ok := body["mcp_allow_ephemeral"]; ok {
		s.mcpEphemeral.Store(saved.MCPAllowEphemeral)
	}
	if _, ok := body["mcp_allow_configured"]; ok {
		s.mcpConfigured.Store(saved.MCPAllowConfigured)
	}
	s.conf.mu.Lock()
	s.conf.running.RequireAPIKey = s.requireKey.Load()
	s.conf.running.MCPAllowEphemeral, s.conf.running.MCPAllowConfigured = s.mcpEphemeral.Load(), s.mcpConfigured.Load()
	if p := s.cors.Load(); p != nil {
		s.conf.running.CORSOrigins = append([]string(nil), *p...)
	} else {
		s.conf.running.CORSOrigins = []string{}
	}
	s.conf.mu.Unlock()

	v, err := s.settingsView()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// settingsToConfig turns a partial body into config keys, checking each the
// way the node will when it starts. Empty values become nil — the key is
// removed and the default applies — so a cleared field does not leave
// "public_listen": "" behind to puzzle the next person reading the file.
func settingsToConfig(body map[string]json.RawMessage) (map[string]any, error) {
	set := map[string]any{}
	for k, raw := range body {
		switch k {
		case "listen", "public_listen", "engine_bind", "jit_ttl", "mesh_admin":
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, fmt.Errorf("%s: want a string", k)
			}
			if err := checkString(k, v); err != nil {
				return nil, err
			}
			if v == "" {
				set[k] = nil
			} else {
				set[k] = v
			}
		case "local_bias":
			var v float64
			if err := json.Unmarshal(raw, &v); err != nil || v < 0 || v > 100 {
				return nil, errors.New("local_bias: a number of requests from 0 to 100; 0 means no preference for this node's own engines")
			}
			// Written even when 0: the default is 0.5, so removing a 0 would
			// quietly bring the bias back.
			set[k] = v
		case "max_output_tokens":
			var v int
			if err := json.Unmarshal(raw, &v); err != nil || v < 0 {
				return nil, errors.New("max_output_tokens: a number of tokens, or 0 for no ceiling")
			}
			// Written even when 0: here 0 means "no ceiling", while an absent
			// key means the 16384 default. Removing it would turn a choice
			// to have no ceiling back into the default one.
			set[k] = v
		case "cache_disk_mib":
			var v int
			if err := json.Unmarshal(raw, &v); err != nil || v < 0 {
				return nil, errors.New("cache_disk_mib: a size in MiB, or 0 to turn the disk cache off")
			}
			set[k] = zeroIsUnset(v)
		case "cache_disk_dir":
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, errors.New("cache_disk_dir: want a path")
			}
			if v != "" && !filepath.IsAbs(v) {
				return nil, fmt.Errorf("cache_disk_dir: %q is not an absolute path", v)
			}
			if v == "" {
				set[k] = nil
			} else {
				set[k] = v
			}
		case "rate_weighted_routing", "prefix_affinity", "require_api_key", "jit_load", "jit_auto_evict", "web_ui",
			"mcp_allow_ephemeral", "mcp_allow_configured":
			var v bool
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, fmt.Errorf("%s: want true or false", k)
			}
			set[k] = v
		case "cors_origins":
			var v []string
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, fmt.Errorf("cors_origins: want a list of origins")
			}
			if err := config.ValidateCORSOrigins(v); err != nil {
				return nil, err
			}
			if len(v) == 0 {
				set[k] = nil
			} else {
				set[k] = v
			}
		case "mesh_port":
			return nil, errors.New("mesh_port is the same on every node, so it is not changed from one node's page; edit config.json on all of them together")
		default:
			return nil, fmt.Errorf("%q is not a server setting this page can change", k)
		}
	}
	return set, nil
}

func checkString(k, v string) error {
	switch k {
	case "listen":
		return checkHostPort(k, v, false)
	case "public_listen":
		return checkHostPort(k, v, true)
	case "engine_bind":
		// Loopback or the tailnet, and nothing typed: the tailnet address is
		// Tailscale's to assign, and the tailnet is how this system
		// distributes work. An address set by hand in config.json still
		// works; it is just not something this page writes.
		if v != "" && v != config.EngineBindTailnet {
			return fmt.Errorf("engine_bind: %q; use \"\" for this machine only or %q for this node's tailnet address", v, config.EngineBindTailnet)
		}
	case "mesh_admin":
		if v != "" && v != "same-owner" && v != "off" {
			return fmt.Errorf("mesh_admin: %q; use same-owner or off", v)
		}
	}
	return nil // jit_ttl is checked by config.Update, with the node's own parser
}

func checkHostPort(k, v string, emptyOK bool) error {
	if v == "" {
		if emptyOK {
			return nil
		}
		return fmt.Errorf("%s: an address is required, such as 127.0.0.1:1234", k)
	}
	_, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("%s: %q is not host:port, such as 127.0.0.1:1234", k, v)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s: port %q is not 1-65535", k, port)
	}
	return nil
}

// zeroIsUnset turns a zero into "remove the key", where zero and absent mean
// the same thing.
func zeroIsUnset[T int | float64](v T) any {
	if v == 0 {
		return nil
	}
	return v
}
