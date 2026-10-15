#!/usr/bin/env python3
"""Render a kindred recommend JSON into the five export formats.

Every number printed comes from the run's own JSON (or the seed block read
from the corpus) -- nothing is recomputed here.

usage: scripts/render-rec-export.py <recs.json> <seed.json> <slug> <outdir>

The seed block (summary/words/hits/kudos/bookmarks/complete/tags) is NOT in
the recommend response, so it is passed in: read it from the corpus
(works + work_tags joined to tags) and hand it over as JSON.

Reproduces the 2026-10-08 export set byte for byte (exports/82026196-*),
apart from the deliberate `**Degraded signals:**` / `degraded:` lines, which
the earlier rendering did not carry.
"""
import csv
import json
import os
import sys

THIN = "------------------------------------------------------------------------------"


def fmt(n):
    return f"{int(n):,}"


def top_evidence(item):
    ev = item.get("evidence") or []
    if not ev:
        return ("", "")
    best = max(ev, key=lambda e: (e.get("value", 0) or 0) * (e.get("weight", 0) or 0))
    return best.get("signal", ""), best.get("reason", "")


def ended(item):
    return "complete" if item["stats"].get("complete") else "WIP"


def render_md(d, seed):
    items, meta = d["items"], d["meta"]
    tune = d.get("tune", {})
    sid = d["seeds"][0].split(":", 1)[-1]
    L = []
    L.append(f"# {len(items)} recommendations from `{sid}`")
    L.append("")
    L.append(f"**Seed:** [{seed['title']}]({seed['url']})  ")
    L.append(
        f"**Engine:** kindred `{meta.get('tune')}`, {meta['seeds']} seed / "
        f"{meta['candidates']} candidates / pool {meta['pool_size']}  "
    )
    sh = meta.get("shortfall")
    if sh:
        L.append(f"**Coverage:** {sh['returned']} of {sh['requested']} requested — {sh['reason']}")
    else:
        L.append(f"**Coverage:** {len(items)} of {len(items)} requested")
    deg = meta.get("degraded") or []
    L.append(f"**Degraded signals:** {', '.join(deg) if deg else 'none'}")
    L.append("")
    L.append("| # | Title | Words | Kudos | Complete |")
    L.append("|---:|---|---:|---:|:---:|")
    for i, it in enumerate(items, 1):
        s = it["stats"]
        L.append(
            f"| {i} | [{it['title']}]({it['url']}) | {fmt(s.get('word_count', 0))} | "
            f"{fmt(s.get('kudos', 0))} | {'yes' if s.get('complete') else 'WIP'} |"
        )
    L.append("")
    L.append("## Why each one (top signal)")
    L.append("")
    for i, it in enumerate(items, 1):
        _, reason = top_evidence(it)
        L.append(f"{i}. **{it['title']}** — {reason}")
    return "\n".join(L) + "\n"


def render_reddit(d, seed):
    items = d["items"]
    L = []
    L.append(f"Title: AO3 fics like “{seed['title']}”")
    L.append("")
    L.append("---")
    L.append("")
    L.append(f"### {seed['title']} on AO3")
    L.append("")
    L.append(" ".join((seed.get("summary") or "").split()))
    L.append("")
    L.append("#### Details")
    L.append("")
    L.append(f"Words: {fmt(seed.get('word_count', 0))}")
    L.append("")
    L.append(f"Hits: {fmt(seed.get('hits', 0))}")
    L.append("")
    L.append(f"Kudos: {fmt(seed.get('kudos', 0))}")
    L.append("")
    L.append(f"Bookmarks: {fmt(seed.get('bookmarks', 0))}")
    L.append("")
    L.append(f"Complete: {1 if seed.get('complete') else 0}")
    L.append("")
    L.append("#### Tags")
    L.append("")
    L.append(", ".join(seed.get("tags") or []))
    L.append("")
    L.append("#### Similar fics")
    L.append("")
    for i, it in enumerate(items, 1):
        s = it["stats"]
        L.append(
            f"{i}. [{it['title']}]({it['url']}) — {fmt(s.get('word_count', 0))} words, "
            f"{fmt(s.get('kudos', 0))} kudos, {ended(it)}"
        )
        L.append("")
    return "\n".join(L).rstrip("\n") + "\n"


def render_txt(d, seed):
    items, meta = d["items"], d["meta"]
    sid = d["seeds"][0].split(":", 1)[-1]
    L = []
    L.append(f"{len(items)} recommendations from seed {sid} — {seed['title']}")
    L.append(seed["url"])
    L.append(f"engine: kindred tune={meta.get('tune')} pool={meta['pool_size']}")
    sh = meta.get("shortfall")
    if sh:
        L.append(f"coverage: {sh['returned']}/{sh['requested']} — {sh['reason']}")
    deg = meta.get("degraded") or []
    L.append(f"degraded: {', '.join(deg) if deg else 'none'}")
    L.append(THIN)
    for i, it in enumerate(items, 1):
        s = it["stats"]
        _, reason = top_evidence(it)
        tags = ", ".join(it.get("tags") or [])
        if len(tags) > 140:
            tags = tags[:140]
        L.append(f"{i:3d}. {it['title']}")
        L.append(f"     {it['url']}")
        L.append(
            f"     {fmt(s.get('word_count', 0))} words | {fmt(s.get('kudos', 0))} kudos | "
            f"{fmt(s.get('hits', 0))} hits | {ended(it)}"
        )
        L.append(f"     tags: {tags}")
        L.append(f"     why:  {reason}")
        L.append("")
    return "\n".join(L) + "\n"


def render_csv(d, path):
    items = d["items"]
    with open(path, "w", newline="", encoding="utf-8") as fh:
        w = csv.writer(fh)
        w.writerow(["rank", "id", "title", "url", "words", "kudos", "hits", "complete",
                    "score", "top_signal", "top_reason", "tags"])
        for i, it in enumerate(items, 1):
            s = it["stats"]
            sig, reason = top_evidence(it)
            w.writerow([
                i, it["id"], it["title"], it["url"], s.get("word_count", 0),
                s.get("kudos", 0), s.get("hits", 0), s.get("complete", 0),
                f"{it['score']:.4f}".rstrip("0").rstrip("."), sig, reason, "; ".join(it.get("tags") or []),
            ])


def main():
    recs_path, seed_path, slug, outdir = sys.argv[1:5]
    with open(recs_path, encoding="utf-8") as fh:
        d = json.load(fh)
    with open(seed_path, encoding="utf-8") as fh:
        seed = json.load(fh)
    os.makedirs(outdir, exist_ok=True)
    base = os.path.join(outdir, slug)
    with open(base + ".md", "w", encoding="utf-8") as fh:
        fh.write(render_md(d, seed))
    with open(base + ".reddit.md", "w", encoding="utf-8") as fh:
        fh.write(render_reddit(d, seed))
    with open(base + ".txt", "w", encoding="utf-8") as fh:
        fh.write(render_txt(d, seed))
    render_csv(d, base + ".csv")
    print(f"wrote {base}.{{md,reddit.md,txt,csv}} — {len(d['items'])} items")


if __name__ == "__main__":
    main()
