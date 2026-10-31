# node-sealed-v1 输入合同

此配置只面向可信、合作的自包含 Node 单元测试，并且只开放在途共享。不提供对恶意 JavaScript 的安全隔离，也不证明测试不存在时间或随机性依赖。选择此配置表示调用方确认用例确定性、不依赖外部状态；一般项目继续使用 `node-v1` 且关闭复用。

## 可核验的项目依赖边界

- 源码、测试和 JSON 数据均来自同一封存 manifest，执行前独立物化，结束后复核原始声明输入。
- 相对路径/文件 URL 只能解析到该物化目录。只接受 `.cjs`、`.mjs`、`.json`；拒绝 `.js` 的祖先 package.json 类型推断、npm 包、`node_modules`、原生插件、远程及 data URL 模块。
- 内置模块仅允许 `node:test`、`node:assert`、`node:assert/strict`、`node:timers`、`node:timers/promises`，也接受对应的非 `node:` 名称（Node 本身支持时）。CommonJS、ESM 及动态 import 使用同一同步解析 hook。
- `process.getBuiltinModule` 使用同一白名单，拒绝 `process.binding` / `_linkedBinding`、fetch、WebSocket。文件、网络、子进程、worker 等模块不在白名单。
- Node permission model 允许读取物化目录与可信适配器目录，只允许写适配器事件文件，不授予子进程、worker、addon、WASI 或 inspector 权限。Reporter 的可信权限不授予项目模块。
- 子进程环境只保留固定 `TZ=UTC`、`LANG=C`、`LC_ALL=C` 和 Windows 必需的 SystemRoot。用户 NODE_OPTIONS、NODE_PATH、LD_PRELOAD、Python 环境等均不继承。
- 使用 `--test-isolation=none`，保持一个 Node 进程，`workers` 必须为 1。用例内部仍可声明并发；CPU/内存继续是准入估计。
- 执行根与可信适配器目录均解析为真实路径，再生成模块边界及 permission grants；macOS `/var` 别名和 Windows junction 不会导致合法相对模块被误拒绝，不扩大到父目录授权。

执行身份包含源码快照、Node 可执行文件字节、reporter 与 guard 摘要、实际环境、范围、超时、策略和资源。启动与结算都核验提供者环境。宿主 OS、系统运行库和 Node 自身是受信平台基础，不跨机器复用；不宣称将整个操作系统封存。

这条边界排除了本轮要解决的外部 npm/原生扩展依赖问题。它有意缩小适用范围：需要 fs、数据库、浏览器、npm 包或文件写入的测试会被拒绝或失败，不能自动改用通用执行配置。受限配置失败也不能在通用执行器重试后沿用原证据身份。

## 复用及结果

同一 trust domain、相同执行键且仍有订阅的活动任务可以共享；已完成结果不缓存。每个调用方保留独立 Request，原始 Job/Attempt/result_digest 不改写。取消一方不影响其他订阅；不同未提交字节产生不同快照和 Job。

标准 Node/pytest 仍要求 `reuse_policy: none`。本配置的模块限制不是 Focus 的执行授权或网络沙箱；`focus-broker` 与 `network: denied` 依然拒绝，禁止降级。

## 验证

`internal/runner/sealed_test.go` 覆盖真实 CJS/ESM 与已声明相对模块，拒绝外部包、fs、动态 fs import、原生 binding、网络和子进程能力。`internal/runtime/real_shared_test.go` 以资源屏障建立真实共享、独立取消、原始执行身份与脏快照分离。`examples/node-sealed` 可由 CLI 直接执行。
