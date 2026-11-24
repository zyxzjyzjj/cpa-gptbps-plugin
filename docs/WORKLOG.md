# 开发与验收记录

日期：2026-11-24（Asia/Shanghai）。本文件持续更新同一交付单元的结论。

## 来源与边界

- 当前 BPS 核心：GPT-BPS / bps-local `203c716ccd1cad0452b5dc7535c46e6531e73af0`。
- 迁入 `internal/{auth,basispoints,gateway,proxy,retryafter,sanitize}` 共 87 个源文件及测试；只对本模块 import 前缀统一替换。原始 SHA-256 见 `upstream-manifest.json`。
- 保留当前工作区已经验证的 `CaptureBrowserAuthorizationInTab` 本地修补，不读取任何私有 runtime 文件。
- CPA：`8522538`，v8、C ABI 1、JSON schema 10；逐一核对 pluginapi、rpc_schema、原生 Codex 登录、executor usage reporter、management/store 安装代码。
- CPAMP：`dcfa8bc`；核对 `httpqueue.Client.Pop` 与 `usage.NormalizeRaw` 的实际消费路径。
- 遵循官方开发规范：C shared library、JSON envelope、宿主 buffer 释放、受认证管理路由、打包到 ZIP 根目录、第三方 registry 与 release checksums。

## 有意增加的适配

- `gateway.StandaloneHandler.ServeWithCredentials`：共享原来的 admission/task/pacing，只接收 CPA 选中的请求级凭据和代理。
- Windows 原有 device 测试按权限语义修正：Unix 检查 0600；Windows 文件权限是 ACL，不能用 Unix mode 断言。实际保存与不覆盖行为没有改变。
- 非流式聚合原终态；流式使用 host.stream callbacks，断流/失败通过结构化 error_detail 报告，不让 CPA 将中途失败统计为成功。
- 输入声明 `openai-response` / `codex`，输出采用 CPA 的 `codex` 终态事件契约。非流式在宿主统计之后由原生 translator 解包回原 Responses 对象，避免 CPA 把顶层 prompt_cache_retention 误当成完整 usage 并记成 0。
- CPA `8522538` 会丢掉“原生跨格式 translator 返回字节不变”的合法 Codex→Responses 流事件。插件在 ABI 内部给自己的帧加临时标记，在 response_after_translator 中剥离，使宿主能区分已翻译与 fallback；标记不发到 BPS 或客户端，也不修改未标记的原生 provider 数据。
- 支持 CPA 通用 OAuth callback box 写入的 `.oauth-codex-bps-<state>.oauth` 文件；只读取当前内存会话的固定文件名，保留 state/PKCE 校验，消费后删除。
- CPA refresh 生命周期添加一次性交换、pending 标记及私有 receipt；不会自动重试不确定 RT 交换。
- `auth.parse` 不接管原生 Codex 文件；BPS 凭据通过宿主写入，使用 `type: codex-bps`。
- 授权助手完全沿用 BPS 的 PKCE、注册 redirect 和浏览器拦截；不替换为 Codex localhost redirect。

## Windows 本机真实上游验证

- Windows 原生 DLL 已被真实 CPA 加载，`registered: true`、`effective_enabled: true`；模型挂在实际 BPS 凭据上。
- 受认证 status endpoint 200，公共资源 `/v0/resource/plugins/codex-bps/panel` 200。浏览器检查了页面渲染、控件可访问性及控制台，无页面 JS error。未在浏览器中填写或暴露管理密钥。
- 使用已有、仍有效的独立 BPS 凭据，经过本机链式代理访问真实 BPS；未强制刷新 RT，未再次执行真实账号登录。

| 路径 | 真实结果 | Input / output | Cache read |
| --- | --- | --- | --- |
| Responses 非流式 | 200，`OK`，4.651s | 22,358 / 5 | 22,355 |
| Responses 流式 | 200，completed，3.237s | 22,358 / 5 | 22,355 |
| function tool 首轮 | 200，`sum_numbers`，参数 a=19、b=23 | 22,837 / 88（reasoning 15） | 0 |
| function tool 第二轮 | 本地相加回传 42，200，最终回答 42 | 22,942 / 5 | 22,834 |
| custom tool 首轮 | 200，`echo_literal`，原文 `BPS_TOOL_OK` | 22,811 / 59 | 0 |
| custom tool 第二轮 | 原文结果回传，200，最终确认 `BPS_TOOL_OK` | 22,898 / 8 | 22,808 |
| 严格 JSON schema | 200，最终 `{"ok":true}` 并验证 schema | 22,482 / 9 | 0 |
| Chat Completions | CPA 原生转换，200，`CHAT_OK` | 22,359 / 6 | 22,347 |
| HTTPS 图片 | 200，对公共示例图作出五词描述 | 22,621 / 12 | 22,347 |
| 客户端流式取消 | 收到首段文本后断开；usage queue 为 failed | 未获得终态 usage | 未估算 |
| 取消后恢复 | 200，`RECOVERED.`，5.030s | 22,360 / 8 | 22,347 |

首次冷请求的真实上游 cached 为 0、cache-write 为 22,355；后续相同前缀实际读到缓存。缓存命中依据 token 字段，不依据时延。最终常规验收脚本所有检查通过；成功项按 `--resume` 复用有效证据，未机械重复请求。

预期不支持的真实路径：`previous_response_id`、data/base64 图片、强制 function tool_choice 均为 400；Responses compact 与独立 count_tokens 均为 501。hosted tools 沿用原过滤逻辑，单元测试覆盖其行为；未把过滤后的文本生成算作 hosted-tool 成功。

