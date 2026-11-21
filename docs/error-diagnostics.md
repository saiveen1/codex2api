# 结构化错误诊断

诊断增强可观测性，不改变客户端响应、重试条件、账号冷却或计费决策。**400 不会因诊断功能自动重试**；持续重试由独立设置控制。

## 在哪里查看

- **用量 / 运维错误页**：错误摘要旁的“诊断详情”，可查看关联信息、实际提交上下文、错误证据及复制安全 JSON。
- **数据库**：`usage_logs.diagnostic_context` 保存 `schema=2` 的 JSON 字符串，最大 24 KiB。现有 usage/运维错误 API 和运维错误导出携带该字段；沿用 usage 日志保留策略。
- **文件**：`${LOG_DIR:-logs}/error_diagnostics.jsonl`。单行最大 32 KiB，单文件 10 MiB，保留 `.1`、`.2`、`.3` 三份轮转；创建权限 `0600`（Windows 另受 ACL 控制）。每实例须独立文件。

`LOG_DISABLED=true` 关闭文件写入，不关闭原有用量数据库记录，也不移除数据库中的安全诊断上下文。没有数据库的嵌入式用法仍可使用文件诊断。

HTTP 响应头 `X-Codex2API-Request-ID`、usage 的 `request_id`、诊断 `request_id` 对应。ID 由服务端生成，不采信客户端同名头；WebSocket 每个逻辑轮次独立，`parent_request_id` 关联握手。不会向协议帧插入内部诊断内容。

```sh
id='从响应头或用量记录复制的请求 ID'
jq -c --arg id "$id" 'select(.request_id == $id or .parent_request_id == $id)' \
  logs/error_diagnostics.jsonl
```

需要历史轮转时只添加实际存在的文件。数据库旧行的字段默认空字符串；页面明确标为历史上下文未采集，**不会编造或自动回填原始请求**。

## 无可用账号与空模型目录

在管理后台「使用统计」直接点击失败状态，即可查看请求当时的诊断。Responses 的 `decision` 记录终止分支、是否尝试上游、请求和路由模型，以及作用域内拒绝原因与有界账号样本。例如：同组 28 个账号鉴权不可用、另有 1 个账号只允许其他 Key。已有上游尝试与最终调度记录用同一个请求 ID 关联；终态诊断不重复计费或计入 Key 用量。SSE 若已提交，详情分别显示实际 HTTP 状态与业务终态。

在「API 密钥」点「检查模型目录」，可以用当前配置检查本地目录，不会试跑推理或获取上游 Codex manifest。普通 `/v1/models` 的空结果仍是 `200 + data: []`，响应头 `X-Codex2API-Model-Status: no_visible_models` 与请求 ID 可供反馈。最近一次已记录的真实空目录快照按 Key 限频写入、最长保留 24 小时；当前检查与历史快照分开。目录探测不计入推理用量或请求额度。

候选计数是失败决策时的有界观察，不会再派发请求或占用并发。并发竞争、缓存切换、亲和以及未知筛选可能无法细分；目录可见不保证该时刻有并发位。新证据只含固定原因码、计数、限定格式的模型标识与管理员可见的内部 ID，不进入公开 Key 用量页。上线前未保存的原因不会补写。

## 记录的“提交上下文”是什么

`request` 是网关入站结构；`attempt.request` 是本次上游派发的结构。两者不能互相替代。

HTTP 在派发前通过独立 `GetBody` 读取器取得实际传输字节，识别 identity / gzip / zstd 后做有界结构解析；不读取活跃 `req.Body`，不额外向上游发送请求。因此 HTTP 成功握手、之后 SSE 失败的请求仍能保留出站结构。编码不支持、解码/读取失败、无法取得独立读取器会分别标记，**不再把压缩字节当成坏 JSON**。

Responses WebSocket 在最终帧发送点保存结构；未接入最终帧采集的适配器只能标为 `adapter_input`，不声称它等同于真实发送帧。底层连接池的发送重试仍属于原 Handler 派发序号。

结构字段包括：

| 字段 | 含义 |
| --- | --- |
| `fingerprint` / `shape_fingerprint` | 完整已检查内容的指纹 / 结构分布指纹，用于比较相同请求或相似结构；不是可逆正文 |
| `model_fingerprint` | 模型标识指纹；模型明文仍可关联原 usage 字段 |
| `fields` / `unknown_fields` | 白名单顶层字段名和未知字段数量 |
| `input_items` / `messages` / `tools` | 已扫描的数量与类型分布；计数受预算限制 |
| `parameters` | 已知协议参数的类型、长度、元素数；仅固定枚举、布尔值和有界数值控制保留值 |
| `item_samples` | 最多 24 个有顺序的输入项样本，保留原索引、类型、角色、内容类型、长度及 call_id/name 指纹 |
| `tool_samples` | 最多 12 个工具形态样本：类型、名称指纹、schema 长度、properties/required 数量、strict |
| `encrypted_items` / `missing_call_ids` | 加密项数量、缺少调用关联 ID 的数量 |
| `instructions_bytes` | instructions/system 的长度，不保留内容 |
| `samples_truncated` / `inspection` | 样本裁剪、扫描超限或不可用的明确标记 |

样本保留前半部分与最后一部分，便于比较长历史的首尾变化；不是完整历史快照。`image_choice_without_tool` 等结构线索是事实，**不是已经确定的错误根因**。

## 错误证据与明确的未知状态

错误解析覆盖普通 `error.message`、嵌套 Responses 错误、`detail` 字符串/对象/校验数组、仅 code 的错误等格式。SSE/原生 Grok SSE 错误在协议转换前保存安全证据；正常 `error:null` 不被当作失败。传输异常单独标明 DNS、TLS、超时、取消或 EOF，不猜测提供者拒绝原因。

