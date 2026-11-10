# Mooncell 数据集成（ETL）设计 v1

> 状态：骨架已落地（分支 `feat/etl`），本文是后续实现的唯一权威设计。
> 定位：Kettle / Apache Hop 的**现代化简化版**——保留其经过 20 年验证的「作业 / 转换」两层模型，
> 在表达式、执行计划、失败恢复、观测性与交付形态上做现代化改造。

---

## 0. 目标与非目标

**目标**

1. 多种数据库（PostgreSQL、MySQL、达梦 DM8、KingbaseES；不含 Oracle）之间、以及后续 HTTP 接口之间的数据流动。
2. 浏览器内拖拽设计「作业（控制流）」与「转换（数据流）」。
3. 千万级单表同步：流式读、按批提交、内存有界。
4. 定时调度（cron），重叠/错过策略明确。
5. 详细、可追溯的运行记录：运行实例 → 节点 → 步骤度量 → 日志。
6. 扩展靠「新增一个步骤实现 + 注册 + 一个表单」，引擎零改动。

**非目标（v1 明确不做）**

资源库、集群 / Carte、元数据注入、JavaScript 脚本步骤、Oracle、分布式执行、多 Console 实例。

**与现有业务的关系**

完全独立的模块：独立的连接配置（不复用「数据资源」）、独立的表、独立的路由前缀 `/api/etl/`、独立的前端目录 `src/etl/`。
唯一的耦合点是 Console 进程级设施：登录鉴权、审计、自更新 drain 门禁、SQLite 句柄。

---

## 1. 术语

| 中文（界面） | 代码 | 说明 |
|---|---|---|
| 转换 | `pipeline` | 数据流：步骤之间流动**行**。 |
| 步骤 | `step` | 转换中的节点（表输入、表达式、插入/更新…）。 |
| 作业 | `workflow` | 控制流：作业项按结果分支执行。 |
| 作业项 | `action` | 作业中的节点（运行转换、SQL 取值、条件…）。 |
| 连线 | `hop` | 节点之间的有向边，挂在**命名端口**上。 |
| 端口 | `port` | 节点的命名输入/输出：`main`、`true`/`false`、`error`、`success`/`failure`。 |
| 连接 | `connection` | 数据库连接配置（密码加密落库）。 |
| 运行 | `run` | 一次作业或转换的执行实例。 |
| 行元数据 | `schema` | 有序字段描述（名称、逻辑类型、长度、精度）。 |

---

## 2. 总体架构

```
┌─────────────────────────── 前端 src/etl/ ───────────────────────────┐
│ 工程列表 · 设计器（画布 / 组件面板 / 属性检查器 / 底部抽屉）· 运行记录 · 调度 · 连接 │
└───────────────────────────────┬──────────────────────────────────────┘
                                │ /api/etl/*（JSON + SSE）
┌───────────────────────────────▼──────────────────────────────────────┐
│ api       HTTP handlers、DTO、权限                                    │
│ service   组装：仓储 · 连接池 · 运行管理器 · 调度器 · drain 门禁         │
├──────────────────────────────────────────────────────────────────────┤
│ workflow  作业：定义 → 编译(DAG) → 运行（节点状态持久化 · 重试 · 续跑）   │
│ pipeline  转换：定义 → 编译(Plan，类型推导) → 运行（goroutine + 有界通道）│
│ steps     内置步骤（编译期注册）     actions  内置作业项                  │
├──────────────────────────────────────────────────────────────────────┤
│ core      值类型 · 行元数据 · 批次 · 步骤契约 · 注册表 · 变量 · 度量 · 日志 │
│ expr      表达式语言（expr-lang/expr）+ 函数目录（释义 / 示例）           │
│ sqltext   SQL 中 ${绑定参数} / #{受控文本替换} 的解析                     │
│ conn      连接器：方言 · 类型映射 · 批量写 · 批量查找 · 元数据             │
│ secret    AES-256-GCM 凭据加密（独立密钥文件）                            │
│ store     SQLite 持久化（etl_* 表）                                      │
│ schedule  cron 解析（robfig/cron 仅用解析器）+ 自有触发循环              │
└──────────────────────────────────────────────────────────────────────┘
```

