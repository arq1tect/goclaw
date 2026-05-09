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

### 3. Kimi Code API provider (Moonshot Allegretto)

Native provider type `kimi` for `https://api.kimi.com/coding/v1`. The endpoint
allowlists client User-Agents and rejects `Go-http-client/*` with 403; we send
`KimiCLI/1.5`. Wire format is OpenAI Chat Completions 1:1, so no separate adapter
was added — only a generic `WithUserAgent` builder on `OpenAIProvider`.

Files (any one of these is fork-specific; preserve on merge):

- `internal/providers/kimi_constants.go` — `KimiCLIUserAgent = "KimiCLI/1.5"`
  (single source of truth; bump when Moonshot rotates the allowlisted UA).
- `internal/providers/openai_config.go` — `userAgent` field + `WithUserAgent(ua)`
  builder. Generic; not Kimi-specific.
- `internal/providers/openai_http.go` — conditional `User-Agent` header set in
  `doRequest` after the existing header block.
- `internal/providers/openai_request.go`:
  - `skipTemp` predicate extended to drop `temperature` for Kimi-family models
    (Kimi rejects any value except 1).
  - Reasoning/Thinking preservation for replay-required models — Kimi requires
    reasoning blocks to be present in tool-replay requests.
- `internal/providers/openai_useragent_test.go` — table test for default UA,
  KimiCLI UA, arbitrary UA.
- `internal/providers/reasoning_resolution.go` — kimi family marked as
  reasoning-capable.
- `internal/store/provider_store.go` — `ProviderKimi`, `KimiDefaultAPIBase`
  (`https://api.kimi.com/coding/v1`), `KimiDefaultModel` (`kimi-for-coding`),
  entry in `ValidProviderTypes`.
- `internal/config/config_channels.go` — `Kimi` field on `ProvidersConfig`,
  case in `APIBaseForType`, OR-clause in `HasAnyProvider`.
- `internal/config/config_load.go` — env loaders `GOCLAW_KIMI_API_KEY`,
  `GOCLAW_KIMI_API_BASE`.
- `internal/config/config_secrets.go` — masking for the Kimi key.
- `cmd/gateway_providers.go` — config-based registration block, mirrors
  byteplus-coding pattern.
- `cmd/onboard_managed.go` — placeholder onboarding entry.
- `internal/http/providers.go` — DB-driven `case store.ProviderKimi:` in
  `registerInMemory`. Same UA injection — used when operator adds Kimi via
  dashboard.
- `internal/http/provider_models.go` + `internal/http/provider_models_catalog.go`
  — hardcoded Kimi models list (`kimi-for-coding`, `kimi-k2.6`, `kimi-k2.5`)
  to bypass `/models` 403 (the catalog endpoint is also UA-allowlisted).
- `ui/web/src/constants/providers.ts` and `ui/desktop/frontend/src/constants/providers.ts`
  — `Kimi Code (Allegretto)` dropdown entry with auto-fill `api_base`.

Conflict resolution rules:

- Preserve the `WithUserAgent` builder, the `userAgent` field, and the
  `if p.userAgent != ""` header set in `doRequest`.
- Preserve the Kimi entries in `skipTemp` and reasoning predicates. Upstream
  may add new model families to these gates — keep both sides.
- Preserve `kimi_constants.go` and the `ProviderKimi` constants. If upstream
  ever adds its own Kimi support, prefer ours and reconcile carefully.
- Hardcoded models list (`kimiModels()` in `provider_models_catalog.go`) is a
  workaround for UA-allowlisted `/models` endpoint — keep it until Moonshot
  opens that endpoint.

Risks (informational, not action items):

- Spoofing `KimiCLI/1.5` to use the consumer Allegretto subscription from a
  non-CLI client is a Moonshot ToS gray area. Mitigation: keep an
  `api.moonshot.cn` API-key fallback ready; the UA bump is one constant.
- When the new `ProviderAdapter` transport is wired in the agent loop, the
  `userAgent` field needs to flow through `adapter_openai.go:ToRequest`.

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
