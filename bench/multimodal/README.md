# Mixed-capability routing — investigated, not built

**Status: the premise was tested and did not hold. Nothing here is built, and
the benchmark as first designed should not be.** This records what was checked,
what turned out to be true, and the one real item that survived.

## What this was going to be

A benchmark for something no published suite measures: a fleet where only some
engines can read an image. The design was to load one engine `-vision off` and
two `-vision on`, drive mixed text and image traffic, and measure whether each
routing mode placed correctly.

It rested on a trade that looked real. This model carries an image projector,
and llama.cpp used to fail a prompt carrying an image while drafting — `failed
to process mtmd chunk` — so ModelFabric turned speculation off for any model that
loaded one (it no longer does, as of 2026-09-25). Loading `-vision off` avoided the defect and got MTP back, worth
about 2× on decode. A fleet would then *want* mixed capability: keep one engine
fast and text-only, let the others take the images.

## What was measured, 2026-09-24

The trade does not exist on the builds this fleet runs.

| node | build | image | drafts | accepted | decode |
|---|---|---|---|---|---|
| minion | LM Studio `cuda12-avx2@2.40.0` | 48KB | 119 | 78 (66%) | 75 tok/s |
| minion | same | 739KB, 3,299 image tokens | 119 | 59 (50%) | 75 tok/s |
| minion | same, `-parallel 2` | 986KB + 827KB **concurrent** | 95 / 121 | 57 / 48 | 55 / 43 tok/s |
| helion | Metal `advsimd@2.41.0` | 48KB | 79 | 52 (66%) | 24.5 tok/s |

Loaded with `-vision on -spec mtp -context 65536 -parallel 2` — the benchmark's
own configuration plus the projector — images and speculation coexist, at the
sizes and concurrency the defect was supposed to break. The replies described
the images correctly, so the engine was reading them rather than merely not
crashing. Upstream has been working this area
([llama.cpp#28587](https://github.com/ggml-org/llama.cpp/pull/28587)); whatever
fixed it is already in these builds.

So there is no reason for an engine on this fleet to be text-only, and a
benchmark whose fleet shape is "one engine gives up images for speed" would be
measuring a configuration nobody would choose.

Two things follow beyond this directory. `-vision off` should come out of
`SWE_LOAD_ARGS` — see [`../swe/README.md`](../swe/README.md), where the reason
recorded for the flag was already wrong when it was written. And the published
2026-09-24 results are unaffected, because both arms used identical settings.

## What survived

**llm-d filters image requests by engine capability.** Every exported endpoint carries a
`modelfabric.sh/vision` label. ModelFabric chooses the vision scheduling profile for requests
carrying images, and the generated EPP configuration filters out engines without a projector.
Text requests use the default profile. ModelFabric's own router also filters image requests
by capability.

The useful benchmark case is a mixed fleet where some engines cannot serve images, rather than
one where every engine has the same capability. That can happen when an engine is loaded without
its projector or a runtime does not support image inputs.

## What traditional routers do here

Capability routing exists, but per *model*: LiteLLM carries `supports_vision`
in model metadata, OpenRouter filters providers by modality. That is "this
model can see, that one cannot" — routing by model name.

What none of them do is place within one model name across replicas that differ
in capability, because nobody deploys that. A Kubernetes Deployment has uniform
arguments and llm-d's InferencePool assumes homogeneous replicas. It is a
sound assumption in a container world and a false one here, which is why the
hole above is ModelFabric's to close rather than something to wait upstream for.

## If this is revisited

The question to ask first is whether any engine in the fleet genuinely cannot
serve something the others can — a real capability gap, not one created by a
workaround. If there is one, the harness sketched here still applies: mixed
traffic at fixed concurrency, assert the incapable engine never receives what
it cannot serve, count what each routing mode gets wrong. Images from
[SWE-bench Multimodal](https://huggingface.co/datasets/SWE-bench/SWE-bench_Multimodal)
(862 real assets across 617 instances) make realistic payloads without the
JavaScript container estate that running the benchmark properly would need.
