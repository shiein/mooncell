# Mooncell

内网自动化部署平台 —— 离线 / 单机 / 内网交付环境的「上传即部署、自动备份、一键还原、在线看日志」控制台。

## 仓库结构

```
mooncell/
├── console/   Web 控制台:React + Tailwind 前端 + Go 后端,前端 go:embed 进单二进制
├── agent/     部署目标机上的常驻服务:Go 单二进制,执行落盘 / 进程起停 / 备份 / 日志流
└── docs/      方案文档与设计原型
```

- **console** 与 **agent** 是两个独立的 Go 模块,各自编译为单二进制,通过 HTTP + Token 通信。
  单机场景下可同机部署;架构上支持一台 Console 管多台 Agent。
- 详见 [docs/deploy-platform-design-v1.md](docs/deploy-platform-design-v1.md)。

## 当前进度

| 模块 | 状态 |
|---|---|
| Console 前端(8 个页面 1:1 还原)| ✅ 完成 |
| Console 登录后端(SQLite + bcrypt + httpOnly 会话)| ✅ 完成 |
| Agent 骨架(token 认证 + 能力自检 + 系统上报)| ✅ 完成(P0)|
| Console↔Agent 协议(代理 + 总览页接真实数据)| ✅ 完成 |
| Console 业务数据持久化(应用/部署/备份/文件柜/审计落 SQLite)| ✅ 完成(JSON 文档存储,重载不丢)|
| Agent go-binary Deployer + systemd Runner(部署→健康检查→自动回滚)| ✅ 完成(Ubuntu 真机验证)|
| Console↔Agent 部署代理 + 前端部署页接真实部署(go-binary)| ✅ 完成(后端真机验证;前端构建态)|
| Deployer:java-jar(复用 systemd Runner)| ✅ 完成(Ubuntu+JRE 真机验证:部署→健康→回滚)|
| Deployer:static-nginx(软链原子切换)| ✅ 完成(真机验证:切链→健康→回滚链)|
| Deployer:python(单文件入口 · python3 + systemd)| ✅ 完成(真机验证:部署→还原→日志→回滚闭环;多文件/venv 待增强)|
| Deployer:tomcat-war(容器托管 · 原子换 WAR + 清展开目录 + reload 钩子)| ✅ 完成(真机 stand-in 验证:部署→回滚→还原闭环;UI 已接入:WAR 目标路径 + 健康检查 + reload 开关,服务端映射白名单动作)|
| 部署日志 SSE 实时流(Agent 逐步推送 → Console 代理透传 → 前端实时呈现)| ✅ 完成(全链路增量到达验证;前端构建态)|
| 一键还原(列历史备份 + 用备份制品重跑部署流水线,还原前自动备份、失败自动回滚)| ✅ 完成(go-binary + java-jar 真机验证:同步/SSE 还原闭环、回滚连 unit 一起还原;前端构建态)|
| 应用运行时日志实时流(Agent 跟随 systemd journal → SSE → Console 代理 → 前端 tail+跟随)| ✅ 完成(go-binary 真机验证:直连/经代理双路、断开级联取消;前端构建态,失败回退模拟)|
| 真实操作审计落库(Console 代理部署/还原时据会话操作人 + Agent 实际结果服务端权威写审计)| ✅ 完成(真机验证:成功/回滚/还原三态正确入 SQLite,source=agent;前端真实操作改乐观显示不重复落库)|

| 离线安装(install.sh:两端装为 systemd 服务 + 自动生成共享 token 打通)| ✅ 完成(真机验证:装→登录→Console↔Agent 连通→升级/卸载)|
| 角色权限 RBAC(admin/operator/viewer)+ 用户管理| ✅ 完成(后端按角色强制鉴权 curl 验证;前端门控构建态)|
| 文件柜真实化(二进制落盘 + 提取码 + 公开直链 + 强制 attachment 下载)| ✅ 完成(上传/下载/凭码/删除/权限 curl 验证;前端构建态)|
| 多 Agent(注册表 + 代理按 ?agent= 路由 + 应用选目标机)| ✅ 完成(双实例真机验证路由;前端 Agent 管理页 + 新建应用选 Agent,构建态)|
| pm2 Runner(systemd 之外的进程托管,ecosystem 配置 + 回滚配置还原)| ✅ 完成(真机 stand-in 验证:部署→回滚还原 ecosystem 闭环)|
| pm2 应用日志流(`pm2 logs --raw`)+ python venv 解释器| ✅ 完成(真机验证:venv 解释器实际生效 + pm2 日志增量到达)|
| 安全/正确性加固(review 修复)| ✅ 完成(reload 钩子白名单、启动失败不再判成功、还原源防误删、审计仅服务端追加;Go 单测覆盖)|
| node Deployer(`node <script>` · systemd/pm2 · 自定义 node 路径)| ✅ 完成(systemd 真机验证;go/java/python/node 四类进程后端均支持两种 Runner)|
| 部署链路服务端化 + releaseId 幂等| ✅ 完成(真机验证:前端只交制品+version+releaseId,Console 据已存类型化配置生成 Agent 请求;同 releaseId 幂等跳过)|

