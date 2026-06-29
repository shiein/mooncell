# Codex 审查整改清单 v1

> 来源:Codex 对当前 main 分支的 review,经逐条拉代码验证后保留的**可行且合理**项。
> 每条均已核对 `file:line` 证据,给出根因、修复方案、影响面/回滚、验收方式。
> 标 ⚠️ 的为"部分成立"(底层缝真实,描述细节略有出入)。
>
> 验证日期:2026-06-29 · 验证方式:逐文件读源码 + 关键正则/返回值实证。

## 执行顺序总览

按"真实可利用性 + 修复成本"排序,不按 Codex 原 P1/P2 标签:

1. **T1** 制品库部署参数传错(功能直接坏,一行修)
2. **T2** pm2Name denylist + **T3** externalBind 只认 loopback(两个真安全面,改动小)
3. **T4** pm2 接管复校 deploy_roots + **T5** 部署锁扩容到启停/下线 + **T6** 自更新 busy 门禁(纵深 + 竞态,可与 **T7** upload truncate 一批)
4. **T8/T9** 前端状态三态化(Topbar 假在线 + 失败伪装)
5. **T10** nohup 启动用户文档化 + PM 的备份/任务中心(后续迭代)

---

## T1 · 制品库部署参数错位(功能不可用)【最高优先】

- **严重度**:高(整条"从制品库部署"路径不可用)
- **证据**:`console/src/lib/api.js:361`、`console/src/components/pipeline.jsx:370`

### 根因
`deployViaAgentStream` 签名第 7 位才是 `artifactId`:

```js
async function deployViaAgentStream(appId, version, releaseId, file, onEvent, onUpload, artifactId)
```

调用处只传到第 6 位,把 `pickedArt.id` 落进了 `onUpload` 槽,`artifactId` 永远 `undefined`:

```js
const res = await deployViaAgentStream(app.id, version, releaseId, realFile,
  (type, data) => {...}, pickedArt ? pickedArt.id : null);   // ← 第6个参数
```

后续 `if (artifactId)` 永远 false → 走 file 分支,而选制品时 `realFile` 为 null → `fd.append('artifact', null)`,提交空制品。

### 修复
把 `artifactId` 放到第 7 位(`onUpload` 显式给 `undefined`,或后续接真实上传进度回调):

```js
const res = await deployViaAgentStream(app.id, version, releaseId, realFile,
  (type, data) => {...}, undefined, pickedArt ? pickedArt.id : null);
```

> 可选更稳:把 `deployViaAgentStream` 尾部参数改 options 对象 `{ onUpload, artifactId }`,杜绝位置错位再犯。

### 影响面 / 回滚
- 仅前端调用约定,不动后端。回滚 = 还原这一行。

### 验收
- 选制品库已留存制品 → 部署成功,Agent 收到非空 `artifactId`(后端 `prepareDeploy` 走制品库分支)。
- 上传新文件部署仍正常(回归)。

---

## T2 · pm2 生命周期过度信任 pm2Name(爆炸半径)

- **严重度**:高(`pm2 stop/delete all` 会打到所有 pm2 进程)
- **证据**:`agent/deploy_api.go:138`(`pm2NameReq`)、`agent/deployer.go:657`(`containerNameRe`)、调用点 `deploy_api.go:210/281`、`agent/logs_api.go:52`

### 根因
`containerNameRe = ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`,`all`、纯数字 id(pm2 进程索引)都能过正则。`pm2NameReq` 只做正则校验即放行,于是:
- `pm2Name=all` → `pm2 stop all` / `pm2 delete all` 命中**全部** pm2 进程(含非 Mooncell 托管)。
- 纯数字 → 按 pm2 索引定位任意进程。

### 修复
`pm2NameReq` 在正则之外增加 denylist(命中即回退托管名 `deploy-<id>`,不喂给 pm2 argv):
- 拒 `all`(大小写不敏感)
- 拒纯数字(`^[0-9]+$`)
- 拒 `-` 开头(参数形态)

> 长期方案(Codex 建议的"Agent 侧绑定记录"):部署时记录 app→真实 pm2Name 映射,生命周期/日志只允许操作绑定过的名字。工作量中等,本期先上 denylist。

### 影响面 / 回滚
- 仅收紧校验,不改正常进程名行为。若用户真有进程名叫 `all`/纯数字(极罕见)会被拒——可接受,且这种命名本就危险。

### 验收
- `?pm2Name=all`、`?pm2Name=0` 的 status/lifecycle/undeploy/logs 请求 → 回退托管名或拒绝,不触达全局 pm2。
- 正常进程名(如 `my-api`)的接管启停/日志仍正常(回归)。

---

## T3 · externalBind 漏判内网具体 IP(认证面)

