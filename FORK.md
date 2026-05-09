# Fork Architecture — `arq1tect/goclaw`

Single source of truth for what is custom in this fork vs upstream
`nextlevelbuilder/goclaw`. Used by `.github/workflows/upstream-sync.yml`
when Claude resolves merge conflicts. Keep this file accurate — out-of-date
context here directly degrades automated conflict resolution.

## Branch Model

- `custom` — production branch (default). All fork-specific commits land here.
- `dev` — fast-forward mirror of `upstream/dev`. Maintained by CI; never commit directly.
- No `main` branch. `upstream/main` is referenced directly when needed.

CI hourly:
1. FF-merge `upstream/dev` -> `origin/dev`.
2. Merge `origin/dev` -> `custom`. If conflicts, Claude resolves using this file as guide.
3. On any change to `custom`, trigger `deploy.yml` -> redeploy to goclaw-ath server.

## Fork-Specific Components (PRESERVE on every merge)

### 1. Custom Migration System
- `custom-migrations/` directory — independent migrations numbered `001_`, `002_`, ...
- Tracked in separate `custom_schema_migrations` table (NOT `schema_migrations`).
- `internal/upgrade/custom_migrate.go` — `RunCustomMigrations` runner.
- `cmd/migrate.go` — `customMigrateCmd()` and subcommands (`custom-migrate up/down/version`).
- `cmd/gateway_stores_pg.go` and `cmd/gateway_stores_sqlite.go` — `RunCustomMigrations` invocation
  after `checkSchemaOrAutoUpgrade`.

**Invariant:** custom migrations NEVER collide with upstream migration numbers because
they live in a separate directory with a separate tracking table. Do not renumber, move,
or delete files in `custom-migrations/`.

### 2. read_audio bridge tools (Claude CLI mode)
- `feat(mcp/bridge): expose read_audio and read_video tools` (24dba08c)
- `feat(media/read_audio): accept direct path arg, embed path in voice/audio tags` (e2a06913)
- `feat(read_audio): auto-convert ogg/opus/m4a/etc to mp3 via ffmpeg` (d3968856)

These commits modify how `read_audio` integrates with the MCP bridge. Preserve them
when upstream touches `internal/tools/read_audio*` or bridge code.

## Conflict Resolution Rules

1. **`custom-migrations/`** — never rename, renumber, move, or delete.

2. **`internal/upgrade/version.go` -> `RequiredSchemaVersion`** — set to the highest
   migration number from `migrations/` directory only (upstream). Custom migrations
   have their own tracking and do NOT affect this value.

3. **`cmd/gateway_stores_pg.go` and `cmd/gateway_stores_sqlite.go`** — preserve our
   `upgrade.RunCustomMigrations()` calls. Integrate upstream changes around them.

4. **`cmd/migrate.go`** — preserve `customMigrateCmd()` registration in `migrateCmd()`
   and the entire custom-migrate functions block at the end of the file. Preserve
   the named (non-blank) import of `github.com/golang-migrate/migrate/v4/database/postgres`.

5. **`internal/tools/read_audio*` and bridge code** — preserve our path-arg handling,
   ogg-to-mp3 ffmpeg conversion, and bridge exposure of read_audio/read_video.

6. **Custom workflow files** — `.github/workflows/upstream-sync.yml` and
   `.github/workflows/deploy.yml` are ours. If upstream adds workflows, keep both;
   never overwrite ours.

7. **i18n files** (`ui/web/src/i18n/locales/`) — preserve our custom translation keys
   while adding upstream new keys.

8. **All other conflicts** — keep BOTH sides. Merge logically. Prefer the version
   that compiles and passes tests.
