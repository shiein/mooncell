# 内网自动化部署平台方案(v1)

> 适用场景:离线 / 单机 / 内网交付环境,技术栈繁杂(Java、Go、Python、Node、前端静态),
> 以日志为主要调试手段。目标:上传即部署、自动备份、一键还原、在线看日志。

---

## 1. 总体架构

```
┌─────────────────────────────────────────────┐
│            Console(Go 单二进制)              │
│  React + Tailwind(go:embed) ←→ Go(net/http) │
│         SQLite(modernc.org/sqlite,纯 Go)    │
└──────────────┬──────────────────────────────┘
               │ HTTP + SSE(共享 Token)
               ▼
┌─────────────────────────────────────────────┐
│              Agent(Go 单二进制)              │
│  Deployer 插件  Runner 抽象  备份管理  日志流  │
│        本地状态:文件(_deploys 去重记录)     │
└──────────────┬──────────────────────────────┘
               ▼
   pm2 / systemd / tomcat 容器 / nginx 目录
```

> **实现说明(v1.1)**:Console 前端最初设想 Vue3 + Element Plus、后端 Node/Fastify;实际落地改为
> **React + Tailwind 前端 + Go 后端**,前端构建产物经 `//go:embed` 嵌入,Console 本身也成为单二进制
> (二进制 + `config.toml` + sqlite 即可部署)。下文 §9 技术选型、§10 目录约定已按实际实现订正;
> Agent 侧设计未变。
>
> **实现现状(v2,2026-06)**:真实 Agent 已落地并经真机验证——go-binary / java-jar / python / node
> 五类进程 Deployer(systemd / pm2 Runner)、static-nginx(软链原子切换)、tomcat-war(容器换 WAR);
> 部署/还原/启停/日志均走真实 Agent,Console 据已存类型化配置服务端生成 Agent 请求(关闭注入面)。
> Runner 仅 **systemd / pm2**(nohup 未实现,不暴露);实时流统一用 **SSE**(未用 WebSocket);
> 制品传输为**单次 multipart 上传 + sha256 强校验**(分块/断点续传未实现)。
> 仅业务展示数据(部分页面)仍可用 mock 种子,生产空库从零起即全真实。

- **Console**:Web 控制台。管理应用、触发部署、查看日志、备份还原、文件柜、登录。
- **Agent**:部署目标机上的常驻服务。执行实际的文件落盘、进程起停、备份、日志读取。
- 单机场景下 Console 与 Agent 可同机部署;架构上天然支持后续一台 Console 管多台 Agent。

### 为什么这样拆

| 决策 | 理由 |
|---|---|
| Agent 用 Go | 单静态二进制,无运行时依赖,交叉编译 amd64/arm64(UOS/麒麟友好),常驻内存占用小 |
| Console 用 SQLite | 离线单机零依赖;数据量级(应用数 × 部署记录)远到不了需要 PG 的程度 |
| Console/Agent 分离 | 部署动作需要 root 或特定用户权限,与 Web 服务隔离;日志流、文件操作贴近目标机 |
| 不下发任意 shell | 安全边界:Agent 只暴露有限的、类型化的 API,杜绝命令注入和误操作 |

---

## 2. 核心领域模型

```
User          登录用户(本地账号)
Agent         目标机上的 agent 实例(单机版只有 1 条记录)
Application   一个被部署的应用(如 "数据查询平台后端")
  └ DeployConfig   部署配置:类型 + 类型化参数(JSON)
  └ Release        每次部署产生一条记录(制品、状态、操作人、耗时)
  └ Backup         每次部署前自动产生的备份(指向备份目录 + 元数据)
CabinetFile   文件柜文件(上传者、提取码、大小、过期策略)
```

关键点:**DeployConfig 的参数不是自由文本,而是由部署类型的 JSON Schema 约束**。
前端根据 Schema 渲染动态表单,后端与 Agent 双重校验。

---

## 3. 部署类型(Deployer 插件)

每个 Deployer 定义三件事:**配置 Schema**、**流水线步骤**、**绑定的 Runner**。

### 3.1 类型清单与配置项

