# doory-go

Faithful Go rewrite of the [doory](https://github.com/shaongitbd/doory) backend
(Django + DRF + channels + MCP), pinned as a behavior oracle under
`.upstream/current/`, with the existing React frontend embedded into a single
binary release.

The port follows the PiG ↔ Pi parity methodology (see `PLAN.md`):
pinned oracle, probe-first paired scenarios, compiler-derived ledgers,
divergence ledger with a shrink-only ratchet, and a Makefile gate ladder.

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
make upstream-mirror   # refresh .upstream/current to the pinned commit
make generate          # regenerate inventories/ledgers (never hand-edit)
make check             # build, vet, lint, unit, drift gates, divergence guards, parity-fast
make verify            # check + full parity + contracts
make parity            # parallel oracle/Go scenario pairs
make coverage          # regenerate the coverage report embedded in AGENTS.md
```
