package main

import (
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/mem"
)

// SystemInfo 是 Agent 上报的资源水位;Console 总览页据此画 CPU/内存曲线、磁盘水位条与告警。
type SystemInfo struct {
	CPUPercent  float64 `json:"cpuPercent"`
	MemPercent  float64 `json:"memPercent"`
	MemUsedMB   uint64  `json:"memUsedMB"`
	MemTotalMB  uint64  `json:"memTotalMB"`
	DiskPercent float64 `json:"diskPercent"`
	DiskUsedGB  uint64  `json:"diskUsedGB"`
	DiskTotalGB uint64  `json:"diskTotalGB"`
}

// readSystem 采集一次资源水位。CPU 取 300ms 采样窗口,避免阻塞太久又能拿到瞬时值。
// diskPath 为备份/部署所在磁盘的探测点(默认根分区)。
func readSystem(diskPath string) SystemInfo {
	var s SystemInfo

	if pcts, err := cpu.Percent(300*time.Millisecond, false); err == nil && len(pcts) > 0 {
		s.CPUPercent = round1(pcts[0])
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		s.MemPercent = round1(vm.UsedPercent)
		s.MemUsedMB = vm.Used / 1024 / 1024
		s.MemTotalMB = vm.Total / 1024 / 1024
	}
	if du, err := disk.Usage(diskPath); err == nil {
		s.DiskPercent = round1(du.UsedPercent)
		s.DiskUsedGB = du.Used / 1024 / 1024 / 1024
		s.DiskTotalGB = du.Total / 1024 / 1024 / 1024
	}
	return s
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
