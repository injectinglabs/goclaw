# 22 - Per-User Skill Ownership Model

How custom (non-system) skills behave across multiple users in the same
tenant: install, share, disable, delete. Codifies the data model and
the invariants that hold across the four user-facing actions.

This document supersedes the older "single canonical row + grants"
description in §15 (`Core Skills System`) for everything related to
**custom user-installed skills**. System skills (`is_system = true`)
remain global and untouched.

---

## 1. Why per-user

Before migration 71, skills were keyed by `UNIQUE (tenant_id, slug)` —
exactly one row per slug per workspace, owned by whoever installed
first. That produced three real user-visible bugs:

| Bug | Symptom |
|---|---|
| **Silent second-installer** | Admin installs `aeo` privately; member tries to install the same `aeo`; backend returned "unchanged" but member's catalog stays empty (no `owner_id` match, no `public` flag). Member can't see or use the skill they thought they installed. |
| **Cross-admin nuke** | Any tenant admin could delete any `visibility=public` skill, including ones installed by other admins — wiping the row for everyone. Senior admin accidentally erases junior admin's published skill. |
| **Disable footgun** | Owner disables their own skill but a shared copy from another user is still `enabled=true` → agent silently falls through to the shared one. "I just disabled it, why is it still firing?" |

The per-user model fixes all three structurally rather than via UI
sleight-of-hand: each user has their own physical row, so install,
delete, and toggle only ever touch the caller's own data.

---

## 2. Data model

### `skills` table (migration 71)

Unique key changes from `(tenant_id, slug)` to:

```sql
CREATE UNIQUE INDEX idx_skills_tenant_slug_owner
  ON skills (tenant_id, slug, owner_id)
  WHERE status != 'deleted';
```

Result: two users in the same tenant can each have their own `aeo`
row, with independent `file_path`, `version`, `enabled`, `visibility`,
and `source_*` columns. The install handler's `ON CONFLICT` target now
matches the new unique key, so reinstalling YOUR row triggers an
upsert (version++); reinstalling someone else's slug creates YOUR row
as a separate INSERT.

Per-user version sequences: `GetNextVersion(slug)` is now scoped by
`owner_id` too, so reinstalls don't collide on a shared version
counter.

### `skill_user_disables` table (migration 72)

```sql
CREATE TABLE skill_user_disables (
    id         uuid PRIMARY KEY,
    skill_id   uuid REFERENCES skills(id) ON DELETE CASCADE,
    user_id    varchar(255) NOT NULL,
    tenant_id  uuid REFERENCES tenants(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(skill_id, user_id)
);
```

A row here means "this caller does NOT want this skill active for
themselves" — independent of the row's `enabled` column (which is
controlled by the row owner). Re-enable = DELETE.

### Pre-existing `skills.enabled` column

Still present, but its meaning narrows: it's now a **system flag**
(auto-`false` when deps are missing → `status='archived'`). The
user-facing Toggle button no longer touches it; see §4.

---

## 3. Visibility & dedup

A user calling `GET /v1/skills` gets all rows matching:

```sql
(is_system = true) OR (owner_id = caller) OR (visibility = 'public') OR
  EXISTS (skill_user_grants WHERE skill_id = s.id AND user_id = caller)
```

…then collapsed with `DISTINCT ON (slug)` so the UI receives at most
one entry per slug. Preference order for the dedup tiebreaker:

```
ORDER BY slug,
         is_system DESC,                        -- system skills win
         (owner_id = caller) DESC,              -- then your own row
         (visibility = 'public') DESC,          -- then a shared row
         version DESC,
         id ASC                                 -- deterministic
```

A second user's `private` row is invisible to the caller — even to a
tenant owner. Role no longer grants clairvoyance over teammate
inventories; sharing is the only way another user's row enters your
catalog.

The agent uses the same predicate via `ListAccessible` (in
`internal/store/pg/skills_grants.go`), with `DISTINCT ON (slug)` so
`use_skill` resolves to exactly one row.

---

## 4. Toggle: cascade-by-slug

**`POST /v1/skills/{id}/toggle {enabled: true|false}`**

Resolves the skill's slug from `id`, then issues a single
batch INSERT (disable) or DELETE (enable) into
`skill_user_disables` for **every** row of that slug the caller can
see — their own row PLUS rows shared by other users (visibility=public).

```sql
-- Disable cascade (single statement)
INSERT INTO skill_user_disables (id, skill_id, user_id, tenant_id, created_at)
SELECT gen_random_uuid(), s.id, $caller, s.tenant_id, NOW()
  FROM skills s
 WHERE s.tenant_id = $tenant AND s.slug = $slug
   AND s.status != 'deleted'
   AND (s.owner_id = $caller OR s.visibility = 'public')
ON CONFLICT (skill_id, user_id) DO NOTHING;
```

Why cascade: the dedup'd row the user clicked is only one of several
physical rows that resolve to that slug. If the toggle only touched
that single id, `ListAccessible` would fall through to the next-best
row on the next agent turn and the skill would seemingly stay on. The
cascade collapses the toggle to a single, predictable user-level
intent: "this skill is OFF for me, period."

`skills.enabled` (the canonical column) is no longer reachable from
the user-facing handler; only system code (dep auto-archive, install
upsert) writes to it.