实施路线见方案文档 §12(P0 → P3)。

## 制品格式(明确约定)

Agent 按**文件魔数**(非扩展名)自动判断单文件还是压缩包,压缩包统一**智能解包**:
若包内只有单一顶层目录(如 `myapp-v1/…` 整包包裹)自动去掉该层,散落文件则原样保留(过滤 `__MACOSX`/点文件)。

| 类型 | 接受的制品格式 | 落盘方式 |
|---|---|---|
| go-binary | 单个可执行二进制 | 原子替换 |
| java-jar | 单个 `.jar` | 原子替换 |
| python | 单文件 `.py` **或** 压缩包(.tar.gz/.zip,多文件 + 依赖) | 单文件→原子替换;压缩包→智能解包到应用目录,入口为 binPath |
| node | 单文件 `.js` **或** 压缩包(.tar.gz/.zip) | 同 python |
| static-nginx | 压缩包(.tar.gz / .zip) | 智能解包到带时间戳 release,原子软链切换 |
| tomcat-war | 单个 `.war` | 原子替换到 webapps |

多文件压缩包应用的**备份/回滚为整目录**(`app.tar.gz`),单文件为 `app`;还原按内容自动判断。
> 制品解包(tar.gz / zip)走 Go 标准库,无需系统 `unzip`;仅多文件应用的**备份打包**用到系统 `tar`(Linux 自带)。

## 第二轮 review 修复(已落地 + 验证)

- **pm2 日志/状态服务端派生**:Console 据已存应用 runner/agentId 转发 `runner=pm2`、路由目标机,不再信任前端参数(真机验证:不带 runner 参数也走 pm2 logs)。
- **回滚也用 processHealthy**:systemd/pm2 回滚路径不再用空健康检查直接判成功,改查进程态(单测 + 逻辑同前向)。
- **systemd unit 注入防护**:Name/User/Workdir/BinPath/Args/Env 等含换行/控制字符直接拒绝写 unit(单测覆盖)。
- **制品 sha256 强校验**:前端 Web Crypto 算 sha256 → Console 透传 expectedSha256 → Agent 部署前校验,不匹配直接失败(真机验证:错误 sha 被拦截)。
- **部署三态不坍缩**:success / rolledback / failed 三态如实落库与展示,failed 不再被写成"已回滚"。
- **node 单文件模型对齐**:artifactExt 改 `.js`(与 python 单文件一致),UI/Agent 不再期望/落盘错位。

## 第三轮 review 修复(已落地 + 验证)

- **P0 压缩包安全解包**:改用 Go 标准库解包,逐条校验路径在 dest 内、拒绝绝对路径/`..` 穿越/软硬链接(zip-slip),单文件 4GB 上限防炸弹。不再依赖系统 `unzip`。真机:恶意 `../escape.txt` 被拒、未写出 dest 外。
- **P0 多文件部署失败安全**:解到独立 staging 目录、校验入口后才原子切换旧目录;解包/校验失败旧目录与旧配置完好(单测 + 真机)。
- **releaseId 幂等隔离 + 并发安全**:记录按 `op/appId/releaseId` 命名空间(部署/还原、不同应用互不命中);同应用部署/还原串行锁。
- **sha256 强校验**:Console 服务端权威计算制品 sha 并下发(不信任客户端),Agent 拒绝格式非法(非 64 hex)与不匹配。真机:非法 sha 被拒。
- **真实运行态不前端伪造**:真实部署/还原后 pid/cpu/mem/uptime 置「待 Agent 查询」,不再写随机值。
- **生产空库不 seed mock**:`[demo] seed`(默认 false)关闭时空库即全真实;演示数据须显式开启。
- **Runner 与实现对齐**:UI 移除未实现的 nohup,进程类仅 systemd/pm2。

## 已知边界 / 待办(诚实声明)

