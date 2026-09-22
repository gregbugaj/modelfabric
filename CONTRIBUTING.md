# Contributing

Thanks for looking. ModelFabric is a single Go binary that serves an OpenAI-compatible
API across a mesh of machines you own; the shortest description of what it does
is in [README.md](README.md).

## Before you start

Open an issue for anything that changes behaviour, adds a dependency, or takes
more than an afternoon. A short conversation beats a large pull request that
goes the wrong way.

Small things — a typo, a broken link, a clearly wrong error message — just send.

## Getting set up

You need Go (the version in `go.mod`) and Node 22 for the dashboard's tests.

```bash
make build      # not `go build`: it also stages the dashboard into the binary
make test       # go test -race ./...
make check-ui   # parses the dashboard's JavaScript
make test-ui    # its unit tests
go vet ./... && gofmt -l internal cmd
```

CI runs exactly those, on Linux and macOS, plus a docs build. If they pass
locally they will pass there.

You do **not** need a GPU or a second machine for most changes. The catalog,
router, config and dashboard all have tests that run anywhere.

## What a good change looks like

- **A fix comes with a test that fails without it.** Many tests here name the
  specific failure they prevent; that is the style.
- **Comments say why.** The reason a line exists outlives what it does.
- **Behaviour changes update the docs** in `site/content/` in the same
  commit.
- **Verify against something running.** This project talks to GPUs and other
  machines; reasoning that looks sound is regularly wrong. If you claim an
  endpoint behaves a certain way, call it first.

If you use an AI coding agent, [AGENTS.md](AGENTS.md) has the context it needs,
including the traps that have produced confidently wrong answers here before.

## Dependencies

ModelFabric has almost none on purpose — it installs as one binary and is expected to
run on a machine nobody administers. Adding a module is a decision: say what it
buys and what it costs in the pull request.

## Docs

The site is Nextra, under `site/`. `pnpm dev` runs it locally.

Never paste a real tailnet address or machine name into the docs. Screenshots
are scrubbed before they land (`100.x.y.z` addresses, internal hostnames); the
docs build fails on a leak.

## Licence

By contributing you agree your work is licensed under
[Apache-2.0](LICENSE), the same as the rest of the project.
