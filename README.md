# doory-go

Faithful Go rewrite of the [doory](https://github.com/shaongitbd/doory) backend
(Django + DRF + channels + MCP), pinned as a behavior oracle under
`.upstream/current/`, with the existing React frontend embedded into a single
binary release.

The port follows the PiG ↔ Pi parity methodology (see `PLAN.md`):
pinned oracle, probe-first paired scenarios, compiler-derived ledgers,
divergence ledger with a shrink-only ratchet, and a pnpm gate ladder (`package.json`
scripts are the build-target manager; there is no Makefile).

## Layout

- `PLAN.md` — the rewrite plan (methodology, families, phases, divergences)
- `.upstream/current/` — frozen mirror of upstream @ the pin in
  `internal/dooryversion/dooryversion.go` (never edit)
- `internal/dooryversion/` — the sole version pin
- `apps/`, `doory/` — the Go port (not started; every file carries a
  `// Ports backend/...` comment)
- `docs/parity/` — `PORT_MAP.md` (file denominator), `DIVERGENCES.md` (D\<N\> ledger)
- `ledger/` — compiler-derived inventories (routes, interfaces, env, migrations, tests)
- `test/parity/` — families, paired scenarios, drivers, unit evidence, probes
- `automation/ci/` — drift gates and the divergence guard

## Commands

```bash
pnpm run help          # list all build targets
pnpm upstream-mirror   # refresh .upstream/current to the pinned commit
pnpm generate          # regenerate inventories/ledgers (never hand-edit)
pnpm check             # build, vet, lint, unit, drift gates, divergence guards, parity-fast
pnpm verify            # check + full parity + contracts
pnpm parity            # parallel oracle/Go scenario pairs
pnpm coverage          # regenerate the coverage report embedded in AGENTS.md
```
