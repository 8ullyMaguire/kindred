# Guest access, trust levels and self-governance — kindred

Status: **proposal**, from the owner's request of 2026-10-16. It records intent in this repo's own
terms; nothing here is implemented. The estate-wide standard (items GS-1…GS-5, every setting
configurable, defaults and firewalls) is:

    secondbrain/10-Projects/2026-10-16-guest-access-trust-and-self-governance.md

This file is the site-specific half: what exists here today, what this site adopts, and the
firewalls that differ from the standard because this site is not the same shape as the others.

Stack: Go, with a web UI (docs/SPEC-web-ui.md); CLI-first recommender.

## Where kindred stands today (measured 2026-10-16)

- web UI exists; **no accounts or roles** (no `trust_level`/`moderator`/`is_admin` hits)
- no consent state

## What this site adopts

- GS-3 only, and only for any future non-essential cookie; GS-1/GS-2/GS-4/GS-5 are not applicable while the surface has no accounts

## Config deltas (everything else is the standard's default)

- `consent.required_for_guest = false` until a guest identity exists — ask, do not gate, so a reader is never blocked by a cookie question

## Firewalls specific to this site

- The recommender's value is read-only: nobody needs an account to be recommended to, so there is nothing for a guest to own and nothing to fold.

## Order of work

GS-3 (informational) only. The standard's §9 explains the ordering: consent first, because the guest
identity *is* a cookie and the gate has to exist before the thing it gates; the fold second, because
offering guest access without it creates work to clean up later.

## If this is implemented

Every step needs a test that fails without it, and the numbers belong in this file when they
are real — the estate's rule is that a claim of done carries the command that proves it. The
two items in the standard that nothing in this repo has yet are the elastic trust rule (§4)
and the jury drawn from the eligible pool (§5): both are cheap to fake and therefore need a
test that would pass for the wrong reason to be written, and killed, on purpose.
