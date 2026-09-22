package runtime

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/gregbugaj/modelfabric/internal/catalog"
	"github.com/gregbugaj/modelfabric/internal/process"
)

// Apply resolves requested settings against a runtime's defaults.
func Apply(d *Definition, m catalog.Model, req Requested) (Applied, error) {
	eng, ok := engines[d.Engine]
	if !ok {
		return Applied{}, fmt.Errorf("unsupported engine %q", d.Engine)
	}
	return eng.Apply(d, m, req), nil
}

// LaunchSpec builds the launch for one instance of a model on a runtime.
//
// argv is generated entirely from trusted configuration and the catalog, and is
// executed directly without a shell — "do not assemble executable shell text
// from inference requests".
func LaunchSpec(d *Definition, m catalog.Model, a Applied, bind string, port, generation int, startup, stop time.Duration) (process.LaunchSpec, error) {
	eng, ok := engines[d.Engine]
	if !ok {
		return process.LaunchSpec{}, fmt.Errorf("unsupported engine %q", d.Engine)
	}
	if bind == "" {
		bind = "127.0.0.1"
	}
	return process.LaunchSpec{
		DeploymentID:   m.Key,
		Generation:     generation,
		Engine:         d.Engine,
		ServedModel:    eng.ServedModel(m),
		Argv:           eng.Argv(d, m, a, bind, port),
		Env:            d.envList(),
		Cwd:            d.Dir(),
		Endpoint:       LocalURL(bind, port),
		Model:          m.Key,
		StartupTimeout: startup,
		StopTimeout:    stop,
	}, nil
}

// Ready reports whether the engine at endpoint is serving that model, asking
// in whatever way the engine supports — an engine that cannot be asked at all
// is treated as unknown rather than ready.
func Ready(ctx context.Context, engine, endpoint, served string) error {
	eng, ok := engines[engine]
	if !ok {
		return fmt.Errorf("unsupported engine %q", engine)
	}
	return eng.Ready(ctx, endpoint, served)
}

// LocalURL is where this node reaches an engine it launched.
//
// Loopback works when the engine binds loopback or every interface. It does not
// when the engine binds one specific address — a tailnet IP, so llm-d's EPP on
// another host can reach it without exposing it on the LAN. Such an engine does
// not listen on 127.0.0.1 at all, and a loopback readiness probe would wait out
// the whole startup timeout for an engine that was ready in seconds.
func LocalURL(bind string, port int) string {
	switch bind {
	case "", "127.0.0.1", "localhost", "0.0.0.0", "::", "[::]":
		return fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	return "http://" + net.JoinHostPort(bind, strconv.Itoa(port))
}
