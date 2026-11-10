# 数据集成（ETL）后续任务交接 Prompt

> 用法：把下面「---」之间的全部内容作为任务说明交给执行者（Devin）。

---

你将接手 Mooncell 仓库（`https://github.com/shiein/mooncell`）中**数据集成（ETL）模块**的后续开发。模块是 Kettle / Apache Hop 的现代化简化版，骨架、核心引擎和全部前端页面已经完成并经过真机验证。你的任务是把它做到可交付生产。

## 0. 开始前必读（按顺序）

1. `docs/etl-design-v1.md` —— **唯一权威设计**。任何与它冲突的实现都要先改文档并说明理由。
2. `README.md` 与仓库根目录的约定（单二进制、纯 Go、离线内网交付、国产化 UOS/麒麟、浏览器下限 Chrome 92）。
3. 已有代码：后端 `console/internal/etl/`，前端 `console/src/etl/`，接入点 `console/internal/consoleapp/{app,auth,config,db,selfupdate}.go`、`console/src/App.jsx`、`console/src/components/Shell.jsx`。
4. 分支：在 `feat/etl` 上继续工作。**每个逻辑改动单独提交并 push**，提交信息写「现象 → 根因 → 修复 → 验证」，中文。不要提交 `console/dist/`、本地 `config.toml`、`*.db`、`*.key`。

## 1. 硬性约束（违反任何一条都视为不合格）

- **不破坏既有业务**：应用部署、数据资源、服务器运维等模块的行为、接口、数据一律不改。ETL 只允许通过已有的接入点挂载。
- **纯 Go、无 cgo**：不得引入任何需要 cgo 的依赖（这是不支持 Oracle、不用 DuckDB 的原因）。新增 Go 依赖须为纯 Go、许可证宽松（MIT/BSD/Apache-2.0），并在提交信息里说明理由。
- **SQL 安全**：变量进入 SQL 一律走绑定参数（`${var}`）；文本替换只允许 `#{var}` 且值必须通过 `sqltext` 的标识符白名单。动态标识符必须用方言 `Quote`。禁止字符串拼接值。
- **凭据**：连接密码只以 `secret` 包加密落库，AAD 绑定连接 ID；永不回显前端、永不写日志/错误/审计；密钥丢失必须 fail-closed。
- **设计令牌**：前端只用 `console/src/index.css` 中的 CSS 变量和既有组件（`components/primitives.jsx`）。不得引入新色系、渐变、emoji 标签、大写字距标题、发光等装饰。视觉必须与现有 Console 是同一个产品，亮/暗主题都要检查。
- **Chrome 92 兼容**：不得使用 `structuredClone`、`Object.hasOwn`、CSS `:has()`、容器查询等 92 不支持的特性。
- **Console 的 SQLite 只有 1 个连接**：`store` 包里任何查询必须读完并关闭 rows 后才能发下一条语句，否则会自锁（`service_test.go` 用单连接复现这个约束，新增存储代码必须被它覆盖）。
- **不要过早宣称完成**：每项任务都要有能证伪的测试（坏输入、边界、失败路径、并发、取消），不只测 happy path。交付说明要写清楚已验证、未验证和剩余风险。

## 2. 本地开发与验证

- 本机 Go 环境默认 `GOOS=windows`，跑测试和构建本机二进制必须显式指定：`GOOS=darwin GOARCH=arm64 go test ./...`（Linux 构建机按实际架构）。
- 后端：`cd console && GOOS=darwin GOARCH=arm64 go test ./internal/etl/... ./internal/consoleapp/...`
- 前端：`cd console && pnpm install && npx vite build`
- 真机联调：用 docker 起**独立的临时**数据库容器（不同端口、自设密码，测完删除），不要动机器上已有的容器。在一个临时目录里放 `config.toml`（`[server] port`、`[admin]`、`[etl] enabled = true`），运行构建出的 Console 二进制。
- UI 检查：用项目自带的 Playwright（`console/node_modules/@playwright/test`）写脚本登录、截图、做交互；亮/暗两套都要截图检查。
- e2e 约定：侧边栏按钮只有中文 label，选择器用 `getByRole('button', { name: '数据集成', exact: true })`；跑 e2e 用 `GOOS=darwin GOARCH=arm64 sh e2e/run-e2e.sh`。

## 3. 当前已完成并验证的部分（不要重写，在其上扩展）

