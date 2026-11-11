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
* **数据类 vs 基础设施错误**：`conn.IsDataError(dialect, err)` 按方言错误载体判定——PG/Kingbase 取 SQLSTATE 前缀（`22xxx` 数据异常、`23xxx` 完整性约束），MySQL 取错误码白名单（1048/1062/1264/1366/1406/1451/1452 等），DM 取 `DmError.ErrCode` 白名单（按 DM8 真机实测维护：服务端 -6602 唯一/-6604 检查/-6607 外键/-6609 非空/-6169 超长/-6149 数值溢出/-6118 日期格式，驱动端 6092 溢出/6007 类型转换/6015 日期格式；未实测的码一律按基础设施处理，由 `conn/live_dataerr_test.go` 与 `steps/live_fallback_test.go` 四库同断言守护），SQLite 取基础错误码（18/19/20）；取消、超时、网络、`ErrBadConn` 一律基础设施。`table_output` 批量写入失败时仅数据类错误退化逐行（每行单事务，`flushRows`）；逐行阶段非数据类错误同样拒行但计数，连续 100 行即视为系统性故障整步失败——宁可失败可观测，也不静默把故障伪装成坏行。
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
  "settings": { "batchSize": 1000, "channelBatches": 8, "memoryBudgetMB": 512, "pushdown": false, "onError": "abort" },
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

> 优化器（下推）在 `Plan` 层实现（`pipeline.Pushdown`）：把「表输入 → 过滤记录(若干) → 字段选择(至多一个,纯改名)」折叠为一条源端 SQL `SELECT <列|*> FROM (<原 SQL>) _mc_push WHERE <条件>`。
> **默认关闭**（`settings.pushdown`，缺省 false，旧定义兼容），仅运行路径在显式开启时改写；**预览永不下推**——预览走原链，保证所见即引擎语义。
> 条件经 `expr.ToSQL` 翻译成**可证等价子集**（R1-B2），白名单之外一律放弃该分支退回行内求值：
> * 允许：整数/精确小数/浮点/日期/时间戳字段与数值字面量的 `==` `!=` `<` `<=` `>` `>=`、`in [字面量…]`、`== nil`/`!= nil`/`isNull(f)`、`&&`/`||`（|| 两臂与 not 复合操作数内不许出现次序比较与裸布尔字段——NULL 时引擎报错而 SQL 三值并入另一侧，投递结果会不一致）、`not`（NULL 安全子树按德摩根律改写，单个次序比较/裸布尔字段生成 `NOT` 但位置受限）、数值 `+`/`-`（仅作比较操作数）、布尔 kind 字段单独作条件（NULL 敏感叶）、常量 `true`/`false`。
> * NULL 语义折算：`f != v → (f <> ? OR f IS NULL)`（引擎 `nil != 1` 为真）；`f not in […] → (f NOT IN (…) OR f IS NULL)`；字段对字段 `==`/`!=` 补双 NULL 分支。
> * **NULL 敏感叶与行内复核**：次序比较（含数值 `+`/`-` 操作数）、裸布尔字段及其 `not` 遇到 NULL 时，引擎求值报错——过滤不接错误端口（下推前提）即**整次运行失败**；SQL 却只是不投递该行。若照搬，下推会把「运行失败」静默变成「NULL 行被滤掉」。因此：叶内字段全部被源端元数据声明为不可空时原样下推（严格等价）；否则下推**超集** `(叶 OR f IS NULL …)` 并**保留该过滤步骤在行内复核**（`SQLCond.Residual`）——非 NULL 的不命中行仍在源端被滤掉，NULL 行照常送到引擎、照常报错。超集只出现在合取位置（顶层 / `&&`），在其内保持超集；链在复核过滤处结束，其后的字段选择不再折叠。驱动不上报可空性（如 PG 的 pgx）时一律按可空处理。
> * 禁止：`var()`、一切函数调用（`length`/`trim`/`now()`/`today()` 等）、字符串比较与字符串 in（大小写/长度/转义语义随方言漂移）、三元、`??`、`/` `%` `*` `**` `..`、匹配/包含谓词、成员访问、非字面量 in 列表。
> * **字面量零内联**：翻译出的 SQL 不含任何值字面量——全部生成方言占位符（`?`/`$N`），编号续接原 SQL 的 `${}` 参数之后，值写入节点 `pushArgs` 由表输入按序绑定。`pushArgs` 以 `json.Number` 保存字面量原文，绑定时整数还原为 `int64`、其余为 `float64`——`[]any` 经 JSON 往返会把整数解成 float64，超过 2^53 的 bigint（雪花 ID 等）会舍入并匹配到错误的行。某一过滤放弃下推（端口检查不过或翻译失败）时，其占位符序号与参数一并回滚，保证占位符与 `pushArgs` 一一对应。
> 仅当表输入仅有唯一出边、过滤的「否/错误」端口未连线时才改写；以下情形不下推（会改变结果）：表输入设了 `maxRows`（限的是过滤前的前 N 行）；链上节点 `copies>1` 且 `copiesDistribution=copy`（每副本各出一份）；字段选择接了错误端口、没有下游，或有多条出边且其分发方式与表输入不同。改写后重编译失败回退原计划并记 `warn` 日志。下推后被并入的步骤不出现在运行节点中，日志记录「下推优化」条目。
> 为此 v1 约束：**步骤配置必须是声明式的**，不接受不透明脚本。

