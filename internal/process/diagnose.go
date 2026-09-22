package process

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Reading an engine's own last words.
//
// "engine exited before readiness (exit status 1)" is true and useless: the
// reason is always in the instance log, one line above the exit, and it has
// twice cost real debugging time to go and find it. So the launch error says
// what the engine said, and the log stays there for everything else.
//
// The patterns are deliberately few. Each one is a failure that has actually
// happened here, matched on text the engine prints itself; anything not
// recognised leaves the error exactly as it was rather than guessing.

// diagnoseTail is how much of the log to read. The reason sits in the last
// handful of lines, and a model load can print megabytes of tensor detail
// before it.
const diagnoseTail = 16 << 10

// diagnosis is one recognised failure: what happened, and what to do about it.
type diagnosis struct {
	reason string
	hint   string
}

// cudaAlloc pulls the size out of llama.cpp's allocation failure, which prints
// MiB with two decimals: "allocating 15339.44 MiB on device 0".
var cudaAlloc = regexp.MustCompile(`allocating ([0-9.]+) MiB on device`)

// diagnose reads an engine's log and returns why it exited, or nil when the
// ending is not one ModelFabric recognises.
func diagnose(logPath string) *diagnosis {
	if logPath == "" {
		return nil
	}
	tail, err := readTail(logPath, diagnoseTail)
	if err != nil || tail == "" {
		return nil
	}
	low := strings.ToLower(tail)

	switch {
	case strings.Contains(low, "cudamalloc failed: out of memory"),
		strings.Contains(low, "failed to allocate cuda"),
		strings.Contains(low, "unable to allocate cuda"):
		want := ""
		if m := cudaAlloc.FindStringSubmatch(tail); len(m) == 2 {
			if mib, err := strconv.ParseFloat(m[1], 64); err == nil {
				want = fmt.Sprintf(" while allocating %.1fGB", mib/1024)
			}
		}
		return &diagnosis{
			reason: "the GPU ran out of memory" + want,
			hint:   "mfsh doctor lists what else is using the GPU; or load with a smaller -context, fewer -gpu-layers, or on another node",
		}

	case strings.Contains(low, "address already in use"):
		return &diagnosis{
			reason: "the port it was given is already in use",
			hint:   "an engine from an earlier run may have outlived its node — mfsh doctor finds orphans",
		}

	// Metal and CPU paths phrase exhaustion differently from CUDA.
	case strings.Contains(low, "failed to allocate buffer"),
		strings.Contains(low, "ggml_metal") && strings.Contains(low, "out of memory"),
		strings.Contains(low, "cannot allocate memory"):
		return &diagnosis{
			reason: "the machine ran out of memory for the model",
			hint:   "load with a smaller -context, or on a node with more memory",
		}

	case strings.Contains(low, "error: invalid argument"),
		strings.Contains(low, "unknown argument"),
		strings.Contains(low, "unrecognized argument"):
		return &diagnosis{
			reason: "the engine rejected one of its arguments",
			hint:   "check any -arg you passed against this runtime's llama-server",
		}

	// Only after the allocation cases: a corrupt or truncated file also ends
	// in "failed to load model", and those say more.
	case strings.Contains(low, "failed to load model"):
		return &diagnosis{
			reason: "the engine could not load the model file",
			hint:   "mfsh doctor verifies the file against its pin; a re-download may be needed",
		}
	}
	return nil
}

// explain adds the engine's own reason to a launch error, leaving the original
// message in place — the exit status still matters when someone reports it.
func explain(err error, logPath string) error {
	d := diagnose(logPath)
	if d == nil {
		return err
	}
	return fmt.Errorf("%w — %s (%s)", err, d.reason, d.hint)
}

// readTail returns the last n bytes of a file as text, starting at a line
// boundary so a partial first line is not reported as content.
func readTail(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if fi.Size() > n {
		start = fi.Size() - n
	}
	if _, err := f.Seek(start, 0); err != nil {
		return "", err
	}
	b := make([]byte, fi.Size()-start)
	read, err := f.Read(b)
	if read == 0 && err != nil {
		return "", err
	}
	s := string(b[:read])
	if start > 0 {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return s, nil
}
