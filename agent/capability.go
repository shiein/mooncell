package main

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Capability 是 Agent 启动自检上报的一项能力;Console 据此过滤可选 Runner(不可用项置灰)。
type Capability struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	OK    bool   `json:"ok"`
	Ver   string `json:"ver"`
}

// probe 定义一项能力如何探测:命令 + 参数,输出里用正则抽版本号。
type probe struct {
	key, label, bin string
	args            []string
	verRe           *regexp.Regexp
}

var verNum = regexp.MustCompile(`\d+(\.\d+)+`)

var probes = []probe{
	{"systemd", "systemd", "systemctl", []string{"--version"}, verNum},
	{"java", "Java", "java", []string{"-version"}, verNum}, // java 把版本写到 stderr
	{"pm2", "pm2", "pm2", []string{"--version"}, verNum},
	{"nginx", "nginx", "nginx", []string{"-v"}, verNum}, // nginx 把版本写到 stderr
	{"python", "Python", "python3", []string{"--version"}, verNum},
	{"node", "Node", "node", []string{"--version"}, verNum},
}

// detectCapabilities 逐项探测;探不到的(如未装 pm2 / tomcat)标记 ok=false,Console 端置灰。
func detectCapabilities() []Capability {
	caps := make([]Capability, 0, len(probes)+1)
	for _, p := range probes {
		caps = append(caps, runProbe(p))
	}
	caps = append(caps, detectTomcat())
	return caps
}

func runProbe(p probe) Capability {
	c := Capability{Key: p.key, Label: p.label, Ver: "未检测到"}
	if _, err := exec.LookPath(p.bin); err != nil {
		return c
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// 版本类命令有的走 stdout 有的走 stderr,合并取。
	out, _ := exec.CommandContext(ctx, p.bin, p.args...).CombinedOutput()
	c.OK = true
	if m := p.verRe.Find(out); m != nil {
		c.Ver = string(m)
	} else {
		c.Ver = "已安装"
	}
	return c
}

// detectTomcat 没有统一的命令,靠 CATALINA_HOME 或常见安装目录判断。
func detectTomcat() Capability {
	c := Capability{Key: "tomcat", Label: "Tomcat", Ver: "未检测到"}
	home := os.Getenv("CATALINA_HOME")
	candidates := []string{home, "/opt/tomcat", "/usr/local/tomcat"}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if fi, err := os.Stat(dir + "/bin/catalina.sh"); err == nil && !fi.IsDir() {
			c.OK = true
			if ver := readTomcatVersion(dir); ver != "" {
				c.Ver = ver
			} else {
				c.Ver = "已安装"
			}
			return c
		}
	}
	return c
}

func readTomcatVersion(dir string) string {
	b, err := os.ReadFile(dir + "/RELEASE-NOTES")
	if err != nil {
		return ""
	}
	if i := strings.Index(string(b), "Tomcat Version"); i >= 0 {
		if m := verNum.Find(b[i:]); m != nil {
			return string(m)
		}
	}
	return ""
}
