# Console 自更新设计

> 目标:Console 以 `nohup ./mooncell-console &` 这类**无监管**方式部署在 Linux 上时,管理员能在
> 前端上传新的 Console 二进制,由 Console **替换自身并就地重启**完成升级——无需 ssh、无需 systemd。
>
> 适用场景:离线 / 内网 / 单机,Console 由 nohup(或裸进程)拉起,没有 systemd/supervisor 守护。

---

## 1. 背景:范式已被 Agent 验证

Agent 早已实现完全相同的机制(`agent/update_api.go` 的 `selfUpdate` + `agent/main.go` 的
`--version`/`--selftest`),正是为「纯 nohup 无监管」设计:

```
收新二进制 → 校验 sha256 + ELF 架构 + --selftest + --version
          → 备份当前为 <exe>.old → 原子替换自身(os.Rename 覆盖运行中的可执行文件)
          → 延迟 syscall.Exec 用新二进制就地重启(同 PID,端口在新映像启动时重新 bind)
```

`syscall.Exec` 替换的是**进程映像本身**,PID 不变、不依赖任何守护进程,所以 nohup 场景能重启。
Console 自更新 = 把这套搬到 Console,且**更简单**:Agent 是 Console 跨机推送二进制;Console 是
管理员**从浏览器直传**给本机,本地校验后替换自己,少一整条跨机传输链。

> Console 二进制 `//go:embed all:dist` 内嵌了前端,故**换一个二进制 = 前后端一起升级**,
> 正合"上传二进制更新自己"的诉求。

### 可直接复用的现成资产

| 资产 | 位置 | 复用方式 |
|---|---|---|
| ELF 架构识别 | `console/agentupdate.go` `elfArch()` / `agentArchELF` | 校验上传包架构与本机 `runtime.GOARCH` 一致 |
| 自更新全流程范式 | `agent/update_api.go` `selfUpdate()` | 几乎照抄(去掉跨机部分) |
| `--version`/`--selftest` 范式 | `agent/main.go:33-47` | 移植到 `console/main.go`(selftest 不绑端口) |
| 多部分上传 + sha 流式校验 | `console/agentupdate.go` `uploadAgentBinary` | 同款落临时文件 + `io.MultiWriter(out, h)` |
| admin RBAC | `console/auth.go` `requireRole("admin")` | 端点鉴权 |

---

## 2. 总体流程

```
                          浏览器(admin)
                               │ ① 上传 mooncell-console 新二进制(multipart)
                               ▼
   ┌─────────────────────── Console(运行中)───────────────────────┐
   │ POST /api/console/self-update (requireRole admin, selfUpdateMu) │
   │   ② 落临时文件 <exe>.new + 流式算 sha256                         │
   │   ③ 校验:sha256(可选) + ELF 架构==runtime.GOARCH               │
   │   ④ 自检:<exe>.new --selftest 退出码 0(加载当前 config,不绑端口)│
   │   ⑤ 版本:<exe>.new --version 读取(可选与声明版本核对)          │
   │   ⑥ 备份当前 → <exe>.old                                        │
   │   ⑦ 原子替换:os.Rename(<exe>.new, <exe>)                        │
   │   ⑧ 写 200 响应(restart=self-exec)→ flush                      │
   │   ⑨ 延迟 500ms → syscall.Exec(<exe>, os.Args, env) 同 PID 重启   │
   │        exec 失败 → 把 <exe>.old 回滚回 <exe>(保下次重启可用)     │
   └────────────────────────────────────────────────────────────────┘
                               │ ⑩ 新映像重新 bind 端口、openDB、跑迁移
                               ▼
                     前端轮询 /api/console/info 直到版本变为新版
```

任一校验不过(③④⑤)即**保持旧版**、删 `<exe>.new`、返回错误——无损。

---

## 3. 详细设计

### 3.1 CLI flags(`console/main.go`)

在 `main()` 最前面、`loadConfig`/`openDB` **之前**加轻量 flag(与 Agent 对齐):

- `--version` / `-v`:打印 `consoleVersion` 后退出。
- `--selftest`:**只** `loadConfig("config.toml")`(校验配置可解析 + 通过 `unsafeConsoleConfigReason`
  安全闸),打印 `ok <version> <goos>/<goarch>` 后退出。
  - **关键约束**:selftest **不绑端口、不 `openDB`**——运行中的实例持有端口与 SQLite 文件,
    新二进制 selftest 若绑端口/开库会与之冲突,把好包误判成坏包。只验"能跑 + 接受当前配置"即可。

