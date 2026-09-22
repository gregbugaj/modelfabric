package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/discovery"
	"github.com/gregbugaj/modelfabric/internal/mesh"
	"github.com/gregbugaj/modelfabric/internal/router"
)

// This exercises Envoy's real formatter and overwrite behavior. EPP v0.10.0
// cannot bind only to loopback, so this test substitutes a static destination
// and omits ext_proc; it does not claim to verify the EPP scheduling chain.
func TestEnvoyAttributionIntegration(t *testing.T) {
	binary := os.Getenv("MFSH_TEST_ENVOY")
	if binary == "" {
		t.Skip("set MFSH_TEST_ENVOY to the Envoy binary")
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set(upstreamHeader, "192.0.2.1:9999")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"m","choices":[{"message":{"content":"isolated backend"}}]}`)
	}))
	t.Cleanup(backend.Close)
	backendURL, _ := url.Parse(backend.URL)
	backendPort := backend.Listener.Addr().(*net.TCPAddr).Port
	var listeners []net.Listener
	for range 2 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		listeners = append(listeners, ln)
	}
	listenPort, adminPort := listeners[0].Addr().(*net.TCPAddr).Port, listeners[1].Addr().(*net.TCPAddr).Port
	cfg := string(discovery.EnvoyConfig(discovery.EnvoyOptions{ListenPort: listenPort, AdminPort: adminPort}))
	start, end := strings.Index(cfg, "                  - name: envoy.filters.http.ext_proc"), strings.Index(cfg, "                  - name: envoy.filters.http.router")
	if start < 0 || end <= start {
		t.Fatal("generated Envoy filter layout changed; update the integration fixture")
	}
	cfg = cfg[:start] + cfg[end:]
	start = strings.Index(cfg, "    - name: original_destination_cluster")
	if start < 0 {
		t.Fatal("generated Envoy destination cluster missing")
	}
	cfg = cfg[:start] + fmt.Sprintf(`    - name: original_destination_cluster
      type: STATIC
      connect_timeout: 1s
      load_assignment:
        cluster_name: original_destination_cluster
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: {address: 127.0.0.1, port_value: %d}
`, backendPort)
	dir := t.TempDir()
	configPath, logPath := filepath.Join(dir, "envoy.yaml"), filepath.Join(dir, "envoy.log")
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	for _, ln := range listeners {
		ln.Close()
	}
	cmd := exec.Command(binary, "-c", configPath, "--disable-hot-restart", "--concurrency", "1", "--log-level", "warn")
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-exited
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Logf("Envoy log:\n%s", data)
		}
	})
	base, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", listenPort))
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := client.Post(base.String()+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.Header.Get(upstreamHeader) != backendURL.Host || len(resp.Header.Values(upstreamHeader)) != 1 {
				t.Fatalf("Envoy status %d, upstream headers %q; want 200 and only %q", resp.StatusCode, resp.Header.Values(upstreamHeader), backendURL.Host)
			}
			break
		}
		select {
		case <-exited:
			t.Fatal("Envoy exited before serving a request")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("Envoy did not start: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	for _, tc := range []struct {
		name    string
		handler func(*Server) http.Handler
	}{
		{"front", (*Server).FrontHandler}, {"public", (*Server).PublicHandler},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFrontFixture(t, true, false)
			f.srv.scheduler = func(string) (*url.URL, bool) { return base, true }
			f.srv.m.SetInstanceProvider(func() []mesh.InstanceState {
				return []mesh.InstanceState{{ID: "envoy-backend", Address: "127.0.0.1", MetricsPort: backendPort}}
			})
			rec := call(tc.handler(f.srv), testKey, "")
			if rec.Code != http.StatusOK || rec.Header().Get(router.NodeHeader) != "self" || rec.Header().Get(router.EngineHeader) != "envoy-backend" || rec.Header().Get(upstreamHeader) != "" || rec.Header().Get("X-Fabric-Via") != "llm-d" {
				t.Fatalf("status %d, headers %v", rec.Code, rec.Header())
			}
			events := f.srv.traffic.recent(0, false)
			if len(events) != 1 || events[0].Node != "self" || events[0].Engine != "envoy-backend" || events[0].Upstream != backend.URL || events[0].Via != "llm-d" {
				t.Fatalf("Activity attribution: %+v", events)
			}
		})
	}
}
