# AGENTS.md

Working agreement for the doory → Go faithful rewrite. The methodology, phases,
families, and divergence ledger design live in `PLAN.md`; this file records the
session-level rules.

## Commit rule (owner-controlled commits)

- **Never commit unless the owner explicitly calls for it** (e.g. "commit",
  "commit now"). No exceptions: not for "small" changes, not at milestone ends,
  not to "keep things tidy". Between calls the working tree stays dirty — that
  is the expected state.
- When the owner calls for a commit, make **exactly one commit** for the work
  done so far:
  - **Concise title** (imperative mood, ≤ 72 chars, no trailing period).
  - **Detail in the body**: what changed, why, and the relevant anchors
    (PLAN.md section, divergence `D<N>` id, milestone, family).
  - No drive-by refactors or unrelated file changes bundled into it.
- **Then stop**: return to waiting for the owner's next instruction. Do not
  chain further commits and do not start new work unprompted after committing.
- Never rewrite/force-push history or amend a commit the owner has not asked
  to be amended.

## Standing conventions

- `.upstream/current/` is the pinned behavior oracle. Never edit it; the
  mirror test (`test/upstream-parity/`) must stay green (`pnpm upstream-mirror`
  restores it).
- The version pin in `internal/dooryversion/dooryversion.go` is the only place
  the upstream commit may appear.
- `package.json` scripts are the build-target manager (`pnpm run help` lists
  the gate ladder). There is no Makefile.
- Generated inventories/ledgers are never hand-edited; they are regenerated
  (`pnpm generate`).
- Parity is the law: probe the oracle before porting, never weaken a
  comparator, and every deliberate difference needs a `D<N>` entry in
  `docs/parity/DIVERGENCES.md` with a remove-when condition.
- Testing scope: backend behavior only (API/WS/MCP contracts, DB state, side
  effects); the embedded frontend is validated later, case by case.
- Default dialect is SQLite; Postgres support is parity-tested via the
  dialect matrix (D4). New code must stay dialect-neutral.
