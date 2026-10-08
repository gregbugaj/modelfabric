package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/gregbugaj/modelfabric/internal/catalog"
)

func twoCards() Hardware {
	return Hardware{GPUs: []GPU{
		{Name: "NVIDIA RTX 6000 Ada Generation", MemoryMB: 49140},
		{Name: "NVIDIA GeForce RTX 4090", MemoryMB: 24564},
	}}
}

func TestParseGPUs(t *testing.T) {
	for _, c := range []struct {
		name, in, want string
		bad            bool
	}{
		{"nothing chosen", "", "", false},
		{"all is nothing chosen", "all", "", false},
		{"one card", "1", "1", false},
		{"several, in any order and spacing, are stored one way", " 1, 0 ", "0,1", false},
		{"a card named twice is one card", "0,0", "0", false},
		{"each is the command line's shorthand, not a setting", "each", "", true},
		{"a name is not a number", "CUDA0", "", true},
		{"no card has a negative number", "-1", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseGPUs(c.in)
			if (err != nil) != c.bad {
				t.Fatalf("error %v, want an error: %v", err, c.bad)
			}
			if got := normalGPU(c.in); got != c.want {
				t.Errorf("stored as %q, want %q", got, c.want)
			}
		})
	}
}

func TestCheckGPUs(t *testing.T) {
	cuda := &Definition{Name: "llama.cpp-cuda", Backend: "cuda"}
	for _, c := range []struct {
		name string
		d    *Definition
		gpu  string
		hw   Hardware
		want string // part of the error, "" for none
	}{
		{"a card that is there", cuda, "1", twoCards(), ""},
		{"both cards", cuda, "0,1", twoCards(), ""},
		{"no choice needs no GPU at all", &Definition{Backend: "metal"}, "all", Hardware{}, ""},
		{"a card that is not there, with what is", cuda, "2", twoCards(),
			"there is no GPU 2 here: this machine has GPU 0 (RTX 6000 Ada Generation, 48 GB) and GPU 1 (GeForce RTX 4090, 24 GB)"},
		{"a machine with no NVIDIA GPU", cuda, "0", Hardware{}, "nvidia-smi reports no GPU"},
		{"a runtime that is not CUDA", &Definition{Name: "llama.cpp-metal", Backend: "metal"}, "0", twoCards(),
			"choosing a GPU works with CUDA runtimes; llama.cpp-metal uses metal"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := CheckGPUs(c.d, c.gpu, c.hw)
			switch {
			case c.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// The engine is confined by environment and in nvidia-smi's numbering. CUDA
// numbers devices fastest first unless told otherwise, so on a machine with a
// 6000 Ada and a 4090 its device 0 is not the card nvidia-smi calls 0.
func TestAGPUChoiceReachesTheEngine(t *testing.T) {
	d := testDef()
	d.Backend = "cuda"
	d.Env = map[string]string{"CUDA_VISIBLE_DEVICES": "7", "LD_LIBRARY_PATH": "/opt/cuda"}
	m := catalog.Model{Key: "m", Path: "/models/m.gguf"}
	for _, c := range []struct {
		name string
		gpu  *string
		want string // CUDA_VISIBLE_DEVICES as the engine ends up with it
	}{
		{"no choice: the definition's own environment stands", nil, "7"},
		{"all is no choice", ptr("all"), "7"},
		{"a card chosen at load wins over the definition", ptr("1"), "1"},
		{"two cards", ptr("1,0"), "0,1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, err := Apply(d, m, Requested{Settings: Settings{GPU: c.gpu}})
			if err != nil {
				t.Fatal(err)
			}
			spec, err := LaunchSpec(d, m, a, "127.0.0.1", 18000, 1, time.Minute, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			// The process takes the last value given for a name.
			got, order := "", ""
			for _, kv := range spec.Env {
				if v, ok := strings.CutPrefix(kv, "CUDA_VISIBLE_DEVICES="); ok {
					got = v
				}
				if v, ok := strings.CutPrefix(kv, "CUDA_DEVICE_ORDER="); ok {
					order = v
				}
			}
			if got != c.want {
				t.Errorf("CUDA_VISIBLE_DEVICES=%q, want %q (env %v)", got, c.want, spec.Env)
			}
			chosen := c.gpu != nil && *c.gpu != "all"
			if chosen && order != "PCI_BUS_ID" {
				t.Errorf("a chosen card needs nvidia-smi's numbering, got CUDA_DEVICE_ORDER=%q", order)
			}
			if !chosen && a.GPU != "" {
				t.Errorf("nothing was chosen but the engine reports GPU %q", a.GPU)
			}
		})
	}
	t.Run("two engines on two cards are two configurations", func(t *testing.T) {
		a0, _ := Apply(d, m, Requested{Settings: Settings{GPU: ptr("0")}})
		a1, _ := Apply(d, m, Requested{Settings: Settings{GPU: ptr("1")}})
		if a0.Fingerprint() == a1.Fingerprint() {
			t.Error("a load on GPU 1 would be taken for the one already running on GPU 0")
		}
	})
}