### 4.3 运行模型

* 每个步骤一个 goroutine；每条连线一条 `chan core.Batch`（容量 `channelBatches`）→ 天然背压。
* 一个端口多条连线：`copy`（默认，每条下游收到全量，零拷贝）或 `distribute`（轮询分行给各连线）。这是 `distribution` 语义——**多条连线之间怎么分**。
* 一条连线到达多副本节点时，副本间怎么分由目标节点的 `copiesDistribution` 决定（R1-B3）：缺省 `distribute`（轮询分行，合计只收一份输入，行数守恒）；`copy` 表示每个副本都收全量。两个维度互相独立。
* 多条连线汇入同一输入端口：合并为一个流（schema 必须兼容）。
* 步骤返回 → 引擎关闭它的全部输出通道；下游读到所有输入关闭即结束。
* 任一步骤返回错误 → 取消整个转换的 `ctx`（快速失败），其余步骤收尾，运行标记失败。
* 并行副本（`copies>1`，上限 8）：步骤展开为 N 个独立实例（各自 RunContext/端口/临时目录，`rc.CopyIndex` 可自分区），共享节点度量与节点级运行状态；输入按上条 `copiesDistribution` 投到各副本。无输入端口的源步骤设副本编译报错（会全量重复）；`Stateful` 步骤（排序/去重/合并/同步）设副本只告警——副本间不共享状态。对共享外部资源做一次性动作的配置设副本编译报错（`InferContext.Copies`）：`table_output` 开「开始前清空表」（各副本各自清表，删掉彼此已写的行）、`csv_output`（覆盖互相截断，追加按 4KB 冲刷交错出残缺行）。另有两类编译告警（R1-B3）：`insert_update`/`update`/`delete`/`sync_after_merge`/`db_lookup` 设副本时提示并发按键读/写需唯一约束，否则竞态重复；副本节点下游可达 `OrderedInput` 步骤（`unique_sorted`、`merge_rows`）时提示行序被分片打乱（途经 `sort_rows` 重建全序则不再告警）。
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
| 输出 | `table_output` | 表输出（裁剪表 / 提交记录数 / 批量；连 error 端口且批失败属数据类错误时退化逐行定位坏行，基础设施错误整步失败） | 已实现 |
| 输出 | `insert_update` | 插入 / 更新（原生 upsert / 派生行集连接更新） | 已实现 |
| 输出 | `update` / `delete` | 更新 / 删除（未命中键的全部行进错误输出，未连则跳过；「已处理」只含命中行，两端口互斥） | 已实现 |
| 转换 | `sort_rows` | 排序（外部排序溢写） | 已实现 |
| 转换 | `unique_sorted` | 去除重复记录（要求有序） | 已实现 |
| 转换 | `value_mapper` | 值映射 | 已实现 |
| 连接 | `merge_rows` / `sync_after_merge` | 合并记录 / 数据同步（含删除） | 已实现 |
| 查询 | `stream_lookup` / `db_lookup` | 流查询 / 数据库查询 | 已实现 |
| 输入 | `csv_input` / `csv_output` | CSV 读 / 写（声明类型、UTF-8/GBK） | 已实现 |
| 输入 | `http_input` | HTTP GET/POST（${变量}、页码分页、JSON 点路径拍平） | 已实现 |