| 类型 | 关键配置项 | Runner |
|---|---|---|
| `java-jar` | jar 目标路径、JVM 参数、环境变量、启动用户、健康检查 URL/端口、日志路径 | systemd / pm2 |
| `tomcat-war` | WAR 目标路径(webapps 下)、健康检查、部署后是否 `systemctl restart tomcat` | tomcat 容器 |
| `go-binary` | 二进制目标路径、启动参数、工作目录、环境变量、健康检查 | systemd / pm2 |
| `python` | 解释器路径(支持 venv)、入口脚本、启动参数(支持 .py 或 .tar.gz 多文件包) | systemd / pm2 |
| `node` | 入口文件、pm2 进程名 / ecosystem 配置、node 路径(支持 .js 或 .tar.gz 多文件包) | pm2 / systemd |
| `static-nginx` | 目标目录、是否整目录替换(vs 仅 dist 内容)、部署后是否 `nginx -s reload`、nginx 二进制路径 | 无进程(可选 reload 钩子) |

> Runner 仅实现 **systemd / pm2** 两种;早期设想的 nohup + pidfile 未实现,UI 不暴露。

通用配置项(所有类型共有):应用名、备份保留份数(默认 5)、部署前/后钩子(**仅限白名单内置动作**,如 reload nginx、清理缓存目录,不是自由脚本)、健康检查(端口探活 / HTTP 200 / 进程存活,超时与重试次数)。

### 3.2 Runner 抽象

```go
type Runner interface {
    Start(ctx, cfg) error
    Stop(ctx, cfg) error      // 优雅停止 → 超时后 SIGKILL
    Status(ctx, cfg) (ProcStatus, error)
    LogPath(cfg) []string     // 该应用的日志文件集合
}
```

已实现的 Runner:

1. **systemd**(默认):Agent 按模板生成 unit 文件(`/etc/systemd/system/deploy-<app>.service`)→ `daemon-reload` → `systemctl start`。崩溃自动拉起、开机自启、journald 兜底日志;启停经 `systemctl start/stop`,状态查 `is-active` / 主 pid。
2. **pm2**:落 ecosystem.json + `pm2 start/stop`,以 `pm2 pid/jlist` 解析状态。Agent 启动时探测 pm2 是否可用,不可用则该 Runner 在 UI 置灰。
3. **tomcat 容器**:容器由运维长驻,平台只原子替换 webapps 下 WAR + 清展开目录 + 可选 `systemctl restart tomcat`,不直接起停容器进程。

> 早期设想的 **nohup + pidfile** Runner 未实现:pidfile 自管理不如交给 systemd/pm2 可靠,故收敛到这两种。

### 3.3 部署流水线(状态机)

每次部署是一个步骤序列,逐步执行、逐步记录、可中断、失败可自动回滚:

```
上传制品(单次 multipart + sha256 强校验)
  → 备份当前版本(制品 + 可选配置文件 → backups/{app}/{ts}/)
  → 停止服务(static 类型跳过)
  → 替换制品(原子操作:先落到 tmp,校验后 rename;静态目录用软链切换,见下)
  → 启动服务
  → 健康检查(N 次重试,每次间隔可配)
  → 标记成功 / 失败
失败且开启自动回滚 → 还原备份 → 重启 → 再次健康检查
```

每一步的 stdout/stderr 和耗时都落库,前端实时展示流水线进度(SSE)。

**静态文件部署建议用软链切换**而非直接覆盖:

```
/data/web/myapp-releases/20260612_1030/   ← 新版本解压到这
/data/web/myapp -> myapp-releases/20260612_1030   ← nginx root 指向软链
```

切换 = 改软链(原子),回滚 = 软链指回旧目录,秒级且无半成品状态。nginx root 配一次软链路径即可,无需 reload(除非配置变更)。

---

## 4. 备份与还原

- **触发**:每次部署自动备份;也支持手动备份。
- **内容**:当前制品(jar/二进制/dist 目录/war)+ 配置中勾选的额外文件(如 application.yml)。
- **存储**:`/opt/deploy-agent/backups/{app}/{timestamp}/`,附 `meta.json`(版本、sha256、关联 Release、操作人)。
- **保留策略**:按份数滚动(默认 5),可按应用配置;磁盘水位告警(Agent 上报磁盘使用率)。
- **还原**:选择某个备份 → 走与部署相同的流水线(停止 → 还原 → 启动 → 健康检查)。还原本身也会先备份当前版本,防止"还原错了回不去"。

---

