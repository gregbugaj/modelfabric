#!/usr/bin/env python3
"""Picks a fixed, language-balanced subset of the polyglot exercises.

    subset.py EXERCISES_DIR N [SEED]

Prints the chosen exercises as one comma-separated line, ready for aider's
`--keywords`, which keeps only the exercises whose directory name contains one
of them.

Why this exists. aider's harness does:

    random.shuffle(test_dnames)
    if num_tests > 0:
        test_dnames = test_dnames[:num_tests]

with no seed, so `--num-tests 60` means *a different* 60 of the 225 on every
invocation. Two routing modes would then be solving different exercises, which
is not a comparison — and the same flag cannot reproduce a past run either.

Two things fall out of picking the subset here instead:

  * every mode runs the same exercises, because the list is passed in;
  * the split across the six languages is even, which shuffling does not
    promise. The languages are far from equal in size (javascript 49,
    cpp 26), so an unseeded draw of 60 can come back badly skewed, and a
    routing difference would then be tangled with a language difference.

The shuffle still runs, over the exercises we passed. That only changes the
order they arrive in, not which work is done, and varying arrival order between
modes is fine — arguably better than fixing it.

The keyword match is a substring test, so this refuses to emit a name that is
contained in another exercise's name: that would quietly pull in a second
exercise. As of the 2026-09 checkout no such pair exists.
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
    # Round-robin over the languages rather than n//6 each: it gives the same
    # even split when the languages are big enough, and degrades sensibly when
    # one runs out or when n is not a multiple of six.
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