新增 `var consoleVersion = "dev"`(构建时 `-ldflags "-X main.consoleVersion=vX.Y.Z"` 覆盖,与
`agentVersion` 一致)。

### 3.2 自更新端点

```
POST /api/console/self-update     requireRole("admin")
GET  /api/console/info            requireAuth   → {version, os, arch}
```

- `/api/console/info`:供前端展示当前版本 + 升级后轮询确认重启完成。
- 自更新限 **admin**(上传"将被当作自己执行"的二进制是强能力;但 admin 本就能推 Agent 包、部署
  任意制品,信任面不扩大)。

### 3.3 handler(新文件 `console/selfupdate.go`)

镜像 `agent/update_api.go`,要点:

1. **并发串行**:`selfUpdateMu sync.Mutex`(加到 `api` 结构),`TryLock` 失败回 409
   "已有自更新进行中"。固定临时路径 `<exe>.new` 与"备份→替换"是非原子临界区,必须串行。
2. **上传上限**:`http.MaxBytesReader` 截断(Console 二进制约 15–40MB,给 256MB 余量,复用
   `selfUpdateMaxBytes` 同量级常量)。
3. **定位自身**:`os.Executable()` + `filepath.EvalSymlinks`(解析软链到真实路径)。
4. **落盘 + sha**:流式 `io.Copy(io.MultiWriter(out, h), file)` 写 `<exe>.new`,算 sha256。
5. **校验链(fail-closed,任一不过即删 `.new` 保旧版)**:
   - sha256:若表单带 `sha256` 则比对(传输完整性)。
   - 架构:`elfArch(<exe>.new) == runtime.GOARCH`(空=非 linux ELF/不识别 → 拒,拦 Mach-O/PE/跨架构)。
   - 自检:`exec.CommandContext(10s, <exe>.new, "--selftest")` 退出码 0。
   - 版本:`<exe>.new --version` 读真实版本。**允许同版本**(见 §3.4)。
6. **替换 + 重启**:
   - `copyFile(exe, exe+".old", 0o755)` 备份当前(供手工回滚)。
   - `os.Rename(<exe>.new, exe)`(Linux 允许覆盖运行中可执行文件,旧映像用旧 inode 继续跑)。
   - 写 `200 {ok:true, version, restart:"self-exec", old:consoleVersion}` + `Flush()`。
   - `go func(){ time.Sleep(500ms); syscall.Exec(exe, os.Args, os.Environ()) }()`:
     先让 200 回到浏览器,再就地重启。
     - **exec 失败兜底**:把 `exe+".old"` rename 回 `exe`,日志告警——保证下次手动重启回到已知可用版本
       (nohup 无自愈网,这步是底线)。

### 3.4 版本策略:允许同版本

与此前「Agent 同版本号也允许更新」的决定一致(管理员常忘记改版本号):**不做"新版本必须 ≠ 当前版本"
的拦截**。`--version` 仅用于读取展示与"声明版本 vs 二进制自报版本"的可选核对(传错包防呆),不阻止同版本覆盖。

---

## 4. 前端设计

admin 页(建议放在 Agent 管理页底部,或新开「系统」页)加一张「Console 升级包」卡片,仿现有
「Agent 升级包」卡(`console/src/pages/Agents.jsx` 的 `AgentBinariesCard`):

- 顶部显示当前 Console 版本 + os/arch(读 `GET /api/console/info`)。
- 文件选择 + 「上传并升级」按钮 → `confirmDialog` 二次确认(文案明确告知**将就地重启、约数秒断连、
  在飞操作会中断**)。
- 触发后进入"升级中"态:轮询 `GET /api/console/info`,版本变为新版即 toast「已升级到 vX,重启完成」;
  超时(如 30s)未恢复则提示"未确认重启,请检查进程或用 `<可执行文件>.old` 手工回滚"。

> 复用已有 `confirmDialog`/`toast`/`Btn`/`Field` 原语;上传走 `multipart`,与 `uploadAgentBinary` 同款。

---

## 5. 与 Agent 自更新的异同

| 维度 | Agent 自更新 | Console 自更新 |
|---|---|---|
| 触发 | Console 跨机推送 | 浏览器直传本机 |
| 鉴权 | 共享 token | 会话 + admin 角色 |
| 校验链 | sha + 架构 + selftest + version | **同款** |
| 重启 | self-exec 同 PID | **同款** |
| selftest | 不绑端口 | 不绑端口 **且不开 DB** |
| 升级内容 | 仅 Agent 二进制 | 二进制(含内嵌前端)= 前后端一起 |
| 回滚 | `<exe>.old` 手工 | `<exe>.old` 手工 |

