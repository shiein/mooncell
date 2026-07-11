package main

import "testing"

func TestResolvePm2Target(t *testing.T) {
	cases := []struct{ target, cwd, want string }{
		{"/abs/app.jar", "/opt/svc", "/abs/app.jar"}, // 已绝对:不动
		{"app.jar", "/opt/svc", "/opt/svc/app.jar"},  // 相对:按 pm_cwd 绝对化
		{"app.jar", "", "app.jar"},                   // 无 cwd:保持原样(交上层拦)
		{"", "/opt/svc", ""},                         // 空目标
		{"sub/app.jar", "/opt/svc", "/opt/svc/sub/app.jar"},
	}
	for _, c := range cases {
		if got := resolvePm2Target(c.target, c.cwd); got != c.want {
			t.Errorf("resolvePm2Target(%q,%q)=%q, want %q", c.target, c.cwd, got, c.want)
		}
	}
}

// TestParsePm2DeployTarget_RelativeJarUsesPmCwd:java-jar 接管,pm_exec_path 是 java 解释器、
// 启动参数含相对 jar 时,目标须按该进程 pm_cwd 绝对化,而非 Agent 自身工作目录。
func TestParsePm2DeployTarget_RelativeJarUsesPmCwd(t *testing.T) {
	jlist := `[
		{"name":"svc","pm_id":1,"pm2_env":{"pm_exec_path":"/usr/bin/java","pm_cwd":"/opt/svc","args":["-jar","app.jar"]}}
	]`
	got, err := parsePm2DeployTarget(jlist, "svc", "java-jar")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got != "/opt/svc/app.jar" {
		t.Fatalf("相对 jar 应按 pm_cwd 绝对化为 /opt/svc/app.jar,got %q", got)
	}
}

// TestParsePm2DeployTarget_AbsoluteJarUnchanged:绝对 jar 不受 pm_cwd 影响。
func TestParsePm2DeployTarget_AbsoluteJarUnchanged(t *testing.T) {
	jlist := `[
		{"name":"svc","pm_id":1,"pm2_env":{"pm_exec_path":"/usr/bin/java","pm_cwd":"/opt/svc","args":["-jar","/data/app.jar"]}}
	]`
	got, err := parsePm2DeployTarget(jlist, "svc", "java-jar")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got != "/data/app.jar" {
		t.Fatalf("绝对 jar 应保持不变,got %q", got)
	}
}

// TestUnsafePm2NameRejectsNumeric:身份守卫依据——纯数字/all/- 开头判为不安全接管名。
// runDeployPm2 据此拒绝这类接管名,与 status/lifecycle/undeploy 的拒绝口径一致。
func TestUnsafePm2NameRejectsNumeric(t *testing.T) {
	for _, n := range []string{"0", "42", "all", "-rf"} {
		if !isUnsafePm2Name(n) {
			t.Errorf("%q 应判为不安全接管名", n)
		}
	}
	for _, n := range []string{"my-proc", "svc_1", "web.api"} {
		if isUnsafePm2Name(n) {
			t.Errorf("%q 是合法稳定名,不应判不安全", n)
		}
	}
}
