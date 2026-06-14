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
| Deployer:tomcat-war(容器托管 · 原子换 WAR + 清展开目录 + reload 钩子)| ✅ 完成(真机 stand-in 验证:部署→回滚→还原闭环;UI 接入待办)|
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

## 已知边界 / 待办(诚实声明)

- **配置保真**:✅ 已重构——真机部署/还原的 Agent 配置由 Console 据已保存的类型化应用配置在服务端生成,前端只提交制品 + version + releaseId,配置注入面关闭。env 已贯通(应用实体含 env 时透传)。
- **demo/真实混用**:非进程类(static/tomcat)在前端仍走模拟并写本地 release/backup 状态;新建应用预检为定时器模拟。生产模式应禁用模拟写库或明确标记演示。**待切分。**
- **业务实体写入**:release/backup/app 仍可经通用 `PUT /api/data/{kind}` 前端直写(审计已锁服务端只追加、部署结果走服务端 deploys 表幂等)。release/backup 应进一步拆为服务端权威 API。**待收口。**
- **stand-in 验证**:tomcat-war / pm2 用替身验证了平台职责(文件替换/编排/回滚),未验容器/pm2 运行时本身;node-pm2 为单测 + 与已验 python-pm2 同机制。
- 前端多为构建态 + 关键页无头浏览器回归,未做全量 E2E。

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
> 还原边界:目前仅 go-binary/java-jar(进程类)支持备份还原,复用部署同款流水线;static-nginx 历史版本以 release 软链保留,还原走另一套机制(后端已明确拒绝并提示)。备份版本标签由制品旁车 `<binPath>.ver` 记录,确保"还原到 vX"对应的就是 vX 的真实制品。

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