依赖方向严格自上而下；`core` 不依赖任何兄弟包。`steps` / `actions` 只依赖 `core`、`expr`、`sqltext`、`conn`。

### 2.1 源码布局

```text
console/internal/etl/
├── doc.go                 # 包总览，指向本文
├── core/                  # 零依赖的领域内核
│   ├── kind.go            # 逻辑类型与值转换
│   ├── schema.go          # Field / Schema / Row / Batch
│   ├── step.go            # 步骤契约、端口、注册表（泛型 Define）
│   ├── problem.go         # 校验问题
│   ├── vars.go            # 变量作用域链
│   ├── metrics.go         # 步骤度量（原子计数）
│   └── log.go             # 结构化日志接口
├── expr/                  # 表达式编译、类型推导、函数目录
├── sqltext/               # SQL 参数语法解析
├── conn/                  # 连接器与方言
├── secret/                # 凭据加密
├── pipeline/              # 转换：模型 / 编译 / 运行 / 预览
├── workflow/              # 作业：模型 / 编译 / 运行
├── steps/                 # 内置步骤
├── actions/               # 内置作业项
├── schedule/              # 调度
├── store/                 # 持久化
└── service.go, api*.go    # 组装与 HTTP
console/src/etl/           # 前端模块（与其它页面零共享状态）
```

---

## 3. 数据内核（`core`）

### 3.1 逻辑类型

转换内部只认 8 种逻辑类型；**数据库类型只在输入步骤（读出）与输出步骤（写入）两处与逻辑类型互转**，跨库类型问题被收敛到连接器。

| Kind | Go 表示 | 说明 |
|---|---|---|
| `string` | `string` | |
| `integer` | `int64` | |
| `number` | `float64` | 近似数值，**不用于金额** |
| `decimal` | `decimal.Decimal`（shopspring） | 精确十进制，NUMERIC/DECIMAL 默认映射到它 |
| `boolean` | `bool` | |
| `date` | `time.Time`（00:00:00，Location=UTC） | 只有日期 |
| `timestamp` | `time.Time` | 日期时间；时区按连接配置解释 |
| `binary` | `[]byte` | |

NULL 统一用 Go `nil`。`core.Convert(v, from, to, opts)` 是唯一的类型转换入口（字段选择改类型、表达式结果落地、写库前绑定都走它），转换失败返回 `*ConvertError`，由引擎决定进错误输出还是失败。

### 3.2 行元数据与批次

```go
type Field struct {
    Name      string
    Kind      Kind
    Length    int    // -1 = 未知
    Precision int    // decimal 总位数；-1 = 未知
    Scale     int
    Nullable  bool
    Origin    string // 产生该字段的步骤 ID（界面「字段从哪来」）
    NativeType string // 源库原生类型提示，仅展示 / 建议 DDL 用
}

type Schema struct{ /* 不可变：fields + name→index */ }
type Row   []any
type Batch struct {
    Schema *Schema
    Rows   []Row
}
```

**不可变批次规则（正确性关键）**：批次一经发送，发送方与所有接收方都**不得修改**其中的 `Row`。
需要改值的步骤用 `core.MapRows` / `row.With(...)` 产生新行（写时复制）。
这让「复制分发」可以零拷贝地把同一批次交给多个下游。

### 3.3 步骤契约

```go
// 用泛型把「配置结构」绑定到步骤实现，注册表对外只暴露类型擦除后的 StepType。
type Definition[C any] struct {
    Meta     Meta                                  // ID、名称、分类、图标、端口声明、说明
    Defaults func() C                              // 新建步骤时的默认配置
    Infer    func(ic *InferContext, cfg *C) error  // 设计期：输入元数据 + 配置 → 输出元数据 + 问题
    Open     func(rc *RunContext, cfg *C) (Runner, error)
}

type Runner interface {
    Run(ctx context.Context) error // 从 rc.In(port) 读批次，向 rc.Out(port) 写批次
}

func Define[C any](d Definition[C]) StepType // 在 init() 中调用 Register(Define(...))
```

