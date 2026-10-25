# Concord sync

The project page at https://concord.polarisocial.xyz/projects/kindred is a **live document** of this
repository: every doc in `docs/`, plus one row per shipped surface and one row per audited roadmap
idea. It is generated from the tree, not typed into a form.

    python3 scripts/concord-sync/gen.py                 # -> /tmp/concord-kindred-payload.json
    scp /tmp/concord-kindred-payload.json thinkcentre:/tmp/
    ssh thinkcentre 'cd /tmp && python3 apply.py /tmp/concord-kindred-payload.json'

`gen.py` runs anywhere (it only reads this repo and the vault). `apply.py` must run **on the host
that owns the database** (`/home/alvaro/.local/share/concord/concord.db`) and takes one path.

## What the two files promise

- **Documents** upsert on `(project_id, kind, slug)` and bump `revision` only when the body actually
  changed: a document edited in place is a document whose history nobody can see.
- **Features insert by title, and a re-run never overwrites a status.** The body is derived and can
  be wrong; the status is a person's judgement. Refreshing a body is `~148 bodies refreshed`.
- **One transaction.** A failure leaves the page exactly as it was. The database is WAL, so the
  running server does not need to be stopped for a write this small — but take a backup first, as
  `/home/alvaro/concord-backup-<date>.db`.

## Where each row comes from

| rows | source |
|---|---|
| documents | `README.md`, `docs/*.md` (kind by filename: SPEC → spec, PLAN → plan, DECISIONS → adr, HANDOFF → changelog, else wiki), plus the shelf SPEC from the vault |
| `CLI: kindred <cmd>` | the usage block in `cmd/kindred/main.go` |
| `Page: <template>` | `internal/web/registry.go` — the template and the `Paths` that render it |
| `API: <family>` | `internal/api/server.go` |
| systems (harvest, shelf, bookmarks, QA, media, feeds) | the units and the packages, named in each row |
| `#N <idea>` | `docs/PLAN-recsurfaces-31-100.md`, status carried across: BUILT → shipped, PARTIAL → in_progress, OPEN → draft, DECLINED/NO PREMISE → rejected |
| three open items | `docs/what-is-left.md`, each verified against the running API (both corpus-query modes answer 501) |

Two documents are deliberately absent: `docs/WHAT-IS-LEFT.md` is the 2026-10-05 predecessor of
`what-is-left.md` and publishing both would put two contradicting "what is left" documents on one
page, and the root and `docs/` `HANDOFF.md` are different documents that would collide on the slug
(the root one — the arena handoff — is published as `handoff-arena-ranking`).
