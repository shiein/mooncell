package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// 部署后持续健康巡检 + Agent 资源指标留存。Console 单后台 goroutine 周期性:
//  1. 采各 Agent 资源水位落 metrics 表(供总览画真实历史曲线),并按窗口裁剪;
//  2. 对进程类应用经其 Agent 查真机运行态,把权威 status/pid 写回应用实体,
//     发生掉线/恢复迁移时记审计(本平台面向内网离线,不外发通知)。
//
// 巡检不与「服务端权威写」模型打架:在飞操作(部署/还原/启停/下线)的应用经 busy 集跳过;
// Agent 不可达时绝不臆造 failed(掉线判定只在拿到确定 not-active 响应时成立);
// 只做保守迁移:running→failed(掉线)/ failed|stopped→running(恢复),手动 stopped 不被翻动。

// monitorRunners 是「有真机运行态可查」的进程 runner;static-nginx(软链)/tomcat 不在内。
var monitorRunners = map[string]bool{"systemd": true, "pm2": true, "nohup": true}

// markBusy/unmarkBusy/isBusy:在飞操作引用计数,健康巡检据此跳过,避免把部署/重启中的应用误判掉线。
func (a *api) markBusy(id string) {
	a.busyMu.Lock()
	a.busy[id]++
	a.busyMu.Unlock()
}

func (a *api) unmarkBusy(id string) {
	a.busyMu.Lock()
	if a.busy[id] <= 1 {
		delete(a.busy, id)
	} else {
		a.busy[id]--
	}
	a.busyMu.Unlock()
}

func (a *api) isBusy(id string) bool {
	a.busyMu.Lock()
	defer a.busyMu.Unlock()
	return a.busy[id] > 0
}

// runMonitor 启动巡检循环(阻塞,应在独立 goroutine 调用)。intervalSec<=0 关闭巡检。
func (a *api) runMonitor(intervalSec, keepHours int) {
	if intervalSec <= 0 {
		log.Printf("[monitor] 健康巡检已关闭(monitor.interval_seconds<=0)")
		return
	}
	if keepHours <= 0 {
		keepHours = 24
	}
	interval := time.Duration(intervalSec) * time.Second
	log.Printf("[monitor] 健康巡检启动:周期 %ds,指标保留 %dh", intervalSec, keepHours)
	for {
		a.monitorTick(keepHours)
		time.Sleep(interval)
	}
}

// monitorTick 单轮:采指标 + 巡检应用健康 + 裁剪过期指标。
func (a *api) monitorTick(keepHours int) {
	a.sampleMetrics()
	a.checkAppsHealth()
	a.store.trimMetrics(time.Now().UnixMilli() - int64(keepHours)*3600*1000)
}

// monitorAgentIDs 返回需采集/路由的 Agent id 列表:内置 default + 已注册远端。
func (a *api) monitorAgentIDs() []string {
	ids := []string{"default"}
	if rows, err := a.store.listAgents(); err == nil {
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// sampleMetrics 对每台 Agent 采一次资源水位落库;不可达则跳过(不写 0 值污染曲线)。
func (a *api) sampleMetrics() {
	now := time.Now().UnixMilli()
	for _, id := range a.monitorAgentIDs() {
		cl := a.resolveAgentByID(id)
		if cl == nil {
			continue
		}
		status, body, err := cl.get("/api/system")
		if err != nil || status != http.StatusOK {
			continue
		}
		var s struct {
			CPUPercent  float64 `json:"cpuPercent"`
			MemPercent  float64 `json:"memPercent"`
			DiskPercent float64 `json:"diskPercent"`
		}
		if json.Unmarshal(body, &s) != nil {
			continue
		}
		a.store.insertMetric(id, now, s.CPUPercent, s.MemPercent, s.DiskPercent)
	}
}

// monApp 是巡检关心的应用字段子集。
type monApp struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Runner     string `json:"runner"`
	AgentID    string `json:"agentId"`
	Status     string `json:"status"`
	LastDeploy int64  `json:"lastDeploy"`
}

// checkAppsHealth 巡检所有进程类应用的真机运行态并权威写回。
func (a *api) checkAppsHealth() {
	raws, err := a.store.appsRaw()
	if err != nil {
		return
	}
	for _, raw := range raws {
		var app monApp
		if json.Unmarshal(raw, &app) != nil || app.ID == "" {
			continue
		}
		if !monitorRunners[app.Runner] {
			continue // 无真机运行态可查(static 软链 / tomcat 容器)
		}
		if a.isBusy(app.ID) {
			continue // 部署/还原/启停/下线在飞,跳过避免误判
		}
		cl := a.resolveAgentByID(app.AgentID)
		if cl == nil {
			continue
		}
		status, body, err := cl.get(a.appStatusPath(app.ID, app.Runner))
		if err != nil || status != http.StatusOK {
			continue // Agent/应用不可达:不臆造掉线(可能只是 Agent 网络问题)
		}
		var st struct {
			Active bool   `json:"active"`
			Pid    string `json:"pid"`
			Cpu    string `json:"cpu"`
			Mem    string `json:"mem"`
		}
		if json.Unmarshal(body, &st) != nil {
			continue
		}
		a.applyMonitorState(app, st.Active, st.Pid, st.Cpu, st.Mem)
	}
}

// applyMonitorState 据巡检到的真机运行态保守更新应用实体的权威 status/pid/cpu/mem + lastCheck,
// 仅在 running→failed(掉线)、failed|stopped→running(恢复)两类迁移时改 status 并记审计。
func (a *api) applyMonitorState(app monApp, active bool, pid, cpu, mem string) {
	raw, ok := a.store.getEntity("app", app.ID)
	if !ok {
		return
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	prev, _ := m["status"].(string)
	now := time.Now().UnixMilli()
	m["lastCheck"] = now
	m["lastCheckActive"] = active

	transition := "" // 非空则记审计
	if active {
		if pid != "" && pid != "0" {
			m["pid"] = pid
		}
		m["cpu"] = orDash(cpu)
		m["mem"] = orDash(mem)
		if prev == "failed" || prev == "stopped" {
			m["status"] = "running"
			transition = "恢复运行"
		} else {
			m["status"] = "running" // 已 running:幂等,不记审计
		}
	} else {
		// 仅「本以为在跑」却探到不活动才判掉线;手动 stopped / 已 failed / static 不翻动。
		if prev == "running" {
			m["status"] = "failed"
			m["pid"] = nil
			m["cpu"] = "—"
			m["mem"] = "—"
			transition = "掉线"
		}
	}

	if b, err := json.Marshal(m); err == nil {
		a.store.putEntity("app", app.ID, b)
	}
	if transition != "" {
		name := app.Name
		if name == "" {
			name = app.ID
		}
		a.store.appendAudit("system", "健康巡检", name+" "+transition, boolStr(transition == "恢复运行", "成功", "失败"))
		log.Printf("[monitor] %s(%s)%s", name, app.ID, transition)
	}
}

// listAgentMetrics 处理 GET /api/agents/{id}/metrics?minutes=:返回某 Agent 近 N 分钟资源时序。
func (a *api) listAgentMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = "default"
	}
	minutes := atoiDefault(r.URL.Query().Get("minutes"), 60)
	if minutes < 1 {
		minutes = 60
	}
	if minutes > 1440 {
		minutes = 1440
	}
	since := time.Now().UnixMilli() - int64(minutes)*60*1000
	points, err := a.store.listMetrics(id, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取指标失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"points": points})
}
