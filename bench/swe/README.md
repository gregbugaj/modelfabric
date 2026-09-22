# SWE-bench routing benchmark

Measures how routing affects a multi-turn coding agent: the same SWE-bench
Verified tasks, run through ModelFabric's own router, and again through llm-d's
scheduling profiles. An agent resends its growing conversation every turn, so
where each turn lands decides whether 30–90k tokens are read from cache or
prefilled again.

Results: [`docs/reports/`](../../docs/reports/). The first run is
[2026-09-19-swe-routing-pilot.html](../../docs/reports/2026-09-19-swe-routing-pilot.html).

## What is pinned

| | |
|---|---|
| Agent | mini-swe-agent 2.4.6, `swebench.yaml` config, tool calling, 100-step cap, 4 workers |
| Grader | swebench 5.0.2, dataset `SWE-bench/SWE-bench_Verified`, test split |
| Tasks | the first 20 of Verified (`--slice 0:20`, all astropy) |
| Python | 3.12 via uv 0.11.28; every package in `requirements.lock` |
| Agent's client | litellm 1.101.0, which is how mini-swe-agent calls a model — the `openai/<model>` name and `model_kwargs` in the generated `model.yaml` are its calling convention |
| llm-d | EPP v0.10.0 + Envoy 1.33.14, installed by `mfsh llmd install` |
| Model | Qwen3.8-27B Q4_K_M (LM Studio hub), MTP speculative decoding, model.yaml sampling |
| Per engine | `-vision on -spec mtp -context 65536`, with slots **measured per node**: 2 on xpredator, 4 on minion, 1 on helion (`mfsh tune`, 2026-09-25 — see "Slots are per node"). Runs before that date used 2 everywhere, and are not comparable on throughput |
| Fleet | xpredator: RTX 5090, upstream llama.cpp b11153 (cuda-12.8). minion: RTX 6000 Ada, LM Studio `cuda12-avx2@2.40.0`. helion: Apple Silicon, Metal `advsimd@2.41.0` |
| Entry | the agent enters through entrypoint-01 over public HTTPS, not loopback — see "Entrypoint" |
| Recording | `mfsh log -engines -json` runs alongside each mode into `runs/<name>.engines.ndjson`: per-engine rates, occupancy and lifetime token totals, sampled every second |

Each node runs the runtime ModelFabric selects for its own hardware, so builds
differ by design: the mesh is heterogeneous (CUDA boxes today, a Mac on Metal
later), and the benchmark measures routing across the fleet as it really is.
What must hold is that every mode runs on the *same* fleet: record each
engine's runtime with the results (`reload.sh` prints it) and compare runs only
when those match. Routing that ignores per-node speed differences is part of
what is being measured — the 5090 and the 6000 Ada already prefill at
different rates.

## Run it

```bash
make ui build                               # ./mfsh with the dashboard; deploy the same binary to every peer
bench/swe/setup.sh                          # Python env in ~/.local/share/modelfabric/bench/swe
SWE_ENTRYPOINT=entrypoint-01 \
SWE_AGENT_BASE=https://api.example.com/v1 \
SWE_CONTROL_ADDR=http://100.64.0.1:1234 \
  bench/swe/sequence.sh 0927 router tuned
bench/swe/grade.sh 0927-router               # once per run
python3 bench/swe/report/build.py --run router=0927-router --run tuned=0927-tuned \
  --out docs/reports/2026-09-27-swe-routing-3node.html
```

The first `--run` is the baseline the others are measured against, so pass the
unscheduled arm first even when the sequence ran `tuned` first.