- **严重度**:高(绑到内网具体 IP 对外,却放行默认凭据)
- **证据**:`console/config.go:129`、`agent/config.go:70`

### 根因
`externalBind` 只把 `""/0.0.0.0/::/[::]` 判为对外,绑到 `192.168.x.x`、`10.x.x.x` 这类具体内网 IP 时返回 false → `unsafe*ConfigReason` 直接放行 → 仍可用默认管理员密码 / 默认 Agent token 对外启动。

### 修复
反转判定:**只有 loopback 才允许默认凭据**,其余一律视为对外。

```go
func externalBind(addr string) bool {
    s := strings.TrimSpace(addr)
    if s == "" { return true }            // 空 = 监听所有网卡
    ip := net.ParseIP(s)
    if ip == nil { return true }          // 主机名等无法证明是本机 → 保守判对外
    return !ip.IsLoopback()
}
```

> 说明:`cfg.Server.Addr` 是纯 host(port 是独立字段),`net.ParseIP` 直接可用,无需拆 `host:port`。Console 与 Agent 两处 `config.go` 同步改(逻辑相同)。

### 影响面 / 回滚
- **兼容性**:之前绑内网 IP + 默认凭据能启动,改后会 `log.Fatalf` 拒绝启动。这是**预期的安全收紧**,但属行为变更——需在 README/升级说明里写明:绑非 loopback 地址必须改密码 + 改 token。
- 回滚 = 还原 `externalBind`。

### 验收
- `server.addr = "127.0.0.1"` + 默认凭据 → 正常启动(回归)。
- `server.addr = "192.168.1.10"` + 默认凭据 → 拒绝启动并打印原因。
- `server.addr = "192.168.1.10"` + 改过的密码/token → 正常启动。

---

## T4 · pm2 接管模式绕过 deploy_roots(纵深防御)

- **严重度**:中(利用需先控制目标机 pm2,但白名单边界应对称)
- **证据**:`agent/deploy_api.go:53`(prepareDeploy 校验下发 BinPath)、`agent/pm2.go:217-224`(接管后覆盖 BinPath 未复校)

### 根因
`prepareDeploy` 校验的是 Console 下发的 `cfg.BinPath`,但接管模式随后用 `pm2DeployTarget`(读 `pm2 jlist`)解析真实目标并 `cfg.BinPath = target`,**覆盖后没有重新 `withinRoots`**。后续 `placeArtifact` 直接写 target,可落在 deploy_roots 之外。

### 修复
`runDeployPm2` 中 `cfg.BinPath = target` 之前,对 target 绝对化 + 复校:

```go
target, perr := pm2DeployTarget(pm2ProcName(cfg), cfg.Type)
if perr != nil { /* 已有失败分支 */ }
abs, _ := filepath.Abs(target)
if !withinRoots(abs, a.cfg.Paths.DeployRoots) {
    add("校验目标", false, "接管目标不在 deploy_roots 白名单内: "+abs)
    res.Result = "failed"
    return res
}
cfg.BinPath = abs
```

### 影响面 / 回滚
- 仅接管模式;若用户 pm2 进程的真实路径本就在 deploy_roots 外,会被拒(符合白名单语义,引导用户把目标纳入 deploy_roots 或调整配置)。
- 回滚 = 删除这段复校。

### 验收
- 接管一个 `pm_exec_path` 在 deploy_roots 内的进程 → 部署正常(回归)。
- 接管一个 `pm_exec_path` 在白名单外(如 `/usr/bin/...`)的进程 → 拒绝,不写盘。

---

## T5 · 部署/还原锁未覆盖启停/下线(竞态)

- **严重度**:中(窗口窄,但会并发改同一 unit/pm2/nohup spec)
- **证据**:`agent/deployer.go:768`(`lockApp`)、`deployer.go:784`(仅 `runIdempotent` 用锁)、`agent/deploy_api.go:210`(`appLifecycle` 无锁)、`deploy_api.go:269`(`undeploy` 无锁)

### 根因
`lockApp(cfg.ID)` 只在 `runIdempotent`(部署/还原)里。`appLifecycle`(start/stop)与 `undeploy`(删 unit/pm2/nohup)裸操作,与正在进行的部署并发时可竞争同一资源。

### 修复
`appLifecycle` 与 `undeploy` 在拿到合法 `id` 后,统一进同一把应用级锁:

```go
defer a.lockApp(id)()
```

> 取舍:`lockApp` 是阻塞 `sync.Mutex`,长部署期间 stop 会等到部署结束——语义可接受。若希望"忙时快速失败",可给 `lockApp` 加 `TryLock` 变体,启停/下线拿不到锁就回 409「应用正忙」。本期先用阻塞锁(简单、与现有部署一致)。

### 影响面 / 回滚
- 行为变化:同一应用的部署与启停/下线由"可并发"变"串行"。这是修复目标本身。
- 回滚 = 删 `defer a.lockApp(id)()`。

