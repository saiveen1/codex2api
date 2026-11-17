# 内置 OAuth 网关

本 fork 在现有管理后台提供 **OAuth 网关** 页面（`/oauth-gateway`）。它使用 codex2api 自身的管理员鉴权、数据库和账号池，不需要部署独立桥接服务，也不会通过 HTTP 调用自己的账号导入 API。

默认外部执行网关为 `https://oauth.cala.lol`，接口合同见 <https://oauth.cala.lol/docs>。客户端按 public v1 revision 3 校验能力，实际选项和容量以部署的 capabilities 响应为准。填写服务根地址，不要把 `/docs` 或 `/api/v1/oauth` 填进地址栏。

## 启用

1. 给 codex2api 进程配置稳定、足够随机的 `CODEX_CRED_ENCRYPTION_KEY`，然后重启。已有密钥不要更换；密钥丢失或更换会导致已加密的任务和账号凭据无法读取。此变量也启用项目原有的账号凭据加密，新写入使用加密，已有明文凭据仍可读取。
2. 使用原有管理后台登录，进入 **OAuth 网关**。默认地址无需修改；输入网关的**客户 CDK（激活码）**，不是 codex2api 的 `ADMIN_SECRET`，也不是外部网关的管理员密钥。
3. 保存后页面自动读取能力、配额和本地 Codex 分组；也可测试连接或分别重试失败项。CDK 直接作为 Bearer；推荐使用 CDK 而非会过期的短期会话。刷新保留已选分组和授权选项。
4. 粘贴或上传账号材料，设置代理和可选目标分组，先预检，再明确创建授权任务。修改材料或选项后需要重新预检，按钮旁会显示原因和下一步。
5. 材料输入、预检和未归档任务显示完整账号标识。不要在不可信屏幕或录屏中展示密码/TOTP；明文显示不改变管理员鉴权、服务端加密和日志保护。

Docker Compose 需要将该环境变量明确传入容器，例如在服务的 `environment` 下加入 `CODEX_CRED_ENCRYPTION_KEY: ${CODEX_CRED_ENCRYPTION_KEY}`。仅在宿主机设置变量而未传入容器，不会启用功能。不要把真实密钥、CDK 或账号材料提交到仓库。

未配置加密密钥时，这个模块返回 `503 / encryption_required`；原有模型转发、账号管理和 OAuth 登录入口不受影响。启动失败或 worker 因持久化故障停止时同样拒绝新操作，不降级为内存或明文任务。

## 材料和代理

TXT 每行：

```text
user@example.invalid----synthetic-password----JBSWY3DPEHPK3PXP
```

这是合成材料，不能用于真实登录。密码首尾空格原样保留；含 `----` 的密码应使用 JSON 账号数组。每账号可提供 `proxy` 覆盖，服务按代理分批。单任务最多 100 个账号、请求体最多 1 MiB，同时还受外部网关的实际容量限制。

- **OAuth 代理**只用于外部执行机完成授权。自带代理会通过该执行机的 `/proxy-check` 检查，不用 codex2api 本机的连通性替代；失败不回退到系统代理或直连。
- **运行代理**只用于账号后续的模型请求，必须在 codex2api 的代理管理中已添加并启用。留空沿用 codex2api 现有的正常代理策略（该策略本身可能允许直连）；不复制 OAuth 代理，也不采纳网关结果里的代理字段。
- 目标分组仅允许 Codex 分组。新账号及其分组同一事务提交后，才加载到运行时账号池。

## 任务和安全边界

授权成功与入库成功分别显示。后台轮询不依赖管理页面是否打开，重启后继续查询原任务。

- 原始请求 ID、外部批次 ID、提交/入库检查点先落库，再执行对应副作用。提交响应丢失只查询原 ID，不创建新授权；`404` 不作为未执行证明。
- 只自动重试原任务的 lookup/status/result 读取，连续失败的最短间隔为 5/10/20/30 秒，第五次失败停止并待核查。鉴权、配置及身份问题不盲重试。
- 用户选择“重试导入”只处理已确认未写入的失败项，不触发新 OAuth。SQL 提交结果不明或重启时处于 `importing` 的项目保持 `import_unknown`，必须人工核对，不能盲重放。
- 重复账号、结果类型错误或身份冲突标记为 `import_rejected`，不允许“重试导入”；可在明确确认丢弃授权结果后归档。它们不计为成功入库。
- 普通新增仍拒绝覆盖相同 OAuth 身份或凭据路由的已有账号。只有显式指定 `recovery_account_id` 的修复任务，才可在原主体、原工作区和凭据版本匹配时更新原账号；保留其主键、代理、分组、状态历史和统计。
- 终态任务清除不再需要的密码、TOTP 和 OAuth 代理材料。只有显式选择 `save_credentials:true`，成功导入/修复后才另建加密恢复保管记录。归档任务不关闭恢复；请使用“停止恢复并清除材料”撤销保管。结果未知的任务只保留对账所需材料，不盲目重放。
- 页面不将账号材料、密码、TOTP、CDK 或代理认证写入 localStorage/sessionStorage。只有未确认提交的非敏感本地请求 ID 存入当前标签页的 sessionStorage，用于离开页面后继续核对。关闭标签页前仍应记录原 ID；任务台账保留在服务端。
- 更换外部网关地址必须重新输入对应凭证；任务保留它创建时明确选择的连接快照，不会默默换网关。HTTP 仅允许回环地址，外部地址使用 HTTPS；重定向不携带凭据跟随。