| 错误字段 | 含义 |
| --- | --- |
| `source` | `upstream_http`、`upstream_handshake`、`upstream_stream`、`downstream_response`、`websocket_gateway` 或仅剩用量摘要的 `usage_fallback` |
| `detail=generic` | 已捕获通用错误说明，如 Bad Request；该说明没有定位具体原因，不代表响应一定只有这一字段 |
| `message_present` / `message_bytes` | 检查到消息字段/文本的存在与长度；不是说原文已保存 |
| `message_class=message_omitted` | 收到了消息，但未保留原文；不得解释为“上游什么都没给” |
| `code/type/param` | 白名单机器码与参数路径；只返回码没有 message 时也保留可用证据 |
| `*_redacted` / `*_fingerprint` | 值存在但不在安全白名单；保留指纹，不当作字段不存在 |
| `body_bytes/body_format/body_fingerprint` | 本次读取到的错误内容规模、格式与指纹；受原有读取预算限制，不保证等于上游全部正文 |
| `evidence` | 最多 32 个字段结构，深度最多 4；未知字段名用指纹替代，记录类型、长度、元素数及指纹，不保存值 |
| `detail=empty` | 读取结束后错误正文为空 |
| `detail=unavailable` | 没采到可用证据；不能认定上游没有信息 |
| `size_limit` / `evidence_truncated` / `read_error` | 大小限制、裁剪或读取未完成 |

`capture_status` 表示整条诊断的采集程度：complete / partial / unavailable / size_limit / invalid。complete 表示这些受限元数据已采到，**不表示保存了原始正文或已经查明根因**。

## 请求、尝试和最终结果

| 字段/事件 | 含义 |
| --- | --- |
| `upstream_attempt` | 已观察到派发拒绝/传输错误，错误正文此时可能尚未读取 |
| `attempt_outcome` | 失败尝试及失败后的恢复结果，附带独立的尝试上下文 |
| `request_end` | 最终结果；不把下游网关包装错误覆盖掉已捕获的上游证据 |
| `websocket_error` | WebSocket 网关错误，不保存任意 message/details 正文 |
| `attempt.index` / `usage_attempt` | 实际派发序号 / 原业务重试计数，两者可能不同 |
| `attempt.http_status` / `downstream_http_status` / `outcome_status` | 上游状态 / 下游状态 / 业务结果；HTTP 200 不保证成功 |
| `retry_scheduled` | 当时计划重试；随后仍可能被取消，不代表已派发 |
| `attempt.protocol/request_encoding/request_bytes/request_capture` | 协议、编码、线缆字节数和采集来源 |

续想快照及旧流的证据绑定原尝试，不能被后续账号/请求覆盖。恢复成功会另有成功结果，不应把所有事件行重复统计为失败请求。

全局 middleware 在大小限制、限流和鉴权之前。没有原 usage 调用的本地 HTTP/WS 拒绝和 panic，可通过 `RequestDiagnosticMiddleware(store, db)` 保存一条零 token、零计费诊断用量，避免重复记账；admin/UI 路由不纳入此采集。模型计费、重试和流转行为不因诊断改变。

## 隐私、容量和不可恢复部分

- 新诊断不保存提示词、对话文本、工具名称/参数/输出、JSON Schema 内容、图片、密文、Authorization、Cookie、代理 URL 或任意请求头。请求只保留结构和指纹；旧 usage 的既有字段保留原语义。
- 错误消息也可能回显用户数据，因此新上下文保存固定分类、字段结构和指纹，而非任意原文。未知错误码/字段使用存在标记和指纹说明其未显示。
- 指纹是短 SHA-256，不是加密或匿名化保证；仍须限制数据库、日志和导出的访问。
- 请求读取/解码最多 32 MiB，结构扫描最多 20,000 个项目；错误结构解析最多 8 KiB。超过上限明确标记，不能将其视为完整采集。
- 每条数据库上下文最大 24 KiB；必要时去掉样本/字段证据但保留大小限制标记，不截断成无效 JSON。日志缓冲、SQL 批量写入上限和 usage 保留策略沿用项目机制。
- 文件写入失败不会改变业务结果。数据库/缓冲写入失败时无法保证持久化成功；诊断不是零丢失审计系统。
- 未经过共享派发/读取入口的协议路径可能只有适配器输入或 unavailable；界面须遵守采集标记，不能把入站数据冒充实发数据。
- 不能恢复上线前丢失的正文；不能单凭结构指纹解释上游未说明的语义拒绝。进一步缩小原因需要受控实验和新的授权，不能自动重放生产请求。

## 定向验证

```sh
go test ./proxy -run 'TestDiagnostic|TestUpstreamTrace|TestUsageLog(ErrorMessage|AndClient|FailureMessage)' -count=1
go test ./database -run 'Test(NormalizeUsageDiagnosticContext|UsageDiagnosticContext|UsageTraceRoundTrip|UsageLogInsertColumnCount)' -count=1
go test ./admin -run 'TestOpsErrorExportRetainsDiagnosticContext' -count=1
go test ./proxy/wsrelay -run 'TestPrepareWebsocket|TestBuildErrorEvent_|TestFormatDialHandshakeError' -count=1
cd frontend
node --experimental-strip-types --test src/lib/diagnosticContext.test.mjs
node --experimental-strip-types --test tests/diagnosticContext.browser.test.mjs
```

均使用合成数据和本地测试设施；不为验证诊断重放生产请求。
