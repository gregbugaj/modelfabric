# AGENTS.md

Context for AI coding agents working on ModelFabric. Humans want [README.md](README.md);
this is the part that would clutter it.

## What this is

ModelFabric is a peer-to-peer model mesh over Tailscale. One Go binary per machine.
Each node serves an OpenAI-compatible API on `localhost:1234` and routes to
whichever machine in the mesh holds the model, so a laptop can use a
workstation's GPU. It supervises `llama-server` (llama.cpp) and `mlx-lm`, and
optionally schedules them with llm-d.

It is a single binary with no runtime dependencies it did not have to have.
Adding a Go module is a decision, not a convenience — say so and give the
reason.

## Build and test

```bash
make build      # go build ALONE IS NOT ENOUGH — see below
make test       # go test -race ./...
make check-ui   # parses web/*.js; a syntax error here yields a blank dashboard
make test-ui    # node --test web/*.test.mjs   (needs Node 22+)
go vet ./...
gofmt -l internal cmd     # must print nothing
```

**`go build ./...` does not update `./mfsh`.** It compiles and discards. Use
`make build`, which also stages `web/` into the embed directory. Skipping it
means the binary serves the dashboard from the last `make ui`, and you will
debug a UI change that was never shipped.

**Node 22 is required for the UI tests.** The system default here may be v16,
which fails with `bad option: --test`. Use `NODE=~/.nvm/versions/node/v22*/bin/node make test-ui`
or put 22 on PATH.

## The rule that matters most

**Verify against the running system, not against your reasoning.** This
codebase talks to GPUs, other machines and other people's servers, and almost
every wrong answer in its history came from inference that looked sound:

- Claiming an endpoint works without calling it. `/v1/models` did not include
  `nodes` on every path; `X-Fabric-Node` is absent on some paths. Both were
  written as fact, then disproved by one `curl`.
- Reading a screenshot as proof the code changed, when the server was serving a
  build from hours earlier. Kill the process by PID and restart before believing
  a UI change.
- Trusting a probe that returned `undefined`. A raw newline inside a regex
  literal made every readout `undefined`, which read as "the feature is broken".
  Check that your instrument works before concluding the thing it measures does.

When something fails, **read the engine's own log first** —
`~/.local/state/modelfabric/logs/<instance>.log` (macOS: `~/Library/Logs/modelfabric`). It
is almost always one line above the exit.

## Code style

Match the surrounding code; it is consistent and deliberate.

- **Comments say why, not what.** The tree is full of comments naming the bug
  that motivated a line. Keep that: a comment that only restates the code is
  noise, one that records a measurement or a failure is why the next person
  does not reintroduce it.
- **Zero values describe llama.cpp.** `runtime.Traits` and `mesh.Engine` state
  only where an engine *differs*, so a new adapter is small and an old peer that
  reports nothing behaves as it always did.
- **Say what is not known.** Where ModelFabric cannot determine which node served a
  request, it records nothing rather than guessing. Prefer `unknown` and an
  honest error to a plausible default.
- Errors are lower-case, no trailing punctuation, and name what to do next
  where there is something to do.
- Tests are table-driven with named cases; the name states the expectation.

## Testing expectations

- A bug fix comes with a test that fails without it. Several tests here encode
  a specific past failure — keep that style, and say which failure in a comment.
- When a change makes an existing test wrong, **rewrite the test and say why**
  rather than deleting it. A test defending removed behaviour is worse than none.
- Go tests must pass with `-race`.

## The mesh, when you have one

Nodes are real machines. Restarting one unloads its models, and loading a 27B
takes ~20s on a GPU node.

- `mfsh doctor` is the fastest way to find out what is wrong, locally or — from
  the dashboard's Doctor page — on a peer.
- `make deploy` stages a build onto every peer and **restarts nothing**. Restart
  is deliberate and yours to trigger: `make deploy-restart` restarts nodes one
  at a time and skips any with models loaded unless `-force` is passed to
  `scripts/deploy.sh`. Ask before running either against someone's mesh.
- Engines bound to loopback are invisible to anything that dials them directly
  (llm-d). `engine_bind` is the setting; the Overview page shows which nodes are
  on the tailnet.
- Do not kill processes you did not start. A GPU can be shared with something
  else, and `mfsh doctor` lists what is holding it.

## Commits and pull requests

- One change per commit; the subject says what changed and why in one line.
- Run `make build test check-ui test-ui`, `go vet ./...` and `gofmt -l` before
  pushing. CI runs exactly these.
- Update `site/content/` in the same change when behaviour changes. Never
  paste a real `100.x.y.z` address, a real hostname or a real domain into docs;
  use `entrypoint-01`, `100.64.0.x` and `api.example.com`, which is reserved
  and can never resolve to someone else's server. The project site is
  `https://modelfabric.sh`. The project's previous domain was registered by
  someone else in September 2026; `internal/docslint` fails on it anywhere in
  the repository.
  `internal/docslint` fails the tests on any tailnet address outside
  `100.64.0.x` in the repo's Markdown, and on an em dash in the site's prose.
  Nothing checks for dead links yet.
- Screenshots in the docs are scrubbed (`sites-01` → `entrypoint-01`, addresses
  → `100.64.0.x`). Re-capture rather than hand-editing.

## Security

- Never widen a listener without saying so. The management API is loopback-only
  by default, and peers reach `/api/v1` only when Tailscale reports the same
  owner.
- Request bodies (prompts) are **not** recorded unless capture is switched on,
  and then only in memory. `log_bodies_file` is the exception and is config, not
  a button, because a file forgets nothing.
- Engine ports have no authentication of their own. Binding them to a tailnet
  address means Tailscale ACLs are the only thing in front of them — that is a
  real trade, and it belongs in the message when you make it.
