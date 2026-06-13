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
| 部署日志 SSE 实时流(Agent 逐步推送 → Console 代理透传 → 前端实时呈现)| ✅ 完成(全链路增量到达验证;前端构建态)|
| 一键还原(列历史备份 + 用备份制品重跑部署流水线,还原前自动备份、失败自动回滚)| ✅ 完成(go-binary + java-jar 真机验证:同步/SSE 还原闭环、回滚连 unit 一起还原;前端构建态)|
| 应用运行时日志实时流(Agent 跟随 systemd journal → SSE → Console 代理 → 前端 tail+跟随)| ✅ 完成(go-binary 真机验证:直连/经代理双路、断开级联取消;前端构建态,失败回退模拟)|
| 真实操作审计落库(Console 代理部署/还原时据会话操作人 + Agent 实际结果服务端权威写审计)| ✅ 完成(真机验证:成功/回滚/还原三态正确入 SQLite,source=agent;前端真实操作改乐观显示不重复落库)|

实施路线见方案文档 §12(P0 → P3)。

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
