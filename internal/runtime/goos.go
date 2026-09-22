package runtime

import "runtime"

// runtimeGOOS is wrapped so tests can reason about platform-specific selection
// without build tags.
func runtimeGOOS() string { return runtime.GOOS }
