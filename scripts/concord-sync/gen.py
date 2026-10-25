#!/usr/bin/env python3
"""Build the Concord payload for the kindred project: documents + features.

Sources of truth, all measured from the tree rather than remembered:
  docs/*.md, README.md            -> project_documents (kind by filename)
  cmd/kindred/main.go usage block -> one shipped row per subcommand
  internal/web/registry.go        -> one shipped row per page template (+ its routes)
  internal/api/server.go          -> shipped rows for the API families
  docs/PLAN-recsurfaces-31-100.md -> 72 audited rows, status carried across
  this week's units               -> shipped rows for harvest/shelf/bookmarks/QA
"""
import json
import os
import re
import sys

REPO = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
VAULT = os.path.expanduser("~/secondbrain/10-Projects/kindred")

DOC_KIND = [
    (r"^README\.md$", "readme"),
    (r"^SPEC", "spec"),
    (r"^PLAN", "plan"),
    (r"^DECISIONS", "adr"),
    (r"^[Hh][Aa][Nn][Dd][Oo][Ff][Ff]", "changelog"),
]
SKIP_DOCS = {"WHAT-IS-LEFT.md"}  # superseded by what-is-left.md (same title, Oct 5 vs Oct 23)


def doc_kind(name):
    for pat, kind in DOC_KIND:
        if re.match(pat, name):
            return kind
    return "wiki"


def title_of(body, fallback):
    for line in body.splitlines():
        if line.startswith("# "):
            return line[2:].strip()[:200]
    return fallback


def documents():
    out = []
    files = [("README.md", os.path.join(REPO, "README.md"))]
    for name in sorted(os.listdir(os.path.join(REPO, "docs"))):
        if name.endswith(".md") and name not in SKIP_DOCS:
            files.append((name, os.path.join(REPO, "docs", name)))
    files.append(("HANDOFF.md", os.path.join(REPO, "HANDOFF.md")))  # the current one at the root
    for name, path in files:
        if not os.path.exists(path):
            continue
        body = open(path, encoding="utf-8").read()
        slug = re.sub(r"[^a-z0-9]+", "-", name[:-3].lower()).strip("-")
        # Two different documents are both called HANDOFF.md (the arena handoff at the root, the
        # general one in docs/). The slug is the identity on the page, so the root one is named
        # for what it is rather than shadowed by directory order.
        if name == "HANDOFF.md" and os.path.dirname(path) == REPO:
            slug = "handoff-arena-ranking"
        out.append({
            "kind": doc_kind(name),
            "slug": slug,
            "title": title_of(body, name[:-3]),
            "body": body,
        })
    # The shelf SPEC lives in the vault: it documents a shipped command and the numbers behind it.
    shelf = os.path.join(VAULT, "SPEC-shelf.md")
    if os.path.exists(shelf):
        body = open(shelf, encoding="utf-8").read()
        out.append({"kind": "spec", "slug": "spec-shelf", "title": title_of(body, "Shelf spec"), "body": body})
    return out


COMMANDS = [
    ("serve", "run the HTTP API (the read-only mirror, incl. the unofficial AO3 surface)"),
    ("ingest", "measure the corpus and build the index"),
    ("recommend", "recommend entities from seeds"),
    ("crawl", "fetch AO3 work pages into the mirror (the only networked subcommand)"),
    ("shelf", "a reader's 100 fandoms and 200 works each, from their bookmarks"),
    ("harvest", "fill the mirror from AO3's catalogue of fandoms (breadth, enrich, backfill-types)"),
    ("bookmarks", "read a reader's public AO3 bookmarks into the mirror and their taste map"),
    ("profile", "build, list, show and rate taste profiles"),
    ("rate", "fold one like/dislike into a profile (alias for 'profile rate')"),
    ("corpus-query", "ask a question about the corpus (fandom-ranking, underrated, tag-neighbours, surprise, tag-trend)"),
    ("browse", "recent works matching a fandom family and a concept family"),
    ("eval", "score the recommender against held-out bookmarks (recall@K, NDCG@K)"),
    ("stats", "print corpus and index statistics"),
    ("dump", "write an anonymised, signed snapshot for peers"),
    ("verify", "verify a snapshot's signature and hashes"),
    ("fetch", "pull a snapshot from a peer's .onion service"),
    ("tune", "inspect or set signal weights"),
    ("embed-check", "fingerprint the embedding table and compare it with the previous build"),
    ("als", "train implicit-feedback factors over the mirror's reader interactions"),
    ("tagtree", "cluster the tag embeddings into a family tree (#37)"),
    ("worktree", "cluster works into theme families (#47)"),
    ("seasons", "which tags belong to which calendar month (#80)"),
    ("velocity", "which tags are rising in a recent window"),
    ("author-eval", "score the author-similarity proxy against held-out readers"),
    ("discover", "list the mirror's instance-level crawl sources"),
    ("autocrawl", "run the crawl over those sources"),
]