- **内核** `core`：8 种逻辑类型与唯一转换入口 `core.Convert`、不可变批次、泛型步骤契约 `core.Define[C]`、命名端口与错误输出、变量作用域链、Kettle 对齐的度量、结构化日志、`Meta.Order` 面板排序。
- **表达式** `expr`：基于 expr-lang，禁用内置函数，自有 50+ 函数目录（释义/示例/点选插入片段），精确小数运算符重载，中文字段名，编译期类型推导。
- **SQL 参数** `sqltext`：`${}` 绑定、`#{}` 白名单替换，跳过字面量、注释和 dollar-quote。
- **连接器** `conn`：PG / Kingbase / MySQL / 达梦方言 + 仅测试用 SQLite；连接池；设计期探测缓存；批量 INSERT / LOOKUP / UPDATE 构造；无时区时间按连接时区解释。
- **转换** `pipeline`：编译（结构、环、端口、类型推导）→ goroutine + 有界通道运行（背压、快速失败、提前结束排空、panic 隔离）→ 预览（不写库）。
- **作业** `workflow` + `actions`：命名端口、确定的汇合语义、失败是否被处理、并行、超时、重试、续跑；内置开始 / 运行转换 / 执行 SQL / SQL 取值 / 条件 / 设置变量 / 等待 / 成功 / 中止；HTTP、子作业、通知以 planned 形态注册。
- **步骤**：生成记录、表输入、字段选择、表达式、过滤记录、唯一行（哈希）、空操作、表输出、插入/更新。
- **服务层**：运行管理器（并发上限、定义级串行、SSE、日志限量落库）、调度（CAS 防重复、重叠/错过策略、中文释义）、HTTP API、自更新 drain 门禁。
- **前端**：工程列表、设计器（拖拽、连线、属性表单、自动校验、撤销重做、整理布局、历史版本、预览、实时运行叠加）、运行详情（甘特、步骤度量、日志）、调度、连接。
- **真机结论（PostgreSQL 18）**：典型场景「A 取 max → B 计数 → 条件 → 增量插入/更新」正确；插入约 18 万行/秒；**全量 UPDATE 约 1.1 万行/秒（逐行 UPDATE 是瓶颈）**。

## 4. 任务清单（按优先级；每完成一项就提交）

### P1-A 正确性与真机认证（最高优先）

1. **三库真机认证**：在 MySQL 8、达梦 DM8、KingbaseES 的临时容器上，跑通与 PG 相同的端到端场景。把每个库的测试写成带 build tag（如 `//go:build etl_live`）的集成测试，通过环境变量提供 DSN，默认不跑。重点核对：
   - 达梦：大结果集是否需要显式 fetch size 才能流式读取；NUMBER/DECIMAL 扫描出的 Go 类型；单语句参数上限（当前保守取 8000，需实测）；TIMESTAMP WITH TIME ZONE 的语义；列名大小写（未加引号时为大写）。
   - Kingbase：`ON CONFLICT`、information_schema 与 pg_catalog 查询的兼容性；如有差异，从 `postgres` 方言拆出独立实现。
   - MySQL：TINYINT(1)/BIT 映射为布尔、`unsigned`、DATETIME 与 TIMESTAMP 的时区行为（驱动 `Loc`）。
   结论写回 `conn/*.go` 顶部注释与设计文档 §7。
2. **时间语义统一**：确认每种方言下，无时区时间与带时区时间在「读出 → 表达式比较 → 写回」全链路一致（PG 已修，参考 `conn/dialect.go` 的 `normalizeCommon` 和 `dialect_test.go`）。补齐各方言测试。
3. **精确小数显示**：预览与日志里的 decimal 按字段 Scale 补齐尾零（如 3441.90），不改变内部值。
4. **作业续跑使用快照**：目前「运行转换」作业项读的是转换的最新版本。改为：作业运行开始时，把引用到的转换定义一起快照（写入运行记录），续跑和复盘都用快照。
5. **`toDate()` / `truncDay()` / `today()` 的结果类型**：目前推导为 timestamp，应为 date。在 `expr` 编译后按顶层调用修正 Kind，并补测试。
6. **预览参数**：设计器预览目前只用参数默认值。在预览栏增加参数输入（复用 `RunParamsDialog` 的字段逻辑）。

### P1-B 性能（千万级目标）

7. **批量 UPDATE**：插入/更新当前逐行 UPDATE（约 1.1 万行/秒）。实现两种加速并做成配置项：
   - (a) 目标表的关键字段有唯一约束或主键时，用原生 upsert：PG/Kingbase `ON CONFLICT DO UPDATE`、MySQL `ON DUPLICATE KEY UPDATE`、达梦 `MERGE INTO`；
   - (b) 通用路径：批内用 `UPDATE … SET c = CASE k WHEN … END WHERE k IN (…)`，或「临时表 + 一条 UPDATE … FROM」。
   语义必须与 Kettle「插入/更新」一致（含「值未变化时跳过」与「只插入不更新」）。目标：PG 上 100 万行全量更新 ≥ 5 万行/秒。