**合并/同步语义**（与 Kettle Merge rows (diff) + Synchronize after merge 对齐）：

* `merge_rows` 两个输入端口：`main`=参照（现有/旧数据）、`info`=对比（新数据），
  两路字段布局必须一致且都已按合并键升序排序（各前置 `sort_rows`）。
  输出 = 对比流字段 + 标志字段：`identical`（键值全同）/ `changed`（键同值异，发对比行）/
  `new`（只在对比流）/ `deleted`（只在参照流，发参照行）。
* `sync_after_merge` 按标志分流写目标表：`new`→插入、`changed`→更新、`deleted`→删除、
  `identical`→原样透传不写库；一批一事务；标志缺失/未知、键为 NULL 的行进错误输出；
  写库失败整批回滚、步骤失败（与 `update`/`delete` 一致）。
  字段映射留空时自动映射除键与标志字段外的全部字段。

**查询步骤语义**：

* `stream_lookup`：`info`=参照流整体装哈希表（受内存预算限制，超限报错），
  主流按键等值匹配追加返回字段；参照流重复键保留首行；未命中填默认值（默认 NULL）。
  参照端口是 info 端口（整流读完才读主流）：主流与参照有共同上游时编译报错（共同上游向两路交替发送，主流通道写满后互等卡死，是否触发取决于数据量）。
* `db_lookup`：主流逐行 `SELECT … WHERE k=? AND …` 等值查目标表；结果按查询键缓存
  （`noCache` 关闭），多行命中默认取第一行（`orderBy` 排序，`failOnMulti` 则进错误输出）；
  键为 NULL 直接判未命中（`= NULL` 恒不成立）。

**文件 / 接口步骤语义**：

* `csv_input`：读 `file_root` 下的相对路径文件（边界见 §16），按**声明字段**（位置对应列）
  逐行类型转换，空串→NULL、首行表头跳过；编码 utf-8/gbk；转换失败/列数超出的行进错误输出。
* `csv_output`：写 `file_root` 下的相对路径文件（可追加；父目录须已存在，不自动建目录），
  `FormatFieldValue` 按字段 scale 输出。
* `http_input`：`${变量}` 文本替换 URL/头/正文（变量须在转换参数中声明）；
  `dataPath` 指向响应 JSON 中的数组（对象→单行），字段按相对点路径取值；
  分页为页码参数递增（GET 加 query、POST 注入 JSON body），返回不足 `size` 即末页，
  `maxPages` 为安全上限；非 2xx、非 JSON、dataPath 不存在均为步骤级失败。

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
| `http` | HTTP 请求 | success / failure | 已实现 |
| `workflow` | 子作业（嵌套 ≤8 层） | success / failure | 已实现 |
| `notify` | 通知（Webhook） | success / failure | 已实现 |

