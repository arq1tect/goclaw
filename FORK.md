# Fork Architecture — `arq1tect/goclaw`

Single source of truth for what is custom in this fork vs upstream
`nextlevelbuilder/goclaw`. Used by `.github/workflows/upstream-sync.yml`
when Claude resolves merge conflicts. Keep this file accurate — out-of-date
context here directly degrades automated conflict resolution.

## Branch Model

- `custom` — production branch (default). All fork-specific commits land here.
- `main` — fast-forward mirror of `upstream/main`. Maintained by CI; never commit directly.
- `dev` — frozen at `upstream/dev` HEAD as of 2026-05-04 (last upstream/dev commit). Kept
  for historical reference only; upstream moved active trunk to `main` and `dev` is no
  longer maintained upstream. Do not sync, do not merge into `custom`.

CI hourly:
1. FF-merge `upstream/main` -> `origin/main`.
2. Merge `origin/main` -> `custom`. If conflicts, Claude resolves using this file as guide.
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

### `agent_config` (agent administration family, second tool)

Read + patch-update of any agent's configuration in the caller's tenant.
Mirrors HTTP `PUT /v1/agents/{id}` (`internal/http/agents.go::handleUpdate`)
including its side effects, but as an in-process builtin tool — no HTTP
round-trip, no API key needed.

Sub-actions: `read`, `update`.

Files:
- `internal/tools/agent_config.go` — tool implementation (~530 lines).
- `internal/tools/agent_config_test.go` — unit tests (24 cases incl. dispatch, validation, JSONB shape, identity sync, rename, error paths).
- `cmd/gateway_tools_wiring.go` — registers the tool and wires `pgStores.Agents` + `msgBus`.
- `cmd/gateway_builtin_tools.go` — adds `agent_config` builtin entry (`Enabled: false` default, category `admin`).

Side effects replicated from `handleUpdate`:
- Cache invalidation: `bus.EventCacheInvalidate` for `CacheKindAgent` (old agent_key, and new on rename) + `CacheKindBootstrap` for the agent UUID.
- `IDENTITY.md` `Name:` field sync via `bootstrap.UpdateIdentityField` when `display_name` changes.
- `bus.EventAgentStatusChanged` broadcast on status change.

Conflict-resolution rules:
- `agentConfigMutableFields` in the tool is a curated copy of `agentAllowedFields` from `internal/http/validate.go` with `agent_type` intentionally excluded (treated as immutable here). If upstream adds new fields to its allowlist, decide per-field whether to expose via this tool.
- `agentConfigSlugRe` duplicates `slugRe` from `internal/http/validate.go` to avoid tools→http dependency. They must stay in sync; if upstream tightens the slug regex, mirror here.
- `status` settable values exclude `summoning` (reserved for the async summoner). If upstream adds new lifecycle states, evaluate whether they should be operator-settable.
- The tool relies on `store.AgentStore.Update` to atomically handle the `is_default` uniqueness side effect (the PG implementation clears `is_default` on other agents in the tenant when one is set). If upstream changes this contract, the tool needs explicit handling.
- `msgBus` wiring is optional in the constructor for testability; production gateway always wires it.

### `agent_provision` (agent administration family, third tool)

The central admin tool: create, delete, and list agents in the caller's
tenant. Create is atomic — agent record + bootstrap seed + operator
context-file overlay in one call. No HTTP round-trip, no summoner trigger.

Sub-actions: `create`, `delete`, `list`.

Files:
- `internal/tools/agent_provision.go` — tool implementation (~530 lines).
- `internal/tools/agent_provision_test.go` — unit tests (27 cases incl. dispatch, validation, context-file allowlist, default workspace fallback, store errors, create+delete round trip).
- `cmd/gateway_tools_wiring.go` — registers the tool and wires `pgStores.Agents` + `msgBus` + `agentCfg.Workspace` (for default workspace path).
- `cmd/gateway_builtin_tools.go` — adds `agent_provision` builtin entry (`Enabled: false` default, category `admin`).

Create flow (atomic):
1. Validate agent_key slug + uniqueness, provider presence, enum values, JSONB shapes.
2. Construct `store.AgentData` with defaults (agent_type=predefined, status=active, restrict_to_workspace=true, memory_config={"enabled":true}, compaction_config={}).
3. `agentStore.Create(ctx, ag)` — gets UUID.
4. `bootstrap.SeedToStore(ctx, agentStore, id, predefined)` — seeds default templates for files we did NOT provide (AGENTS_CORE.md, AGENTS_TASK.md, etc.).
5. For each entry in `context_files`: `SetAgentContextFile` UPSERT — overlays operator-provided content over seeds.
6. Emit `agent.created` audit event.
7. Return `agent_uuid`, `agent_id`, basic config + list of `context_files_written`.

