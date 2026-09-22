package discovery

import (
	"os"
	"testing"
)

// TestWriteEnvoyConfigForValidation is a helper, not an assertion: it writes
// the generated bootstrap so the real Envoy binary can validate it.
func TestWriteEnvoyConfigForValidation(t *testing.T) {
	out := os.Getenv("ENVOY_OUT")
	if out == "" {
		t.Skip("set ENVOY_OUT to dump the config")
	}
	if err := os.WriteFile(out, EnvoyConfig(EnvoyOptions{ListenPort: 8090, EPPPort: 9002, AdminPort: 19000}), 0o644); err != nil {
		t.Fatal(err)
	}
}
