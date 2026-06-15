// 集成测试用的极小制品:一个 HTTP 服务,用于在真机上验证 native-binary Deployer 的部署闭环。
// 放在 testdata/ 下,Go 工具链构建 agent 时会忽略本目录。
package main

import (
	"fmt"
	"net/http"
	"os"
)

// version 由 -ldflags "-X main.version=..." 注入;health 控制 /healthz 返回码(模拟健康/异常)。
var (
	version = "dev"
	health  = "ok" // "ok" → 200;其它 → 503,用于验证健康检查失败 + 自动回滚
)

func main() {
	addr := os.Getenv("APP_ADDR")
	if addr == "" {
		addr = ":18080"
	}
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if health == "ok" {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "healthy %s\n", version)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "unhealthy %s\n", version)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "demoapp %s\n", version)
	})
	fmt.Printf("demoapp %s listening on %s\n", version, addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
