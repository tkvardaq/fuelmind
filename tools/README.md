# tools/

One-off CLI helpers that ship with the repo but are not part of the
local core. Each tool is a `main` package in its own subdirectory so
`go build ./cmd/...` does not pick them up.

## Planned tools (later phases)

- `tools/hashgen/` — compute SHA-256 of an update artifact (Phase 6)
- `tools/dbquery/` — dev CLI to run ad-hoc SQL against the local DB
  (Phase 3+)
- `tools/adapters-list/` — list the registered POS adapters in a
  given binary (Phase 1+)

Phase 0 ships no tools. The path exists for organization.