Delete flow:
1. Resolve agent (tenant-scoped via store).
2. Require `confirm=true` — otherwise return guard error explaining the 27+ cascade tables.
3. `agentStore.Delete(ctx, id)` — hard DELETE with ON DELETE CASCADE.
4. Emit `bus.EventCacheInvalidate` for `CacheKindAgent`.
5. Emit `bus.TopicAgentDeleted` broadcast (matches WS handler; HTTP handler omits this currently, but the broadcast is needed for downstream orphan cleanup such as provider deregistration).
6. Emit `agent.deleted` audit event.
7. Return confirmation.

Conflict-resolution rules:
- `agentProvisionSlugRe` duplicates the slug regex from `internal/http/validate.go` for cross-package independence (same rule as `agent_config`).
- `agentProvisionContextFileAllowlist` mirrors the allowlist used by `agent_context_files`; intentional duplication.
- `agent_type` is forced to `predefined` — the tool refuses to create `open` agents (deprecated in upstream v3).
- The summoner subsystem is intentionally NOT invoked; the operator provides context files directly. Operators who want a summoner-style first draft should call the HTTP `POST /v1/agents` endpoint with `agent_description`.
- Workspace path defaults to `{agentCfg.Workspace}/{agent_key}` (matches HTTP `handleCreate`); falls back to `data/workspace/{agent_key}` if `SetDefaultWorkspace` was never called.
- Delete emits `TopicAgentDeleted` even though HTTP `handleDelete` does not. If upstream eventually adds it to HTTP, the tool's behavior becomes redundant (not buggy).

### `agent_grants` (agent administration family, fourth tool)

Skill and MCP-server grants per agent. Mirrors HTTP
`POST/DELETE /v1/skills/{id}/grants/agent` and
`POST/DELETE /v1/mcp/servers/{id}/grants/agent`, plus list/enumerate helpers.

Sub-actions: `list`, `grant_skill`, `revoke_skill`, `grant_mcp`,
`revoke_mcp`, `list_skills_available`, `list_mcp_available`.

Files:
- `internal/tools/agent_grants.go` — tool implementation (~470 lines).
- `internal/tools/agent_grants_test.go` — unit tests (22 cases incl. dispatch, ID resolution by UUID and slug/name, store errors, tool_allow/deny payloads, combined grant→list flow).
- `cmd/gateway_tools_wiring.go` — registers and wires the tool with `pgStores.Agents`, `pgStores.Skills` (cast to `SkillManageStore`), `pgStores.MCP`, and `msgBus`.
- `cmd/gateway_builtin_tools.go` — adds `agent_grants` builtin entry (`Enabled: false`, category `admin`).

Side effects replicated from HTTP handlers:
- Skill grant/revoke: `skillStore.BumpVersion()` + `bus.EventCacheInvalidate` (`CacheKindSkillGrants`) + audit `skill.grant_changed`.
- MCP grant/revoke: `bus.EventCacheInvalidate` (`CacheKindMCP`) + audit `mcp_server.agent_granted` / `agent_revoked`.

Conflict-resolution rules:
- The tool defines two **minimal local interfaces** (`agentGrantsSkillStore`, `agentGrantsMCPStore`) inside the tool file rather than depending on the full `store.SkillManageStore` / `store.MCPServerStore` interfaces. Production wiring passes the full stores (they satisfy the minimal interfaces); tests stub only the methods we use. This isolates the tool from upstream interface drift.
- Skill ID resolution accepts UUID or slug. Slug path uses `ListSkills` linear scan — acceptable for low-frequency operator use. If upstream adds `GetSkillBySlug`, switch to it.
- MCP server ID resolution accepts UUID or name via `GetServerByName`.
- Skills wiring is conditional on `pgStores.Skills` implementing `store.SkillManageStore`. If skills are not present or only base `SkillStore` is available, skill sub-actions return "skill store not wired"; MCP sub-actions still work.

### `agent_hooks` (agent administration family, fifth tool)

