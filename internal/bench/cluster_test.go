package bench

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// frontDoor stands in for a node's front door: chat requests only, no
// tokenizer, and the serving node named by X-Fabric-Node in turn from nodes.
// An empty name sends no header, as some paths do not.
func frontDoor(t *testing.T, nodes ...string) string {
	t.Helper()
	engine := (&fakeLlama{}).handler()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if node := nodes[int(n.Add(1)-1)%len(nodes)]; node != "" {
			w.Header().Set("X-Fabric-Node", node)
		}
		engine.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The cluster run is about where the work went. Every request of a load level
// must be counted against the node that served it, and a request whose node
// the front door did not name is "unknown", never credited to a guess.
func TestClusterLoadRecordsWhereRequestsWent(t *testing.T) {
	tests := []struct {
		name  string
		nodes []string
		want  []string // nodes the spread must name
	}{
		{name: "two nodes share the load", nodes: []string{"a", "b"}, want: []string{"a", "b"}},
		{name: "an unnamed node is unknown", nodes: []string{""}, want: []string{"unknown"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ClusterConfig{Model: "m", PP: []int{256}, TG: 4, Concurrency: []int{1, 4}, LoadPP: 256}
			rep, err := RunCluster(context.Background(), frontDoor(t, tc.nodes...), "", cfg, ClusterReport{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Load) != 2 || len(rep.Shared) != 2 {
				t.Fatalf("load %d, shared %d levels; want 2 each", len(rep.Load), len(rep.Shared))
			}
			for _, l := range append(rep.Load, rep.Shared...) {
				total := 0
				for _, c := range l.Spread {
					total += c
				}
				if total != l.N || l.Failed != 0 {
					t.Errorf("n=%d: spread %v adds to %d, %d failed", l.N, l.Spread, total, l.Failed)
				}
			}
			last := rep.Load[len(rep.Load)-1]
			for _, node := range tc.want {
				if last.Spread[node] == 0 {
					t.Errorf("spread %v does not name %s", last.Spread, node)
				}
			}
			if rep.Load[0].Speedup != 1 {
				t.Errorf("one request at a time has speedup %v, want 1: it is the baseline", rep.Load[0].Speedup)
			}
			if !strings.Contains(rep.Text(), "under load") {
				t.Errorf("text has no load table:\n%s", rep.Text())
			}
		})
	}
}

// A front door that asks for a key must get the one it was given.
func TestClusterSendsTheKey(t *testing.T) {
	engine := (&fakeLlama{}).handler()
	var bad atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			bad.Add(1)
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		engine.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	cfg := ClusterConfig{Model: "m", PP: []int{256}, TG: 2, Concurrency: []int{2}, LoadPP: 256}
	if _, err := RunCluster(context.Background(), srv.URL, "k", cfg, ClusterReport{}, nil); err != nil {
		t.Fatal(err)
	}
	if bad.Load() != 0 {
		t.Fatalf("%d requests went without the key", bad.Load())
	}
}
