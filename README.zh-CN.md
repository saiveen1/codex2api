# Codex2API · saiveen1 fork

[English](README.md) | **中文**

Codex2API 的个人维护分支，提供 OpenAI / Anthropic 兼容网关。本页主要介绍 **[saiveen1/codex2api](https://github.com/saiveen1/codex2api)** 自己维护的功能与修复，不重复上游完整功能列表。

## 本 fork 的功能

### 内置 OAuth 网关管理

- 在现有后台直接导入 `账号----密码----TOTP_SECRET` 或 JSON 材料，通过明确选定的外部网关授权，并直接写入本地账号池；无需另部署桥接服务。
- 支持每账号 OAuth 代理、执行机代理检查、独立运行代理、任务查询与安全归档；结果未知不重新授权、不盲重放入库。
- 使用原管理员登录和数据库，启用前配置 `CODEX_CRED_ENCRYPTION_KEY`。本轮仅手动授权新增账号，不覆盖已有账号，也不触发 401 自动恢复。

详见[启用方式、API 与限制](docs/oauth-gateway.md)。

### 注重隐私的请求诊断

对应提交：`a6179fb`、`4939c24`。

- 关联网关请求 ID、上游尝试、上游请求 ID 和 WebSocket 父请求，分别记录 HTTP 状态与业务结果。
- 区分入站请求结构和实际提交上游的结构，保存有界元数据、指纹及 HTTP / SSE / WebSocket 错误证据。
- 在「用量」「运维错误」页增加**诊断详情**，支持安全 JSON 复制和带诊断上下文的错误导出。证据随用量记录落库，并写入轮转 JSONL；`LOG_DISABLED=true` 只关闭文件输出，不关闭已有用量数据库记录。
- 新诊断上下文不保存提示词原文、对话文本、工具参数/输出或凭据。明确区分证据缺失、脱敏、裁剪和无法采集，不把上游通用报错编造成已定位的根因。

详见[错误诊断文档](docs/error-diagnostics.md)。

### 显式 HTTP 400 重试开关

对应提交：`a80d951`。

- 在「上游错误持续重试」设置中增加独立的 **HTTP 400 重试 / Bad Request** 开关，复用现有状态码选择机制，保留其他状态码设置。
- 不自动开启 400 重试，保留已有取消和持续时间上限。反复提交无效请求可能增加延迟与费用；诊断功能本身不会触发重试。

### 可选 ChatGPT Web 图片通道（实验性）

对应提交：`cc6fabf`。

- 新增显式限定的 `chatgpt_web` API Key 通道，默认关闭，与原生 Codex 隔离；支持 `gpt-image-2` 文生图、JSON / multipart 图生图、生图工作台和异步图片任务。
- 网关继续统一管理账号、凭据刷新、授权、用量和图库；可选私网 Python worker 执行网页协议，不运行 Chrome，也不另建账号池。
- 网页能力/额度单独观测，固定账号和出口绑定，使用持久化提交保护与稳定的 job/output ID。提交结果未知时不重新生成；已知任务只允许继续查询和下载。
- 不静默回退到 Codex，不切换账号，也不因下载失败重放生成。Mock 测试通过**不代表**真实账号可用或已具备生产验收结论。

详见[接入方式与限制](docs/chatgpt-web-images.md)、[worker 协议](docs/chatgpt-web-worker-protocol.md)和[worker 配置](services/chatgpt-web-worker/README.md)。该组件使用独立许可证，见下文。

## 本 fork 的修复与兼容性改进

### Codex HTTP 首发编码兼容

对应提交：`e9a8610`。

- 开启请求压缩时，仅在最终正文不超过 **128 KiB**、编码结果是完整单块 zstd 且确实节省字节时压缩。更大或不适合压缩的正文在**首次派发**前直接选择不压缩，不修改上下文，不增加恢复重发。
- 编码/长度头与实际正文保持一致，清理冲突的账号自定义头，`/responses/compact` 保持不压缩；普通原生 Codex HTTP 与原生图片 pipeline 共用此策略，不作用于 WebSocket 帧。
- 补齐 `x-oai-request-id` 等上游请求 ID 关联及缺值回退。这是保守兼容策略，**不是**上游正文大小上限，也不声称修复所有 HTTP 400。

`CODEX_REQUEST_COMPRESSION` 与后台设置见[配置文档](docs/CONFIGURATION.md)。

### 诊断证据正确性

包含于 `4939c24`。

- 正常流事件中的 `error:null` 不再遮住后续真实失败；详情弹窗中的空 attempt error 不再遮住最终 event 错误证据。
- 旧流证据绑定原尝试，压缩正文先解码再检查结构，历史记录不伪造请求上下文。

## 文档与源码

使用这些改动时，应从本 fork 源码构建；上游预构建镜像不保证包含本 fork 的功能。

- [文档索引](docs/README.md) · [API](docs/API.md) · [部署说明](docs/DEPLOYMENT.md)
- [配置说明](docs/CONFIGURATION.md) · [错误诊断](docs/error-diagnostics.md)
- [ChatGPT Web 图片通道](docs/chatgpt-web-images.md) · [可选 Compose 配置](docker-compose.chatgpt-web.yml)

## Upstream

基于 **[james-6-23/codex2api](https://github.com/james-6-23/codex2api)**，当前合入至 **v3.1.5 / `811ad45`**。原有网关及继承的功能、修复归功于上游和贡献者，不计为本 fork 新增；完整记录见[上游更新日志](CHANGELOG.md)。

## 许可证与必要声明

- 网关沿用上游 **MIT License** 声明，保留既有版权与第三方声明；本页不改变其许可归属。
- 可选的 `services/chatgpt-web-worker/` 子树使用 **AGPL-3.0-only**，包含对 `yukkcat/chatgpt2api` 网页协议的适配。保留其 [LICENSE](services/chatgpt-web-worker/LICENSE)、[NOTICE](services/chatgpt-web-worker/NOTICE) 和 [PROVENANCE](services/chatgpt-web-worker/PROVENANCE.md)；组合分发前按该子树说明核对发布及对应源码提供要求，不默认认为独立进程构成豁免。
- ModelTrace 的来源声明与 MIT 条款保留于 [NOTICE](third_party/modeltrace/NOTICE) 和 [LICENSE](third_party/modeltrace/LICENSE)。

项目不提供担保；生产使用前请自行验证兼容性、数据处理方式及适用条款。