8. **压测工具**：新建 `console/internal/etl/bench`（build tag），覆盖 1000 万行的 PG→MySQL「表输出」与「插入/更新」两组，记录吞吐、峰值 RSS、提交间隔的影响。结果写入设计文档 §18。如果内存随行数增长，说明有泄漏，必须修复。
9. **表输出错误行退化**：连接了错误输出时，批次失败后退化为逐行写入，定位坏行并转到错误输出，而不是整步失败（见 `steps/table_output.go` 的 TODO）。逐行写入时每行单独提交（PG 中失败语句会使整个事务失效）。

### P2 组件（每个组件 = 步骤实现 + 注册 + 前端表单 + 单测，引擎零改动）

10. 转换步骤：`sort_rows` 排序（超出 `RunContext.MemoryBudget` 用 `TempDir()` 外部归并排序）、`unique_sorted` 去除重复记录（要求有序）、`value_mapper` 值映射、`update` 更新、`delete` 删除、`merge_rows` 合并记录 + `sync_after_merge` 数据同步（含删除，用于全量同步）、`stream_lookup` 流查询（info 端口）、`db_lookup` 数据库查询（带缓存）、`csv_input` / `csv_output`、`http_input`（GET/POST，分页，JSON path 拍平成行）。
    - 每个步骤都要声明 `Order`、`Keywords`（包含 Kettle 英文原名）、`Description`；有错误行可能的步骤设置 `ErrorPort`。
    - 前端表单放在 `console/src/etl/forms.jsx`（文件过大时按类别拆到 `forms/` 目录），用 `registerForm` 注册，风格与现有表单一致。
11. 作业项：`http`（请求结果写入变量）、`workflow` 子作业、`notify`（Webhook / 邮件，凭据同样加密存储）。把它们的 `Maturity` 从 planned 改为 stable。

### P2 体验

12. 画布：复制/粘贴（⌘C/⌘V，跨定义可用）、多选拖动后整体保存位置、注释（文档模型已有 `notes`）、连线右键菜单（停用、删除、插入步骤）、节点右键菜单。
13. 工程列表：按 `folder` 分组/筛选（字段已存在），列表支持按最近运行状态排序。
14. 运行详情：失败节点一键定位到设计器并选中该节点；错误输出行数可点开看样本（运行时采样前 N 条错误行，存入运行记录，有上限）。
15. 导入 / 导出：定义导出为 JSON 文件，导入时校验格式版本并映射连接（按名称匹配，缺失时让用户选择）。

### P3 平台

16. 权限：新增「数据集成开发者」角色或按定义授权（参照 `dataresource` 的授权模型和用户管理页），接口层强制校验，前端按权限隐藏入口。审计覆盖所有写操作。
17. `deploy/install.sh` 与升级流程：备份时把 `mooncell-etl.key` 和数据库一起处理；README 增加数据集成章节，说明密钥备份的重要性。
18. 并行副本（`copies > 1`，`RunContext` 已预留）、下推优化器（在 `pipeline.Plan` 层把「表输入 → 过滤 → 字段选择」合并成源端 SQL）。
19. e2e：在 `console/e2e/` 增加数据集成用例：新建连接（可用 SQLite 以外的真实库时跳过）→ 新建转换 → 拖入步骤 → 连线 → 校验通过 → 预览 → 保存 → 运行 → 运行详情显示成功；以及作业的条件分支、调度保存与 cron 预览。

## 5. 每项任务的完成标准

- 后端：`GOOS=darwin GOARCH=arm64 go test ./...` 全绿，`go vet` 无告警，新增代码有证伪性测试。
- 前端：`npx vite build` 通过；用 Playwright 截图检查涉及的页面（亮/暗），交互改动要有脚本验证。
- 真机相关的改动，附上所用数据库版本、数据量、结果数字。
- 设计变化同步更新 `docs/etl-design-v1.md`（§8 组件表的状态、§18 性能数据等）。
- 提交说明写清：已验证项、未验证项、剩余风险、兼容性与回滚方式（定义格式变化时必须提供 `Migrate`，并保证旧定义可读）。

## 6. 遇到以下情况先停下来询问

- 需要修改 ETL 以外的任何业务模块；
- 需要引入 cgo 或许可证不明确的依赖；
- 需要改变已持久化数据的格式且无法向后兼容；
- 设计文档与实际需求存在实质冲突。

---
