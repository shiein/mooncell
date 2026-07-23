# Mooncell 数据资源模块详细开发方案 v1

## 一、方案摘要与技术决策

在现有 Mooncell Console 内新增“数据资源”模块，继续保持：

- Go `net/http` 后端、React 前端、SQLite 配置存储、单二进制部署。
- 不增加微服务、消息队列、独立 SQL 服务或新的 UI 设计系统。
- 页面直接复用现有 `Shell`、`PageHead`、`Btn`、`Dialog`、`Field`、`Badge`、表格、空状态、Toast、CSS 变量及深浅色主题。
- DataSphere 只参考连接配置、DSN 和元数据适配思路；不复用其中正则只读判断、动态标识符拼接和未完成的写操作代码。

可参考 [WhoDB](https://github.com/clidey/whodb)、[Pgweb](https://github.com/sosedoff/pgweb)、[usql](https://github.com/xo/usql) 的工作台交互和 `database/sql` 驱动组织方式，但不嵌入或二次开发它们。

前端仅新增：

- CodeMirror 6：SQL 编辑、语法高亮、快捷键、基于元数据的补全。
- `sql-formatter`：客户端格式化。
- 不引入新的组件库。

后端仅新增：

- `database/sql` 数据库适配层。
- Excelize 流式生成 XLSX；CSV 使用标准库 `encoding/csv`。
- 不引入 ORM、通用 SQL 平台或重量级多方言解析器。

### 数据库驱动

| 数据库 | 驱动决策 | 说明 |
|---|---|---|
| PostgreSQL | `github.com/jackc/pgx/v5/stdlib`，驱动名 `pgx` | 纯 Go、支持 `database/sql`。[pgx 官方仓库](https://github.com/jackc/pgx) |
| MySQL | `github.com/go-sql-driver/mysql`，驱动名 `mysql` | 纯 Go、支持上下文取消及连接池。[官方仓库](https://github.com/go-sql-driver/mysql) |
| 达梦 DM8 | `gitee.com/chunanyong/dm v1.8.20` | 达梦官方文档说明其驱动实现 `database/sql`，但驱动包以本地包形式提供；按用户授权使用参考项目中的依赖并固定版本。[DM Go 编程指南](https://eco.dameng.com/document/dm/zh-cn/pm/go-rogramming-guide.html) |
| KingbaseES | `gitea.com/kingbase/gokb` 固定到 DataSphere 当前 commit | Gokb 为纯 Go `database/sql` 驱动。[Kingbase Gokb 文档](https://docs.kingbase.com.cn/cn/KES-V9R1C10/application/client_interface/GO/Gokb/go-1) |
| Vastbase G100 | Vastbase 官方 `pq` 本地包，模块 `pq v0.0.0`，匿名导入 `_ "pq"`，驱动名 `openGauss` | 按官方文档执行，不替换成 `github.com/lib/pq`。[设置 pq 驱动](https://docs.vastdata.com.cn/zh_CN/VastbaseG100/V3.0.8/1/25aa20e37ab34ae19f9be93dee18d007)、[连接数据库](https://docs.vastdata.com.cn/zh_CN/VastbaseG100/V3.0.8/1/cf650d9f5c5044698a6ddb1d7b15a228) |
| Oracle | v1 排除 | 官方 `godror` 依赖 CGO 和 Oracle Client，不符合当前纯 Go 单二进制约束。 |

Vastbase 驱动按以下方式落位：

```go
require pq v0.0.0
replace pq => ./third_party/vastbase/pq
```

PostgreSQL 使用 `pgx`，不再引入 PostgreSQL 的 `lib/pq`。开始业务开发前先制作最小验证程序，同时导入 `pgx/stdlib` 和 Vastbase `pq`，确认：

- `sql.Drivers()` 同时包含 `pgx` 与 `openGauss`；
- `CGO_ENABLED=0` 的 Linux amd64/arm64 构建通过；
- 两种数据库均能真实连接。

若该共存验证失败，停止 Vastbase 集成并报告，不自动增加 sidecar 或其他语言子模块。

## 二、数据模型、权限与安全边界

### SQLite 表

沿用当前启动时显式迁移方式，增加 `migrateDataResources`，不引入迁移框架。

1. `data_resources`

   - `id`、`name`、`db_type`
   - `host`、`port`、`database_name`、`default_schema`
   - `username`、`credential_cipher`
   - `ssl_mode`：仅 `disable`、`require`
   - `created_by`、`created_at`、`updated_at`
   - `last_test_status`、`last_test_at`
   - 名称大小写不敏感唯一。

2. `data_resource_grants`

   - 主键：`resource_id + username`
   - `access_mode`：`read` 或 `write`
   - `granted_by`、`granted_at`

3. `saved_sql`

   - `id`、`username`、`resource_id`
   - `name`、`sql_text`
   - `created_at`、`updated_at`
   - 同一用户、同一资源下名称唯一。

工作台和事务状态只保存在内存，不落 SQLite。

### 凭据保护

数据库密码不得明文存储或返回前端：

- 使用 AES-256-GCM，加密内容带版本号和随机 nonce。
- 新增配置 `data_resource.credential_key_file`，默认 `mooncell-data.key`。
- 首次启动且尚无数据资源时自动生成 32 字节随机密钥，权限 `0600`。
- 已存在资源但密钥丢失时拒绝启动，不生成新密钥伪装成功。
- 密钥文件必须与 `mooncell.db` 分开备份。
- API 只返回 `hasPassword`，编辑时空密码表示保留原密码。
- 日志、审计、错误响应中不得出现密码或完整 DSN。

### 权限模型

| 操作 | admin | `write` 授权 | `read` 授权 |
|---|---:|---:|---:|
| 查看资源、元数据、表结构、DDL | 是 | 是 | 是 |
| 执行查询、导出、保存个人 SQL | 是 | 是 | 是 |
| DML、DDL、导入 | 是 | 是 | 否 |
| 新增、编辑、删除、测试连接 | 是 | 否 | 否 |
| 管理资源授权 | 是 | 否 | 否 |

- admin 对所有资源隐式拥有 `write`，不需要授权记录。
- 普通用户只能看到 `data_resource_grants` 中自己的资源。
- 授权统一放在现有“用户管理”页：创建、编辑用户时增加“数据资源授权”区域，每个资源选择“只读”或“读写”。
- 数据资源列表不再增加第二套授权入口。
- `GET /api/users` 增加 `dataResourceGrants`；创建、更新用户时与应用授权一起事务性写入。
- 撤销或降级授权后，立即回滚该用户在对应资源上的活动事务并使工作台失效。
- 账号切换、退出登录、401 时清空资源列表、元数据和工作台状态，禁止显示前一账号缓存。

### 只读模式

只读采用两层限制：

1. 代码预检查负责快速反馈。

   - 明确识别到 `INSERT`、`UPDATE`、`DELETE`、`MERGE`、`TRUNCATE`、DDL、`CALL`、`EXECUTE` 时直接返回 `403 DATA_RESOURCE_READ_ONLY`。
   - 无法可靠分类的 SQL 在只读模式下一律拒绝。
   - 检查器只用于单语句识别和用户提示，不作为最终安全边界。

2. 数据库只读事务负责最终约束。

   - 自动提交模式下，每次只读查询内部使用 `BeginTx(ReadOnly: true)`，执行后自动提交或回滚。
   - 关闭自动提交后，整个手工事务以只读事务创建。
   - Vastbase 若驱动不支持 `TxOptions.ReadOnly`，适配器在固定 `sql.Conn` 上执行厂商支持的 `BEGIN`/`SET TRANSACTION READ ONLY`；无法建立或验证只读事务时拒绝执行。
   - 某驱动未通过真实数据库只读认证时，该类型资源不能向普通用户授予 `read` 权限。

只读用户执行 `DELETE` 或 `TRUNCATE` 的确定行为：

- 正常路径：Mooncell 在发往数据库前返回 403，SQL 不执行。
- 即使绕过第一层分类，数据库只读事务仍应拒绝并回滚。
- 审计记录“只读写入被拒绝”，数据行数和表结构不得变化。
- 推荐资源本身使用数据库只读账号作为第三层保护，但 Mooncell v1 不负责创建数据库账号。

## 三、后端接口与执行模型

### 适配器契约

每种数据库实现同一最小接口：

```go
type DataSourceAdapter interface {
    Ping(ctx context.Context) (ServerInfo, error)
    Begin(ctx context.Context, readOnly bool) (Transaction, error)

    Children(ctx context.Context, parent MetadataNode) ([]MetadataNode, error)
    Describe(ctx context.Context, object MetadataNode) (ObjectStructure, error)
    DDL(ctx context.Context, object MetadataNode) (string, error)
    SQLTemplate(object MetadataNode, operation string) (string, error)

    PageSQL(sql string, limit, offset int) (string, error)
    CountSQL(sql string) (string, error)
    QuoteIdentifier(string) string
    NormalizeError(error) DatabaseError
}
```

约束：

- 动态数据库对象名只能来自已读取的元数据，并由适配器安全引用。
- 元数据过滤值参数化绑定。
- 不在前端拼接 DDL、导入 SQL 或对象限定名。
- 一项能力不支持时由 `Capabilities` 明确关闭菜单，不使用空结果伪装成功。

每个资源维护一个懒加载 `*sql.DB`：

- 默认 `MaxOpenConns=5`、`MaxIdleConns=2`、连接最大生命周期 30 分钟。
- 更新资源配置后关闭旧连接池。
- 存在活动手工事务时禁止更新或删除资源并返回 409；先提交、回滚或等待超时。
- 外部数据库连接池与 Mooncell SQLite 的单连接池完全分离。

### 公共 API

资源管理：

```text
GET    /api/data-resources
POST   /api/data-resources
GET    /api/data-resources/{id}
PUT    /api/data-resources/{id}
DELETE /api/data-resources/{id}

POST   /api/data-resources/test
POST   /api/data-resources/{id}/test
```

`DataResourceInput` 固定字段：

```text
name, dbType, host, port, databaseName, defaultSchema,
username, password?, sslMode
```

不开放任意 DSN 或任意驱动参数。连接测试超时 10 秒，返回：

```text
ok, latencyMs, serverVersion, currentDatabase,
readOnlyTxSupported, errorCode?
```

元数据：

```text
GET  /api/data-resources/{id}/metadata/children?parentId=
GET  /api/data-resources/{id}/metadata/structure?nodeId=
GET  /api/data-resources/{id}/metadata/ddl?nodeId=
POST /api/data-resources/{id}/metadata/sql-template
```

`nodeId` 视为不可信输入，服务端解码后仍重新校验资源、类型和标识符。

工作台：

```text
POST   /api/data-resources/{id}/workspaces
PATCH  /api/data-resources/{id}/workspaces/{workspaceId}/auto-commit
POST   /api/data-resources/{id}/workspaces/{workspaceId}/execute
POST   /api/data-resources/{id}/workspaces/{workspaceId}/commit
POST   /api/data-resources/{id}/workspaces/{workspaceId}/rollback
POST   /api/data-resources/{id}/workspaces/{workspaceId}/export
DELETE /api/data-resources/{id}/workspaces/{workspaceId}
```

保存 SQL：

```text
GET    /api/data-resources/{id}/saved-sql
POST   /api/data-resources/{id}/saved-sql
PUT    /api/data-resources/{id}/saved-sql/{sqlId}
DELETE /api/data-resources/{id}/saved-sql/{sqlId}
```

服务端始终以当前登录用户作为所有者，不接受请求体中的用户名。

导入：

```text
POST   /api/data-resources/{id}/imports/preview
POST   /api/data-resources/{id}/imports/{importId}/execute
DELETE /api/data-resources/{id}/imports/{importId}
```

统一错误结构：

```json
{
  "error": "用户可读错误",
  "code": "STABLE_ERROR_CODE",
  "sqlState": "可选且已脱敏",
  "txState": "none|active|failed"
}
```

### SQL 执行规则

- 一次请求只允许一条 SQL；服务端独立识别引号、注释和 PostgreSQL dollar quote，拒绝多语句。
- 编辑器有选区时执行选区，否则执行光标所在语句。
- v1 不提供“执行整个脚本”。
- 显式 `BEGIN`、`COMMIT`、`ROLLBACK`、`SAVEPOINT` 一律拒绝，事务只能通过工作台按钮控制。
- `SELECT` 默认返回 100 行，接口最大允许 500 行。
- 默认尝试用适配器的 `COUNT(*) FROM (<query>)` 返回总数；统计失败或超时时仍返回数据，并标记 `totalStatus=unavailable`，不启动后台任务。
- 自动提交查询和只读事务中的统计、分页在同一只读事务内执行。
- 可写手工事务中的任意 SQL 不重复执行；此时任意查询不自动统计总数，避免具有副作用的函数被调用两次。
- `TRUNCATE`、`DROP`、无 `WHERE` 的 `DELETE/UPDATE` 在读写模式也要求二次确认。
- DDL、DCL、`TRUNCATE` 和过程调用只允许自动提交模式；关闭自动提交时拒绝，避免 MySQL、DM 等数据库的隐式提交破坏已有事务。

执行响应包括：

```text
executionId, statementType, columns, rows,
returnedRows, total, totalStatus, hasMore,
affectedRows, durationMs, messages, txState
```

大整数、DECIMAL 使用字符串传输，避免 JavaScript 精度丢失；二进制字段以类型标识和截断预览返回，不把大型 BLOB 全量放进 JSON。

### 事务状态

- 默认 `autoCommit=true`，每条写 SQL 独立提交，“提交”和“回滚”按钮禁用。
- 用户关闭自动提交后，第一条 SQL 才创建事务。
- 只有 `autoCommit=false && txState=active` 时“提交”按钮可用。
- 提交或回滚后事务结束；如果自动提交仍关闭，下一条 SQL 再创建新事务。
- 活动事务未处理前不能重新开启自动提交。
- 同一工作台同一时间只执行一个请求。
- 手工事务空闲 15 分钟自动回滚。
- 退出登录、会话失效、授权撤销、资源删除、Console 重启均不得提交未完成事务，只能回滚或由连接断开触发数据库回滚。
- 查询取消使用浏览器 `AbortController` 和请求上下文取消；取消后根据驱动实际状态返回 `active` 或 `failed`。

## 四、页面与功能设计

### 导航和资源列表

- 侧栏在“应用”之后增加“数据资源”，所有登录用户可见。
- 非 admin 路由白名单增加 `data-resources`、`data-workbench`。
- 列表继续使用现有 `PageHead + card + table`：
  - 名称
  - 数据库类型
  - 地址
  - 数据库
  - 连接状态
  - 当前权限
  - 更新时间
- 普通用户只有“进入”操作。
- admin 增加“新建、编辑、测试、删除”。
- 新建和编辑使用现有 `Dialog`、`Field`、`Btn`，不制作新的表单体系。
- 删除要求输入资源名称确认。
- 无资源、未授权、连接失败等状态使用现有 `EmptyState` 和 Toast。

### 工作台

工作台保留现有侧栏和顶栏，内容区改为全宽、无额外大卡片：

- 左侧元数据树默认约 280px，可拖动调整、可折叠。
- 右侧上方为 SQL 编辑器，下方为结果/消息区，分隔条可拖动。
- 尺寸仅保存在当前浏览器，不进入后端。
- CodeMirror 主题直接映射 Mooncell CSS 变量，字体使用现有 `IBM Plex Mono`，按钮、菜单、边框、圆角均沿用当前规范。
- 窄屏下左侧树折叠为抽屉，不重新设计移动端工作台。

工具栏固定为：

```text
执行 | 取消 | 自动提交开关 | 提交 | 回滚 |
格式化 | 导出 | 保存 SQL | 加载 SQL
```

格式化方言映射：

- PostgreSQL、Kingbase、Vastbase → PostgreSQL
- MySQL → MySQL
- DM → PL/SQL

格式化失败时保留原 SQL，并明确提示，不修改后端执行内容。

### 元数据树

一个数据资源只绑定一个数据库，禁止通过树切换到其他数据库；其他数据库需创建独立资源，避免授权范围扩大。

- PostgreSQL、Kingbase、Vastbase：数据库 → schema → 表、视图、物化视图、函数、序列。
- MySQL：数据库 → 表、视图、函数、存储过程、触发器。
- DM：数据库 → schema/owner → 表、视图、函数、过程、序列。
- 节点按需加载，并提供手动刷新，不增加复杂缓存。

表右键菜单：

- 查看表结构
- 查看/导出 DDL
- 生成 SELECT
- 生成 INSERT、UPDATE、DELETE，仅 `write` 可用
- 导入数据，仅 `write` 可用

生成 SQL 只插入编辑器，不自动执行。

表结构展示：

- 字段：名称、完整类型、可空、默认值、注释
- 主键、唯一约束、外键、检查约束
- 索引
- DDL

PostgreSQL 系表不提供完整 `SHOW CREATE TABLE`，适配器生成的 v1 DDL只保证字段、默认值、主外键、唯一约束、检查约束、索引和注释；不承诺完整还原权限、Owner、表空间、分区和厂商扩展参数。

### 导入与导出

导入：

- 仅 CSV、XLSX。
- 默认最大 100MB，可配置。
- 先上传预览前 20 行，再选择工作表、表头和字段映射。
- 仅执行参数化 `INSERT`，不支持 upsert、忽略冲突或部分成功。
- 默认每批 500 行，在一个独立事务内执行；任意一行失败则全部回滚，并返回失败行号和字段。
- 有活动手工事务时禁止导入。
- 临时文件在完成、取消或 30 分钟超时后删除。

导出：

- 结果区可选择“当前 100 行”或“全部结果”。
- 支持 UTF-8 CSV 和 XLSX。
- 全量导出重新执行最近一次成功的查询；自动提交下数据可能与屏幕快照有时间差。
- 同步流式输出，不增加后台任务。
- 默认限制 20 万行或 200MB，先到者终止并返回明确错误。
- DDL 导出为 UTF-8 `.sql`。

### 保存 SQL

- 每个用户只能查看、修改、删除自己的 SQL。
- SQL 默认绑定当前资源。
- 保存字段只有名称和 SQL 内容，不增加文件夹、标签、共享、版本历史。
- 资源授权被撤销后无法加载；重新授权后原 SQL 仍可使用。
- 删除资源时同时删除其保存 SQL。

### 审计

复用现有审计能力，不新增审计子系统：

- 记录资源增删改、测试、授权变化、DML/DDL、导入、提交、回滚和只读拦截。
- 普通 SELECT 不逐条写审计，避免淹没现有日志。
- SQL 审计只保存资源、用户、语句类型、SQL 哈希、结果和耗时，不保存完整 SQL、参数值或结果数据。

## 五、实施顺序、测试和验收

### 实施顺序

1. 驱动和构建门槛

   - 完成五种驱动的最小编译测试。
   - 优先验证 `pgx + Vastbase pq` 同二进制共存。
   - 验证 Linux amd64/arm64 的 `CGO_ENABLED=0` 构建。

2. 存储与权限

   - 增加三张 SQLite 表、凭据加密、资源 CRUD、连接测试。
   - 扩展用户管理的数据资源授权。
   - 完成 API 和前端路由的 fail-closed 权限校验。

3. 元数据和只读工作台

   - 完成适配器基础接口、元数据树、表结构、DDL、SQL 编辑器。
   - 完成 SELECT 100 行、总数、消息区和 CSV/XLSX 导出。

4. 写操作和事务

   - 完成 `read/write` 权限、自动提交开关、提交、回滚、超时回滚。
   - 完成危险 SQL 确认和数据库只读事务验证。

5. 辅助功能

   - 完成 SQL 模板、个人 SQL、CSV/XLSX 导入。
   - 完成审计、错误归一化和界面收口。

### 自动化测试

后端单元测试：

- 资源 CRUD、密码不回传、密钥丢失拒绝启动。
- admin 全量、普通用户授权过滤、未授权直接 403。
- 用户授权更新事务性、一处失败不留半成品。
- 多语句、显式事务 SQL、只读写操作均被拒绝。
- 自动提交和手工事务状态机。
- 超时、退出、撤权、资源关闭触发回滚。
- 元数据标识符安全引用。
- CSV/XLSX 导入失败整批回滚。
- 导出行数和字节数限制。
- 保存 SQL 用户隔离。

前端 E2E：

- admin 创建、编辑、测试、删除资源。
- 普通用户只能看到授权资源。
- 同浏览器切换账号不显示上一用户资源或元数据。
- 撤权后已打开工作台立即失效。
- 自动提交开启时提交按钮禁用；关闭且执行后启用。
- 只读用户执行 `DELETE`、`TRUNCATE` 显示明确拒绝。
- 深色、浅色主题与现有页面一致。
- 工作台拖动、折叠、空状态、错误状态正常。

真实数据库认证，每种数据库至少覆盖：

- 连接成功、密码错误、超时。
- schema、表、视图、函数等元数据读取。
- 表结构、DDL、SQL 模板。
- SELECT 默认 100 行和总数。
- 自动提交 DML。
- 手工事务 commit、rollback、超时 rollback。
- 只读事务中 `DELETE`、`TRUNCATE` 均失败且数据不变。
- 中文、NULL、DECIMAL、时间、二进制字段。
- CSV/XLSX 导入导出。

只有真实产品版本通过后才在发布说明中标记为“已认证”；驱动可编译或单元测试通过不等于数据库兼容认证。

### 明确边界和默认值

- 一个资源只对应一个数据库。
- MySQL v1 以 8.0+ 为目标；PostgreSQL 以 14+ 为目标。
- DM、Kingbase、Vastbase 的最终支持版本以实际部署环境的真机认证记录为准。
- 不支持 Oracle、SSH 隧道、跨数据库查询、可视化改单元格、SQL 分享、脚本批量执行、后台导出任务、数据库账号创建和完整 schema 设计器。
- 不承诺执行包含客户端专用 `DELIMITER`、复杂过程创建脚本的 SQL。
- 不为某个数据库的特例破坏统一事务行为。