Hook CRUD per agent and tenant scope. Mirrors the WS RPC `hooks.*`
methods (`internal/gateway/methods/hooks.go`) end-to-end except for
`hooks.test` (separate concern, not in this tool).

Sub-actions: `list`, `get`, `create`, `update`, `delete`, `toggle`,
`set_agents`.

Files:
- `internal/tools/agent_hooks.go` — tool implementation (~570 lines).
- `internal/tools/agent_hooks_test.go` — unit tests (22 cases incl. dispatch, scope validation, agent_ids resolution by slug, mutable-field allowlist, toggle, set_agents).
- `cmd/gateway_tools_wiring.go` — registers and wires the tool with `pgStores.Agents` and `pgStores.Hooks` (cast to `hooks.HookStore`); registration is conditional on the hook store being available.
- `cmd/gateway_builtin_tools.go` — adds `agent_hooks` builtin entry (`Enabled: false`, category `admin`).

Notable design choices:
- **Global scope is blocked**: scope=global hooks require master-scope context per the WS handler. An admin agent does not have master-scope, so the tool refuses scope=global and instructs the operator that global hooks must be created out-of-band.
- **Mutable-field allowlist on update**: `name`, `matcher`, `if_expr`, `timeout_ms`, `on_timeout`, `priority`, `enabled`, `config`, `handler_type`, `event`, `metadata`. Protected columns (`source`, `version`, `tenant_id`, `id`, `created_by`) are silently dropped with the list reported in `ignored_fields`. This matches the WS handler's anti-escalation guard ("source strip closes the C2 forge").
- **Agent binding**: for scope=agent, `agent_ids` accepts a list of agent_key or UUID strings; the tool resolves all via `agent_store.GetByKey`/`GetByID`. `Create` populates the `hook_agents` junction atomically via the PG store's internal insert; `set_agents` uses `SetHookAgents` to replace the junction without touching other fields.
- Wiring is conditional: if `pgStores.Hooks` does not implement `hooks.HookStore`, the tool is not registered (gateway logs the skip).

### `agent_telegram` (agent administration family, sixth and final tool)

Telegram channel management via the Bot API 9.6 Managed Bots flow.
Lets an admin agent spawn a child bot for each new agent it provisions:
send a one-tap deep link to the operator via the parent bot, poll
Telegram for the resulting child token, then register the child as a
new channel bound to the target agent.

Sub-actions: `list_channels`, `request_managed_bot`,
`poll_managed_bot_token`, `unlink`.

Files:
- `internal/tools/agent_telegram.go` — tool implementation (~580 lines).
- `internal/tools/agent_telegram_test.go` — unit tests (21 cases incl. dispatch, username validation, parent resolution by id/name/caller-agent, deep-link assembly, polling-pending vs ready, duplicate channel guard, unlink by id/name).
- `cmd/gateway_tools_wiring.go` — registers the tool with `pgStores.Agents`, `pgStores.ChannelInstances`, and `msgBus`. Registration is conditional on the channel store being present.
- `cmd/gateway_builtin_tools.go` — adds `agent_telegram` builtin entry (`Enabled: false`, category `admin`).

Notable design choices:
- **No channels-layer extension required.** The tool calls
  `api.telegram.org/bot{token}/{method}` directly via a stdlib `net/http`
  client. Parent bot token is read from `ChannelInstanceStore.Get` which
  transparently decrypts via `GOCLAW_ENCRYPTION_KEY`. The new child token
  is persisted through `ChannelInstanceStore.Create` (re-encrypted).
- **Polling, not push.** Bot API 9.6 normally delivers a `ManagedBotUpdated`
  webhook update when the operator confirms in their client. Handling that
  push would require extending the Telegram channels-layer update parser
  (a merge-conflict-prone fork edit). Instead, the tool exposes
  `poll_managed_bot_token` which calls Telegram's `getManagedBotToken` —
  pending state surfaces as a non-error `{ready:false,status:"pending"}`
  response, success creates the channel. Forge decides retry cadence.
  If push becomes necessary later, add a channels-layer extension and
  let it emit an event that forge listens to; the tool can adopt that
  path without breaking the polling API.
- **Parent resolution order**: `parent_channel_id` → `parent_channel_name`
  → any Telegram channel attached to the caller agent (from
  `store.AgentIDFromContext`). The caller-agent default makes the common
  case (forge spawns child bots) ergonomic.
- **Username validation**: Telegram bot username rules enforced locally
  (5-32 chars, `[A-Za-z0-9_]`, must end with "bot"). Caller failures
  surface early before any Telegram round trip.
