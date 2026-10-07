package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func logWith(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "inst.log")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Verbatim from a real failure on the 5090: a vLLM process held the card and
// llama.cpp could not get its buffer. The exit status alone said nothing.
const cudaOOMLog = `
0.00.811.305 W common_fit_params: failed to fit params to free device memory: n_gpu_layers already set by user to 99, abort
0.01.181.838 E ggml_backend_cuda_buffer_type_alloc_buffer: allocating 15339.44 MiB on device 0: cudaMalloc failed: out of memory
0.01.181.843 E alloc_tensor_range: failed to allocate CUDA0 buffer of size 16084563968
0.01.300.388 E llama_model_load: error loading model: unable to allocate CUDA0 buffer
0.01.300.400 E cmn  common_init_: failed to load model '/models/Qwen3.8-27B-Q4_K_M.gguf'
0.01.301.159 E srv  llama_server: exiting due to model loading error
`

// An allocation failure is reported as such even though the log also ends with
// the generic "failed to load model" that every failure prints.
func TestDiagnoseCUDAOutOfMemory(t *testing.T) {
	d := diagnose(logWith(t, cudaOOMLog))
	if d == nil {
		t.Fatal("the reason was in the log and was not found")
	}
	if !strings.Contains(d.reason, "GPU ran out of memory") {
		t.Errorf("reason %q should name the cause", d.reason)
	}
	if !strings.Contains(d.reason, "15.0GB") {
		t.Errorf("reason %q should carry the size it asked for", d.reason)
	}
	if !strings.Contains(d.hint, "doctor") {
		t.Errorf("hint %q should point at what lists the other GPU users", d.hint)
	}
}

func TestDiagnoseOtherEndings(t *testing.T) {
	for _, c := range []struct{ name, log, want string }{
		{"port taken", "error: bind: address already in use\n", "already in use"},
		{"bad flag", "error: invalid argument: --nope\n", "rejected one of its arguments"},
		{"host memory", "ggml_backend_cpu_buffer: cannot allocate memory\n", "ran out of memory"},
		{"corrupt weights", "llama_model_load: failed to load model\n", "could not load the model file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := diagnose(logWith(t, c.log))
			if d == nil || !strings.Contains(d.reason, c.want) {
				t.Errorf("got %+v, want a reason containing %q", d, c.want)
			}
		})
	}
}

// Anything ModelFabric does not recognise must leave the error alone rather than
// invent a cause; a confident wrong reason is worse than an exit status.
func TestDiagnoseStaysQuietOnAnUnknownEnding(t *testing.T) {
	for _, body := range []string{"", "all fine, then the power went out\n"} {
		if d := diagnose(logWith(t, body)); d != nil {
			t.Errorf("log %q produced %+v; want no guess", body, d)
		}
	}
	if d := diagnose(""); d != nil {
		t.Errorf("no log path produced %+v", d)
	}
	if d := diagnose("/nonexistent/inst.log"); d != nil {
		t.Errorf("missing log produced %+v", d)
	}
}

// The original error survives: the exit status is what someone quotes in a
// report, and the reason is added to it rather than replacing it.
func TestExplainKeepsTheOriginalError(t *testing.T) {
	base := ErrLaunch
	got := explain(base, logWith(t, cudaOOMLog))
	if !strings.Contains(got.Error(), "GPU ran out of memory") {
		t.Errorf("explain lost the reason: %v", got)
	}
	if !strings.Contains(got.Error(), base.Error()) {
		t.Errorf("explain lost the original: %v", got)
	}
}

// A long log is read from its end: the reason is in the last lines, after
// however many megabytes of tensor detail.
func TestDiagnoseReadsTheEndOfALargeLog(t *testing.T) {
	d := diagnose(logWith(t, strings.Repeat("loading tensor blk.0.attn_q.weight\n", 40000)+cudaOOMLog))
	if d == nil || !strings.Contains(d.reason, "GPU ran out of memory") {
		t.Errorf("got %+v, want the reason from the tail", d)
	}
}
