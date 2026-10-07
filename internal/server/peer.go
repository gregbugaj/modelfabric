package server

import (
	"fmt"
	"github.com/gregbugaj/modelfabric/internal/tsid"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/gregbugaj/modelfabric/internal/router"
)

// PeerHandler exposes probes, inference and read-only mesh state on the
// tailnet port. Management and metrics additionally require same-owner access;
// tailnet membership alone does not authorize reconfiguration.
func (s *Server) PeerHandler() http.Handler {
	full := s.Handler()
	// Same-owner browsers still need cross-site request protection.
	return s.browserSafe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if peerAllowed(r.Method, r.URL.Path) {
			// Inference is allowed; loading a model on demand is not. JIT
			// would let any tailnet member put models on this GPU.
			full.ServeHTTP(w, r.WithContext(router.WithoutJIT(r.Context())))
			return
		}
		// Management and metrics require the same Tailscale owner. Never expose
		// onward proxying here; remote management is limited to one hop.
		if strings.HasPrefix(r.URL.Path, "/api/v1/") && !strings.HasPrefix(r.URL.Path, "/api/v1/nodes/") ||
			r.URL.Path == "/metrics" {
			if ok, why := s.sameOwner(r); ok {
				full.ServeHTTP(w, r.WithContext(router.WithoutJIT(r.Context())))
				return
			} else if why != "" {
				writeError(w, http.StatusForbidden, why)
				return
			}
		}
		writeError(w, http.StatusForbidden,
			"not available over the mesh; use this node's loopback address for management")
	}))
}

// sameOwner reports whether the tailnet device making r belongs to this
// node's owner (see tsid). why explains a refusal.
func (s *Server) sameOwner(r *http.Request) (ok bool, why string) {
	// Accept only recognized mesh_admin values; typos must not enable access.
	switch s.meshAdmin {
	case "", "same-owner":
		// the default: devices Tailscale reports as this node's owner
	case "off":
		return false, "management over the mesh is off on this node (mesh_admin)"
	default:
		return false, fmt.Sprintf("mesh_admin is set to %q, which ModelFabric does not recognise; management over the mesh is refused (use \"same-owner\" or \"off\")", s.meshAdmin)
	}
	if s.ids == nil {
		return false, "management over the mesh is off on this node (mesh_admin)"
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false, "cannot tell who is asking"
	}
	self, err := s.ids.Self(r.Context())
	if err != nil {
		return false, "this node cannot read its own Tailscale identity: " + err.Error()
	}
	peer, err := s.ids.WhoIs(r.Context(), host)
	if err != nil {
		return false, "Tailscale does not identify " + host + ": " + err.Error()
	}
	if !tsid.SameOwner(self, peer) {
		return false, fmt.Sprintf("%s (%s) is not owned by this node's owner; only the owner's devices manage it over the mesh", peer.Device, ownerName(peer))
	}
	return true, ""
}

func ownerName(id tsid.Identity) string {
	if len(id.Tags) > 0 {
		return strings.Join(id.Tags, ",")
	}
	return id.Login
}

// SetMeshAdmin wires Tailscale identity lookups and the mesh_admin policy
// ("same-owner", the default, or "off").
func (s *Server) SetMeshAdmin(ids *tsid.Resolver, policy string) { s.ids, s.meshAdmin = ids, policy }

// handleNodeProxy maps /api/v1/nodes/{node}/<path> to that node's /api/v1/<path>.
// It is loopback-only; the peer applies its same-owner check.
func (s *Server) handleNodeProxy(w http.ResponseWriter, r *http.Request) {
	node, rest := r.PathValue("node"), "/api/v1/"+r.PathValue("rest")
	if node == s.m.State().Node {
		r.URL.Path = rest
		s.Handler().ServeHTTP(w, r)
		return
	}
	base := ""
	for _, p := range s.m.Peers() {
		if p.Node == node && p.Alive {
			base = p.BaseURL
		}
	}
	if base == "" {
		writeError(w, http.StatusNotFound, "no live node named "+node+" in the mesh")
		return
	}
	target, err := url.Parse(base)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = rest
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target.Host
			// The peer authorizes by tailnet identity, so nothing the caller
			// sent as a credential should travel with the request: a browser
			// or CLI can carry a cookie or an X-Api-Key meant for this node.
			for _, h := range []string{"Authorization", "Cookie", "X-Api-Key", "Api-Key", "Proxy-Authorization"} {
				pr.Out.Header.Del(h)
			}
			// The browser's own request was checked here, by this node. To the
			// peer, this node's dashboard origin is a foreign one: forwarded,
			// it would refuse the dashboard's every action as cross-site.
			dropBrowserHeaders(pr.Out.Header)
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeError(w, http.StatusBadGateway, node+" did not answer: "+err.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

func dropBrowserHeaders(h http.Header) {
	for k := range h {
		if k == "Origin" || k == "Referer" || strings.HasPrefix(k, "Sec-Fetch-") {
			h.Del(k)
		}
	}
}

func peerAllowed(method, path string) bool {
	switch {
	case method == http.MethodGet && (path == "/z/state" || path == "/z/mesh" ||
		path == "/z/endpoints.yaml" || path == "/healthz" || path == "/v1/models" ||
		strings.HasPrefix(path, "/v1/models/")):
		return true
	case method == http.MethodPost && strings.HasPrefix(path, "/v1/"):
		// Inference only: every /v1 POST route is an OpenAI/Anthropic-style
		// generation, embedding, rerank or transcription call.
		return true
	}
	return false
}