> 行为对齐两端,便于维护与运维心智统一。

---

## 6. 风险与边界

1. **重启瞬间约 0.5–2s 断连**:exec 替换映像时端口短暂未 bind,浏览器会闪断后自动重连。UI 须提示。
2. **在飞操作被切断**:正在跑的部署/还原 SSE、分块上传会话、巡检 goroutine 在 exec 时全部中止。
   建议在空闲窗口升级(与 Agent 同注意点)。**进行中的上传会话(`uploads` map)是进程内状态,重启即丢**,
   需用户重传——可在确认弹窗提示。
3. **DB 不在自更新范围**:只换二进制。新版本若改表结构,靠重启后 `openDB` 的
   `CREATE TABLE IF NOT EXISTS` / 迁移处理(与现有升级一致)。**自更新前建议管理员先备份
   `mooncell.db`**(尤其跨大版本)。
4. **无自愈网(nohup 特性)**:`--selftest` 闸 + `.old` 回滚是底线;新包跑不起来就保持旧版,
   exec 失败回滚磁盘二进制。
5. **架构锁定**:仅接受匹配本机 `runtime.GOARCH` 的 linux ELF;darwin 上跑的 Console(开发机)
   会拒绝任何上传(自更新是 Linux 部署特性),应给清晰错误文案。
6. **信任面**:admin-only;admin 已是最高信任(能推 Agent 包、部署任意制品),不放大边界。
   但仍是高权操作,建议进审计(`appendAudit("Console 自更新", "vA → vB", 结果)`)。

---

## 7. 测试计划

后端(`console/selfupdate_test.go`,纯函数 + handler 用 httptest):
- 架构不符(Mach-O/PE/错架构 ELF)→ 拒,保旧版,`.new` 被删。
- sha256 不匹配 → 拒。
- selftest 失败(伪造一个 `--selftest` 非零退出的桩)→ 拒。
- 版本核对:声明版本与二进制自报不一致 → 拒;**同版本 → 放行**(锁定 §3.4 决定)。
- 并发:`selfUpdateMu` 占用时第二次请求 409。
- `--version`/`--selftest` flag:`console/main.go` 加后,`go run . --version` 打印版本、
  `--selftest` 退出码 0 且不监听端口。

> exec/替换自身这类副作用强的步骤不在单测覆盖(与 Agent 一致),靠真机验证(§8)。

## 8. 真机验收(121.43.75.171 测试机,nohup 启动)

1. `nohup ./mooncell-console &` 起旧版,记 PID 与版本。
2. 前端上传新版二进制 → 确认升级。
3. 断连数秒后页面恢复;`/api/console/info` 版本变新;**PID 不变**(self-exec 同 PID)。
4. 构造坏包(错架构 / selftest 失败)上传 → 报错且版本不变、进程仍在跑。
5. 验证 `<可执行文件>.old` 存在,可手工 `mv` 回滚。

---

## 9. 实施步骤(约 80% 抄 Agent)

1. `console/main.go`:加 `consoleVersion` 变量 + `--version`/`--selftest` flag(selftest 只 loadConfig)。
2. `console/auth.go`:`api` 加 `selfUpdateMu sync.Mutex`。
3. `console/selfupdate.go`(新):`selfUpdate` handler + `consoleInfo` handler + `buildSelfUpdate` 校验,
   复用 `elfArch`。
4. `console/main.go`:注册 `POST /api/console/self-update`(admin)+ `GET /api/console/info`(auth)。
5. 前端:`api.js` 加 `consoleSelfUpdate(file, version)` / `getConsoleInfo()`;新增「Console 升级包」卡。
6. `console/selfupdate_test.go`:§7 用例。
7. `deploy/build-release.sh`:确认 Console 也带 `-ldflags -X main.consoleVersion=...`(与 agent 对齐)。
8. README / 本文:补"Console 自更新"说明。

---

## 10. 开放问题(待确认)

- **前端入口位置**:放 Agent 管理页底部,还是新开「系统/关于」页?(倾向后者,未来放版本/DB 备份等系统操作)
- **是否自更新前强制提示备份 DB**:跨版本迁移风险——弹窗加一句提示即可,还是要求先点"已备份"?
- **`--selftest` 是否也校验内嵌 dist 完整性**:go:embed 是编译期保证,通常无需;保持最简(只 loadConfig)。
