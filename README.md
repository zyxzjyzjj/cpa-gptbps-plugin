# CPA Codex BPS

为 CLIProxyAPI 提供 `codex-bps` provider 的原生插件。通过 BPS WebSocket 接口执行 Responses 请求，复用当前 GPT-BPS 的授权、任务池、上下文、客户端工具和事件转换逻辑。

```text
Responses / Chat Completions 客户端
  → CPA 原生路由、凭据选择与协议转换
  → codex-bps 原生动态库
  → 原 GPT-BPS 任务池与请求转换
  → BPS WebSocket
  → CPA 原生逐请求统计 / usage queue
  → CPA-Manager-Plus
```

## 商店安装

在 CPA 的第三方插件商店来源中导入：

```text
https://raw.githubusercontent.com/zyxzjyzjj/cpa-gptbps-plugin/main/registry.json
```

然后安装 **Codex BPS**，开启全局插件开关。宿主必须是支持 CGO 插件的版本；管理接口的 `X-CPA-SUPPORT-PLUGIN: 1` 仅代表构建支持，还应确认 `/v0/management/plugins` 中该插件为 `registered: true`、`effective_enabled: true`。

配置示例见 [config.example.yaml](config.example.yaml)。插件 ID、库文件名和配置键均为 `codex-bps`。发布包采用 CPA 要求的命名格式，库位于 ZIP 根目录，并提供 `checksums.txt`。

当前验收宿主为 CPA v8 源码 `8522538`。插件仅使用 ABI 1 / schema 1 的能力字段；较旧宿主可加载并不保证拥有新版的用量报告、错误分类及流清理行为。

## 授权与导入

打开 CPA 插件菜单 **Codex BPS**，或同源页面：

```text
http://127.0.0.1:8317/v0/resource/plugins/codex-bps/panel
```

输入 CPA 管理密钥，导入独立 BPS OAuth JSON。凭据通过 `host.auth.save` 写入宿主，包含 `type: codex-bps`，不会接管原生 `codex` 账号。页面不存储密钥、不加载第三方脚本；凭据和授权操作均经过受管理密钥保护的接口。

推荐使用发布包中的桌面授权助手：

```powershell
.\bps-login.exe --out "$env:USERPROFILE\.bps-local\cpa-bps.json" --proxy http://127.0.0.1:7890
```

Linux/macOS 对应命令是 `./bps-login`，需要桌面 Chromium/Chrome/Edge。原始 BPS 浏览器实现会创建临时 profile，并在 BPS 注册回调发送到网络之前捕获它。目标文件必须不存在，成功后将 JSON 导入插件页面。`--browser-path` 可指定浏览器；`--cdp-page` 可接入已由调用者创建的本机授权标签页，结束只关闭该页。

也可以在页面创建 PKCE 链接并提交完整回调地址，或选设备码。部分工作区禁用设备授权，此时需要浏览器模式。浏览器手动回调受 BPS 页面跳转行为影响，推荐桌面助手的自动拦截方式。

流程参考 CPA 正常 Codex 的随机 state、S256 PKCE、授权码交换、轮询、持久化与刷新生命周期，但 BPS 必须保留自己的注册参数：

- BPS client ID：`app_fnr0pYvVwwFDocDumLG3H2Bp`。
- BPS redirect：`https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback`。
- 原生 Codex 的 client ID、localhost 回调及其 RT 不能直接替换使用。

刷新仅使用原 BPS client ID。与独立 CLI 相比，插件增加 CPA 刷新生命周期适配：成功结果落入私有 receipt，遇到不确定结果保留 pending 标记并要求重新授权，防止自动重复消费可能已轮换的 RT。当前有效 AT 不会为了测试被强制刷新。

## 调用

模型必须是账号实际有权使用的 BPS 模型。默认配置暴露 `gpt-5.6-sol`，模型列表是配置声明，不能证明其他账号具有权限。若与其他 provider 重名，可在凭据上设置 CPA `prefix`，明确选中 BPS。

```bash
curl http://127.0.0.1:8317/v1/responses \
  -H "Authorization: Bearer YOUR_CPA_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.6-sol","input":"只回复 OK","stream":true,"reasoning":{"effort":"low"}}'
```

`stream: false` 会聚合原流的终态响应；`stream: true` 保持增量事件，工具事件仍按原核心在校验完成后发出。CPA 负责 Chat Completions 等客户端协议与 Responses 的转换。

