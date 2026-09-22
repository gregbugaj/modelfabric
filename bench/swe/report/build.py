#!/usr/bin/env python3
"""Builds the routing comparison page from two or more graded runs.

Usage:
  build.py --work DIR --run LABEL=NAME [--run LABEL=NAME ...] --out PAGE.html
  build.py --work DIR --direct NAME --llmd NAME --out PAGE.html   # the old pair

DIR is $SWE_WORK (runs/ and eval/ beneath it); NAME is a run directory under
runs/, graded by grade.sh into eval/<model>.NAME.json. LABEL is what the page
calls that mode.

The first --run is the baseline: deltas and the "better" marks are measured
against it, so pass the unscheduled arm ("router") first.

A task any mode failed to finish is left out of the like-for-like comparison
and reported as timed out for that mode, because a run that lost three tasks
to the sandbox limit is not comparable on totals to one that lost none.

Writes the page (standalone HTML) and its data as .json beside it. The page's
narrative text lives in template.html; edit it when the findings change.
"""
import argparse, glob, json, math, os, re

HERE = os.path.dirname(os.path.abspath(__file__))


def load_run(work, name):
    out = {}
    for f in glob.glob(os.path.join(work, "runs", name, "*", "*.traj.json")):
        t = json.load(open(f))
        prompt = cached = output = turns = peak = 0
        prev, lat = None, []
        for m in t["messages"]:
            ex = m.get("extra") or {}
            ts = ex.get("timestamp")
            if m.get("role") == "assistant":
                u = (ex.get("response") or {}).get("usage") or {}
                p, o = u.get("prompt_tokens", 0), u.get("completion_tokens", 0)
                prompt += p
                cached += (u.get("prompt_tokens_details") or {}).get("cached_tokens") or 0
                output += o
                turns += 1
                peak = max(peak, p + o)
                if prev and ts:
                    lat.append(ts - prev)
            if ts:
                prev = ts
        out[t["instance_id"]] = dict(exit=t["info"].get("exit_status"), turns=turns, prompt=prompt,
                                     cached=cached, out=output, maxctx=peak, lat=lat, model_s=sum(lat))
    return out


def grade(work, name, run):
    reports = glob.glob(os.path.join(work, "eval", f"*.{name}.json"))
    if not reports:
        raise SystemExit(f"no grading report for {name}; run grade.sh {name}")
    resolved = set(json.load(open(reports[0]))["resolved_ids"])
    for k, v in run.items():
        v["resolved"] = k in resolved


def aggregate(rows):
    # A run can be empty, a trajectory can have no assistant turn, and usage
    # can be missing: percentiles off an empty list and a cache percentage
    # over zero prompt tokens both raised instead of reporting the run.
    lat = sorted(x for r in rows for x in r["lat"])
    P = sum(r["prompt"] for r in rows)
    C = sum(r["cached"] for r in rows)
    pc = lambda q: round(lat[min(len(lat) - 1, int(len(lat) * q))], 1) if lat else None
    return dict(tasks=len(rows), resolved=sum(r["resolved"] for r in rows),
                submitted=sum(r["exit"] == "Submitted" for r in rows),
                turns=sum(r["turns"] for r in rows), prompt=P, cached=C, prefilled=P - C,
                cache_pct=round(100 * C / P, 1) if P else None, out=sum(r["out"] for r in rows),
                p50=pc(.5), p90=pc(.9), p95=pc(.95), p99=pc(.99), max=round(lat[-1], 1) if lat else None,
                model_min=round(sum(lat) / 60, 1))


def percentile_points(lat, n=120, lo=0.5):
    """Latency at each percentile, as [percent, seconds], for the docs chart.

    Sampled evenly in -log10(1-p) rather than evenly in rank, because that is
    the axis the chart draws: every further nine gets the same width and the
    same number of points. Sampling evenly in rank instead puts almost nothing
    past p99, which is the only part of these runs that differs.

    Starts at the median. Everything below it is two modes doing the same
    thing at the same speed, and drawing it only shrinks the part that isn't.
    """
    if len(lat) < 2:
        return []
    v = sorted(lat)
    at = lambda p: v[min(len(v) - 1, max(0, math.ceil(len(v) * p) - 1))]
    # Past 1 - 0.5/len the curve is just the single slowest call repeated.
    hi = 1 - 0.5 / len(v)
    t0, t1 = -math.log10(1 - lo), -math.log10(1 - hi)
    out = []
    for i in range(n + 1):
        p = 1 - 10 ** -(t0 + (t1 - t0) * i / n)
        out.append([round(100 * p, 4), round(at(p), 2)])
    return out