def page_features():
    """template -> the route paths that render it.

    The registry is a slice of pageSpec literals carrying `Paths: []string{...}`, so the parse is
    "from this Template to the next one" rather than a per-line regex: two specs share search.html
    (the home page and /search), and a per-line scan loses the one whose Paths sit on the next line.
    """
    src = open(os.path.join(REPO, "internal/web/registry.go"), encoding="utf-8").read()
    marks = list(re.finditer(r'Template:\s+"([a-z0-9_]+)\.html"', src))
    pages = {}
    for i, m in enumerate(marks):
        name = m.group(1)
        block = src[m.end(): marks[i + 1].start() if i + 1 < len(marks) else len(src)]
        paths = re.findall(r'"([^"]*)"', re.search(r"Paths:\s*\[\]string\{([^}]*)\}", block).group(1)) \
            if re.search(r"Paths:\s*\[\]string\{([^}]*)\}", block) else []
        prefix = bool(re.search(r"Prefix:\s*true", block))
        pages.setdefault(name, [])
        for path in paths:
            label = path + ("* (prefix)" if prefix else "")
            if label not in pages[name]:
                pages[name].append(label)
    return pages


API_FAMILIES = [
    ("recommend", ["/api/v1/recommend"], "The recommender endpoint: POST seeds + filter, GET for the browse default. Answers with works, the per-signal breakdown and meta.degraded."),
    ("taste", ["/api/v1/taste"], "Read and write the caller's taste profile (GET returns the map the ranking uses, PUT folds an explicit like/dislike set in)."),
    ("arena", ["/api/v1/arena"], "Pairwise arena: compare two entities, batch-compare, leaderboards, my-ranking, per-entity rank. The engine that produces judgements the weights are tuned on."),
    ("algo", ["/api/v1/algo"], "Per-kind algorithm elections: list/compare kinds, standings per kind, and the choice/judge endpoints that record a preference between two algorithms."),
    ("interleave", ["/api/v1/interleave"], "Interleaved A/B of two rankings with per-arm standings — the attribution test that does not need a ground truth."),
    ("themes", ["/api/v1/themes"], "Theme families (worktree clusters) and the works inside each."),
    ("chat", ["/api/v1/works/{id}/path", "/api/v1/works/{id}/bridges"], "The 'how do I get from this to that' surface: path between two works through shared tags, and the bridges that connect two fandoms."),
    ("similarity", ["/api/v1/tags/{id}/similar", "/api/v1/authors/similar", "/api/v1/works/{id}/similarity"], "Similar tags, similar authors (tag profile merged with author co-readership), and the similarity of any two works."),
    ("corpus-query", ["/api/v1/corpus-query"], "Corpus questions that are not a ranking: fandom-ranking, underrated, tag-neighbours, surprise, tag-trend, tag-gap."),
    ("ao3", ["/api/v1/ao3/"], "The unofficial AO3 read surface: tags, tag works, works, one work, and works/{id}/recommend."),
    ("feeds", ["/feed/"], "Seed and tag feeds, plus the OPML/OpenSearch discovery documents a reader can subscribe to."),
    ("ops", ["/api/v1/stats", "/api/v1/calibration", "/api/v1/crawl-queue", "/api/v1/embed/drift", "/healthz"], "Operational surfaces: corpus stats, the calibration curve, crawl queue depth, embedding drift, and a liveness check."),
]