* `http`：URL/请求头/正文支持 `${var}` 插值；`statusVar` 写状态码、`resultVar`+`dataPath` 写响应 JSON（子树序列化文本）、`extract` 按点路径取值转类型写变量；非 2xx、请求失败、提取路径缺失或类型转换失败均判失败。默认超时 30 秒。
* `workflow`：经 `PipelineRunner.RunWorkflow` 走运行快照取被引定义（续跑语义与转换一致）；`params` 为对作业变量求值的表达式；子作业各节点状态以 `ParentNode` 挂在本作业项下归入同一运行；嵌套深度按 host 计数，超过 8 层判失败（防循环引用）。
* `notify`：v1 为 Webhook——POST `template`（`${var}` 插值）到 `url`，可配请求头；`secretHeader`+`secret` 把凭据注入指定请求头，明文 `secret` 仅在提交时出现，保存定义时由服务层 `secret.Box` 以 `etl/notify/<defID>/<nodeID>` 为 AAD 密封为 `secretCipher` 并写回 `aadSuffix`，运行侧经 Env 注入的打开器解密；提交空凭据清除已存密文。邮件通知需要部署级 SMTP 配置，后续补充。

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
* 重叠策略：`skip`（默认，已有运行则本次跳过并记录）/ `queue`（排队最多 1 个；须等同定义的**全部**在途运行——含手动与 parallel 运行——结束才开始，排队中可取消）/ `parallel`。手动运行不等待任何运行。
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
* **常规行数据不落库**；唯一例外是 error 端口拒绝行采样（见下）。
* 保留策略：按天数与每个定义的运行数双限裁剪（`etl.run_retention_days`、`etl.run_keep_per_definition`）。

### 11.4 运行详情

* **拒绝行采样**：步骤把行写入 error 端口时回调采样，每个节点保留前 20 条，单值超 512 字符截断（字节值转 hex），
  拒绝总数持续计数；随 `etl_run_nodes.samples_json` 落库，运行详情「拒绝」列可点开查看。
* **失败节点定位**：运行详情的失败节点可一键打开对应设计器并选中该节点；
  作业中的步骤沿「作业 → 转换/子作业」引用链解析到所属定义，被引定义已删除时提示而不跳转。

---

## 12. 持久化（`store`，SQLite 同库 `etl_*` 表）

| 表 | 关键列 | 说明 |
|---|---|---|
| `etl_connections` | id, name, db_type, host, port, database_name, default_schema, username, secret_cipher, options_json, last_test_* | 密码 AES-GCM 加密，AAD=连接 ID |
| `etl_definitions` | id, kind, name, folder, description, doc_json, format, revision, created_*, updated_* | 定义为 JSON 文档（同 Console 文档存储取向）；`revision` 乐观锁 |
| `etl_definition_versions` | definition_id, revision, doc_json, saved_by, saved_at | 历史版本（保留最近 N 个，可回滚） |
| `etl_schedules` | id, target_kind, target_id, cron, timezone, enabled, overlap, misfire, params_json, next_fire_at, last_fire_at | |
| `etl_runs` | id, kind, definition_id, revision, trigger, schedule_id, parent_run_id, resume_of, status, params_json, snapshot_json, error, triggered_by, started_at, finished_at | |
| `etl_run_nodes` | run_id, node_id, node_type, status, attempt, metrics_json, output_vars_json, error, samples_json, started_at, finished_at | 作业项 + 步骤共用；samples_json=error 端口拒绝行采样 |
| `etl_run_logs` | run_id, seq, ts, level, node_id, message | 每运行限量 |
| `etl_state` | scope, key, value_json, updated_at | 增量游标等 |

---

## 13. 凭据（`secret`）

