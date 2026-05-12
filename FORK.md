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

### 4. Knowledge Graph extensions

Adds custom entity/relation type management on top of upstream KG: per-agent
type catalogs with seed presets, dynamic extraction prompt built from DB types,
and an agent-write tool with configurable guardrails.

**Schema (custom-migrations):**

- `custom-migrations/001_kg_custom_types.up.sql` / `.down.sql` — `kg_entity_types`
  and `kg_relation_types` tables (per-agent, UNIQUE(agent_id, name),
  `properties_schema JSONB`, `is_system` flag); `seed_kg_default_types(agent_id)`
  SQL function with 10 entity + 22 relation defaults; backfill for existing agents.
- `custom-migrations/002_add_relation_source_updated_at.up.sql` / `.down.sql` —
  adds `source` and `updated_at` to `kg_relations` for provenance + UI badges.

**Store layer:**

- `internal/store/knowledge_graph_store.go` — interface extended with
  `GetEntityTypes`, `UpsertEntityType`, `DeleteEntityType`, `CountEntitiesByType`,
  the relation analogs, and `SeedKGTypes(agentID, preset)`. `UpdateEntity` added.
- `internal/store/pg/knowledge_graph_types.go` — Postgres implementation.
  Presets: `general` (10/22 default), `legal` (corporate domain — company,
  jurisdiction, obligation, risk, decision, contract, document, date, question,
  person + 10 relations), `development`, `empty` (no-op).
- `internal/store/pg/knowledge_graph.go`, `_relations.go`, `_scan_rows.go` —
  extensions for relation source/updated_at and UpdateEntity.

**HTTP API (8 new routes on top of existing 16):**

- `internal/http/knowledge_graph.go` — registers entity-types and relation-types
  CRUD: `GET/POST/PATCH/DELETE /v1/agents/{agentID}/kg/entity-types[/{typeID}]`
  and same for `relation-types`.
- `internal/http/knowledge_graph_types_handlers.go` — handlers with deletion
  guard (HTTP 409 when type is in use by entities/relations).
- `internal/http/knowledge_graph_handlers.go` — adds entity update + relation
  CRUD endpoints.

**Extractor:**

- `internal/knowledgegraph/extractor_prompt_dynamic.go` — `BuildExtractionPrompt`
  generates the LLM prompt from per-agent types in the DB. Falls back to the
  static prompt when no custom types are defined.
- `internal/knowledgegraph/extractor.go` — wired to the dynamic builder. New
  `NewExtractorWithTypes` constructor. Called from
  `cmd/gateway_managed.go:buildKGExtractFunc` which loads types per-agent.

**Agent-write tool:**

- `internal/tools/knowledge_graph_mutate.go` — `KnowledgeGraphMutateTool`. Per-run
  guardrails via builtin_tools settings: `MaxEntitiesPerRun` (default 10),
  `MaxRelationsPerRun` (default 20), `AllowedEntityTypes` and
  `AllowedRelationTypes` (CSV whitelist; empty = all). In-process `kgMutateCounter`
  with mutex enforces limits per (agentID, runID) window.
- `internal/tools/knowledge_graph_mutate_test.go` — unit tests.
- `cmd/gateway_builtin_tools.go` — seed entry `knowledge_graph_mutate` (disabled
  by default; admin enables via dashboard).
- `cmd/gateway_managed.go` — wires the tool to the KG store on startup.
- `internal/channels/events.go` — tool status string for chat UI.

**UI (frontend):**

- `ui/web/src/pages/memory/kg-types-tab.tsx` — type management tab.
- `ui/web/src/pages/memory/kg-type-form-dialog.tsx` — create/edit type with
  color picker + properties_schema builder.
- `ui/web/src/pages/memory/kg-entity-form-dialog.tsx`,
  `kg-relation-form-dialog.tsx` — direct entity/relation CRUD UI.
- `ui/web/src/pages/memory/hooks/use-kg-types.ts`,
  `hooks/use-knowledge-graph.ts` — react-query hooks.
- `ui/web/src/pages/builtin-tools/kg-mutate-settings-form.tsx` — guardrail
  config UI.
- `ui/web/src/pages/memory/knowledge-graph/kg-entities-tab.tsx`,
  `kg-entity-detail-dialog.tsx`, `kg-graph-view.tsx` — extended with type-aware
  rendering (dynamic colors, icons, badges).
- `ui/web/src/types/knowledge-graph.ts`, `lib/query-keys.ts` — shared types
  and query keys.
- `ui/web/src/i18n/locales/{en,vi,zh}/{memory,storage,tools}.json` — translations.

**Conflict resolution rules:**

- Schema (`custom-migrations/001_*`, `002_*`) — never renumber, move, or merge
  with upstream migrations.
- `KnowledgeGraphStore` interface (`internal/store/knowledge_graph_store.go`) —
  preserve all type-management methods. If upstream extends the interface, keep
  both sides.
- Extractor — if upstream changes the static prompt or extractor signature,
  preserve `BuildExtractionPrompt`'s call site in `extractor.go` and the
  `NewExtractorWithTypes` constructor.
- `knowledge_graph_mutate.go` and its registration in `gateway_managed.go` —
  keep the wiring block. The tool is gated by builtin_tools enabled flag.
