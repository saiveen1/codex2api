# 结构化错误诊断

诊断仅增强本地可观测性，不改变客户端响应、账号冷却、重试次数或重试条件。**400 不会因本功能被自动重试。**

## 日志与关联

默认开启，写入 `${LOG_DIR:-logs}/error_diagnostics.jsonl`。沿用 `LOG_DISABLED=true` 总开关；关闭时不写诊断文件。每行一个 JSON 对象，`schema=1`。单行最大 32 KiB，单文件最大 10 MiB，保留 `.1`、`.2`、`.3` 三份轮转文件。文件创建权限为 `0600`（Windows 还受 ACL 控制）。只允许一个服务进程写同一日志文件；多实例应使用各自目录。

HTTP 响应头 `X-Codex2API-Request-ID` 与 usage 中的 `RequestID`、诊断的 `request_id` 对应。它由服务端生成，不采信客户端提交的同名 ID。WebSocket 每个逻辑轮次有独立 ID，`parent_request_id` 关联握手请求；不会修改 WebSocket 协议帧来注入诊断字段。

示例（在已部署本改动的实例上执行）：

```sh
id='从响应头复制的请求 ID'
jq -c --arg id "$id" 'select(.request_id == $id or .parent_request_id == $id)' \
  logs/error_diagnostics.jsonl logs/error_diagnostics.jsonl.[123]
```

轮转文件尚未生成时只传存在的文件。可以用 `request_id` 在管理后台 usage/运维错误记录中关联准确模型和账号信息。

## 如何读记录

| 字段/事件 | 含义 |
| --- | --- |
| `upstream_attempt` | HTTP/WS 派发阶段已观察到拒绝或传输错误；此时错误正文可能尚未读取 |
| `attempt_outcome` | 统一 usage 收口记录失败尝试，以及发生失败后的恢复结果；包含提供者、流式及续想路径 |
| `websocket_error` | WebSocket 本地/网关拒绝，只有白名单错误 code/type，不保存 message/details |
| `request_end` | 请求/轮次结束，补齐早期拒绝、最终结果和总耗时 |
| `attempt.index` | 本请求内实际上游派发序号，含续想；0 表示无可归属尝试 |
| `usage_attempt` | 原有业务重试计数，可能与派发序号不同 |
| `retry_scheduled` | 原 usage 路径在当时决定重试；不保证随后真的派发，取消可能中断等待 |
| `attempt.http_status` | 上游响应/适配器返回的 HTTP 状态，缺省表示未观察到 |
| `downstream_http_status` | 记录当时下游 writer 状态；最终状态看 `request_end` |
| `outcome_status` / `outcome` | 业务结果，不能用 HTTP 200 代替；尚无最终证据时为 `unknown` |
| `stage` | 传输、HTTP 拒绝、流、续想、本地拒绝、鉴权或 panic 等阶段 |
| `attempt.transport` | `timeout`、`canceled`、`dns`、`tls`、`transport`，不回显底层 URL 或错误原文 |
| `error` | 已知 code/type、白名单参数路径、固定 message 分类，以及错误指纹 |
| `request` | 入站结构摘要，不包含正文；无法获得时 `inspection=unavailable` |
| `attempt.request` | HTTP/WS 派发立即失败时可取得的出站结构；不会读取或重放活跃流 |

`outcome=failure` 可以与 HTTP 200 同时出现，例如 SSE `response.failed`、空响应或上游断流。重试恢复成功会有后续成功记录；不要把所有事件行当成独立失败请求计数。

## 针对通用 400 的排查

1. 用请求 ID 找到同请求的派发、usage 及最终记录，确认账号 ID、代理 ID、尝试编号和实际业务状态。
2. 检查 `error.code/type/param`。上游只返回 `{"detail":"Bad Request"}` 时，明确标记 `detail=generic`、`message_class=generic_bad_request`，**并不声称已确定触发字段**。
3. 对比入站与可用的出站结构：请求字节数、已知顶层字段、input/messages/tools 数量、工具/输入类型分布、缺失 call_id 数量、加密项数量、previous_response_id 是否存在、reasoning effort。
4. `image_choice_without_tool=true` 表示结构中指定 image_generation 却没有声明对应工具；它是结构事实，不保证就是该次 400 的原因。`missing_call_ids`、`encrypted_items` 同样是线索，不是自动根因判定。
5. `model_fingerprint` 与 `error.fingerprint` 用于聚类；模型明文请通过请求 ID 查询原 usage。不同模型/错误不会因隐私策略全部混成一个无区分的文本。

## 隐私和资源边界

- 不记录提示词、对话内容、工具名称/参数/输出、JSON Schema 内容、图片、密文值、Authorization、Cookie、代理 URL、API Key 值、IP、User-Agent 或用户自定义字段名称。
- JSON 错误消息也可能包含用户数据，因此不直接保存 message，使用固定 `message_class` 和短 SHA-256 指纹。未知 code/type 映射为 `other`，未知 param 不记录。指纹不是加密，仍应限制日志访问与导出。
- 内部 UUID 与规范 `req_<hex>` 上游 ID保留；其它上游 ID以 `sha256:` 指纹表示。原 usage 的既有记录行为不因本功能改变。
- 请求结构扫描限制 32 MiB、最多 20,000 个 input/messages/tools 元素；超限显式标记 `size_limit` / `item_limit`，计数可能是部分值。只保存摘要，不延长原始请求体的生命周期。
- 错误对象解析最多 8 KiB；大正文标记 `size_limit`，HTML/非 JSON 标记 `unstructured_omitted`。不截断成伪装完整的 JSON。
- 旧 `bad_request.log` / `server_error.log` 和共享控制台错误正文也改成同样的安全摘要；旧文件的原有轮转/保留策略不在本次改动范围内。
- 文件写入失败只输出一次通用告警，恢复后输出恢复提示；不把日志事件、路径或凭据带到 stderr。日志不可写不改变请求业务结果，但可能缺失诊断记录。

## 覆盖范围与限制

全局业务 API middleware 在请求体限制、全局限流及鉴权之前运行；admin/UI 路由不纳入此日志。仅调用 `Handler` 的嵌入式应用可复用 `RequestDiagnosticMiddleware`，并应按同样顺序注册。

HTTP、提供者适配器、SSE/WS 业务错误和续想通过现有派发/usage 收口记录。流式成功握手后的出站 body 不额外复制，故 `attempt.request` 可以缺省；此时仍有入站摘要、派发字节数及业务错误分类。没有结构化错误或没有到达对应 hook 的字段明确为空/不可用，不根据状态码编造原因。底层 WebSocket 池内部发送重试不是新的 Handler 派发，仍使用同一尝试序号。

不采集业务正文意味着无法离线逐字重放原请求。本功能不能追溯还原部署之前那些只留下通用 Bad Request 的具体触发字段，也不能保证上游未说明的错误都能自动定位。生产部署、历史日志清理和真实流量验证需另行授权。

## 定向验证

```sh
go test ./proxy -run 'TestDiagnostic|TestUpstreamTrace|TestClassifyTransportFailure|TestShouldSuppressRetryableResponseFailedBeforeFirstToken|TestUpstreamError(Console|File|Logs)' -count=1
go test ./proxy/wsrelay -run 'TestBuildErrorEvent_|TestFormatDialHandshakeError|TestHandshakeUnauthorizedHTTPResponse' -count=1
go test ./database -run TestUsageTraceRoundTrip -count=1
go test ./security -run 'TestMaskSensitiveData|TestFileLogsDisabled' -count=1
```

全部使用合成数据；不要为验证诊断能力而重放生产请求。