验收中出现过一次上游 WS handshake `request_not_sent=true` / unclassified 失败，保留为真实失败记录；同一路径后续成功。没有放宽原核心的安全重试分类，也没有隐瞒链式代理的不稳定性。

## 请求详情与 CPAMP 验证

- 从真实 CPA `/v0/management/usage-queue` 获取记录，核对 `provider=codex-bps`、`executor_type=plugin:codex-bps`、auth_index、请求 ID、API Key 归属、耗时、TTFT、stream、reasoning、缓存读/写及失败字段。
- 修正适配后，流式与非流式的宿主统计均与响应 usage 一致。function 首轮 reasoning 15 和 cache-write 22,834 也由原生 reporter 保存。
- 以 Go overlay 在 CPAMP 原代码中执行 `usage.NormalizeRaw → store.InsertEvents → SQLite`，参考源码没有被修改。
- 16 条真实记录（8 成功、8 失败；包含验收期间的真实失败）成功归一化并入库，逐项 token/failure 完全一致；缓存读合计 112,699。相同记录第二次导入新增 0 条，SQLite 行数仍为 16。
- 后续图片、取消及恢复另产生 3 条记录，均保存于私有证据目录。取消发生在上游终态 usage 之前，记录 failed、token 0 表示“上游未报告”，不等于实际成本为零。
- 没有把 CPAMP 前端 UI、报价规则或 Codex quota 卡片作为已经完成的验证；兼容性结论针对实际队列消费、解析及 SQLite 持久化。

## 自动化检查

- 迁入的 basispoints/gateway/proxy/retryafter/sanitize 测试通过；Windows auth mode 断言修正后 auth 测试通过。
- 插件适配测试及 `go test -race . -count=1` 通过，包括两种输出、缓存字段、失败归类、原生 Codex 文件不接管、PKCE/state/redirect guard、CPA callback 文件、RT 成功 receipt 与不确定交换不重试、下游 callback 失败时关闭 WS 及关闭后拒绝新任务。
- 公共资源不能通过参数执行 host credential callbacks；敏感路由由宿主管理认证保护。源码扫描未包含本次私有 AT/RT、账号或代理凭据。

## 发布与商店验收

- 公开仓库：`https://github.com/zyxzjyzjj/cpa-gptbps-plugin`。目标仓库原为私有，经用户明确选择“改为公开，允许直接导入”后修改可见性。
- 源码标签 `v0.1.0`：`89fb75ead1a62c88f99e0fece0ffc948e442d471`。
- GitHub Actions run `45353732846`：Windows amd64、Linux amd64/arm64、macOS amd64/arm64 五个原生构建/测试全部成功，publish 成功。
- Release 提供 5 个平台 ZIP 和 `checksums.txt`，校验清单含 5 条 SHA-256。Windows ZIP SHA-256：`e74a846edbc89c66b23b74f122926c2a436d49c0920f6d8ebbef52e3a092a888`。
- 用不带 GitHub Token 的全新 CPA 实例，从公开 registry 发现插件：`auth_required: false`、`installed: false`，随后调用宿主原生商店安装接口。
- 首次安装返回 502；确认插件目录仍为空后重新安装，返回 200 / installed。没有替换成本地 DLL 来冒充商店安装。
- 商店下载并校验发布资产后，真实注册 `registered: true`、`effective_enabled: true`，`restart_required: false`。
- 单独匿名下载 Windows ZIP，再验 SHA-256、根目录 DLL/登录助手，并确认宿主安装 DLL 与发布 ZIP 内 DLL 的 SHA-256 一致。发布版 `bps-login.exe --help` 退出码 0。
- 通过发布版受认证 import 路由将真实 BPS OAuth JSON 交给 `host.auth.save`，200 / ok；模型随后可见。未带管理密钥的同一路由返回 401。
- CPA 原生 `codex-bps-auth-url` 返回 200，state 长度 43，BPS client/注册回调/S256 均正确；仅验证会话创建，随后取消，没有要求用户重复登录。
- 发布版真实流式生成返回 `STORE_OK` / completed，约 3.240 秒；input 22,359、output 6、total 22,365，cache-write 22,356、cache-read 0。该实例使用全新 task state，未把它的冷缓存记作命中。
- 发布版宿主 usage queue 正确产生 1 条成功记录，数字与上游一致，客户端未收到内部 wire marker。
- 商店安装后的详细数值汇总见 `validation-results.json`；发布 ZIP 内记录为构建时快照，商店验收的后续结果以 main 分支及 release 附加验证报告为准。

## 已知限制

- 单个非流式 native C ABI 调用没有独立的下游取消通知；上游仍会在终态、WS 空闲上限或插件关闭时清理。流式可通过 host stream 关闭感知取消。
- 登录链接生成和 PKCE guard 可以离线验证；设备授权是否开放由工作区决定。真实账号之前通过原核心浏览器拦截成功授权，本次不强制重新登录或轮换仍有效的 RT。
- 宿主没有 WS callback，因此 BPS 沿用原来的独立 WS transport。用量由宿主原生 reporter 统计；宿主 request logs 不包含伪造的 WS HTTP 上游流水。
- hosted tools、inline 图片、previous_response_id、强制 tool_choice、compact/count_tokens 不因接入 CPA 而获得支持。
- BPS 系统上下文开销约 22k input tokens，本次真实结果包含这部分，不能当作只有用户短提示的 tokens。
- Linux/macOS 的构建和测试通过不代表这些平台的真实 CPA 加载或账号授权已实测；本次真实宿主与上游环境为 Windows amd64。