每次请求应提供完整历史。工具第二轮携带第一轮的输出及 `function_call_output`，使用一致的工具定义；不支持 `previous_response_id`。

## 统计与 CPA-Manager-Plus

插件保留上游终态的完整 `usage`，由新版 CPA 的 `NewExecutorUsageReporter`、`ParsePluginExecutorResponseUsage` 和 `ObservePluginExecutorStreamUsage` 统一生成请求记录。插件不另报一份重复 usage，不估算或伪造缓存命中。

原生记录包含 provider、凭据索引/标签、模型、请求/trace 标识、时间、耗时、TTFT、成功/失败、输入/输出/推理/缓存 token 等宿主支持的字段。CPA-Manager-Plus 使用 `/v0/management/usage-queue?count=...` 消费 JSON 记录，归一化后持久化到 SQLite。启用 CPA usage 统计并保持 CPAMP 服务运行，避免超过队列保留时间；队列读取会消费记录。

CPA 可选的请求日志记录客户端请求/返回内容，可能包含敏感信息。BPS 上游是 WebSocket，宿主没有 WS HTTP callback，因此这里沿用原核心 transport；上游 HTTP 请求日志不会凭空产生。用量详情与原始上游抓包是不同的功能。

## 保留的行为与限制

| 功能 | 行为 |
| --- | --- |
| 文本、完整历史 | 复用原核心；请求之间不拼接历史 |
| function/custom tools | 转为原 OfficeJS 兼容 envelope，由客户端执行；两种工具均已通过真实两轮测试 |
| tool_choice | 沿用原核心，仅支持 `auto` / `none`；`required` 和强制指定工具返回 400 |
| BPS 托管搜索、file search、图像生成 | 原核心过滤/不支持，不宣称可用 |
| 图片输入 | HTTPS URL 已实测；data/base64/file ID 需要额外 relay，插件没有启用 |
| 结构化输出 | 沿用原格式提示、校验和最终输出处理 |
| 缓存 | 保留实际 `cached_tokens` 及 cache-write 原始信息；是否命中由上游决定 |
| 并发/队列 | 默认每账号 4、全局等待队列 64、排队及启动预算 30 秒 |
| 状态 | 持久 `task.key`，独占 owner lock；每进程独立 state_dir |
| 限速 | 可选每账号 WS 拨号与 response.create 启动限速 |
| 请求时限 | 沿用 WS 消息空闲 120 秒；无总生成超时 |
| 取消 | 流式断连通过宿主流 callback 传播并已实测；非流式 C ABI 无独立取消信号，详见测试记录 |
| 配置更新 | 首次执行后改插件配置需重启，避免活跃 task 槽位发生重叠 |
| 代理 | 凭据 proxy_url → 宿主全局代理 → 插件后备代理；链式/认证代理可先转为本地 relay |
| quota 面板 | 无独立 quota provider；不伪装为原生 Codex quota 卡片 |
| compact / count_tokens | 明确 501，不返回假 token 数 |

## 构建与验证

需要 Go 1.26.0+ 和 C 编译器：

```bash
go test ./...
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o codex-bps.so .
python tools/package.py --version 0.1.0
```

Windows 输出 `.dll`，macOS 输出 `.dylib`。发布工作流在各平台原生编译、测试并生成安装包。真实上游验收、请求用量与已知限制记录在 [docs/WORKLOG.md](docs/WORKLOG.md)；单元测试不替代真实账号权限验证。

可在隔离 CPA 上重现实测；`--keys` 指向仓库外的私有 JSON（键为 `client` 和 `management`），`--out` 也必须在仓库外。读取 usage queue 会消费记录，只对独立测试环境使用：

```bash
python tools/live_check.py --keys /private/keys.json --out /private/bps-results --drain-private-queue
```

`--resume` 会复用同一目录中的已通过证据。该脚本使用合成提示，完成文本、缓存、function/custom tools 往返、JSON schema、Chat Completions 及不支持输入的检查；真实账号与网络失败仍应如实记录。

## 来源与许可

原项目基线 `203c716ccd1cad0452b5dc7535c46e6531e73af0`，协议组件作者 hloolx。完整许可与原项目的组件许可说明保留在 [LICENSE](LICENSE)、[NOTICE](NOTICE) 和 `licenses/`。源码哈希及本地授权修补的来源见 [docs/upstream-manifest.json](docs/upstream-manifest.json)。

这是第三方集成。BPS 内部接口、模型权限及授权政策可能变化。
