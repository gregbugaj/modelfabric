package server

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/runtime"
	"github.com/gregbugaj/modelfabric/internal/supervisor"
)

// A reader that already opened the previous selection must see a complete old
// document. Writing in place truncated that same file beneath concurrent readers.
func TestRuntimeSelectionAtomicallyReplacesPreviousState(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reg, errs := runtime.NewRegistry([]*runtime.Definition{{Name: "selected", Entrypoint: exe}}, "")
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	s := &Server{sup: supervisor.New(supervisor.Config{DataDir: t.TempDir()}, reg, nil, nil, nil, nil)}
	t.Cleanup(s.sup.Shutdown)
	s.runtimeStore = filepath.Join(t.TempDir(), "runtime.json")
	old := `{"runtime":"previous-runtime"}`
	if err := os.WriteFile(s.runtimeStore, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(s.runtimeStore)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	rec := httptest.NewRecorder()
	s.handleSelectRuntime(rec, httptest.NewRequest("POST", "/api/v1/runtimes/select", strings.NewReader(`{"name":"selected"}`)))
	if rec.Code != 200 {
		t.Fatalf("selection: %d %s", rec.Code, rec.Body)
	}
	b, err := io.ReadAll(reader)
	if err != nil || string(b) != old {
		t.Errorf("existing reader saw %q, %v; want previous complete state", b, err)
	}
	if got := LoadSelectedRuntime(s.runtimeStore); got != "selected" {
		t.Errorf("saved selection=%q", got)
	}
}
