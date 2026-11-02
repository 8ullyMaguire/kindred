#!/usr/bin/env python3
"""Print the tune specs for the nPMI bake-off, one `name<TAB>spec` per line.

    sudo kindred tune --name default --json --db ... --corpus ... > /tmp/base.json
    python3 scripts/eval-arms.py /tmp/base.json | while IFS=$'\\t' read -r name spec; do
        sudo kindred tune --name "$name" --set "$spec" --db ... --corpus ...
    done

Every spec sums to 1.0 because `kindred tune --set` validates that (`rank.Score`
renormalises by the sum, so the SUM is the scale). Two consequences worth knowing:

* arm `base` is the default vector rescaled to 1.0, which is BEHAVIOURALLY IDENTICAL
  to the default tune. It exists to prove the arm machinery: if `base` differs from a
  plain `eval` on the same cohort, the specs are wrong and no other arm can be read.
* removing a signal rescales the rest, which preserves their ratios, so `no_idf` is a
  clean ablation of one dimension rather than a shift of every weight.
"""
import json
import sys


def main() -> int:
    if len(sys.argv) != 2:
        print(__doc__, file=sys.stderr)
        return 2
    with open(sys.argv[1]) as f:
        raw = json.load(f)
    w = raw.get("weights", raw)  # tolerate both shapes `tune --json` may print

    def spec(weights):
        total = sum(weights.values())
        if total <= 0:
            raise SystemExit("empty weight vector")
        return ",".join(
            f"{k}={v / total:.12f}" for k, v in sorted(weights.items()) if v > 0
        )

    def share(weights, name, s):
        """Give `name` exactly share s; scale the rest so their ratios hold."""
        rest = {k: v for k, v in weights.items() if k != name and v > 0}
        total = sum(rest.values())
        out = {k: v * (1 - s) / total for k, v in rest.items()}
        out[name] = s
        return out

    def drop(weights, name):
        """Remove one signal; the rest keep their ratios (a clean ablation)."""
        return {k: v for k, v in weights.items() if k != name and v > 0}

    arms = {
        # The machinery check: identical to the default tune by construction.
        "np_base": spec(w),
        # Ablations: how much does each half contribute at all?
        "np_no_idf": spec(drop(w, "idf_overlap")),
        "np_no_neighbour": spec(drop(w, "neighbourhood")),
        # Dose-response for the new signal: 0.06 / 0.12 / 0.24.
        "np_npmi_06": spec(share(w, "tag_npmi", 0.06)),
        "np_npmi_12": spec(share(w, "tag_npmi", 0.12)),
        "np_npmi_24": spec(share(w, "tag_npmi", 0.24)),
        # The direct question: nPMI at the share IDF holds today, IDF off.
        "np_swap12": spec(share(drop(w, "idf_overlap"), "tag_npmi", 0.12)),
    }
    for name, s in arms.items():
        print(f"{name}\t{s}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
