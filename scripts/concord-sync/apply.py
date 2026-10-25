#!/usr/bin/env python3
"""Apply the kindred payload to the LIVE Concord database on thinkcentre.

Run with the concord service stopped: SQLite refuses a write while the server holds the lock, and
a half-applied payload is worse than none. Everything happens in ONE transaction, so a failure
leaves the page exactly as it was.

Documents upsert on (project_id, kind, slug) and bump `revision`: a document edited in place is a
document whose history nobody can see. Features insert only when their title is new for the
project, so re-running this file adds nothing and never overwrites a status somebody set by hand.
"""
import json
import os
import sqlite3
import sys
import time

DB = "/home/alvaro/.local/share/concord/concord.db"
PAYLOAD = sys.argv[1] if len(sys.argv) > 1 else "/tmp/concord-kindred-payload.json"
AUTHOR = 1  # polaris, the account every other seeded document on this instance uses


def main():
    data = json.load(open(PAYLOAD, encoding="utf-8"))
    db = sqlite3.connect(DB, timeout=30)
    db.execute("PRAGMA foreign_keys = ON")
    proj = db.execute("SELECT id FROM projects WHERE slug = ?", (data["project_slug"],)).fetchone()
    if not proj:
        sys.exit(f"no project with slug {data['project_slug']}")
    pid = proj[0]
    now = time.time()

    with db:  # one transaction
        docs_new = docs_upd = 0
        for d in data["documents"]:
            row = db.execute(
                "SELECT id, body FROM project_documents WHERE project_id=? AND kind=? AND slug=?",
                (pid, d["kind"], d["slug"]),
            ).fetchone()
            if row is None:
                db.execute(
                    """INSERT INTO project_documents
                       (project_id, kind, slug, title, body, revision, author_id, created_at, updated_at)
                       VALUES (?,?,?,?,?,1,?,?,?)""",
                    (pid, d["kind"], d["slug"], d["title"], d["body"], AUTHOR, now, now),
                )
                docs_new += 1
            elif row[1] != d["body"] or True:
                db.execute(
                    """UPDATE project_documents
                          SET title=?, body=?, revision=revision+?, updated_at=?
                        WHERE id=?""",
                    (d["title"], d["body"], 1 if row[1] != d["body"] else 0, now, row[0]),
                )
                docs_upd += 1

        existing = {r[0].lower(): r[1] for r in
                    db.execute("SELECT title, id FROM features WHERE project_id=?", (pid,))}
        feats_new = feats_updated = 0
        for f in data["features"]:
            if f["title"].lower() in existing:
                # The body is derived from the tree and can be wrong; the STATUS is a person's
                # judgement and is never overwritten by a re-run of this file.
                db.execute(
                    "UPDATE features SET body=?, effort=?, impact=?, effort_score=?, updated_at=? WHERE id=?",
                    (f["body"], f["effort"], f.get("impact"), f.get("effort_score"), now,
                     existing[f["title"].lower()]),
                )
                feats_updated += 1
                continue
            existing[f["title"].lower()] = None
            db.execute(
                """INSERT INTO features
                   (project_id, author_id, title, body, effort, status, impact, effort_score, created_at, updated_at)
                   VALUES (?,?,?,?,?,?,?,?,?,?)""",
                (pid, AUTHOR, f["title"], f["body"], f["effort"], f["status"],
                 f.get("impact"), f.get("effort_score"), now, now),
            )
            feats_new += 1

    counts = db.execute(
        "SELECT (SELECT count(*) FROM project_documents WHERE project_id=?),"
        "       (SELECT count(*) FROM features WHERE project_id=?)", (pid, pid)).fetchone()
    print(f"project {pid}: docs +{docs_new} (~{docs_upd} touched), features +{feats_new} (~{feats_updated} bodies refreshed)")
    print(f"on disk now: {counts[0]} documents, {counts[1]} features")
    db.close()


if __name__ == "__main__":
    main()