`sequence.sh` does, for each mode: disable llm-d, reload the model on every
node (`reload.sh`, so no run inherits another's caches), check every engine's
real context and runtime, enable llm-d with the profile unless the mode is
`router`, then `run.sh`. A run takes 2–2.5 hours. One run alone:

```bash
./mfsh llmd enable qwen/qwen3.8-27b -profile optimized-baseline   # or llmd disable for the router arm
bench/swe/run.sh my-run
```

To stop a sequence, signal the script and the agent together, so the trap
fires as soon as the agent exits instead of the next mode starting:
`pkill -TERM -f 'bench/swe/sequence.sh'; pkill -TERM -f 'mini-extra swebench'`,
then remove leftover `minisweagent-*` containers.

`run.sh` writes the agent's model config at run time with the node's API key
(owner-only, never committed). Everything bulky lives under `$SWE_WORK`
(default `~/.local/share/modelfabric/bench/swe`): trajectories in `runs/`, grading in
`eval/`. SWE-bench's images go to Docker's data root (about 1–2 GB per task).
Settings are in `env.sh`; override with `SWE_*` environment variables.

`stats.py RUN_DIR [EVAL_REPORT]` prints per-task turns, peak context, cache
share, model time and resolution for any run, finished or not.

## What changed since the pilot

**The `direct` arm is gone, as of 2026-09-25.** It balanced across the engines
by in-flight request count. The arm is now **`router`**: ModelFabric's own
router, which is what a node does when llm-d is not scheduling. It places by
prefix affinity with a load veto, the room filter, and a cost per candidate
weighted by each engine's measured rate.

**Runs before this date are not comparable to runs after it, on placement or
on wall clock.** They ran on a longer request chain with a different thing
choosing the engine, and on this fleet (a 5090, a 6000 Ada and a Mac that
prefills 10× slower) "by request count" and "by how long each would take" are
exactly where placement diverges. The llm-d arms ran on that longer chain too,
so `tuned` then against `tuned` now is closer to comparable, but still not the
same chain.

What is preserved: the fleet, the model, the load arguments, the task slice, the
agent and the grader. So the *findings* below stand as findings about
scheduling — the tail matters and the median does not, the slowest node sets
the pace when nothing is scheduling — while the *numbers* belong to the
configuration that produced them.

**`-vision off` is out of `SWE_LOAD_ARGS`, as of 2026-09-25.** It was put there to
get MTP speculation back: llama.cpp used to fail a prompt carrying an image
while drafting ("failed to process mtmd chunk"), so ModelFabric turned drafting off
for any model that loaded a projector, which silently cost the pilot's 2×
decode. Loading text-only avoided the defect and restored it. ModelFabric no longer
turns drafting off for a vision load at all (2026-09-25), so the flag has
nothing left to buy.

**Measured 2026-09-24, that trade no longer exists.** With
`-vision on -spec mtp -context 65536 -parallel 2` — the benchmark's own
configuration plus the projector — two concurrent image requests of 986KB and
827KB both succeeded on minion with drafting live throughout (95 drafts, 57
accepted; 121 drafts, 48 accepted), and the replies described the images
correctly. helion's Metal build behaves the same. Whatever fixed it is in the
builds this fleet runs; upstream has been working the area
([llama.cpp#28587](https://github.com/ggml-org/llama.cpp/pull/28587)).

So the fleet can serve images *and* keep the 2× decode, which is why the flag
is gone. The published 2026-09-24 results are unaffected — both arms
used identical settings, so the routing comparison stands — but the reason
recorded for the flag was wrong by the time it was written down.

## Slots are per node

Every run through 2026-09-24 gave each engine two slots, because the fleet had
nothing to base a different number on. `mfsh tune` (see
[tuning](../../site/content/benchmark/tuning.mdx)) measured it on
2026-09-25, at this benchmark's own context and prompt size — total tok/s with
that many requests in flight:

| node | 1 | 2 | 4 | 8 | measured best |
|---|---|---|---|---|---|
| xpredator (RTX 5090) | 60 | **77** | OOM | — | **2 slots** |
| minion (RTX 6000 Ada) | 32 | 42 | **53** | OOM | **4 slots** |
| helion (Apple Silicon) | **10** | 9 | — | — | **1 slot** |

Two of the three were wrong, in opposite directions. minion was giving up about
a fifth of its throughput, and helion's second slot made it *slower* while
doubling what it advertised to every router in front of it. That second one
explains a result this benchmark has already published: on two slots each,
helion took **49% of the fleet's prefill and produced 5,824 tokens** against
xpredator's 64,145. It was not mis-routed so much as over-sold, and a slot is
the unit every router in front of these engines counts in — llm-d's room filter
still does, and ModelFabric's own router did until it began weighting a slot by the
engine behind it.

`env.sh` carries the measured counts as `SWE_LOAD_ARGS_<NODE>` overrides, and
`reload.sh` prints what each node was loaded with so the run records it. Runs
from before this date used a uniform two and are comparable to each other, not
to what follows: the fleet's total slot count changed from 6 to 7 and its shape
changed more than that. Re-run `mfsh tune` after any engine-build or
quantization change rather than copying these numbers.

**The fleet is three nodes, not two,** and the agent now enters through a real
entrypoint rather than loopback (see "Entrypoint" below). Numbers from
`2026-09-19-swe-routing-pilot` are a different configuration on different
hardware — a two-node run, speculation on, no entrypoint hop. Compare runs
from the same day, not across that line.

## Before a run

Each of these has produced a false result:

- **Same binary on every node.** Compare `sha256sum` of
  `dist/mfsh-linux-amd64` against each Linux peer's `~/.local/bin/mfsh`, not
  `./mfsh`: `make build` compiles with CGO on and `make dist` with
  `CGO_ENABLED=0`, so those two never match and the check reads as a failure
  every time. `mfsh version` agreeing on every node is the quick form. A stale binary on minion sized its KV cache
  from old defaults: `mfsh ps` said 65536, the engine got 32768, and every
  task routed there overflowed.
- **Real context on every engine.** `reload.sh` prints each engine's `n_ctx`
  from `/props`; it must be 131072 on all of them.
- **Per-model and per-node settings are off.** A saved default for this model
  (the flyout's Load and Inference tabs, or `mfsh defaults`) sits under the
  run's own arguments and silently changes what is measured. `-context` and
  `-parallel` are passed at load and win, but nothing in `SWE_LOAD_ARGS` names
  thinking effort or the sampler, so a node carrying those serves different
  work from its neighbours. `sequence.sh` now refuses to start when any node
  has them; `check-defaults.sh` is the check on its own, and
  `SWE_ALLOW_DEFAULTS=1` overrides it deliberately.
- **No preferred node.** `mfsh prefer` must say none: a preference sends
  every request to one machine first. `sequence.sh` refuses to start otherwise.
- **Tailscale SSH approval, and the way it actually works.** Reaching a peer
  uses `tailscale ssh`, which asks for a browser approval periodically and
  hangs until it gets one. `reload.sh` times out and `sequence.sh` stops rather
  than run on stale engines.
  The part that costs time: **the URL is bound to the connection that printed
  it.** Approving it lets *that* pending session through and nothing else, so
  approving a URL and then starting a fresh `ssh` — the natural way to test
  whether it worked — discards the approval and mints another. Run
  `tailscale ssh minion 'echo ok'` in the foreground and approve the URL it
  prints, while that command is still waiting.
- **What else is on the GPU.** A load can fail with "the GPU ran out of memory
  while allocating 15.0GB" for reasons that have nothing to do with the load
  arguments. On 2026-09-25 a vLLM container from another project
  (`document-split-shared-vllm`) held 15,194 MiB of the 5090's 32,607, leaving
  14,877 — about 300MB short — and the obvious conclusion, that the flag just
  changed was to blame, was wrong. `nvidia-smi --query-compute-apps` before
  blaming the configuration.
- **Host RAM on the peer.** llama.cpp keeps context checkpoints in host RAM:
  about 150 MiB + 4 KiB per token each for this model, up to 32 per slot by
  default. That OOM-killed minion (15 GB RAM) twice. ModelFabric now defaults to 4,
  which did identical prefill work in a replay (`replay-sequential.py`).

## Known limits of the harness

- **2-hour task limit.** mini-swe-agent's sandboxes are `sleep 2h`
  containers, so a task running longer loses its sandbox and ends without a
  patch. The 2026-09-19 llm-d run lost three tasks this way. Neither mode of
  the 2026-09-24 pair hit it.
- **The 131k context is a harder ceiling than the step cap.** Every empty patch
  in the 2026-09-24 pair exited `ContextWindowExceededError` at a peak context
  of ~130,900 — the step cap (100) was never reached. Check `exit` before
  blaming a timeout or a loop: the report's per-task table carries it, and
  `stats.py` prints peak context per task. Raising `-context` trades slots for
  headroom, so it changes what is being measured.
- **Resolve rate is noisy, and now measured.** The model samples at temperature
  1.0, so it moves by a task or two between runs of 20. The 2026-09-24 pair put
  a number on it: the two modes scored 17/20 and 15/20 but agreed on only **13**
  tasks — six of twenty flipped in *both* directions. So a 2-task gap carries no
  information at this n. Routing shows in cache share, call latency and total
  model wait; treat resolve rate as a guard against a routing change breaking
  correctness outright, not as the comparison.
- **Call latency** is measured from the agent's message timestamps: prompt
  ready to response received, so it includes queueing, prefill and decode.

## Runs so far

**Everything in this table predates 2026-09-25** and ran on the older chain.
Read it against runs from before that date, not against anything labelled
`router`. See "What changed since the pilot". These are due to be re-run.

| Run | Mode | Result |
|---|---|---|
| `pilot` | `direct`, 32 checkpoints/slot | 17/20 resolved; the baseline in the 2026-09-19 report |
| `llmd-ob` | llm-d optimized-baseline, prefill uncalibrated (2000 tok/s) | 15/17 resolved, 3 timed out (pile-up on minion) |
| `llmd-ob-cal` | llm-d optimized-baseline, prefill pinned at 871 tok/s | **stopped at 11/20**: 10/11 resolved; 2–3 tasks were piling up on the 5090 |
| `pilot-invalid-minion32k`, `pilot-invalid-oom` | — | discarded: stale binary on minion; OOM kill |
| `0925e-tuned` | llm-d `tuned`, 3 nodes, via the entrypoint-01 entrypoint | 17/20 resolved in **64 min** |
| `0925e-direct` | `direct`, same fleet and entrypoint | 15/20 resolved in **119 min** |
| `0924`, `0924b`, `0925`–`0925d` | — | discarded: see "Before a run" — a stale binary served for 20 min, a node restarted mid-run, a second `mfsh serve` took the first one down |

Nothing has been run on the current chain yet. The first `router` run is
also the first re-measurement of the baseline, which is why "Picking this up
again" starts there.

### The three-node pair, 2026-09-24

Report: [`docs/reports/2026-09-24-swe-routing-3node.html`](../../docs/reports/2026-09-24-swe-routing-3node.html).
Both modes finished all 20 tasks; neither lost one to the sandbox limit.

| | direct | `tuned` |
|---|---|---|
| Wall clock | 119 min | **64 min** |
| Resolved | 15/20 | 17/20 (see below) |
| Agent turns | 743 | 732 |
| Prompt tokens | 29.4M | 25.2M |
| **Prefilled** | **2,897,567** | **662,546** |
| Cache hits | 90.2% | 97.4% |
| p50 call | **7.2 s** | 7.8 s |
| p90 / p95 | 51.0 / 95.7 s | 45.2 / 70.0 s |
| p99 | 613.5 s | **153.8 s** |
| Worst call | 2316 s (39 min) | **394 s** |
| Model wait | 421 min | 228 min |

Two things to take from this and one not to.

**Scheduling did not make the median call faster.** `direct` is marginally
ahead at p50. The entire benefit is in the tail: p99 is 4× better and the worst
single call drops from 39 minutes to 7. Quoting a mean or a median here hides
the whole effect.

**The slowest node sets the pace when nothing is scheduling.** Per-run mean
occupancy:

| | busy, `tuned` | busy, direct |
|---|---|---|
| xpredator (5090, 1694 tok/s prefill) | 83% · 0.93 in flight | 75% · 0.80 |
| minion (6000 Ada, 1018 tok/s) | 90% · 0.91 | 72% · 1.12 |
| helion (Apple Silicon, 166 tok/s) | 91% · 1.61 | 87% · 1.32 |

helion prefills ~10× slower than the 5090, so the same request costs 10× there.
`direct` balanced by request count and could not know that, so under it
both GPUs idled about a quarter of the time while the Mac stayed pinned.
`tuned` keeps all three above 83% and parks the queue depth on helion
deliberately.

That exposes a cost we were not looking for: **llm-d's room filter counts a free
slot as a free slot regardless of the engine behind it**, so the slowest engine
looks available longest and carries the deepest queue. helion's decode fell to
11.2 tok/s under `tuned`.

Weighting a slot by the engine's measured rate was the action item here, and it
has since landed in ModelFabric's own router (`rate_weighted_routing`, on by default:
a candidate costs `(outstanding + 1) ÷ its share of the fastest measured rate`).
It is **unmeasured** — it is one of the reasons the `router` arm is not a
like-for-like replacement for `direct`, and the main thing the next run is for.
It is not in llm-d's room filter, which still counts slots.

**Do not read the resolve rates as a result.** 17 vs 15 is not a routing
finding: only **13 tasks resolved under both**. `tuned` uniquely solved four,
`direct` uniquely solved two — six of twenty flipped. At temperature 1.0 and
n=20 a 2-task gap is inside the sampling noise.

**The three empty patches are context exhaustion, not timeouts.** Every task
that failed to produce a patch exited `ContextWindowExceededError` — the
conversation grew past the 131,072-token pool. No task in either mode hit the
2-hour sandbox limit.

| | direct | `tuned` |
|---|---|---|
| astropy-13398 | 74 turns, peak 130,419 — exceeded | 58 turns, peak 130,976 — exceeded |
| astropy-14598 | 80 turns, peak 130,988 — exceeded | 40 turns, peak 87,483 — **submitted** |

13398 is a task this model cannot finish inside 131k in either mode; exclude
it. 14598 is more interesting and should not be over-read: under `direct` the
agent took twice as many turns on the same task and ran out of context doing
it. Turn count is the agent's own path at temperature 1.0, so one instance
proves nothing — but it is the mechanism by which a routing difference could
cost a *solve* rather than just time, and it is worth watching on the re-run.

The two modes also ran back to back rather than interleaved, so four hours of
drift all land on `direct`. Reversing the order is the check and has not been
run.

### The two-node pilot, 2026-09-19

On the 11 tasks `llmd-ob-cal` finished:

| | direct | optimized-baseline, uncalibrated | optimized-baseline, 871 tok/s |
|---|---|---|---|
| Finished | 11 | 9 (2 timed out) | 11 |
| Resolved | 10 | 8 | 10 |
| Cache hits | 75.3% | 97.0% | 95.5% |
| p95 call | 195 s | 34 s | 59 s |
| Model wait | 221 min | 48 min | 104 min |

When it was stopped, the 5090 had prefilled 5.4M tokens against minion's
0.6M, with 2 running + 1 queued while minion had a free slot for 40+ minutes:
calibration moved the pile-up to the other GPU without preventing it.

All of these live in `~/.local/share/modelfabric/bench/swe` on xpredator (trajectories in
`runs/`, grades in `eval/`).

## Picking this up again

The 2026-09-24 pair answered the question the pilot left open — scheduling is
worth the tail, not the median — and opened four it did not. The change of
chain adds a fifth, and makes it the first: the baseline has to be re-measured before
anything is compared against it.

```bash
make ui build         # then deploy the same binary to every peer
bench/swe/setup.sh    # no-op if the env exists

# 1. Re-establish the baseline on the current chain, router first — which
#    also reverses the order and rules out drift landing on one mode.
SWE_PEAK_PREFILL=614 SWE_ENTRYPOINT=entrypoint-01 \
SWE_AGENT_BASE=https://api.example.com/v1 SWE_CONTROL_ADDR=http://100.64.0.1:1234 \
  bench/swe/sequence.sh 0927 router tuned

# 2. The short-task counterpart, where prefix affinity cannot do the work.
bench/aider/sequence.sh 0927a tuned router
```

- **Re-measure the baseline.** Nothing has run on the current chain. Until
  a `router` arm exists, there is no number for `tuned` to be compared against:
  the 119 minutes belongs to the old `direct` arm.
- **Reverse the order.** `tuned` ran first both times it has been measured.
  Running `router` first, as above, is also the drift check — part of the
  64-vs-119-minute gap could have been four hours of drift.
- **Pin `SWE_PEAK_PREFILL`.** It is the one scheduler input that leaks between
  modes; it has drifted 2000 → 1010 → 1192 → 880 → 628 → 614 tok/s. Unpinned,
  two runs on different days are not comparable.
- **Rate-weighted slots, now that they are live.** The finding was that helion's
  slot is not worth a 5090's, which is why the slow node ended up with the
  deepest queue (1.61 in flight against 0.93 on the 5090) and its decode fell to
  11.2 tok/s. ModelFabric's router now weights a slot by its engine's measured rate;
  the `router` arm is the first measurement of whether that fixes it. llm-d's
  room filter still counts slots, so `tuned` should still show the effect.
- **`load-aware`** is still unmeasured on three nodes.
- **Reasoning effort.** 98% of the output on these tasks is reasoning. Whether
  it buys the resolve rate is a separate experiment.

Before any run, check the fixes are live: `mfsh llmd status` shows `room
filter  on`, and `mfsh endpoints` shows `modelfabric.sh/slots: "2"` and a
`modelfabric.sh/prefill-tok-s` on every engine once they have served traffic.

`report/build.py` takes any number of `--run LABEL=NAME`; the first is the
baseline everything else is measured against, so pass `router` first. Its
`--direct`/`--llmd` shorthands are still there, because the pilot's command line
is in this README's history and in shell history; they only set a label.

## Entrypoint

The agent can talk to loopback on the machine running the benchmark, but the
three-node runs deliberately do not. `SWE_ENTRYPOINT=entrypoint-01` with
`SWE_AGENT_BASE=https://api.example.com/v1` sends every request over public
HTTPS into nginx, to ModelFabric's front door on entrypoint-01, and out across the tailnet
to whichever engine is chosen — the same path a real caller takes, including the
WAN hop.

Measured, that hop is 85 ms RTT plus 88 ms of TLS once per connection, against
a p50 of 7.2 s: about 3%, and not where any of the difference in these runs
comes from. It is in the setup because it exercises entrypoint routing, not
because it costs anything.

Two things it requires:

- `SWE_CONTROL_ADDR` must be the entrypoint's **own loopback-equivalent
  address** for management calls. Keys and logs are read from a node's own
  disk and deliberately not served over the mesh, so `run.sh` fetches the
  entrypoint's key over `tailscale ssh` rather than with an `-addr` flag.
- `public_listen` on the entrypoint must stay on loopback (nginx proxies to
  `127.0.0.1:1235`). Moving it to the tailnet address takes
  `https://api.example.com/v1` down.

## Other scripts

- `check-engines.py`: `mfsh endpoints | check-engines.py` lists each engine's
  real context and runtime.
- `replay-sequential.py`: replays one trajectory turn by turn against one
  engine, which is how checkpoint memory was measured (load with `-arg -v` to
  log checkpoint sizes).
- `replay-interleaved.py`: interleaves conversations to exercise the prompt
  cache. It does not reproduce checkpoint growth.
