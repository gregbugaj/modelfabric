package supervisor

import (
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/mesh"
	modelruntime "github.com/gregbugaj/modelfabric/internal/runtime"
)

func TestInstanceStatesPreserveEngineMeasurements(t *testing.T) {
	measured := &mesh.Engine{Served: "served-model"}
	measured.SetSlots(2)
	measured.SetAskedContext(8192)
	measured.SetEnginePool(8192)
	measured.SetRates(mesh.EngineRates{PrefillTokS: 1200, DecodeTokS: 60, SpecAccepted: .75,
		Trusted: true, PromptTokens: 9000, CachedTokens: 3000, OutputTokens: 600})
	measured.SetKVUsage(.25)
	for _, n := range []int{1, 2, 3} {
		measured.SetObservedInflight(n)
	}
	mismatch := &mesh.Engine{}
	mismatch.SetSlots(2)
	mismatch.SetAskedContext(8192)
	mismatch.SetEnginePool(4096)
	idle := &mesh.Engine{}
	idle.SetKVUsage(0)
	for range 3 {
		idle.SetObservedInflight(0)
	}
	for _, tc := range []struct {
		name   string
		engine *mesh.Engine
		want   mesh.InstanceState
	}{
		{"measured rates totals and load survive the snapshot", measured, mesh.InstanceState{
			ServedModel: "served-model", EngineStats: mesh.EngineStats{KVUsage: .25, LoadAvg: 2, Inflight: 3,
				ContextLength: 8192, KVPoolTokens: 8192, PrefillTokS: 1200, DecodeTokS: 60,
				SpecAccepted: .75, PrefillTrusted: true, PromptTokens: 9000, CachedTokens: 3000, OutputTokens: 600}}},
		{"unmeasured engine keeps unknown sentinels and configured context", &mesh.Engine{}, mesh.InstanceState{EngineStats: mesh.EngineStats{
			KVUsage: -1, LoadAvg: -1, SpecAccepted: -1, ContextLength: 8192}}},
		{"measured idle remains distinct from unknown", idle, mesh.InstanceState{EngineStats: mesh.EngineStats{
			KVUsage: 0, LoadAvg: 0, SpecAccepted: -1, ContextLength: 8192}}},
		{"engine context overrides configured context and explains mismatch", mismatch, mesh.InstanceState{EngineStats: mesh.EngineStats{
			KVUsage: -1, LoadAvg: -1, SpecAccepted: -1, ContextLength: 2048, KVPoolTokens: 4096,
			ContextNote: "loaded for 8192 tokens per request but the engine reports 4096 across 2 slots: it is running 2048"}}},
		{"instance without engine preserves legacy zero measurements", nil, mesh.InstanceState{EngineStats: mesh.EngineStats{ContextLength: 8192}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			s := &Supervisor{instances: map[string]*Instance{"instance": {
				ID: "instance", Model: "model", Runtime: "runtime", Port: 18000, ShimPort: 18001,
				Config: modelruntime.Applied{Parallel: 2, ContextLength: 8192, Vision: true, VisionSkipped: "disabled"},
				State:  "ready", PID: 42, StartedAt: started, engine: tc.engine,
			}}}
			want := tc.want
			want.ID, want.Model, want.Source, want.Runtime = "instance", "model", "model", "runtime"
			want.Address, want.Port, want.MetricsPort = "127.0.0.1", 18000, 18001
			want.Slots, want.Vision, want.VisionOff = 2, true, true
			want.State, want.PID, want.Started = "ready", 42, started
			if got := s.instanceStates(); !reflect.DeepEqual(got, []mesh.InstanceState{want}) {
				t.Fatalf("instance states = %+v, want %+v", got, want)
			}
		})
	}
}

func TestInstanceStatesKeepInstanceAndEngineTogetherDuringReplacement(t *testing.T) {
	// The old two-pass join could combine a replacement instance with the
	// previous instance's engine, or zero its measurements during a load.
	first := &Instance{ID: "instance", Model: "first", engine: &mesh.Engine{Served: "first"}}
	second := &Instance{ID: "instance", Model: "second", engine: &mesh.Engine{Served: "second"}}
	s := &Supervisor{instances: map[string]*Instance{"instance": first}}
	stop := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			for _, inst := range []*Instance{first, second} {
				select {
				case <-stop:
					return
				default:
				}
				s.mu.Lock()
				s.instances[inst.ID] = inst
				s.mu.Unlock()
				inst.engine.SetRates(mesh.EngineRates{PrefillTokS: 1200, DecodeTokS: 60, Trusted: true})
				inst.engine.SetKVUsage(.5)
				inst.engine.SetObservedInflight(2)
				runtime.Gosched()
			}
		}
	}()
	t.Cleanup(func() { close(stop); workers.Wait() })
	for range 5000 {
		states := s.instanceStates()
		if len(states) != 1 || states[0].Model != states[0].ServedModel {
			t.Fatalf("instance and engine came from different snapshots: %+v", states)
		}
	}
}

func TestInstanceStatesAreSortedAndEmptyIsAnArray(t *testing.T) {
	s := &Supervisor{instances: map[string]*Instance{}}
	if got := s.instanceStates(); got == nil || len(got) != 0 {
		t.Fatalf("empty states = %#v, want non-nil empty slice", got)
	}
	for _, inst := range []*Instance{{ID: "z", Model: "a"}, {ID: "a", Model: "a"}, {ID: "b", Model: "z"}} {
		s.instances[inst.ID] = inst
	}
	var ids []string
	for _, state := range s.instanceStates() {
		ids = append(ids, state.ID)
	}
	if !reflect.DeepEqual(ids, []string{"a", "z", "b"}) {
		t.Fatalf("instance order = %v, want [a z b]", ids)
	}
}
