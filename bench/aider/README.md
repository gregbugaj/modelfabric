# Aider polyglot routing benchmark

The short-task counterpart to [`../swe`](../swe/). Both drive the same fleet
through the same routing modes; they differ in the shape of the traffic.

| | SWE-bench Verified | Aider polyglot |
|---|---|---|
| Task | one repository bug, 20–80 turns | one Exercism exercise, a few turns |
| Conversation | grows to 100k+ tokens | small |
| Cache reuse | ~98% | mostly cold |
| What it stresses | prefix affinity | placement under a high request rate |
| A run | ~1.5–2 hours per mode | minutes to an hour |

A profile that wins both is better. One that wins only SWE-bench is good at
cache affinity, which is most of what SWE-bench can see.

## What is pinned

| | |
|---|---|
| Harness | [Aider-AI/aider](https://github.com/Aider-AI/aider) `benchmark/benchmark.py`, in its own container |
| Exercises | [Aider-AI/polyglot-benchmark](https://github.com/Aider-AI/polyglot-benchmark) — C++, Go, Java, JavaScript, Python, Rust |
| Subset | `AIDER_NUM_TESTS` (default 24 of 225) |
| Concurrency | `AIDER_THREADS` (default 8, deliberately above the fleet's slot count so requests queue) |
| Edit format | `whole` |
| Fleet | whatever `bench/swe/env.sh` sets — the same model, load arguments and peers |

## Run it

```bash
bench/aider/setup.sh                        # clone + build the image, once
SWE_ENTRYPOINT=entrypoint-01 \
SWE_AGENT_BASE=https://api.example.com/v1 \
SWE_CONTROL_ADDR=http://100.64.0.1:1234 \
  bench/aider/sequence.sh 0927a router tuned
```

One mode on its own:

```bash
bench/aider/run.sh my-run
```

## The container needs a reachable endpoint

The harness runs model-written code without review, so it runs in a container
— which means **a loopback `SWE_AGENT_BASE` will not work**: `127.0.0.1`
inside the container is the container. Point it at the entrypoint's public
URL. `run.sh` warns rather than failing silently, but it cannot fix it.

This is the one place the two benchmarks genuinely differ in setup, and it is
why the entrypoint topology is convenient here rather than merely realistic.

## Reading the results

Aider writes its own results under `$AIDER_WORK/aider/tmp.<date>-<name>/`, with
a pass rate per edit format. Routing shows in the same places it does for
bench/swe: the per-request records ModelFabric keeps (node and engine per call), and
the engine recording if one was running:

```bash
mfsh log -engines -json > run.ndjson
```

Because these tasks mostly start cold, the number to watch is **which engine
got each request**, not cache hit share — there is little prefix to be
affine to.