def docs_data(data, runs, labels, run_label):
    """The benchmark page's data file, in the shape site/components expects.

    Written from the same aggregation as the HTML page so the two cannot
    disagree, and so nothing on the docs site is retyped by hand. Arms are
    called a and b rather than by mode name: the page reads the names from
    `arms`, which is what lets a later run swap in without touching the
    components.
    """
    if len(labels) != 2:
        raise SystemExit("--docs-data wants exactly two --run arms (baseline first)")
    a, b = labels
    key = {a: "a", b: "b"}
    return dict(
        run=run_label,
        arms={"a": a, "b": b},
        n={"common": len(data["common"]), "all": data["all"][a]["tasks"]},
        agg={"a": data["cmp"][a], "b": data["cmp"][b], "a_all": data["all"][a]},
        # Tasks a mode never finished: the page marks them rather than dropping
        # a row, so a run that lost three is not silently compared on 17.
        timed_out=sorted(set(data["missing"][a]) | set(data["missing"][b])),
        tasks=[dict(id=t["id"], **{key[l]: t["by"][l] for l in labels}) for t in data["tasks"]],
        pct={key[l]: percentile_points(data["lat_hist"][l]) for l in labels},
        calls={key[l]: len(data["lat_hist"][l]) for l in labels},
    )


def task_view(r):
    if r is None:
        return None
    v = {k: r[k] for k in ("exit", "turns", "maxctx", "resolved")}
    v.update(cache=round(100 * r["cached"] / r["prompt"], 1), model_s=round(r["model_s"]))
    return v


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--work", default=os.environ.get("SWE_WORK", os.path.expanduser("~/.local/share/modelfabric/bench/swe")))
    ap.add_argument("--run", action="append", default=[], metavar="LABEL=NAME",
                    help="a graded run and what to call it; repeatable, first is the baseline")
    ap.add_argument("--direct", help="shorthand for --run direct=NAME")
    ap.add_argument("--llmd", help="shorthand for --run llm-d=NAME")
    ap.add_argument("--out", required=True)
    ap.add_argument("--docs-data", metavar="PATH",
                    help="also write the site benchmark page's data (site/data/swe.json)")
    ap.add_argument("--docs-run", metavar="LABEL", default=None,
                    help="what the docs page calls this run; defaults to the date in --out")
    a = ap.parse_args()

    # The old two-run form still works: the pilot's command line is in the
    # README and in shell history, and silently changing what it means would
    # be worse than carrying two lines of translation.
    specs = []
    if a.direct:
        specs.append(("direct", a.direct))
    if a.llmd:
        specs.append(("llm-d", a.llmd))
    for spec in a.run:
        label, _, name = spec.partition("=")
        if not name:
            raise SystemExit(f"--run wants LABEL=NAME, got {spec!r}")
        specs.append((label, name))
    if not specs:
        raise SystemExit("give at least one --run LABEL=NAME (or --direct/--llmd)")
    if len(specs) != len({l for l, _ in specs}):
        raise SystemExit("two runs share a label; the page keys on it")

    runs = {}
    for label, name in specs:
        runs[label] = load_run(a.work, name)
        grade(a.work, name, runs[label])

    labels = [l for l, _ in specs]
    base = labels[0]
    # Every task the baseline attempted, so a mode that lost tasks still shows
    # them as timed out rather than vanishing from the table.
    every = sorted(set().union(*(set(r) for r in runs.values())))
    common = sorted(set.intersection(*(set(r) for r in runs.values())))
    missing = {l: sorted(set(every) - set(runs[l])) for l in labels}

    data = dict(
        modes=labels, base=base, runs={l: n for l, n in specs},
        common=common, missing=missing,
        all={l: aggregate(list(runs[l].values())) for l in labels},
        cmp={l: aggregate([runs[l][k] for k in common]) for l in labels},
        tasks=[dict(id=k, by={l: task_view(runs[l].get(k)) for l in labels}) for k in every],
        lat_hist={l: [round(x, 1) for k in common for x in runs[l][k]["lat"]] for l in labels},
    )
    template = open(os.path.join(HERE, "template.html")).read()
    assert "/*DATA*/null" in template
    # The JSON lands inside a <script>, where the browser looks for "</script>"
    # before the JavaScript parser ever runs: a task id or error string
    # containing one would end the script and inject whatever followed.
    # U+2028/U+2029 are line terminators in JS but legal in JSON strings.
    blob = (json.dumps(data)
            .replace("</", "<\\/")
            .replace("\u2028", "\\u2028")
            .replace("\u2029", "\\u2029"))
    page = template.replace("/*DATA*/null", blob)
    with open(a.out, "w") as f:
        f.write('<!doctype html>\n<html lang="en">\n<head>\n<meta charset="utf-8">\n'
                '<meta name="viewport" content="width=device-width, initial-scale=1">\n</head>\n<body>\n')
        f.write(page)
        f.write("\n</body>\n</html>\n")
    json.dump(data, open(os.path.splitext(a.out)[0] + ".json", "w"), indent=1)
    lost = ", ".join(f"{l} lost {len(v)}" for l, v in missing.items() if v) or "none lost"
    print(f"wrote {a.out}: {len(labels)} modes, {len(common)} tasks compared ({lost})")

    if a.docs_data:
        # The run label the docs page prints. The report file is named by date
        # and the page has always shown that date, so read it back rather than
        # asking for it twice and letting the two drift.
        run = a.docs_run
        if not run:
            m = re.search(r"(\d{4}-\d{2}-\d{2})", os.path.basename(a.out))
            run = m.group(1) if m else os.path.splitext(os.path.basename(a.out))[0]
        json.dump(docs_data(data, runs, labels, run), open(a.docs_data, "w"), indent=1)
        print(f"wrote {a.docs_data}: site data for {run}")


if __name__ == "__main__":
    main()