def shipped_features():
    out = []
    for name, desc in COMMANDS:
        out.append({
            "title": f"CLI: kindred {name}",
            "body": f"{desc}\n\nRegistered in cmd/kindred/main.go (the usage block and the switch are kept in step). Per-command flags: `kindred {name} -h`.",
            "status": "shipped", "effort": "M", "impact": 6, "effort_score": 5,
        })
    pages = page_features()
    for name in sorted(pages):
        routes = pages[name]
        where = "\n".join(f"- `{r}`" for r in routes) or "- (no literal Path in the registry)"
        out.append({
            "title": f"Page: {name}",
            "body": f"Rendered by internal/web/templates/{name}.html, routed in internal/web/registry.go:\n{where}\n\nA page that is not registered is not reachable, which is why the routes are listed rather than described.",
            "status": "shipped", "effort": "M", "impact": 5, "effort_score": 4,
        })
    for slug, keys, desc in API_FAMILIES:
        out.append({
            "title": f"API: {slug}",
            "body": desc + "\n\nRegistered in internal/api/server.go.",
            "status": "shipped", "effort": "M", "impact": 6, "effort_score": 5,
        })
    systems = [
        ("Harvest: breadth from AO3's fandom catalogue",
         "kindred harvest -run -catalog <catalogue>: walks 75,574 fandom names, fetches listing pages, writes works page-by-page. Quota mode (-quota) treats the catalogue's per-fandom works counts as exact targets, which is what the shelf's shortfall needs; Allocate's sqrt weighting is right for a breadth target and wrong for a shortfall.",
         "internal/harvest/harvest.go"),
        ("Harvest: enrich (summaries, language, chapters)",
         "kindred harvest -enrich fills what a blurb cannot carry. -enrich-ids narrows it to the works a shelf names: 113k mirror rows lack a summary and a shelf is 17k of them, so enriching the mirror to serve one shelf is ten times the work for none of the benefit.",
         "internal/harvest/enrich.go"),
        ("Harvest: tag-type backfill",
         "kindred harvest -backfill-types: the mirror's 112k pre-existing works carry tag_type='freeforms' for EVERY tag, so a typed fandom group finds ~110 rows while a name join against the catalogue finds 5,336 fandoms. The backfill repairs the column; nothing in the ranking waits on it.",
         "internal/harvest"),
        ("Harvest: retry ladder for AO3's 525s",
         "patientFetch retries at 3s/8s/20s/45s and counts attempts. AO3 answers 525 in storms that last minutes to hours; a two-attempt fetch gave up inside every storm, and a bounded unit that exits on the first storm never finishes. Units now pause (6 failures → 5m) and abort to a restart rather than dying.",
         "internal/harvest/fetch.go"),
        ("Bookmark import as the taste source",
         "kindred bookmarks -user <name>: reads a reader's public AO3 bookmarks into the mirror, writes their taste map, and imports page by page (collecting all pages before writing meant a storm mid-read lost every page). A partial read exits non-zero so the catch-up loop retries instead of recording a short import as complete.",
         "cmd/kindred/bookmarks.go"),
        ("Shelf: 100 fandoms, 200 works each",
         "kindred shelf: idf-weighted taste from bookmarks, fandoms scored by the mean of their top 25 works, 200 works per page by MMR (λ=0.75) with near-duplicate exclusion (Jaccard ≥0.9 dropped, not penalised) and fandom-variant merging by containment + size ratio. Writes shelf.json, an INDEX.md and one page per fandom; 50 s on 113k works.",
         "internal/shelf/shelf.go"),
        ("Shelf shortfall harvest + enrichment chain",
         "kindred-shelf-harvest.service fills the gaps catalogue's per-fandom quotas; kindred-shelf-finish.service waits for it, rebuilds, enriches only the shelf's works, and rebuilds again. The wait is on the harvest UNIT, because the harvest restarts itself and a file-based wait would end at the first crash.",
         "/etc/systemd/system/kindred-shelf-*.service"),
        ("QA loop: sampled work pages re-fetched and diffed",
         "kindred-catchup.timer every 30 minutes: imports new bookmarks and re-fetches a QA sample of mirrored work pages, comparing what AO3 now says with what the mirror stored. Progress is reported per sample so a storm shows as a shortfall rather than a stall.",
         "cmd/kindred/crawl.go"),
        ("Media tree (/media/…)",
         "A browsable AO3 tag tree over the mirror: /media/ redirects to the canonical /media/Books *a* Literature/fandoms, unique-prefix matching resolves partial paths, and unknown paths 404 rather than rendering an empty tree.",
         "internal/api"),
        ("Feed redirects and discovery documents",
         "/feed/tag/{id} redirects to /feed/tags/{id}; /opensearch.xml and the OPML document let a reader subscribe to a feed from their reader rather than reading a URL by hand.",
         "internal/api/server.go"),
    ]
    for title, body, where in systems:
        out.append({"title": title, "body": f"{body}\n\nSource: `{where}`.", "status": "shipped",
                    "effort": "L", "impact": 8, "effort_score": 7})
    return out


