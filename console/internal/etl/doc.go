// Package etl 是 Mooncell 的数据集成模块：Kettle / Apache Hop 的现代化简化版。
//
// 权威设计见 docs/etl-design-v1.md。包结构（依赖自上而下）：
//
//	api / service  HTTP 与组装
//	workflow       作业（控制流）：编译 DAG、节点状态、重试、续跑
//	pipeline       转换（数据流）：编译执行计划、goroutine + 有界通道运行
//	steps/actions  内置步骤与作业项（init 注册）
//	core           值类型、行元数据、批次、步骤契约、注册表、变量、度量、日志
//	expr / sqltext 表达式与 SQL 参数语法
//	conn           连接器与方言
//	secret / store 凭据加密与持久化
//	schedule       cron 调度
//
// 本模块与 Console 其它业务（应用部署、数据资源、服务器运维）没有数据或状态耦合。
package etl