### 验收
- 部署进行中对同一 app 发 stop/undeploy → 串行执行(等部署完),无 unit/pm2 状态错乱。
- 不同 app 的启停互不阻塞(回归,锁按 id 隔离)。

---

## T6 · Console 自更新无在飞任务门禁

- **严重度**:中(自更新 self-exec 会丢内存里的分块上传会话)
- **证据**:`console/selfupdate.go:39`(`selfUpdate`,仅 `selfUpdateMu`)、`console/auth.go:30`(`busy` map 已存在)、`console/upload.go`(`uploads` 会话在内存)

### 根因
`selfUpdate` 只有 `selfUpdateMu`(防并发自更新互踩),不检查:
1. `busy` 计数(部署/还原/启停在飞)
2. 活跃分块上传会话 `a.uploads`

500ms 后 self-exec 重启进程:**Agent 侧部署不受影响**(在 Agent 上跑),但 **Console 内存里的上传会话全丢**,临时文件成孤儿、用户续传 404。这是真损失点。

### 修复
`selfUpdate` 在校验通过、exec 之前增加空闲门禁,非空闲回 409:

```go
if a.anyBusy() || a.hasActiveUploads() {
    writeJSON(w, http.StatusConflict, map[string]string{"error": "有部署/还原/上传在进行中,请稍后再自更新"})
    return
}
```

需新增两个小 helper:
- `anyBusy()`:`busyMu` 下扫 `busy` map,任一 `>0` 即 true(已有 `isBusy(id)`,补全局版)。
- `hasActiveUploads()`:`uploadsMu` 下判 `len(a.uploads) > 0`。

> 前端自更新弹窗也应先查在飞任务给二次确认(与后端 409 双保险),非必须。

### 影响面 / 回滚
- 仅自更新入口收紧,不影响正常升级(空闲时照常)。
- 回滚 = 删门禁。

### 验收
- 有部署在飞时点自更新 → 409,不重启。
- 完全空闲时自更新 → 正常 self-exec 重启。

---

## T7 · 分块上传超限留脏前缀(latent 数据污染)

- **严重度**:中(需异常/恶意客户端触发,但临时文件会错位)
- **证据**:`console/upload.go:112`(MaxBytesReader)、`console/upload.go:118`(io.Copy 后未 truncate)

### 根因
单块超过 `remaining+1` 时,`io.Copy` 在 MaxBytes 报错前已 `O_APPEND` 写入部分字节,但代码 `return` 时**不推进 `Received`/`NextIndex`、也不 truncate**。客户端重试同一块 → 文件尾部多出脏字节,后续写入错位,最终制品损坏。

> 正常客户端末块恰为 `remaining`、不会触发;但临时文件污染是真 latent bug,且属"失败未回滚到已知good状态"。

### 修复
写入前记录已知 good 偏移,任一失败路径 truncate 回去:

```go
off := sess.Received
n, cerr := io.Copy(f, body)
if cerr != nil {
    f.Truncate(off)   // 回到块写入前的长度,保证幂等重试干净
    f.Close()
    // ... 原有错误响应
    return
}
```

> 注意 `f.Truncate(off)` 需在 `f.Close()` 之前;`O_APPEND` 下 Truncate 仅改长度,下次 append 从 off 继续。

### 影响面 / 回滚
- 仅失败路径行为,正常上传不变。回滚 = 删 truncate。

### 验收
- 构造单块超声明大小 → 返回 413,临时文件长度回到块前(`stat` 验证),重试该块后内容正确。

---

## T8 · Topbar 假"Agent 在线"(UX 失真)

- **严重度**:中(UX,误导运维判断)
- **证据**:`console/src/components/Shell.jsx:106`(硬编码绿色"Agent 在线")、`console/src/lib/agent.js:8`(`useAgent` 三态已实现却未接)

### 根因
`Topbar` 硬编码 `<Badge tone="success" dot>Agent 在线</Badge>`,而 `useAgent` 已提供 `online`(`null` 探测中 / `true` / `false`)却没接进来。

### 修复
`Shell`/`Topbar` 接 `useAgent().online`,按三态渲染:
- `null` → 灰色"探测中"
- `true` → 绿色"在线"
- `false` → 红色"不可达"

未知态不得用绿色。

### 影响面 / 回滚
- 纯前端展示。回滚 = 还原硬编码 Badge。

### 验收
- Agent 停掉 → 顶栏显示"不可达"(红);拉起 → 恢复"在线"。

---

## T9 · ⚠️ 失败伪装为空/旧状态(UX)

- **严重度**:中(UX,错误被折叠不可见)
- **证据**:`console/src/pages/AppDetail.jsx:45`、`console/src/lib/api.js`(`getAppStatus` 失败返回 null)、`console/src/pages/Users.jsx:18`(`setUsers(u || [])`)