# DECLINED is the roadmap's own verdict, not a missing status: the row was audited and the idea
# was turned down, which is what rejected means on this board.
# Open items named by docs/what-is-left.md that the 31-100 audit does not cover, each verified
# against the running API rather than taken from the document's word: both corpus-query modes
# answer 501 on the live mirror (measured 2026-10-25), and character attestation is prose only.
OPEN_ITEMS = [
    ("corpus-query: `similar` mode (declared, 501)",
     "Declared in internal/corpusquery with an honest 'declared but not implemented' comment and a 501 from the API. Measured on the live mirror: GET /api/v1/corpus-query/similar?tag=angst -> 501. It is the recommender's core question and /recommend already answers it, so it is duplication unless a distinct contract is defined — which is the decision the row is waiting on."),
    ("corpus-query: `fandom-landscape` mode (declared, 501)",
     "Same status: declared, 501, unimplemented. Measured: GET /api/v1/corpus-query/fandom-landscape -> 501. A fandom's own landscape (its tropes, its ratings, its size) is what /fandoms approximates; whether this mode should exist at all is the open question, not the implementation."),
    ("Character and pairing attestation",
     "Characters need pairing attestation before they can be recommended on: three candidate filters are known to fail on this archive, and kissing/kissed_in relationships do not exist in AO3's public work data. Nothing in the tree extracts characters or pairs them; the byline work stopped at authors."),
]

STATUS_MAP = {"BUILT": "shipped", "PARTIAL": "in_progress", "OPEN": "draft", "DECLINED": "rejected", "NO PREMISE": "rejected"}
EFFORT_MAP = {"BUILT": "L", "PARTIAL": "L", "OPEN": "M", "DECLINED": "S", "NO PREMISE": "S"}


def roadmap_features():
    path = os.path.join(REPO, "docs/PLAN-recsurfaces-31-100.md")
    rows = []
    for line in open(path, encoding="utf-8"):
        if not line.startswith("| "):
            continue
        cells = [c.strip() for c in line.strip().strip("|").split(" | ")]
        if len(cells) < 4 or not cells[0].isdigit():
            continue
        num, idea, status, evidence = cells[0], cells[1], cells[2].upper(), " | ".join(cells[3:])
        if status not in STATUS_MAP:
            continue
        body = f"Roadmap idea #{num} from docs/PLAN-recsurfaces-31-100.md (audited 2026-10-22).\n\n{evidence}"
        rows.append({
            "title": f"#{num} {idea}"[:180],
            "body": body,
            "status": STATUS_MAP[status],
            "effort": EFFORT_MAP[status],
            "impact": 6 if STATUS_MAP[status] != "rejected" else 2,
            "effort_score": 5,
        })
    return rows


def main():
    docs = documents()
    open_rows = [{"title": t, "body": b, "status": "draft", "effort": "M", "impact": 5, "effort_score": 5}
                 for t, b in OPEN_ITEMS]
    feats = shipped_features() + roadmap_features() + open_rows
    seen, uniq = set(), []
    for f in feats:
        key = f["title"].lower()
        if key in seen:
            continue
        seen.add(key)
        uniq.append(f)
    payload = {"project_slug": "kindred", "documents": docs, "features": uniq}
    out = "/tmp/concord-kindred-payload.json"
    json.dump(payload, open(out, "w", encoding="utf-8"), ensure_ascii=False)
    from collections import Counter
    print(f"docs: {len(docs)} {dict(Counter(d['kind'] for d in docs))}")
    print(f"features: {len(uniq)} {dict(Counter(f['status'] for f in uniq))}")
    print(f"bytes: {os.path.getsize(out)/1024:.0f} kB -> {out}")


if __name__ == "__main__":
    sys.exit(main())