* 独立密钥文件 `mooncell-etl.key`（32 字节随机，0600，默认与数据库同目录，可用 `etl.key_file` 覆盖）。
* 首次启动自动生成；**丢失即所有已存密码不可解**——连接显示「需重新录入密码」，fail-closed，绝不回退到明文。
* 密文格式 `v1:` + base64(nonce‖ciphertext)，AAD 绑定连接 ID，防止密文被挪到别的连接复用。
* 密码永不回显前端；编辑时留空表示不改。
* 作业 `notify` 凭据：保存时 `sealDocSecrets` 把明文 `secret` Seal 为 `secretCipher`，AAD = `etl/notify/` + 定义 ID + "/" + 节点 ID（后缀写回 `aadSuffix` 字段）。**运行时开密的 AAD 由运行中的定义 ID 推导**（`actions.DefIDEntry` 注入 Env，子作业绑子作业自身 ID），与文档 `aadSuffix` 不一致即拒绝——文档后缀只是兼容信息，不被信任。**复制定义时 `rekeyDocSecrets` 把密文按源 ID 解密、按副本 ID 重密封**（无密钥箱时剥离凭据，与导出行为一致）；导出包直接剥离 `secretCipher`/`aadSuffix`。
* `install.sh`：`upgrade` 停机后自动快照 `mooncell.db`(含 WAL/SHM)+`mooncell-etl.key`+`config.toml` 到 `$PREFIX/backups/pre-upgrade-<ts>/`；`backup` 子命令可手工快照（从 `[database].path`/`[etl].key_file` 解析实际路径，自定义路径同样覆盖）。**任何一步失败（数据库文件缺失、备份目录建不出、拷贝失败如磁盘写满）`backup_console_data` 都报警并返回非零**——`backup` 直接失败；`upgrade` 删除本次新建的残缺快照、先 `systemctl start` 恢复已停服务再中止退出，不留停机现场，也不带着残缺快照继续换二进制。函数会在 `if !` 条件里调用，bash 此时对函数体禁用 `set -e`，故每条命令显式检查返回值。

---

## 14. HTTP API（`/api/etl/`）

鉴权分两层：`requireAuthETL` 校验登录态并拒绝零授权普通用户（`features.etl` 同规则下发，未授权者入口隐藏）；各 handler 再按 `etl_grants` 逐路由校验定义级授权（详见 §16）。普通用户视角下未授权即不存在：列表类接口只回授权项，单条操作未授权统一 `403`。

| 方法 路径 | 说明 | 普通用户 |
|---|---|---|
| `GET overview` · `GET catalog` | 概览（按授权过滤）· 组件 / 函数 / 方言目录 | 授权项可见 |
| `GET connections` · `GET connections/{id}` | 连接列表 / 详情（`SecretCipher` 永不序列化，`hasPassword` 布尔代替） | 只读元数据 |
| `POST/PUT/DELETE connections` · `POST …/test` | 连接 CRUD 与测试 | admin 专属 |
| `GET connections/{id}/schemas` · `…/tables?schema=` · `…/columns?schema=&table=` | 设计器选择器（活库元数据，只读） | 任一授权即可 |
| `GET definitions` | 列表（每项带 `access`） | 仅授权定义 |
| `POST definitions` · `POST definitions/import*` | 新建 / 导入 | admin 专属 |
| `GET/PUT/DELETE definitions/{id}` · `POST …/duplicate` | 定义 CRUD（PUT 带 `revision` 乐观锁） | `use` 可读；其余 `edit` |
| `GET definitions/{id}/versions` · `POST …/versions/{rev}/restore` | 历史版本 | 读 `use` / 恢复 `edit` |
| `GET definitions/{id}/export` | 导出 JSON 包（`format=mooncell-etl-export`；凭据不随包） | `use` |
| `POST definitions/validate` | 编译：问题列表 + 每条连线的 schema | 任一授权 |
| `POST pipelines/preview` | 数据预览（真实执行读，等价任意连接读权限） | 任一 `edit` 授权 |
| `POST definitions/{id}/run` | 手动运行（参数覆盖） | `use` |
| `GET runs` · `GET runs/{id}` · `GET runs/{id}/logs` · `GET runs/{id}/events` | 运行记录与实时事件 | `use`（按定义过滤） |
| `POST runs/{id}/cancel` · `POST runs/{id}/resume` | 取消 / 续跑 | `use`（按运行所属定义） |
| `GET/POST schedules` · `PUT/DELETE schedules/{id}` · `GET schedules/preview?cron=&tz=` | 调度（列表按授权过滤并带 `access`） | 读 `use` / 写 `edit` |

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
* 浏览器下限 Chrome 92（与 Console 一致，硬性要求）。`pnpm build` 挂 `scripts/check-compat.sh` 扫描 `dist/assets`，产物含 Chrome 92 之后才有的特性即失败——JS：`structuredClone`、`Object.hasOwn`、`findLast`、`toSorted` 等、`Object/Map.groupBy`、`AbortSignal.timeout/any`、`Promise.withResolvers`、`Array.fromAsync`、`checkVisibility`、Popover API；CSS：`color-mix()`、`:has()`、容器查询、`@layer`、`accent-color`、动态视口单位、`text-wrap`、oklch/lab 等新颜色、相对颜色、`scrollbar-gutter`、独立变换属性 `translate/scale/rotate`。esbuild 只按 target 降语法，不降运行时 API 和 CSS 新特性，所以依赖带进来的只能在产物上拦截。依赖中的 `structuredClone` 经 vite `define` 重写为 `__mcStructuredClone`，由 `index.html` 内联 polyfill 提供。唯一豁免是 `@xyflow/react` 的 `.react-flow__resize-control.handle`（NodeResizer 把手，本项目未使用）。模式写错时 grep 报错即判失败，避免检查静默失效（`:has(` 曾因未转义的括号一直不生效）。