- **配置保真**:✅ 已重构——真机部署/还原的 Agent 配置由 Console 据已保存的类型化应用配置在服务端生成,前端只提交制品 + version + releaseId,配置注入面关闭。env 已贯通(应用实体含 env 时透传)。
- **服务端权威状态**:✅ 真实部署/还原/启停后,应用 version/status/lastDeploy 全部由 Console 服务端权威落库(`applyAppRuntimeState` / `applyLifecycleState`,据 Agent 权威 done 与启停返回的真实状态);前端真实操作只做即时显示、不再 patch 落库,刷新后一律以服务端记录为准。release 由 `appendRelease` 权威落库;运行态(pid)由应用详情轮询 Agent `…/status`。
- **幂等隔离**:✅ Agent 本地按 `op/appId/releaseId` 命名空间去重 + 同应用串行锁;命中已成功记录时核对请求指纹(制品 sha + 落盘路径 + runner + 版本 + 类型),一致才返回缓存,复用于不同制品/配置则拒绝(防碰撞)。Console `deploys` 表主键亦为 `(op, app_id, release_id)` 复合键(旧单列库启动自动迁移),部署/还原、不同应用复用同一 releaseId 互不误命中。
- **日志能力**:✅ journald / pm2 logs 实时跟随 + 时间范围 gzip 下载 + 声明日志文件 tail(log_roots 白名单 + Console 按 logPaths 精确授权,越权 403)+ 前端日志源选择(进程 journal/pm2 与各声明文件切换);轮转由 tail -F / journald 处理。注:日志文件须为具体路径,不支持通配/~。
- **demo/真实混用**:✅ static/tomcat 前端已接真机部署(isRealType),真实操作走服务端权威落库;预检、启停、运行态均已真打 Agent。仅业务展示页的 seed 演示数据为展示用,且后端默认不再 seed——生产空库从零起即全真实(前端水合空库即清空 mock,不再显示演示应用)。
- **Deployer 完整度**:✅ static-nginx 还原(软链切回历史 release)、tomcat/static UI 真机部署接入(isRealType 全开)、python/node 多文件包、reload 钩子(static→nginx-reload / tomcat→tomcat-restart,服务端按类型映射白名单动作、前端只表意)、声明日志文件的前端 tail 选择(Console 按 logPaths 授权)均已完成。剩 requirements 自动安装待补。
- **文件柜**:✅ 二进制落盘 + 提取码 + 公开直链 + 凭码下载 + 匿名上传(`cabinet.anon_upload` 开关,免登录 POST /api/pub/cabinet)+ 过期自动清理(后台每小时删元数据 + 字节)。
- **stand-in 验证**:pm2 部署编排有假 pm2 集成测试(`TestRunDeployPm2Orchestration`,验证 stop/start/pid 调用 + 成功判定 + 制品落盘);tomcat-war 真机替身验证了文件替换/回滚。容器/pm2 的**运行时进程监管本身**是其各自职责、非本平台代码,需在已装 pm2/Tomcat 的环境另行验收。
- **运行态查询**:✅ 应用详情对真实进程类应用 10s 轮询 Agent `…/status`,展示真实 active/pid + CPU%/内存(Agent 用 `ps` 采集受管进程 RSS/CPU)。
- **python 依赖**:✅ 多文件 python 包含 `requirements.txt` 时,部署中用配置的解释器 pip 自动安装(venv 优先);失败即回滚,不放行缺依赖部署。生产需提供带 pip 的 venv 解释器。
- **新建应用预检**:✅ 真打 Agent `/api/precheck`——目标目录白名单 + 可写、端口空闲、运行时/Runner 可用;Agent 不可达时降级提示仍可创建。
- **前端 E2E**:✅ Playwright 无头浏览器跑真实 Console(go:embed 前端 + SQLite 空库)的关键流程——登录失败报错、登录→导航应用列表→打开新建向导、会话刷新保持、登出回登录页。一键 `pnpm test:e2e`(构建→起服务→跑→收尾)。
- **制品上传**:✅ 单次 multipart(≤16MB)+ 超 16MB 自动分块上传 + 断点续传(乱序回 409+nextIndex、重复块幂等、`MaxBytesReader` 控总量、过期会话清理);Console→Agent 仍单次 multipart。默认上限 1GB(`max_upload_mb` 可配)。
- docs/README 已随实现同步。

## 离线部署

```bash
# 构建机(联网):产出离线 bundle
ARCH=amd64 deploy/build-release.sh        # 或 ARCH=arm64(麒麟/UOS)
# → deploy/release/mooncell-amd64/{mooncell-agent, mooncell-console, install.sh}

# 目标机(内网/离线,root):拷入 bundle 目录后
./install.sh install      # 装为 systemd 服务,自动生成共享 token 打通两端
./install.sh upgrade      # 仅换二进制并重启,保留配置与数据
./install.sh status       # 服务状态与访问信息
./install.sh uninstall    # 卸载(默认留数据;--purge 连数据一并删)
```

> 前端可视化验证:部署弹窗已用无头 Chrome 截图核对真实渲染(含 go-binary 真机部署态),布局/类型自适应/上传校验态均正常。
>
> 还原边界:进程类(go-binary/java-jar/python/node)用历史备份制品重跑部署同款流水线;static-nginx 还原切软链回指定历史 release;tomcat-war 还原备份 WAR。三类还原均按 `op/appId/releaseId` 走幂等(重复提交不重复切换/reload),失败自动回滚。备份版本标签由制品旁车 `<binPath>.ver` 记录,确保"还原到 vX"对应的就是 vX 的真实制品。

## 快速开始(Console)

```bash
cd console
pnpm install
pnpm dev          # 前端 5173 + Go 后端 8787

# 生产单二进制
pnpm dist         # vite build + go build -o mooncell
./mooncell        # http://localhost:8787 · 默认 admin / jch@9388
```

详细说明见 [console/README.md](console/README.md)。