## 5. 日志查看

- **来源**:DeployConfig 中声明的日志路径(支持通配,如 `logs/*.log`)+ Runner 自带日志(pm2 log 路径、journald)。
- **实时**:Agent 端 tail -F 语义(处理日志轮转),通过 **SSE** 推到前端(已实现;未用 WebSocket);支持暂停、关键字高亮过滤、最近 N 行回看。
- **离线排查**:按时间范围打包下载日志(Agent 端 tar.gz 后回传)。
- **部署日志**与**应用日志**分开两个入口,前者看流水线,后者看运行态。
- 安全约束:Agent 只允许读取该应用配置中声明的路径,路径规范化后必须落在白名单目录内,防穿越。

---

## 6. 登录与权限

- 本地账号:用户名 + bcrypt 密码,存 SQLite。首次启动向导创建管理员。
- 会话:JWT(短期)+ refresh,或服务端 session(单机更简单,推荐 session + httpOnly cookie)。
- 角色(够用即可,不过度设计):
  - `admin`:用户管理、Agent 管理、全部操作
  - `operator`:部署、还原、看日志
  - `viewer`:只读(看状态和日志)
- 审计:所有写操作(部署、还原、删除备份、文件柜删除)记审计表:谁、何时、对哪个应用、结果。

---

## 7. 文件柜

定位:内网临时文件中转(给现场同事丢个包、丢个 SQL 脚本)。

- **匿名用户**:可上传(限制单文件大小与总配额),上传后获得**提取码 + 直链**;只能凭提取码访问/下载自己的文件,看不到列表。
- **登录用户**:看到全部文件列表(上传者 IP/时间/大小),可下载、删除、设置过期。
- **过期策略**:默认 7 天自动清理(可配),Agent/Console 定时任务清扫。
- 存储直接落 Console 所在机磁盘目录(`/opt/deploy-console/cabinet/`),元数据进 SQLite。
- 安全:上传文件一律按二进制存储、下载强制 `Content-Disposition: attachment`,防止 HTML/SVG 被内网浏览器执行;文件名做规范化。

> 如果你的本意是"匿名可浏览一个公开区",在文件柜上加一个 `public` 标志位即可,登录用户可把文件设为公开,匿名能看到公开列表。这是个一行开关的差异,实现时定。

---

## 8. Console ↔ Agent 通信与安全

- **认证**:Agent 启动时生成/读取 token(config.yaml),Console 添加 Agent 时录入;所有请求带 `Authorization: Bearer`。内网环境这层够用;若有要求可加自签 mTLS。
- **方向**:Console 主动调 Agent(单机/固定 IP 内网,不需要反向注册那套);Agent 提供:
  - `POST /api/apps/{id}/deploy[/stream]`(制品随 multipart 一并上传)、`POST /api/apps/{id}/restore/stream`、`POST /api/apps/{id}/lifecycle`(启停)、`DELETE /api/apps/{id}`(下线)
  - `GET /api/apps/{id}/status`、`GET /api/system`(CPU/内存/磁盘)
  - `WS /api/logs/stream`、`GET /api/logs/download`
  - `GET /api/backups`、`DELETE /api/backups/{id}`
- **制品传输**:当前为单次 multipart 上传 + sha256 强校验(Console 服务端权威计算,Agent 强校验,格式非法即拒)。分块上传 + 断点续传仍是**待实现**项(大 war 包/不稳带宽场景)。
- **幂等**:deploy 请求带 releaseId,Agent 对同一 releaseId 重复请求直接返回当前状态,防止前端重试导致双部署。
- **白名单原则**:Agent 没有"执行任意命令"接口;钩子只能从内置动作列表选择。

---

## 9. 技术选型汇总

