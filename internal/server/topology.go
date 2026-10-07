package server

import (
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/osproc"
)

// Topology describes this node's listeners, access rules and engines.
// The dashboard combines snapshots from manageable nodes.

type Listener struct {
	Name    string `json:"name"` // front | mesh | public | llmd
	Addr    string `json:"addr"`
	Scope   string `json:"scope"`
	Auth    string `json:"auth"`
	Serves  string `json:"serves"`
	Enabled bool   `json:"enabled"`
}

type TopoEngine struct {
	ID          string  `json:"id"`
	Model       string  `json:"model"`
	Addr        string  `json:"addr"`
	Port        int     `json:"port"`
	Runtime     string  `json:"runtime"`
	Slots       int64   `json:"slots"`
	Inflight    int64   `json:"inflight"`
	PrefillTokS float64 `json:"prefill_tok_s,omitempty"`
	Healthy     bool    `json:"healthy"`
	Context     int     `json:"context,omitempty"`
	// KVUsage is the share of this engine's KV pool held by requests in
	// flight, -1 when the engine cannot be asked. See internal/engineshim for
	// what it does and does not count.
	KVUsage float64 `json:"kv_usage"`
}

type TopoGPU struct {
	Name   string `json:"name"`
	VRAMMB int    `json:"vram_mb"`
}

type Topology struct {
	Node      string       `json:"node"`
	Role      string       `json:"role"` // gpu | entrypoint | router
	TailnetIP string       `json:"tailnet_ip,omitempty"`
	Version   string       `json:"version,omitempty"`
	Platform  string       `json:"platform,omitempty"`
	OSVersion string       `json:"os_version,omitempty"`
	Preferred string       `json:"preferred,omitempty"`
	GPUs      []TopoGPU    `json:"gpus,omitempty"`
	Listeners []Listener   `json:"listeners"`
	LLMD      *TopoLLMD    `json:"llmd,omitempty"`
	Engines   []TopoEngine `json:"engines"`
	Peers     []TopoPeer   `json:"peers"`
}

type TopoLLMD struct {
	State     string   `json:"state"`
	Model     string   `json:"model"`
	Profile   string   `json:"profile"`
	Listen    string   `json:"listen"`
	Endpoints []string `json:"endpoints"`
}

type TopoPeer struct {
	Node    string `json:"node"`
	Addr    string `json:"addr"`
	BaseURL string `json:"base_url"`
	Alive   bool   `json:"alive"`
}

func (s *Server) SetMeshListen(addr string) { s.meshListen = addr }

func (s *Server) handleTopology(w http.ResponseWriter, _ *http.Request) {
	self := s.m.State()
	t := Topology{Node: self.Node, Role: "router", TailnetIP: self.Addr, Version: s.build.Version,
		Platform: mesh.Platform(), OSVersion: osproc.OSVersion(),
		Preferred: s.m.Preferred(), Engines: []TopoEngine{}, Peers: []TopoPeer{}}
	if s.sup != nil {
		t.Role = "gpu"
		if s.sup.Entrypoint() {
			t.Role = "entrypoint"
		}
		for _, g := range s.hardware().GPUs {
			t.GPUs = append(t.GPUs, TopoGPU{Name: g.Name, VRAMMB: g.MemoryMB})
		}
	}

	frontAuth := "none (loopback)"
	if s.requireKey.Load() {
		frontAuth = "API key"
	}
	front := "inference, dashboard, management"
	if s.ui == nil {
		front = "inference, management"
	}
	t.Listeners = append(t.Listeners, Listener{Name: "front", Addr: s.frontListen, Scope: "loopback", Auth: frontAuth, Serves: front, Enabled: true})
	if s.meshListen != "" {
		admin := "; management from the owner's devices"
		if s.meshAdmin != "" && s.meshAdmin != "same-owner" {
			admin = ""
		}
		t.Listeners = append(t.Listeners, Listener{Name: "mesh", Addr: s.meshListen, Scope: "tailnet",
			Auth: "tailnet membership", Serves: "inference, mesh state" + admin, Enabled: true})
	}
	if s.publicListen != "" {
		t.Listeners = append(t.Listeners, Listener{Name: "public", Addr: s.publicListen, Scope: "public (via TLS proxy)",
			Auth: "API key, always", Serves: "inference only", Enabled: true})
	}
	if s.llmd != nil {
		ls := s.llmd.Status()
		if ls.State != "" && ls.State != "disabled" {
			t.LLMD = &TopoLLMD{State: ls.State, Model: ls.Model, Profile: ls.Profile, Listen: strings.TrimSuffix(strings.TrimPrefix(ls.URL, "http://"), "/v1"), Endpoints: ls.Endpoints}
			t.Listeners = append(t.Listeners, Listener{Name: "llmd", Addr: t.LLMD.Listen, Scope: "loopback (internal)",
				Auth: "none (loopback)", Serves: "llm-d Envoy → EPP for " + ls.Model, Enabled: ls.State == "running"})
		}
	}

	insts := map[string]int{}
	ports := map[string]int{}
	addrs := map[string]string{}
	kv := map[string]float64{}
	for _, i := range self.Instances {
		ports[i.ID], addrs[i.ID] = i.Port, i.Address
		kv[i.ID] = i.KVUsage
	}
	if s.sup != nil {
		for _, i := range s.sup.Instances() {
			insts[i.ID] = i.Config.ContextLength
		}
	}
	for _, e := range self.Engines {
		te := TopoEngine{ID: e.Name, Healthy: e.Healthy, Inflight: e.Inflight, Slots: e.Slots, PrefillTokS: e.PrefillTokS,
			Port: ports[e.Name], Addr: addrs[e.Name], Context: insts[e.Name], KVUsage: -1}
		if u, ok := kv[e.Name]; ok {
			te.KVUsage = u
		}
		if len(e.Models) > 0 {
			te.Model = e.Models[0]
		}
		for _, i := range self.Instances {
			if i.ID == e.Name {
				te.Runtime = i.Runtime
			}
		}
		if te.Port == 0 {
			if u := strings.TrimPrefix(strings.TrimPrefix(engineBase(s, e.Name), "http://"), "https://"); u != "" {
				if h, p, err := net.SplitHostPort(strings.TrimSuffix(u, "/v1")); err == nil {
					te.Addr = h
					te.Port, _ = strconv.Atoi(p)
				}
			}
		}
		t.Engines = append(t.Engines, te)
	}
	sort.Slice(t.Engines, func(i, j int) bool { return t.Engines[i].Model < t.Engines[j].Model })
	for _, p := range s.m.Peers() {
		t.Peers = append(t.Peers, TopoPeer{Node: p.Node, Addr: p.Addr, BaseURL: p.BaseURL, Alive: p.Alive})
	}
	writeJSON(w, http.StatusOK, t)
}

func engineBase(s *Server, name string) string {
	for _, m := range s.m.State().Models {
		for _, c := range s.m.Candidates(m, true) {
			if c.Name == name {
				return c.BaseURL
			}
		}
	}
	return ""
}