- **HTTP client**: 10-second per-call timeout, JSON-only, response body
  capped at 1 MB. Override via `SetHTTPClient` and `SetAPIBase` (used by
  tests pointing at `httptest.Server`).

Conflict-resolution rules:
- `telegramCreds` shape in `internal/channels/telegram/factory.go` is the
  canonical credential JSON for channel instances. The tool encodes new
  child credentials in the same shape `{"token": "..."}`. If upstream
  extends the credential shape (proxy, api_server), update the
  `poll_managed_bot_token` channel-creation path accordingly.
- Telegram API method names (`getMe`, `sendMessage`, `getManagedBotToken`)
  are stable across the Bot API. If Telegram renames or repositions
  managed-bot methods, update `tgGetBotUsername` / `handleRequestManagedBot`
  / `handlePollManagedBotToken` correspondingly.

### MCP bridge admin opt-in (cross-cutting)

The six admin tools above plus `publish_skill`, `skill_manage`, and
`knowledge_graph_mutate` are exposed to the Claude CLI provider through the
MCP bridge (`internal/mcp/bridge_server.go`). Upstream's bridge advertises a
**single static whitelist** to every agent regardless of `tools_config.allow`,
which means admin tools would either reach all agents or none.

This fork splits the bridge surface into two sets and filters per-agent:

- **`BridgeToolNamesPublic`** — general-purpose tools (filesystem, web,
  memory read, messaging, sessions, browser, etc.). Exposed to every agent.
- **`BridgeToolNamesAdminOptIn`** — admin / cross-agent / catalog-mutating
  tools. Exposed to an agent only when its `tools_config.allow` explicitly
  contains the tool name.

Filtering happens in two places using mcp-go middleware:

1. `WithToolFilter` at `tools/list` — hides admin tools the agent hasn't
   opted into so they don't appear in the model's deferred-tool list.
2. `WithToolHandlerMiddleware` at `tools/call` — denies invocation of an
   admin tool not in the agent's allow-list (defense-in-depth against cached
   tool lists or fabricated calls).

Both paths resolve the agent via `store.AgentIDFromContext(ctx)`, which is
populated by `bridgeContextMiddleware` from the HMAC-signed `X-Agent-ID`
header. When agent context is missing or lookup fails, the bridge falls back
to **public-only** — admin tools never leak.

Why this design (over alternatives considered):

- Per-session `mcp-config.json` `allowedTools` (claude-cli side) would push
  the policy onto a separate file the bridge doesn't validate — easier to
  drift, no defense-in-depth at the call site.
- Two separate bridge endpoints (one public, one admin-gated by URL) would
  double the HMAC plumbing and split the registry; the middleware approach
  keeps a single endpoint and reuses the existing context.

Files:
- `internal/mcp/bridge_server.go` — two tool sets, `BridgeAgentLookup`
  interface (minimal subset of `store.AgentStore` for testability),
  `agentAllowedBridgeTools` resolver, `WithToolFilter` + middleware setup.
- `internal/mcp/bridge_admin_filter_test.go` — 9 unit tests covering nil
  lookup, missing agent in context, lookup error, missing tools_config,
  empty allow, admin subset, full admin allow, unknown-name handling, and
  set non-overlap.
- `internal/gateway/server.go` — passes `s.agentStore` as the fourth
  argument to `NewBridgeServer`.

Operational note: to grant a new agent an admin tool, add its name to
`agents.tools_config.allow` (via dashboard or `agent_config.update`). There
is no global toggle to revoke — opt-in is per-agent by design.

Conflict-resolution rules:

- The Public set is intentionally close to upstream's `BridgeToolNames`.
  When upstream adds a new general-purpose tool to its bridge, add it to
  `BridgeToolNamesPublic`. When upstream changes the variable name, update
  the alias but keep the two-set structure.
- The AdminOptIn set is fork-only. Adding new admin-class tools (future
  `agent_X`) goes into this set, not Public.
- `BridgeAgentLookup` deliberately narrows `store.AgentStore` to one method.
  If upstream extends `AgentStore` with breaking changes, the bridge is
  insulated.
- `WithToolFilter` and `WithToolHandlerMiddleware` are mcp-go v0.44 APIs. If
  upstream's mcp-go bump removes or renames them, update both call sites
  together — they are paired (filter for discovery, middleware for calls).