## 受控原账号恢复

三种操作互相区分：

- **新增账号**：不填写原账号 ID；默认 `save_credentials:false`。重复账号仍被拒绝，不会顺便覆盖。
- **修复已有账号**：填写“修复已有账号 ID”，只提供该账号的一条材料。不能提交目标分组或运行代理变更；原账号必须仍存在且 enabled，主体/工作区必须可证明。已删除、禁用、身份不符或凭据已被其他操作更新时拒绝回写。
- **启用后续自动恢复**：明确勾选“成功后启用 401 自动恢复（加密保管密码和 TOTP）”。只保管本次成功账号的登录材料、授权选项和网关连接快照。历史任务不会因升级自动启用，已清除的密码/TOTP必须重新提供。

启用后，严格识别凭据失效类 401 或永久 token 刷新错误；401 路径先尝试原有 refresh token/session token 刷新。只有确需重新授权才提交密码/TOTP，且网关必须声明 `password_totp`、`expected_identity_guard` 和原请求查询能力。通过普通授权接口携带 `expected_identity` 执行，不依赖外部网关自带自动恢复调度；不能以邮箱相同或 `team_preferred` 替代原主体/工作区校验。

每个账号只允许一个在途恢复，自动授权最多三次，后续尝试至少冷却15分钟；失败、挑战、身份不能证明或结果未知进入人工核查，不把封禁、代理/网络故障、网关 CDK 鉴权失败、`missing_scope` 等一律当作可重登。新凭据通过原账号主键、generation、family和凭据指纹的条件事务写回；未知数据库提交保留 `import_unknown`，不重复写入。

已保管账号不被 401/错误自动清理抢先删除。恢复 worker 故障停止不能撤销已知保管记录的清理保护；手动删除/禁用仍有效，后台不能自动复活。“停止恢复并清除材料”清除保管秘密、撤销未来恢复，已在途授权仍按原ID对账，但停止后的结果不能写回原账号。

恢复记录可在页面查看 `ready`、`refreshing`、`pending`、`recovering`、`manual_review`、`disabled` 状态、尝试次数、关联任务和错误。非重试类失败需要人工纠正材料或账号状态；不能反复点新建任务替代对账。

触发回调只写有界内存信号，以免阻塞模型请求或在账号锁内访问数据库；worker随后落盘。该极短窗口内进程崩溃可能丢失一次触发，后续同类失败可再次触发；已经落盘的任务和未知结果会在重启后继续核查。不自动迁移独立桥接服务的旧任务；旧系统的在途结果应先在那里对账。

## 存储与多实例

任务及连接配置以随机 nonce 的 AES-GCM 密文保存在现有数据库的 `oauth_gateway_state` 单例行中，使用现有凭据加密根密钥的域分离派生密钥。数据库中不保存第二套账号池。SQLite 任务检查点在同一连接上使用 `synchronous=FULL`，在外部副作用前确认写入。

当前模块为**单写入者**：数据库 owner 租约为 120 秒，worker 空闲时也续租；每次状态更新带 revision/owner 条件。另一个实例不会接管尚未过期的任务；失去所有权或写入结果不明后，旧实例停止继续工作。多副本环境请将此管理页请求固定路由到持有者实例。正常关闭释放租约；异常退出后待租约到期再重启选定实例，不通过删除状态行抢占。

**升级/回退边界：** 当前加密状态格式为 v3，首次打开模块会将 v1/v2 安全升级；旧保存标记不视为用户保管授权。没有新增数据库表或 DDL，但旧二进制不能读取 v3，会拒绝启动 OAuth 模块，不能直接降级并假定恢复仍可用。部署前应确认这一持久状态格式变化；不要删除状态行或更换加密根来绕过。

当前容量：最多 8 个未解决任务、200 个当前任务记录、10,000 个归档，状态明文编码上限 64 MiB。归档保留幂等指纹，同一 ID 的原请求不会重新授权。

