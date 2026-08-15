# Agents.md 更新补丁（Semantic Contract v1 对齐）

> 目标文件：`e:\ai_workspace\gta-plugin-sdk\Agents.md`（本文件在工作目录外，需手动应用）
> 原因：Agents.md 是 AI 写插件的入口文档，当前缺 Semantic Contract v1（schema/state/evidence/rule 四层）内容，
> 且 §7 示例用 `gta.decoder/v1`（宿主 CheckManifestVersion 要求 major=v2，照抄会被拒）。
> 应用方式：按下面 4 个补丁块逐一替换 / 追加。

---

## 补丁 1 — §3 包树：补五个语义包 + event/draft.go

替换 §3 中现有 package tree 代码块为：

```text
sdk root
├── decoder.go              # DecodeFuncV2 and Decoder server wrapper
├── registry.go             # registration loop and endpoint logic
├── manifest.go             # ReadManifest, ResolveRegistryAddr
├── plugin_manifest.go      # Manifest types, validation, contract/capabilities/schemas/states/evidence/rules
├── contract/               # Semantic Contract v1 (SSOT: contract.yaml + layered PluginChecker)
│   ├── contract.yaml       # embedded spec: rules, runtime, payload_framing, capabilities
│   ├── plugin_checker.go   # PluginChecker.Check (declaration) / CheckEvent (per-event)
│   ├── checker.go          # CheckDecodeResponse transport-layer checks
│   └── report.go           # Violation / Report (layered, machine-readable)
├── schema/                 # Schema layer: Field/Type/Semantic/Validate/Diff/Ref + vocabulary_gen
├── state/                  # State layer: Subject/Change/ParsePath/CheckAgainst
├── evidence/               # Evidence layer: Evidence/Edge/Graph/Decl/ExtractFromPayload
├── rule/                   # Rule layer: Rule/Registry/Severity/Attribution
├── framing/                # ExtractL7, Reassembler, FlowKey, Segment, TCPFlags
├── proto/
│   ├── plugin.pb.go        # generated protobuf messages
│   └── plugin_grpc.pb.go   # generated gRPC clients/servers
└── event/
    ├── event.go            # Event, Packet, EventContext, StateChange
    ├── identity.go         # EventID, EventType, SourceID, Identity
    ├── relation.go         # CausationID/CorrelationID/OriginID
    ├── draft.go            # Draft (plugin-side event), Validate, ToResponse
    ├── payload.go          # Payload
    ├── value.go            # tagged-union Value and accessors
    ├── value_conv.go       # ValueFromAny/JSON/Map/Slice
    ├── value_json.go       # JSON encoding
    ├── value_msgpack.go    # MsgPack encoding
    └── adapter.go          # StateChange extraction
```

---

## 补丁 2 — §7 Manifest contract：修 v1→v2 + 补语义契约声明

### 2a. 把 §7 开头的 minimal manifest 示例替换为：

```yaml
api_version: gta.decoder/v2
name: example-game
protocol: example-game
type: decoder
```

（原示例写的 `gta.decoder/v1` 会被宿主 `CheckManifestVersion` 拒绝——major 必须与宿主 ProtocolVersion(v2) 一致。）

### 2b. 在 §7 "Optional fields" YAML 示例之后追加小节：

### 7.1 Semantic Contract v1 declarations (schemas / states / evidence / rules)

The manifest can additionally declare the four semantic layers. The host runs the
six-layer checker at **registration time** (errors reject the Register RPC) and
per-event at `plugin.verify` time; a wrong declaration no longer passes silently.
Full spec: `docs/plugin-semantic-contract-v1.md`.

```yaml
contract:
  name: gta.plugin
  version: 1

capabilities:
  decode: true
  schema: true
  state: true          # gates the states[] requirement and _state_changes checking
  evidence: false
  rules: false

schemas:
  - id: game.player.v1
    version: 1
    fields:
      player_id: { type: string, semantic: entity_id, queryable: true, alias: pid }
      hp:        { type: uint32, semantic: amount, unit: hp, aggregatable: true }
      level:     { type: uint16, optional: true }

states:
  - subject_type: player
    schema: game.player.v1
    id_field: player_id
    paths: [hp, level]
```

Rules the checker enforces (rule IDs from `contract.yaml`):

- `schema_id` emitted at runtime must be declared in `schemas` (`gta.schema.undeclared`).
- `event_type` / `schema_id` must not use the reserved `gta.` prefix (`gta.event.reserved-prefix`).
- with `state: true`, `_state_changes` subject/path must be inside the `states[]` whitelist (`gta.state.*`).
- with `evidence: true`, the plugin may only emit `observation`/`derivation` evidence (`gta.evidence.*`);
  `assessment` belongs to the host.
- domain `rules[]` ids must not use the `gta.` namespace (`gta.rule.reserved-namespace`).
- legacy `indexable_fields` still parses and is upgraded to `queryable + alias` on the field.

---

## 补丁 3 — §10.1 保留字段：补 `_evidence`

在 §10.1 列表的 `_state_changes` 条目之后追加：

```text
- `_evidence`: evidence declarations (requires `capabilities.evidence: true`). Read by
  `evidence.ExtractFromPayload`. Kind must be observation|derivation (plugins never emit
  assessments); semantic must be declared in the manifest `evidence[]` block; strength must
  stay inside the declared range. The host projects it into the evidence graph.
```

---

## 补丁 4 — §21 checklist：补四层声明项

在 §21 checklist 的 `entity state is projected via _state_changes ...` 行之后追加：

```text
[ ] schemas/states/evidence/rules declarations pass the host registration-time checker
[ ] every emitted schema_id is declared in manifest schemas[] (gta.schema.undeclared)
[ ] event_type/schema_id do not use the reserved gta. prefix
[ ] with state capability: _state_changes subject/path stay inside states[] whitelist
[ ] with evidence capability: _evidence kind/semantic/strength match the evidence[] declaration
[ ] domain rule ids (if any) do not use the gta. namespace
```

---

## 附：一致性说明（已核对的宿主行为，写文档时不要再改错）

- 注册期：`gta/pkg/plugin/manager.go` Register 在 ValidateManifest + CheckManifestVersion 之后调用
  `sdkcontract.NewPluginChecker().Check(m)`；error 拒绝注册、warn 记日志。
- verify：`gta/cmd/gta-pipeline/verify.go` 逐事件构造 `event.Draft` 跑 `CheckEvent`（event/schema/state/evidence 层），
  并把声明期 Check 一并并入 plugin.verify 报告。
- `get_capture_schema`：MCP 侧从会话 manifest 快照经 `contract.ManifestSchemaIndex` 派生 data.* 字段与
  四层声明视图（field_source=manifest）；schema.json / event_index 采样降为 fallback。
- `GTA_REGISTRY_ADDR`：env > `--registry=` > 默认 `:9091`（SDK `ResolveRegistryAddr` 实现即如此，
  三处文档已统一为此说法）。