* **Infer 与运行共享同一份推导**：设计器的字段下拉、预检、运行前编译，三者用同一段代码，不会出现「界面说能跑、运行时报错」。
* **端口声明**：`Meta.Inputs` / `Meta.Outputs` 声明命名端口、是否允许多条连线、是否 info 端口（如流查询的 `lookup`）。
* **错误输出**：`Meta.ErrorPort=true` 的步骤可连 `error` 端口；错误行 = 输入行 + `_error_code/_error_message/_error_field/_error_step` 四个字段。未连接时，错误行使步骤失败。
* **辅助器**：`core.RowMapper`（逐批映射，覆盖 80% 的简单步骤）、`core.Sink`、`core.Source`，复杂步骤（排序、合并）直接实现 `Runner`。

### 3.4 资源上下文 `RunContext`

每个步骤运行时获得：

| 能力 | 用途 |
|---|---|
| `In(port)` / `Out(port)` | 读写批次（带背压与取消） |
| `Vars()` | 只读变量（类型化） |
| `Log()` | 结构化日志（自动带 run/step） |
| `Metrics()` | 读 / 写 / 输入 / 输出 / 更新 / 拒绝 / 错误 计数 |
| `TempDir()` | 外部排序、去重溢写 |
| `MemoryBudget()` | 有状态步骤的内存上限（超出即溢写或失败，绝不无界增长） |
| `Conn(id)` | 从连接池借连接 |
| `State()` | 增量游标等持久状态（`etl_state`） |
| `Preview()` | 是否预览模式（输出步骤不写库，只回显将写的行） |

---

## 4. 转换（`pipeline`）

### 4.1 定义（JSON，带版本）

```jsonc
{
  "format": 1,
  "kind": "pipeline",
  "params": [{ "name": "maxTs", "kind": "timestamp", "default": null, "description": "增量起点" }],
  "nodes": [
    { "id": "n1", "type": "table_input", "name": "读取订单", "version": 1,
      "config": { "connection": "c_b", "sql": "select * from orders where updated_at > ${maxTs}" },
      "ui": { "x": 120, "y": 80 } }
  ],
  "hops": [{ "id": "h1", "from": "n1", "fromPort": "main", "to": "n2", "toPort": "main", "enabled": true }],
  "settings": { "batchSize": 1000, "channelBatches": 8, "memoryBudgetMB": 512, "onError": "abort" },
  "notes": [{ "id": "t1", "text": "…", "ui": { "x": 0, "y": 0, "w": 240, "h": 80 } }]
}
```

每个节点的 `version` 是**该步骤配置结构的版本**，步骤升级配置时提供 `Migrate(from int, raw json.RawMessage)`，旧任务不会坏。

### 4.2 编译：定义 → 执行计划

```
Decode → 结构校验（ID 唯一、端口存在、连线合法、无环）
      → 拓扑序
      → 逐节点 Infer（输入 schema 来自上游端口；多条连线汇入同一端口时要求 schema 兼容）
      → Plan{ Nodes(有序), Edges(带 schema), Problems }
```

`Plan` 是运行器、预检、设计器字段提示的共同输入。**问题（Problem）分级**：`error` 阻止运行；`warning` 允许运行（例如插入/更新的查询关键字无索引）。

> 未来优化器（下推）在 `Plan` 层实现：把「表输入 → 过滤 → 字段选择」合成一条源端 SQL。
> 为此 v1 约束：**步骤配置必须是声明式的**，不接受不透明脚本。

### 4.3 运行模型

* 每个步骤一个 goroutine；每条连线一条 `chan core.Batch`（容量 `channelBatches`）→ 天然背压。
* 一个端口多条连线：`copy`（默认，每条下游收到全量，零拷贝）或 `distribute`（轮询分发，用于并行副本）。
* 多条连线汇入同一输入端口：合并为一个流（schema 必须兼容）。
* 步骤返回 → 引擎关闭它的全部输出通道；下游读到所有输入关闭即结束。
* 任一步骤返回错误 → 取消整个转换的 `ctx`（快速失败），其余步骤收尾，运行标记失败。
* 并行副本（`copies>1`）：v1 接口预留，运行器 TODO。
* 度量每 1s 快照一次推给运行管理器（SSE），结束时落库。

### 4.4 预览

`POST /api/etl/pipelines/preview { definition, nodeId, limit }`：编译到目标节点为止的上游子图，
输入步骤读到 `limit` 行即停止，输出步骤处于 `Preview()` 模式只回显不写库。设计器底部「数据预览」用它。