---

## 16. 安全与权限

* **定义级授权（P3.16）**：`etl_grants(username, definition_id, access_mode)`，模式 `use`/`edit`；`admin` 隐式全量不落库。
  * `use`：查看定义与历史版本、运行/取消/续跑、查看运行记录与日志、导出。
  * `edit`：`use` 之上再加保存/删除/复制/恢复版本、维护调度；数据预览要求持有任一 `edit`（可改定义者即可经任意连接读数据，属有意边界）。
  * 列表接口只回授权项；单条操作未授权统一 `403`。撤销授权即时生效（下次请求即拒绝，会话不缓存）。
  * 用户管理页按定义下发授权（`etlGrants` 随用户创建/编辑同事务提交；删除用户/定义级联清理）。`features.etl` 仅对 admin 或持有任一授权的普通用户下发 `true`，导航与路由据此 fail-closed。
  * 非管理员复制定义得到的副本自动授予本人 `edit`（否则复制完立刻无权查看）；新建与导入仍为 admin 专属。
  * **引用授权边界（R1-m4）**：`DELETE definitions/{id}?force=1` 删除仍被引用的定义时，非管理员必须对**每一个**引用方持有 `edit`，缺一即 403 且不删；保存作业定义时解码 `CollectRefs`，非管理员引用的每条 pipeline/workflow 须 ≥`use`，防止把未授权定义偷渡进执行路径。概览（overview）对非管理员：definitions/recent runs/active 全部按授权过滤（recent 在 SQL 层、LIMIT 之前过滤），`connections` 恒为 0；取消与续跑运行均写审计（含操作人与定义名）。
* **连接凭据**：`SecretCipher` 永不出 API（`json:"-"`），`hasPassword` 布尔代替；连接的新建/修改/删除/测试 admin 专属；schemas/tables/columns 活库元数据浏览对任一授权用户开放（只读，供设计器选择器用）。
* SQL 由管理员编写、在管理员配置的连接上执行——这是产品能力而非注入面；但**变量值进入 SQL 一律走绑定参数**，文本替换仅限白名单格式。
* **文件根边界（R1-B1）**：`[etl] file_root` 是文件类步骤的唯一可访问目录，默认 `<数据库目录>/etl-files`，启动时创建为 `0750`。`csv_input`/`csv_output` 的 `path` 一律解释为 `file_root` 下的相对路径：绝对路径、含 `..` 越界、解析符号链接后落在根外（对 output 校验父目录与已存在的目标文件本身）——编译期与运行期双重拒绝，对管理员同样生效；运行环境缺 `file_root` 时文件步骤 fail-closed。由此普通用户即使持有 `edit` 也只能读写这一受控目录，无法经预览/运行读写 `mooncell-etl.key` 等任意文件。
* **HTTP 出方向防护（R1-m3）**：`http_input`、`http`、`notify` 走 `core.GuardedHTTPClient`——`DialContext` 先解析主机、逐个校验 IP，再复查连接对端地址，默认拒绝回环（127.0.0.0/8、::1）、链路本地（169.254.0.0/16、fe80::/10）与未指定地址（0.0.0.0/::），防 DNS 重绑定与 302 跳转绕过；私有网段放行（内网接口调用是正当场景）。`[etl] http_allow_loopback = true` 可放开回环，供确有需要的部署。
* 审计：连接增删改、定义保存/删除/复制/导入导出/恢复版本、手动运行/取消/续跑、调度增删改写入现有审计日志（`数据集成·…`）；授权变更走用户管理的创建/编辑审计。
* 日志与错误信息中不得出现密码；DSN 不入日志。

