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

实施路线见方案文档 §12(P0 → P3)。

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