---

## 5. 表达式（`expr`）

* 引擎：`github.com/expr-lang/expr`（纯 Go、沙箱、编译期类型检查）。
* 环境：当前行字段可直接按名引用；非标识符字段名用 `field("订单 金额")`；变量用 `var("maxTs")`。
* **类型推导**：用输入 schema 构造类型化环境编译，`program` 的结果类型 → 输出字段 Kind；设计器即时显示。
* **NULL 语义**：参与算术 / 比较的 NULL 会产生运行时错误 → 该行进错误输出（或使步骤失败）；请用 `coalesce` / `ifNull`。字符串函数对 NULL 返回 NULL。
* **decimal**：通过 `expr.Operator` 重载 `+ - * / 比较` 保持精度（TODO：见 `expr/decimal.go`）。
* **日期格式**：沿用 Kettle 用户熟悉的 `yyyy-MM-dd HH:mm:ss`，内部转 Go layout。
* **函数目录**：每个函数带 `分类 / 签名 / 释义 / 示例 / 返回类型`，`GET /api/etl/catalog/functions` 下发，
  前端表达式编辑器提供分类下拉、搜索、点选插入与示例。

---

## 6. SQL 参数（`sqltext`）

| 语法 | 含义 | 安全性 |
|---|---|---|
| `${name}` | **绑定参数**：替换为方言占位符，值类型化绑定 | 无注入风险（默认用法） |
| `#{name}` | **受控文本替换**：仅允许值匹配 `^[A-Za-z_][A-Za-z0-9_$]*(\.[A-Za-z_][A-Za-z0-9_$]*)?$`，用于表名/schema 名 | 不匹配即运行失败 |

解析器跳过字符串字面量、引用标识符、`--` / `/* */` 注释与 PG `::` 强转，不会误识别。

---

## 7. 连接器（`conn`）

```go
type Dialect interface {
    ID() string; Label() string; DefaultPort() int
    DriverName() string
    DSN(c Config, password string) string
    Quote(ident string) string
    FoldIdent(ident string) string        // 用户输入标识符 → 服务器规范形式（达梦折叠大写）
    Placeholder(i int) string            // 1-based
    MaxParams() int                      // 单条语句参数上限，批量写据此切块
    FieldFor(col ColumnMeta) core.Field  // 数据库列 → 逻辑字段
    Normalize(v any, f core.Field) (any, error) // 驱动扫描值 → 逻辑值
    Bind(v any, target ColumnMeta) (any, error)  // 逻辑值 → 驱动参数
    ListSchemas / ListTables / Columns / Indexes(ctx, q Querier, ...) // 设计器选择器 + 预检
    TruncateSQL(table TableRef) string
}
```

* 方言：`postgres`、`mysql`、`dm`、`kingbase`（基于 postgres 方言，独立 ID 以便差异化）、`sqlite`（**仅测试**，不出现在连接类型目录）。
* 连接池：按连接 ID 缓存 `*sql.DB`；连接配置变更或删除时关闭；运行借用、不持有跨运行事务。
* 批量写：多行 `INSERT … VALUES (…),(…)`，按 `MaxParams()` 与 `batchSize` 取小值切块。
* **插入/更新（Kettle 语义，不依赖唯一约束）**：每批按查询关键字一次性查出已存在的键
  （单键 `IN (…)`，复合键 `(k1=? AND k2=?) OR …`），批内去重（后者覆盖前者），拆为插入批与更新批；
  可选「不执行更新」；关键字段 NULL 按 `IS NULL` 比较或报错（配置项）；一个键命中多行则全部更新。
  预检检查关键字段是否有索引（无索引 → warning）。
  **写入加速**（两条路径，查找/比较语义不变）：
  * 关键字段被唯一索引/主键**精确覆盖**（部分索引、表达式索引不参与判定）且方言支持时，
    用原生 upsert：PG/Kingbase/SQLite `ON CONFLICT … DO UPDATE`、MySQL `ON DUPLICATE KEY UPDATE`
    （`VALUES(col)` 兼容 5.7/8/MariaDB）；`noUpdate` 时 `DO NOTHING` / `key=key` 空更新。
    MySQL 额外要求表上不存在键以外的唯一索引——`ON DUPLICATE` 对任意唯一冲突都会更新命中行，
    可能改到不匹配键的行，此时回退通用路径。达梦无冲突子句 upsert，走通用路径。
  * 通用路径：`UPDATE t SET c = m.c FROM (SELECT ? k, ? c UNION ALL …) m WHERE t.k = m.k`
    （MySQL 为 `UPDATE t JOIN (…) m ON … SET t.c = m.c`；达梦首行 SELECT 带 `FROM DUAL`），
    派生行集 + 连接更新 O(n)——不用 `CASE WHEN k=…`（每行线性扫 WHEN 链，随批大小平方退化，
    实测 MySQL 5000 行 CASE 更新 ~2.4s/条）。首行参数按 `Dialect.ParamCast` 显式 CAST：
    UNION 列类型由首行决定，无类型/文本绑定参数与非文本列连接会冲突或强制 DOUBLE 比较
    （PG/KB CAST 到列原生类型；MySQL 按逻辑类型映射 SIGNED/DECIMAL(p,s)/DATETIME…；
    DM/SQLite 无需 CAST）。按 `MaxParams` 与 `commitSize` 分块。
