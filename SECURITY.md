# Security

## Reporting

Please report vulnerabilities privately through GitHub's
[security advisories](https://github.com/gregbugaj/modelfabric/security/advisories/new)
rather than a public issue.

Include what you did, what happened, and which version (`mfsh doctor` prints
it). You will get an acknowledgement within a week. This is a small project —
there is no bounty, and no SLA beyond a genuine effort to fix things promptly
and credit you unless you would rather not be.

## What ModelFabric assumes

Worth stating plainly, because several of these look like vulnerabilities and
are deliberate trade-offs:

- **The management API is loopback-only.** Over the tailnet, `/api/v1` is
  reachable only from a device Tailscale reports as having the same owner.
  `mesh_admin: "off"` closes even that.
- **The loopback listener needs no API key by default**, matching LM Studio.
  Anything exposed beyond loopback (`public_listen`) always requires one.
- **Engine ports have no authentication of their own.** With `engine_bind` set
  to a tailnet address — which llm-d requires to schedule across
  machines — Tailscale ACLs are the only thing in front of those engines. That
  is the trade, and the dashboard shows which nodes are bound that way.
- **Prompts are not recorded** unless request capture is switched on, and then
  only in memory, dropped as the ring rolls over. `log_bodies_file` writes to
  disk and is config rather than a button for that reason.
- **Downloads are verified.** Models, engine builds, uv and llm-d's
  binaries are checked against published digests before they run; engine
  binaries are pinned by content so a mutable `PATH` cannot be reported as a
  pinned runtime.

## Release integrity

Release binaries carry GitHub build provenance. To check one came from this
repository's workflow:

```bash
gh attestation verify mfsh-linux-amd64 -R gregbugaj/modelfabric
```

`install.sh` separately verifies the SHA-256 against the release's
`checksums.txt` before installing.

## Supported versions

The latest release. ModelFabric has not reached 1.0; fixes go forward, not into old
tags.