## 17. 进程与自更新

* Console 自更新进入 draining 时调用 `etl.Service.BeginDrain()`：暂停调度器派发；**有运行中的任务则拒绝本次自更新**（409，提示任务名）。失败时 `EndDrain()` 恢复。
* Console 被 kill / 断电：下次启动把遗留运行标记为 `interrupted`，可「从失败处继续」。

## 18. 性能目标与验证

* 压测工具 `etl/bench`（`etl_live && etl_bench` 双标签）：源恒为 PG（`live_postgres`），目标由 `BENCH_TARGET=postgres|mysql` 选择，`BENCH_ROWS`/`BENCH_COMMITS` 控规模与提交档；峰值内存取 `getrusage` 高水位（`rss_posix.go`/`rss_other.go`）。场景：`output`（table_output 纯写）、`iu_update`（insert_update 目标含唯一约束走原生 upsert，全量脏行更新）、`iu_insert`（同步骤全量插入）、`iu_update_case`（目标只有普通索引、无唯一约束，走通用「查→更」路径）。
* R1-M2 通用路径优化（`insert_update` / `update` / `merge` 的按键查找与批量更新）：① `UpdateFromSQL` 派生行集在 PG/Kingbase 改用 `VALUES` 派生表（原 `N×SELECT UNION ALL` 每语句 N 个子查询解析开销大；MySQL/DM/SQLite 无对应语法维持 UNION ALL）；② 查找按键块大小方言化——Oracle 系（达梦）IN 列表上限 1000 取 900，其余 5000（原统一 500 使 5000 行提交串行发 10 条查找，为首要瓶颈）；③ 不做「跳过未变化」比对时查找只取键不取更新列；④ PG 系/SQLite 支持 `UPDATE ... FROM ... RETURNING` 时把「查找已存在」融合进更新语句，每提交少一次往返（`fuseUpdate`，`skipUnchanged=false` 且非 upsert 时启用）。
* 内存上界 ≈ `连线数 × channelBatches × batchSize × 平均行宽` + 有状态步骤预算；编译期打印估算值。

### 实测（2026-11，验收返工轮）

* 机器：Apple M4 / 10 核 / 16GB，macOS 15.7.7，OrbStack Docker。
* 库版本：PostgreSQL 16.4-alpine、MySQL 8.4.13（容器）；达梦 V8、Kingbase V008R003C002B0290 用于 live 功能验证。
* 命令样例：
  `ETL_LIVE_POSTGRES=… ETL_LIVE_MYSQL=… BENCH_TARGET=mysql BENCH_ROWS=10000000 go test -tags "etl_live etl_bench" ./internal/etl/bench -v`
  （PG 目标同理 `BENCH_TARGET=postgres BENCH_ROWS=1000000`，源为 `etl_bench_src`（id/name/amount/ts/flag）。）

**PG 目标 · 100 万行（源同库，`iu_*` 配置 `skipUnchanged=false`）**

| 场景 | commit | 耗时 | 吞吐(行/s) | 峰值RSS |
|---|---|---|---|---|
| output | 1000 | 8s | 124,920 | 47MB |
| output | 5000 | 2s | 394,499 | 57MB |
| output | 20000 | 2s | 416,654 | 76MB |
| iu_update（原生 upsert） | 1000 | 14s | 66,903 | 49MB |
| iu_update | 5000 | 12s | 82,912 | 61MB |
| iu_update | 20000 | 11s | 85,145 | 99MB |
| iu_insert | 5000 | 10s | 97,502 | 99MB |
| iu_update_case（通用路径·融合） | 5000 | 19s | 52,043 | 99MB |