* 读：`QueryContext` + 流式 `rows.Next()`。

**真机认证结论**（`conn/live_test.go` + `actions/live_test.go`，build tag `etl_live`，
环境变量 `ETL_LIVE_<DIALECT>` 注入连接；库版本 MySQL 8.4 / DM8 / KingbaseES V008R003 / PostgreSQL 16.4）：

* 全库通用：类型往返、元数据（Columns/Indexes/ListTables/ListSchemas）、ProbeSQL、
  「A 取 max → B 计数 → 条件 → 增量插入/更新」端到端（含无增量重跑分支）全部通过。
* 达梦：无需显式 fetch size 即可流式读；NUMBER→string、BIT→int8、TSWTZ→带偏移 time.Time；
  单语句参数上限实测 **65535**（已从保守值 8000 调整）；未加引号标识符存大写，
  `FoldIdent` 折叠用户输入（全小写→大写，含大写视为刻意），元数据查询 `UPPER()=UPPER()`。
* Kingbase：系统目录为 `sys_*`（无 `pg_catalog` 兼容层），`Indexes` 按方言切换目录前缀；
  `ON CONFLICT` 可用；information_schema 名称存大写，比较用 `UPPER()=UPPER()`。
* MySQL：`BIT(1)`→布尔（`BIT(n>1)`→整数，扫描为 []byte 大端位串）；`TINYINT(1)` 经
  information_schema 为布尔，但 SELECT 探测路径无法与 tinyint(4) 区分（读为整数）；
  `BIGINT UNSIGNED`（探测名 `UNSIGNED BIGINT`，精度 20）映射为精确小数；
  `DATETIME`/`TIMESTAMP` 由驱动按 `Loc`（=连接时区）双向换算，墙上时钟往返不变。

---

## 8. 内置步骤（v1）

| 分类 | 步骤 ID | 名称 | 状态 |
|---|---|---|---|
| 输入 | `generate_rows` | 生成记录 | 已实现 |
| 输入 | `table_input` | 表输入 | 已实现 |
| 转换 | `select_fields` | 字段选择（重命名 / 删除 / 排序 / 改类型） | 已实现 |
| 转换 | `calculator` | 表达式（字段映射 / 新增字段） | 已实现 |
| 流程 | `filter_rows` | 过滤记录（true / false 两路输出） | 已实现 |
| 转换 | `unique_hash` | 唯一行（哈希，内存预算受控） | 已实现 |
| 流程 | `dummy` | 空操作 | 已实现 |
| 输出 | `table_output` | 表输出（裁剪表 / 提交记录数 / 批量） | 已实现 |
| 输出 | `insert_update` | 插入 / 更新（原生 upsert / CASE 批量更新） | 已实现 |
| 输出 | `update` / `delete` | 更新 / 删除 | TODO |
| 转换 | `sort_rows` | 排序（外部排序溢写） | TODO |
| 转换 | `unique_sorted` | 去除重复记录（要求有序） | TODO |
| 转换 | `value_mapper` | 值映射 | TODO |
| 连接 | `merge_rows` / `sync_after_merge` | 合并记录 / 数据同步（含删除） | TODO |
| 查询 | `stream_lookup` / `db_lookup` | 流查询 / 数据库查询 | TODO |
| 输入 | `http_input` / `csv_input` | HTTP / CSV | TODO |

