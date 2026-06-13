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
| Deployer(java-jar / static-nginx)+ 部署闭环 | ⏳ 待开发(需目标机)|
| Console 其余业务(应用 / 部署 / 备份 / 文件柜 真实 API)| ⏳ 待开发(当前为前端 mock)|

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
