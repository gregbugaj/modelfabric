#!/usr/bin/env python3
"""Select a fixed, language-balanced subset of polyglot exercises.

    subset.py EXERCISES_DIR N [SEED]

Print comma-separated exercise names for aider's --keywords filter.
A seeded subset avoids aider's unseeded --num-tests selection and keeps
routing modes on the same exercises. Round-robin selection balances languages;
aider may still shuffle their execution order.
Reject names contained in another exercise name because --keywords uses
substring matching and would select both.
"""
import pathlib
import random
import sys


def exercises(root: pathlib.Path) -> list[str]:
    """Every exercise as aider names it: <lang>/exercises/practice/<name>."""
    out = []
    for lang in sorted(p for p in root.iterdir() if p.is_dir() and not p.name.startswith(".")):
        practice = lang / "exercises" / "practice"
        if not practice.is_dir():
            continue
        out += sorted(str(d.relative_to(root)) for d in practice.iterdir() if d.is_dir())
    return out


def pick(names: list[str], n: int, seed: int) -> list[str]:
    """n exercises, spread as evenly over the languages as n allows."""
    by_lang: dict[str, list[str]] = {}
    for dn in names:
        by_lang.setdefault(dn.split("/")[0], []).append(dn)

    rng = random.Random(seed)
    pools = {lang: rng.sample(v, len(v)) for lang, v in sorted(by_lang.items())}
    # Round-robin supports uneven language counts and subsets not divisible by six.
    chosen: list[str] = []
    while len(chosen) < n and any(pools.values()):
        for lang in sorted(pools):
            if pools[lang] and len(chosen) < n:
                chosen.append(pools[lang].pop())
    return sorted(chosen)


def main() -> int:
    if len(sys.argv) not in (3, 4):
        print(__doc__.strip().splitlines()[2].strip(), file=sys.stderr)
        return 2
    root = pathlib.Path(sys.argv[1])
    n, seed = int(sys.argv[2]), int(sys.argv[3]) if len(sys.argv) == 4 else 20260924
    if not root.is_dir():
        print(f"subset.py: no such exercises directory: {root}", file=sys.stderr)
        return 1

    names = exercises(root)
    if not names:
        print(f"subset.py: no exercises under {root}", file=sys.stderr)
        return 1
    if n > len(names):
        print(f"subset.py: asked for {n} of {len(names)} exercises", file=sys.stderr)
        return 1

    chosen = pick(names, n, seed)
    # A chosen name that is a substring of any other exercise would match both.
    ambiguous = [c for c in chosen if any(c != o and c in o for o in names)]
    if ambiguous:
        print("subset.py: these names are substrings of other exercises and "
              "cannot be used as keywords: " + ", ".join(ambiguous), file=sys.stderr)
        return 1

    langs: dict[str, int] = {}
    for c in chosen:
        langs[c.split("/")[0]] = langs.get(c.split("/")[0], 0) + 1
    print(f"subset.py: {len(chosen)} of {len(names)}, seed {seed} — "
          + ", ".join(f"{k} {v}" for k, v in sorted(langs.items())), file=sys.stderr)
    print(",".join(chosen))
    return 0


if __name__ == "__main__":
    sys.exit(main())