---

## 9. 作业（`workflow`）

### 9.1 语义（相对 Kettle 的改进）

* **命名输出端口**：每个作业项声明输出端口，默认 `success` / `failure`；条件类作业项为 `true` / `false` / `failure`。连线挂在端口上——与转换层同构。
* **汇合语义确定**：一个节点有多条入线时，等**所有上游都已决（完成或被跳过）**后，若至少一条入线被激活则执行，否则跳过并继续向下传播跳过。（Kettle 每条入线触发一次，可能重复执行，这里不会。）
* **失败是否被处理**：某节点失败且其 `failure` 端口有连线 → 视为已处理；作业整体结果 = 是否存在未处理失败。
* **并行分支**：同时就绪的节点并发执行（作业级并发上限）。
* **超时 / 重试**：每个作业项可配置 `timeoutSeconds`、`retry{max, backoffSeconds}`。
* **从失败处继续**：新运行携带 `resumeOf`，上次已成功的节点直接复用其输出变量并标记 `reused`。

### 9.2 作业项（v1）

| ID | 名称 | 输出端口 | 状态 |
|---|---|---|---|
| `start` | 开始（唯一） | success | 已实现 |
| `pipeline` | 运行转换（变量 → 参数映射） | success / failure | 已实现 |
| `sql` | 执行 SQL | success / failure | 已实现 |
| `sql_value` | SQL 取值 → 变量（标量 / 单行多列） | success / failure | 已实现 |
| `condition` | 条件（表达式） | true / false / failure | 已实现 |
| `set_vars` | 设置变量（表达式） | success / failure | 已实现 |
| `wait` | 等待 | success | 已实现 |
| `success` / `abort` | 成功 / 中止 | — | 已实现 |
| `http` / `workflow` / `notify` | HTTP / 子作业 / 通知 | | TODO |

### 9.3 变量作用域链

`内置（RUN_ID、RUN_START、TRIGGER）` < `作业参数默认值` < `调度覆盖值` < `手动运行覆盖值` < `作业项运行时写入`。
变量是**类型化**的（`core.Value{Kind, V}`），转换参数按声明类型接收，类型不符在编译期报错。

### 9.4 典型场景

```
开始 → SQL取值@A  select max(sync_ts) as maxTs from a.orders           → ${maxTs}
     → SQL取值@B  select count(*) as cnt from b.orders where ts > ${maxTs} → ${cnt}
     → 条件       cnt > 1000  ──false──▶ 成功
     → 运行转换「订单同步」(maxTs → maxTs)
         表输入@B  select … from orders where ts > ${maxTs}
         → 字段选择 / 表达式 → 唯一行 → 插入/更新@A（关键字 order_no，提交 5000）
     → 成功
```

---

## 10. 调度（`schedule`）

* cron：`robfig/cron/v3` **只用解析器**（5 段，可选秒；支持 `@daily` 等），触发循环自研以完全掌控语义。
* 目标：作业或转换。
* 重叠策略：`skip`（默认，已有运行则本次跳过并记录）/ `queue`（排队最多 1 个）/ `parallel`。
* 错过策略（Console 停机期间到点）：`skip`（默认）/ `run_once`（启动后补跑一次）。
* 时区：每个调度可配置，默认 Console 本地时区。
* 前端展示 cron 的中文释义与未来 5 次触发时间（`GET /api/etl/schedules/preview?cron=…`）。

---

## 11. 运行、度量与日志

### 11.1 运行生命周期

`queued → running → succeeded | failed | canceled | interrupted`

* `interrupted`：Console 重启时仍处于 queued/running 的运行，启动时统一标记（不会静默消失）。
* 运行管理器：全局并发上限（`etl.max_concurrent_runs`，默认 4），取消（`ctx` 级联），SSE 订阅。
* 每次运行保存**定义快照**（`definition_snapshot`），事后可准确复盘「当时跑的是哪个版本」。

### 11.2 度量（与 Kettle 步骤度量列对齐）