* PG 两种全量更新路径均达验收目标 ≥5 万行/s：原生 upsert 66k–85k；通用路径 52k（优化前 13k，约 4×）。
* `skipUnchanged=true`（默认）时通用路径多一次「按键取回更新列做比对」的往返与扫描，同配置实测约 34k 行/s——语义换吞吐，属预期的可调档位。

**MySQL 目标 · 1000 万行（PG 源 → MySQL，`iu_*` 配置 `skipUnchanged=false`）**

| 场景 | commit | 耗时 | 吞吐(行/s) | 峰值RSS |
|---|---|---|---|---|
| output | 1000 | 2m2s | 81,551 | 39MB |
| output | 5000 | 1m15s | 132,406 | 46MB |
| output | 20000 | 1m7s | 147,973 | 63MB |
| iu_update（原生 upsert） | 1000 | 2m48s | 59,338 | 42MB |
| iu_update | 5000 | 2m4s | 80,008 | 50MB |
| iu_update | 20000 | 1m46s | 93,906 | 82MB |
| iu_insert | 5000 | 1m31s | 109,649 | 82MB |
| iu_update_case（通用路径） | 5000 | 3m15s | 51,259 | 82MB |

**内存不随行数增长（100 万 vs 1000 万，同口径峰值 RSS）**

| 规模 | output 峰值 | iu_update 峰值 | iu_update_case 峰值 |
|---|---|---|---|
| PG 100 万 | 76MB | 99MB | 99MB |
| MySQL 1000 万 | 63MB | 82MB | 82MB |

峰值 RSS 由 `getrusage`（ru_maxrss）测量。1000 万行与 100 万行的峰值同量级（41–99MB），
未见随行数线性增长——流式批处理成立，内存上界符合 §18 估算式。

### 已验证边界

* 通用路径在 MySQL 上仍走「按键查找 + UPDATE...JOIN 派生集」两条语句（无 RETURNING 可融合），
  10M 实测 51k 行/s；达梦同理（UNION ALL + FROM DUAL）。
* `iu_update_case` 吞吐随脏行比例变化：全量更新即本表场景；全量插入由 `iu_insert` 覆盖。

## 19. 测试策略

* 单元：`core`（转换 / schema）、`sqltext`（注释、字面量、`::`）、`expr`（类型推导、NULL）、`secret`（篡改、AAD 错位、密钥丢失）。
* 引擎：用 `generate_rows` + 内存 sink 测背压、取消、错误输出、复制/分发、上游失败传播。
* 端到端：`sqlite` 方言跑 表输入 → 表达式 → 插入/更新 全链路；真机 PG/MySQL/DM 用 testcontainers 之外的固定测试库（离线环境）。
* 下推（R1-B2）：`expr` 单测断言生成 SQL 无值字面量、白名单外结构全部 `ErrNotPushable`；`steps/live_pushdown_test.go`（`etl_live`）在 PG/MySQL/达梦/金仓上对每个允许条件做「开/关下推写入行集合」差分，并对验收反例断言不下推。
* 前端：Playwright 覆盖「新建转换 → 拖入步骤 → 连线 → 校验 → 预览 → 保存 → 运行 → 查看运行详情」。

## 20. 分期

| 阶段 | 内容 |
|---|---|
| P0（本分支骨架） | 内核、编译、运行器、表达式、SQL 参数、凭据、存储、核心步骤 / 作业项、API、全部前端页面 |
| P1 | 调度器完成、SSE 实时、预览完善、历史版本、真机三库类型映射认证、千万级压测与调优 |
| P2 | 排序 / 去重(有序) / 合并记录 / 数据同步 / 流查询 / 值映射 / 更新 / 删除 / CSV / HTTP / HTTP·子作业·通知作业项 |
| P3 | 并行副本、原生 upsert 加速、下推优化器、角色授权 |