### `ListAccessible` filter

```sql
AND NOT EXISTS (
  SELECT 1 FROM skill_user_disables d
  WHERE d.skill_id = s.id AND d.user_id IN ($caller, $actor)
)
```

The actor/caller pair handles the actor-vs-scope split for shared
channels; on direct chats they collapse to the same userID.

---

## 5. Delete: own row only

**`DELETE /v1/skills/{id}`**

```
1. System owner role (operator) → full delete (cross-tenant ops).
2. Caller is the row's owner_id → full delete of THEIR row, cascade
   removes that row's skill_user_disables / S3 mirror / files.
3. Else → 403.
```

Tenant admins (`tenant_users.role = 'owner'/'admin'`) are NOT special.
Sharing transfers nothing — `visibility=public` only adds a read-only
catalog entry for other users; the row stays under its installer's
control.

Other users' personal disable records on this row are dropped
automatically (`ON DELETE CASCADE` on the FK). If the row was the only
one of its slug accessible to a caller, their dedup'd catalog now
shows nothing for that slug — they can re-install if they want.

---

## 6. Sharing

**`PUT /v1/skills/{id} { visibility: "public" }`**

Owner-only write to the row's `visibility` column. After flip:
- Every tenant member sees the row as a "Shared by {installerName}"
  entry in their dedup'd catalog (unless they have their own row,
  which wins the tiebreaker).
- Agent's `use_skill` can resolve to it for users who have no own
  row.
- Members can disable for self via the per-user overlay (§4) without
  affecting the canonical row.

Unshare: same endpoint with `visibility: "private"`. Row reverts to
"only owner sees it." Members who had a disable record on it keep
that record — it becomes a no-op since they can't see the row anyway,
and gets cleaned up automatically when the owner deletes.

---

## 7. Install

Same `POST /v1/skills/install` endpoint, simpler semantics:

```
1. Resolve source → fetch tarball → extract.
2. ON CONFLICT (tenant_id, slug, owner_id) DO UPDATE:
     - Different user with same slug → INSERTs a new row (no upsert).
     - Same user re-installing → upsert refreshes content, bumps
       version, keeps row id.
3. Auto-grant: install does NOT touch other users' rows or grants.
4. Default visibility = 'private' for every install (admin/member
   alike). The previous "admin install = public" was reverted in PR
   #262 — matches the install-modal helper text the SPA already shows.
```

No more "grant on duplicate" path: per-user uniqueness made it
unnecessary.

---

## 8. Invariants

After this rollout the following hold across all client paths:

| Action by user X | Effect on user Y's skills |
|---|---|
| `install` slug Z | None — X gets a separate row keyed by (tenant, slug, X). |
| `delete` X's row | None — Y's row is a separate physical record. |
| `toggle` (Disable or Enable) | None — only X's `skill_user_disables` rows change. |
| `share` (visibility=public) | Y's catalog rendering changes (sees X's row as "Shared by X") but Y's data is untouched. |
| `unshare` (visibility=private) | Y can no longer dedup-fall-through to X's row, but Y's own row + disable records stay intact. |

Plus:

- Agent's `ListAccessible(user)` returns at most one row per slug.
- No user can delete or modify another user's row regardless of
  tenant role.
- System owner role (operator bypass) is the only path to cross-row
  full delete, kept for support / migration tooling.

---

## 9. Migrations applied (2026-06-05)

| # | Description | Stage | Prod |
|---|---|---|---|
| 70 | `skill_hubs` table + Anthropic/Alireza/Netresearch seed | ✅ | ✅ |
| 71 | per-user uniqueness `(tenant, slug, owner_id)` | ✅ | ✅ |
| 72 | `skill_user_disables` table + indexes | ✅ | ✅ |

Bump `RequiredSchemaVersion` accordingly; goclaw binary refuses boot
if the DB hasn't reached the version it expects (`upgrade/version.go`).

---

## 10. Operational notes

### Storage cost
Per-user copies mean each user who installs `aeo` gets their own S3
mirror under `skills/tenants/{tenant_slug}/skills-store/{slug}/{version}/`.
At 5 users × 10 skills × ~100KB per skill ≈ 5MB per tenant. At
1000 tenants ≈ 5GB total ≈ ~$0.12/month on S3 standard. Acceptable.

### Update propagation
When `goclaw skills check-updates` flags a new upstream `source_sha`,
the flag is set on **each user's row independently** (the cron loops
all installed-from-source rows). Each user clicks Update on their own
row; no cascade. Members never see "Maria just updated her aeo,
auto-pulling for me" — that would be a security regression.

### Cache sweeper
`internal/skills/storage/sweeper.go` runs hourly on every goclaw
instance, evicting archived versions when disk crosses
`GOCLAW_SKILLS_CACHE_DISK_LIMIT` (default 70%). Per-user rows are
independent on disk, but the sweeper key is `(tenant, slug, version)`
so a user's reinstalled-and-superseded version evicts cleanly.

### Audit log
Every toggle / install / delete emits a structured event into
`activity_logs` via `emitAudit`. New event type from this work:
`skill.user_toggled` with `scope: "user"` field to distinguish per-user
overlay from the deprecated canonical toggle.