读（从上游）/ 写（到下游）/ 输入（从库或文件）/ 输出（到库或文件）/ 更新 / 拒绝（进错误输出）/ 错误 + 耗时 + 速度（行/秒）。

### 11.3 日志

* 结构化：`{seq, ts, level, nodeId, message}`；级别：`error / warn / info / detail / debug`。
* 每次运行落库上限 `etl.run_log_limit`（默认 5000 条），超出只计数（界面提示「已截断 N 条」）。
* **行数据不落库**，只出现在预览与 debug 采样里。
* 保留策略：按天数与每个定义的运行数双限裁剪（`etl.run_retention_days`、`etl.run_keep_per_definition`）。

---

## 12. 持久化（`store`，SQLite 同库 `etl_*` 表）

| 表 | 关键列 | 说明 |
|---|---|---|
| `etl_connections` | id, name, db_type, host, port, database_name, default_schema, username, secret_cipher, options_json, last_test_* | 密码 AES-GCM 加密，AAD=连接 ID |
| `etl_definitions` | id, kind, name, folder, description, doc_json, format, revision, created_*, updated_* | 定义为 JSON 文档（同 Console 文档存储取向）；`revision` 乐观锁 |
| `etl_definition_versions` | definition_id, revision, doc_json, saved_by, saved_at | 历史版本（保留最近 N 个，可回滚） |
| `etl_schedules` | id, target_kind, target_id, cron, timezone, enabled, overlap, misfire, params_json, next_fire_at, last_fire_at | |
| `etl_runs` | id, kind, definition_id, revision, trigger, schedule_id, parent_run_id, resume_of, status, params_json, snapshot_json, error, triggered_by, started_at, finished_at | |
| `etl_run_nodes` | run_id, node_id, node_type, status, attempt, metrics_json, output_vars_json, error, started_at, finished_at | 作业项 + 步骤共用 |
| `etl_run_logs` | run_id, seq, ts, level, node_id, message | 每运行限量 |
| `etl_state` | scope, key, value_json, updated_at | 增量游标等 |

---

## 13. 凭据（`secret`）

* 独立密钥文件 `mooncell-etl.key`（32 字节随机，0600，默认与数据库同目录，可用 `etl.key_file` 覆盖）。
* 首次启动自动生成；**丢失即所有已存密码不可解**——连接显示「需重新录入密码」，fail-closed，绝不回退到明文。
* 密文格式 `v1:` + base64(nonce‖ciphertext)，AAD 绑定连接 ID，防止密文被挪到别的连接复用。
* 密码永不回显前端；编辑时留空表示不改。
* `install.sh` / 升级备份须把密钥文件与数据库一起备份（TODO：脚本更新）。

---

## 14. HTTP API（`/api/etl/`，v1 全部 admin）

| 方法 路径 | 说明 |
|---|---|
| `GET catalog/steps` · `GET catalog/actions` · `GET catalog/functions` · `GET catalog/dialects` | 组件 / 函数 / 方言目录 |
| `GET/POST connections` · `GET/PUT/DELETE connections/{id}` | 连接 CRUD |
| `POST connections/test` · `POST connections/{id}/test` | 测试连接 |
| `GET connections/{id}/schemas` · `…/tables?schema=` · `…/columns?schema=&table=` | 设计器选择器 |
| `GET/POST definitions` · `GET/PUT/DELETE definitions/{id}` | 定义 CRUD（PUT 带 `revision` 乐观锁） |
| `GET definitions/{id}/versions` · `POST definitions/{id}/versions/{rev}/restore` | 历史版本 |
| `POST definitions/validate` | 编译：问题列表 + 每条连线的 schema |
| `POST pipelines/preview` | 数据预览 |
| `POST definitions/{id}/run` | 手动运行（参数覆盖） |
| `GET runs` · `GET runs/{id}` · `GET runs/{id}/logs?after=` · `POST runs/{id}/cancel` · `POST runs/{id}/resume` | 运行记录 |
| `GET runs/{id}/events` | SSE：度量快照、节点状态、日志增量 |
| `GET/POST schedules` · `PUT/DELETE schedules/{id}` · `GET schedules/preview?cron=&tz=` | 调度 |

错误响应统一 `{ "error": "中文说明", "code": "machine_code", "problems": [...] }`。