### 根因(已实证,Codex 描述方向对、细节略偏)
- `getAppStatus` 失败返回 `null` → `AppDetail` 状态徽章 `live` 为 null 时**回退 `app.status`(stored,可能是 "running")**,而进程行却显示"查询中…" → 徽章与进程行打架,即"伪装"。
- `listUsers()` 失败返回 null → `setUsers(u || [])` 把错误折成**空数组**,页面显示"无用户",错误不可见、无法重试。

### 修复
统一 `loading / ready / error / stale` 四态,错误必须可见且可重试:
- 列表类(Users/Agents/System):区分"加载中 / 空 / 失败(带重试按钮)",不把 null 当空。
- 状态类(AppDetail):拉取失败时徽章不回退 stored running,显式标"状态未知 / 连接失败",可保留上次成功值但打 stale 标记(而非伪装实时)。

### 影响面 / 回滚
- 纯前端。建议抽一个通用 `useAsync`/四态壳子,避免每页各写一套。回滚 = 各页还原。

### 验收
- 断开 Agent/接口 500 → 对应页显示"失败 + 重试",不显示空列表或假 running。

---

## T10 · nohup 忽略"启动用户"(行为不对称)

- **严重度**:中(与 systemd `User=` 不一致,nohup 进程继承 Agent 用户,常为 root)
- **证据**:`agent/deployer.go:740`(systemd 透传 `User: cfg.User`)、`agent/nohup.go:26`(`nohupSpec` 无 user 字段)、`agent/nohup.go:224`(启动不降权)

### 根因
systemd 路径用 `cfg.User` 降权;nohup 的 `nohupSpec` 没有 user 字段,启动继承 Agent 进程用户。同一应用换 nohup runner 会"悄悄以 root 跑"。

### 修复(分两步,本期只做第 1 步)
1. **本期**:UI/校验层明确 **nohup 不支持启动用户**——选 nohup runner 时禁用/置灰"启动用户"输入,或后端校验 `cfg.User != "" && runner==nohup` 时给清晰错误。消除"以为降权了其实没有"的隐患。
2. **后续增强**:Agent 用受控方式降权启动(如 `su -s /bin/sh -c <cmd> <user>` 或 setuid),需处理 pidfile 归属、日志文件权限、用户存在性校验——平台敏感,单列任务。

### 影响面 / 回滚
- 第 1 步仅约束/提示,不改启动行为。回滚 = 移除提示/校验。

### 验收
- nohup runner 下设置启动用户 → 前端禁用或后端拒绝并说明。

---

## 附:PM 视角建议(后续迭代,非 bug)

按与现有架构契合度排序,均为合理产品料:

| 建议 | 可行性 | 备注 |
|---|---|---|
| Console 数据一键备份 + 自更新前强提示 | 高 | 与 **T6** 天然成对,`mooncell.db` 单文件下载即备份,优先做 |
| 全局任务中心 | 中 | `busy` map 已是雏形,升级为带类型/进度的在飞任务视图,顺便当 T5/T6 门禁数据源 |
| 部署前容量门禁(磁盘水位×制品大小×备份保留) | 中 | 需 Agent 磁盘指标(已有 system 接口)+ 阻断/二次确认 |
| 制品库增强(类型/版本/架构,按应用类型过滤 + 一键重部署) | 中 | 正常迭代,无架构障碍 |
| 应用克隆/模板(复制配置改名/端口/路径 + 强制预检) | 中 | 正常迭代 |
| 巡检事件中心(掉线/恢复从审计提炼为运维事件) | 中 | 需先定存储模型(注意审计已不完整,见健康巡检约束),别急 |
| 存储治理(artifacts/cabinet/backups 占用 + 未引用/过期提示) | 中 | 需数据沉淀,后置 |

---

## 变更影响速查(交付前核对)

| 任务 | 改前端 | 改后端 | 行为/兼容变更 | 需更新文档 |
|---|---|---|---|---|
| T1 | ✅ | — | 否 | — |
| T2 | — | ✅ | 收紧(罕见命名被拒) | — |
| T3 | — | ✅(console+agent) | **是**(绑非 loopback 须改凭据) | README/升级说明 |
| T4 | — | ✅ | 收紧(白名单外目标被拒) | — |
| T5 | — | ✅ | 是(同 app 启停/下线串行化) | — |
| T6 | (可选) | ✅ | 收紧(忙时自更新 409) | — |
| T7 | — | ✅ | 否(仅失败路径) | — |
| T8 | ✅ | — | 否 | — |
| T9 | ✅ | — | 否 | — |
| T10 | ✅ | (可选) | 否(仅约束 nohup+user) | UI 提示 |
