package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/config"
	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// The old writer used one .tmp name: concurrent requests truncated each other's
// writes, then most renames failed because a sibling had already moved the file.
func TestConcurrentPreferredWritesAllPersistWholeDocuments(t *testing.T) {
	s := &Server{prefStore: filepath.Join(t.TempDir(), "preferred.json")}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.persistPreferred("node"); err != nil {
				t.Errorf("persist: %v", err)
			}
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(s.prefStore)
	if err != nil || !json.Valid(b) {
		t.Fatalf("state=%q, err=%v", b, err)
	}
	if got := LoadPreferred(s.prefStore); got != "node" {
		t.Fatalf("saved preference=%q", got)
	}
}

func TestPreferredSaveFailureIsReportedWithoutChangingLiveChoice(t *testing.T) {
	for _, clear := range []bool{false, true} {
		name := "save"
		if clear {
			name = "clear"
		}
		t.Run(name, func(t *testing.T) {
			m := mesh.New(config.Default(), "self")
			m.SetPreferred("previous")
			path := filepath.Join(t.TempDir(), "not-a-file")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "child"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			s := &Server{m: m, prefStore: path, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			body := `{"node":"self"}`
			if clear {
				body = `{"node":""}`
			}
			rec := httptest.NewRecorder()
			s.handlePreferred(rec, httptest.NewRequest("PUT", "/api/v1/preferred", strings.NewReader(body)))
			if rec.Code != 500 {
				t.Errorf("status=%d, want persistence error: %s", rec.Code, rec.Body)
			}
			if got := m.Preferred(); got != "previous" {
				t.Errorf("live preference changed to %q after failed save", got)
			}
		})
	}
}

func TestConcurrentPreferredUpdatesKeepDiskAndMemoryTogether(t *testing.T) {
	m := mesh.New(config.Default(), "self")
	s := &Server{m: m, prefStore: filepath.Join(t.TempDir(), "preferred.json"), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(clear bool) {
			defer wg.Done()
			body := `{"node":"self"}`
			if clear {
				body = `{"node":""}`
			}
			rec := httptest.NewRecorder()
			s.handlePreferred(rec, httptest.NewRequest("PUT", "/api/v1/preferred", strings.NewReader(body)))
			if rec.Code != 200 {
				t.Errorf("update: %d %s", rec.Code, rec.Body)
			}
		}(i%2 == 0)
	}
	wg.Wait()
	if got := LoadPreferred(s.prefStore); got != m.Preferred() {
		t.Fatalf("disk=%q, memory=%q", got, m.Preferred())
	}
}