---

## 15. 前端（`console/src/etl/`）

```text
src/etl/
├── EtlModule.jsx          # 模块壳：二级导航（工程 / 运行 / 调度 / 连接）
├── api.js                 # /api/etl 客户端
├── catalog.js             # 组件目录（后端下发 + 本地表单注册表）
├── pages/                 # Projects / Designer / Runs / RunDetail / Schedules / Connections
├── designer/              # Canvas（@xyflow/react）/ Palette / Inspector / BottomDrawer / nodes / edges
├── forms/                 # 每个步骤 / 作业项的配置表单（按 type 注册）
├── components/            # ExpressionEditor（CodeMirror + 函数目录）/ FieldMapper / CronInput / SqlEditor …
└── etl.css                # 模块样式（只用全局设计令牌）
```

* **视觉语言**：完全沿用 Console 设计令牌（暖纸底、terracotta 主色、Instrument Sans + IBM Plex Mono），不引入新色系；
  节点按分类着色只用既有语义色（输入=info、转换=purple、流程=warn、输出=success、作业=primary）。
* **设计器**：左组件面板（搜索 + 分类，拖入画布）/ 中画布（网格、吸附、框选、小地图、键盘快捷键）/ 右属性检查器 /
  底部抽屉（校验问题 · 数据预览 · 运行度量 · 日志）。端口有清晰的语义色与标签，连线上显示 schema 字段数与运行时行数。
* **扩展一个步骤的前端成本**：在 `forms/` 写一个表单组件并 `registerForm('type', Component)`；未注册时退化为 JSON 编辑。
* 浏览器下限 Chrome 92（与 Console 一致）。

---

## 16. 安全与权限

* v1：模块全部接口 `admin` 专属（`requireRole("admin")`）。后续可加「数据集成开发者」角色（TODO）。
* SQL 由管理员编写、在管理员配置的连接上执行——这是产品能力而非注入面；但**变量值进入 SQL 一律走绑定参数**，文本替换仅限白名单格式。
* 审计：连接增删改、定义保存/删除、手动运行/取消、调度启停写入现有审计日志（`数据集成·…`）。
* 日志与错误信息中不得出现密码；DSN 不入日志。

## 17. 进程与自更新

* Console 自更新进入 draining 时调用 `etl.Service.BeginDrain()`：暂停调度器派发；**有运行中的任务则拒绝本次自更新**（409，提示任务名）。失败时 `EndDrain()` 恢复。
* Console 被 kill / 断电：下次启动把遗留运行标记为 `interrupted`，可「从失败处继续」。

## 18. 性能目标与验证

* 千万行 PG → MySQL 表输出、插入/更新各一组压测，记录吞吐、峰值 RSS、提交间隔影响（TODO：`etl/bench`）。
* 内存上界 ≈ `连线数 × channelBatches × batchSize × 平均行宽` + 有状态步骤预算；编译期打印估算值。

## 19. 测试策略

* 单元：`core`（转换 / schema）、`sqltext`（注释、字面量、`::`）、`expr`（类型推导、NULL）、`secret`（篡改、AAD 错位、密钥丢失）。
* 引擎：用 `generate_rows` + 内存 sink 测背压、取消、错误输出、复制/分发、上游失败传播。
* 端到端：`sqlite` 方言跑 表输入 → 表达式 → 插入/更新 全链路；真机 PG/MySQL/DM 用 testcontainers 之外的固定测试库（离线环境）。
* 前端：Playwright 覆盖「新建转换 → 拖入步骤 → 连线 → 校验 → 预览 → 保存 → 运行 → 查看运行详情」。

## 20. 分期

| 阶段 | 内容 |
|---|---|
| P0（本分支骨架） | 内核、编译、运行器、表达式、SQL 参数、凭据、存储、核心步骤 / 作业项、API、全部前端页面 |
| P1 | 调度器完成、SSE 实时、预览完善、历史版本、真机三库类型映射认证、千万级压测与调优 |
| P2 | 排序 / 去重(有序) / 合并记录 / 数据同步 / 流查询 / 值映射 / 更新 / 删除 / CSV / HTTP |
| P3 | 并行副本、原生 upsert 加速、下推优化器、子作业、通知、角色授权 |