## 管理 API

所有接口均在现有管理员鉴权后，使用 `X-Admin-Key`，前缀 `/api/admin/oauth-gateway`。成功响应为 `{"ok":true,"data":...}`，错误为 `{"ok":false,"error":{...}}`；外部网关的 401 映射为 502，不让它被误判为管理员登录失效。敏感响应使用 `Cache-Control: no-store`，写接口限制为 JSON 对象及 1 MiB。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET / PUT | `/settings` | 读取非秘密配置/恢复能力 / 保存 `{oauth_url,oauth_token}`；同地址 token 留空保留 |
| POST | `/connections/test` | `{}`，测试网关和本地账号分组访问 |
| GET | `/capabilities`、`/quota`、`/groups` | 服务能力、配额、本地 Codex 分组 |
| POST | `/proxy-check` | `{options}`，在外部 OAuth 执行机检查自带代理 |
| POST | `/preview` | 本地与网关校验，不执行授权 |
| POST | `/jobs` | 创建已确认任务；必填稳定 `request_id`，新建 202，幂等命中 200 |
| GET | `/jobs`、`/jobs?archived=1`、`/jobs/{id}` | 当前任务、归档和详情；返回完整 `identifier`，不返回密码/TOTP/token |
| POST | `/jobs/{id}/refresh` | `{}`，继续核查原任务 |
| POST | `/jobs/{id}/cancel` | `{}`，请求取消，仍须确认在途结果 |
| POST | `/jobs/{id}/retry-import` | `{}`，只重试已确认未写入的导入 |
| POST | `/jobs/{id}/archive` | `{discard_credentials:boolean}`，安全归档 |
| GET | `/recovery` | 恢复保管状态数组：账号 ID、完整标识、状态、尝试次数、下次检查、原任务及错误 |
| POST | `/recovery/{account_id}/disable` | `{}`，停止恢复并清除保管秘密；成功返回 `{}` |

`preview` / `jobs` 输入可选 `source_text` 或 `accounts`（二选一），另含 `options`、`group_ids`、`runtime_proxy`、boolean `save_credentials`（默认 false）和可选 `recovery_account_id`。指定修复 ID 时只能包含一条材料，`group_ids` 必须为空、`runtime_proxy` 必须留空。请求中的标识必须匹配原账号；无法安全对应手机号与原账号身份时转人工处理。

`PreviewRow` / `ItemView` 返回完整 `identifier` 并保留旧 `identifier_masked` 字段供兼容。未归档历史任务可从本地请求记录取得完整标识；已归档任务不保留逐账号材料，不能还原已经清除的详情。恢复视图不包含密码/TOTP、网关令牌、代理认证或凭据指纹。

## 本地验证

可运行定向测试，不需要任何真实网关凭据：

```sh
go test ./internal/oauthgateway/...
go test ./database ./admin ./auth ./proxy -run 'TestOAuth(Gateway|Recovery)'
cd frontend
npm run typecheck
node --experimental-strip-types --test src/lib/oauthGateway.test.mjs
node --experimental-strip-types --test tests/oauthGateway.browser.test.mjs
npm run build
```

浏览器测试使用本地合成 API，截图写入临时目录，可通过 `OAUTH_GATEWAY_TEST_ARTIFACT_DIR` 指定目录。模拟测试和构建通过不代表真实 OAuth、代理或 PostgreSQL 多实例部署已完成验收。

### 2026-11-13 验证记录

- Go 1.27.4 / Windows amd64：56 个顶层定向用例通过（OAuth 模块 45、数据库 7、管理 API 4，不累加子测试或重复运行）；定向 `go vet` 通过。数据库覆盖加密、错误密钥、单写入者租约、CAS 和账号/分组事务回滚。
- 合成网关与真实隔离 SQLite 账号库联调：原请求幂等与归档重放后仍为 1 次 OAuth 提交、1 个新账号；JWT/结果身份不一致和已有账号覆盖被拒绝，外部 401 不触发管理员退出。
- 前端类型检查、8 个相关辅助用例、3 个浏览器用例和生产构建通过。浏览器覆盖预检失效、展开详情更新、归档、跨页面原 ID 查询、秘密不落浏览器存储及窄屏/键盘操作；桌面与窄屏截图已检查。
- Windows/Linux amd64 主程序构建通过，并核对两者嵌入最新 OAuth 页面资源。首次管理包临时二进制启动被 Windows 拒绝，拆分编译和执行后用同一测试入口验证通过；未更改系统安全设置。
- 未运行全套/race/负载测试、真实 OAuth/代理或 PostgreSQL 集群验收；未推送或部署。
