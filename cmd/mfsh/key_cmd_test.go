package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Run as root on a node running as greg, `mfsh key` read /root, created a key
// there and printed it as the node's; the node refused it. It must refuse to
// touch a key directory the running node does not use, and stay out of the way
// when there is no node to ask.
func TestKeyCommandsUseTheNodesDirectory(t *testing.T) {
	tests := []struct {
		name    string
		node    func(w http.ResponseWriter)
		mine    string
		wantErr string
	}{
		{name: "the node's own directory is used", mine: "/home/greg/.modelfabric",
			node: func(w http.ResponseWriter) { w.Write([]byte(`{"home":"/home/greg/.modelfabric","user":"greg"}`)) }},
		{name: "another user's directory is refused, naming the right user", mine: "/root/.modelfabric",
			node:    func(w http.ResponseWriter) { w.Write([]byte(`{"home":"/home/greg/.modelfabric","user":"greg"}`)) },
			wantErr: "sudo -u greg mfsh key"},
		{name: "a node too old to say is not second-guessed", mine: "/root/.modelfabric",
			node: func(w http.ResponseWriter) { http.NotFound(w, nil) }},
		{name: "no node running leaves this directory as the only one", mine: "/root/.modelfabric"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr := "http://127.0.0.1:1"
			if tc.node != nil {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { tc.node(w) }))
				t.Cleanup(srv.Close)
				addr = srv.URL
			}
			err := sameKeyHome(addr, tc.mine)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("got %v, want an error naming %q", err, tc.wantErr)
			}
		})
	}
}