- Settings defaults (`kgMutateSettings.MaxEntitiesPerRun=10`,
  `MaxRelationsPerRun=20`) — change cautiously; production tuning happens via
  dashboard.

**Tools that agents see:**

- `knowledge_graph_search` (read-only, search/list/traverse) — present in
  upstream, description references the dynamic per-agent type catalog.
- `knowledge_graph_mutate` (write) — fork-only. Disabled by default. Admin
  enables per-tenant via dashboard. Tool description and the dynamic
  extractor prompt together communicate to agents what types are available
  and how to propose new ones.

**Tool descriptions overhaul (2026-05-11) — fork-specific:**

Upstream descriptions for `knowledge_graph_search` and `knowledge_graph_mutate`
hardcode the generic 10-type vocabulary (`person, organization, project,
product, technology, task, event, document, concept, location`) in three
places, which misdirects the LLM toward generic types even when a custom
per-agent catalog (e.g. legal preset) is active. The fork rewrites these
descriptions to reference the per-agent catalog and the `kg-ontology` skill
governance for new types.

Touched in three places — preserve all three on upstream merges:

- `internal/agent/systemprompt.go` — `coreToolSummaries` map: updated
  `knowledge_graph_search` summary; **added** `knowledge_graph_mutate` summary
  (upstream omits it entirely, falling back to `(custom tool)`).
- `internal/tools/knowledge_graph.go` — `Description()` and the `entity_type`
  parameter description.
- `internal/tools/knowledge_graph_mutate.go` — `Description()`, `entity_type`
  parameter description, `relation_type` parameter description.

Rationale: parameter schemas (formal contract) dominate over system prompt
text (informal) when the LLM picks attribute values. Without this overhaul,
McGill/Saul agents default to `concept`/`document` types and ignore the
legal/personal preset catalog. Fixing only `CAPABILITIES.md` (system prompt
side) leaves the parameter schemas conflicting — agents see two contradictory
sources.

The new descriptions explicitly:
- Reference `your agent's catalog (preset-dependent; see CAPABILITIES)`.
- Give example types from both legal and personal presets.
- Direct agents to the `kg-ontology` skill for proposing new types.
- Surface rate limits (10 entities / 20 relations per run) in the mutate
  description.

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

8. **KG tool descriptions** — preserve fork rewrites of `Description()` and parameter
   descriptions in `internal/tools/knowledge_graph.go` and
   `internal/tools/knowledge_graph_mutate.go`, plus the `coreToolSummaries` entries
   in `internal/agent/systemprompt.go`. Upstream descriptions hardcode generic
   10-type vocabulary; fork references per-agent catalog and `kg-ontology` skill.
   See "Tool descriptions overhaul (2026-05-11)" above.

9. **All other conflicts** — keep BOTH sides. Merge logically. Prefer the version
   that compiles and passes tests.

---

## 5. Agent administration tools

A new family of builtin tools that lets an admin-class agent (e.g. `forge`)
provision and edit other agents on this goclaw instance without going through
the dashboard UI or external HTTP API. Tools run in-process, access stores
directly, and are tenant-scoped. Access is governed by `tools_config.allow`
per-agent — they are off by default in `builtin_tools` and must be explicitly
granted.

### `agent_context_files` (first tool in this family)

Cross-agent CRUD for agent-level context files (SOUL/IDENTITY/CAPABILITIES/
USER_PREDEFINED/AGENTS/USER/BOOTSTRAP/HEARTBEAT/MEMORY.json). Actions:
`list`, `read`, `write`, `delete`. Write returns the post-write state
(re-read from DB) for true verification. Files load-bearing for the agent
(SOUL, IDENTITY, CAPABILITIES, USER_PREDEFINED, AGENTS, HEARTBEAT) are
protected from deletion.

Files:
- `internal/tools/agent_context_files.go` — tool implementation.
- `internal/tools/agent_context_files_test.go` — unit tests.
- `internal/store/agent_store.go` — added `DeleteAgentContextFile(ctx, agentID, fileName) (bool, error)` to `AgentContextStore` interface.
- `internal/store/pg/agents_context.go` — PG implementation of Delete.
- `internal/store/sqlitestore/agents_context.go` — SQLite implementation of Delete.
- `internal/tools/context_file_interceptor_test.go` — added `DeleteAgentContextFile` stub method to `stubAgentStore` to satisfy the extended interface (no behavior change).
- `cmd/gateway_tools_wiring.go` — registers the tool and wires the agent store.
- `cmd/gateway_builtin_tools.go` — adds `agent_context_files` entry to the builtin tools registry (`Enabled: false` default, category `admin`).

Conflict-resolution rules:
- The `DeleteAgentContextFile` interface addition is upstream-additive — no existing methods modified. New implementations need not be backported if upstream picks up a different design.
- The local `allowedContextFiles` list in `agent_context_files.go` intentionally duplicates (rather than imports) the allowlist used by `gateway/methods/agents_files.go` — keeps the tool free of cross-file coupling for merge stability. If upstream adds a new bootstrap file we want exposed, add it explicitly to the tool's list.
- The `protectedFromDeletion` set is a tool-local guard. There is no system-wide delete protection on context files in upstream as of this writing; if upstream adds one, our tool guard becomes redundant and can be removed.