| 层 | 选择 | 备注 |
|---|---|---|
| 前端 | React 18 + Vite + Tailwind | 原型视觉由一套自定义 CSS 设计令牌(CSS 变量 + 语义类)决定,Tailwind 关闭 preflight 接入;动态表单用 JSON Schema 自渲染(配置项种类有限,自写更可控);构建目标 chrome92,兼容低版本浏览器 |
| Console 后端 | Go 1.22+ + 标准库 `net/http`(1.22 方法路由) | 单二进制、零运行时依赖;前端经 `//go:embed all:dist` 嵌入,运行时从内存映像直接服务静态资源 |
| DB | `modernc.org/sqlite`(纯 Go,无 CGO) | 无 CGO 利于交叉编译与单文件部署;备份 = 拷文件;写串行,连接数限 1 |
| 会话/认证 | 服务端 session + httpOnly cookie + bcrypt | 见 §6;token 由 `crypto/rand` 生成,落 `sessions` 表 |
| 配置 | TOML(`github.com/BurntSushi/toml`)| `config.toml`:监听地址/端口、db 路径、会话 TTL、默认管理员;缺省字段用内置默认值 |
| Agent | Go 1.22+,chi 或标准库 mux | 静态编译,`GOARCH=arm64/amd64` 双发;本地状态 bbolt |
| 进程内调度 | Console:Go ticker(清理过期文件等);Agent:自带 ticker | 不引入 Redis/BullMQ,单机没必要 |
| 打包交付 | Console:`vite build` → `go build` 产出**单二进制**(前端已嵌入),配 `config.toml` 即可跑;Agent:单二进制 + install.sh(注册 systemd) | |

---

## 10. 服务器目录约定

```
/opt/deploy-agent/
├── agent                  # Go 二进制
├── config.yaml            # token、监听端口、白名单根目录
├── data/                  # bbolt 状态
├── backups/{app}/{ts}/    # 备份
├── tmp/                   # 上传暂存(校验通过才移入目标)
└── units/                 # 生成的 systemd unit 模板副本

/opt/deploy-console/
├── mooncell               # Go 单二进制(前端已 go:embed 嵌入)
├── config.toml            # 监听/db 路径/会话 TTL/默认管理员
├── mooncell.db            # SQLite(首次启动自动创建)
└── cabinet/               # 文件柜存储
```

---

## 11. 初始化与日常流程

**首次初始化向导(Console 第一次打开):**
1. 创建管理员账号
2. 添加 Agent(地址 + token,连通性测试)
3. 创建第一个应用:选部署类型 → 按 Schema 填配置(pm2 进程名 / jar 路径 / 静态目录…)→ Agent 端预检(路径是否存在、端口是否占用、pm2/java 是否可用)→ 保存

**日常部署:**
上传制品 → 自动走流水线 → 实时看进度 → 成功/失败(失败可一键回滚)。

---

## 12. 实施路线

| 阶段 | 内容 |
|---|---|
| **P0** | Console 骨架 + 登录;Agent 骨架 + token 认证;`java-jar`(systemd)与 `static-nginx` 两个 Deployer;上传→部署→健康检查闭环;部署日志展示 |
| **P1** | 自动备份 + 一键还原;应用日志实时流(WS)+ 下载;pm2 Runner(覆盖 node/java/python) |
| **P2** | tomcat-war、go-binary、python Deployer;系统监控面板(CPU/内存/磁盘);审计日志 |
| **P3** | 文件柜;多 Agent 管理;角色细化;离线安装包打磨(install.sh、升级脚本) |

P0 把"一种后端 + 一种前端"的最短路径打穿,流水线、Runner、Schema 三个抽象立住之后,P2 的新增 Deployer 都是填表式工作量。

---

## 13. 风险与边界(提前想清楚)

1. **Agent 运行用户与权限**:写 systemd unit、改 nginx 目录通常要 root。建议 Agent 以 root 跑但 API 面极小(白名单原则就是为这个);或 sudoers 精确授权。交付文档里必须写清楚。
2. **pm2 是环境依赖**:目标机没装 node/pm2 时 pm2 Runner 不可用。Agent 启动自检并上报能力清单(java? pm2? nginx? systemd?),前端按能力清单过滤可选 Runner——这比部署时才报错体验好得多。
3. **端口/进程冲突**:历史遗留的手工 nohup 进程可能占着端口。预检阶段做端口探测,冲突时给出 pid 与命令行,让操作者决定,**不要自动 kill 非托管进程**。
4. **磁盘水位**:备份 + 文件柜 + 日志都是吃磁盘的,Agent 上报水位,低于阈值禁止部署并告警。
5. **日志轮转**:tail 实现必须处理 rename/truncate(用 fsnotify + 重开文件),否则现场看着看着日志"断流"。
6. **大文件上传**:内网 web 上传 1-2GB war/dist 很常见,分块 + 续传是必需品不是优化项。
