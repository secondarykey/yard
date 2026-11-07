package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	dir, logDir := t.TempDir(), t.TempDir()
	c, err := Parse([]byte("\xef\xbb\xbf"+`
[[tool]]
name = "memo"
path = "/memo/"
command = "bin/memo"
args = ["-port", "{port}"]
log = "logs/memo.log"
`), dir, logDir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Addr != ":8080" {
		t.Errorf("addr = %q", c.Server.Addr)
	}
	tool := c.Tools[0]
	if tool.Path != "/memo" {
		t.Errorf("path = %q", tool.Path)
	}
	if tool.Command != filepath.Join(dir, "bin/memo") {
		t.Errorf("command = %q", tool.Command)
	}
	if tool.Dir != dir {
		t.Errorf("dir = %q", tool.Dir)
	}
	if tool.Log != filepath.Join(logDir, "logs/memo.log") {
		t.Errorf("log = %q", tool.Log)
	}
	if tool.Restart != RestartOnFailure || tool.StartTimeout.Duration != 30*time.Second {
		t.Errorf("restart = %q, timeout = %s", tool.Restart, tool.StartTimeout)
	}
	if !c.Admin.Allowed("127.0.0.1:5000") || !c.Admin.Allowed("[::1]:5000") || c.Admin.Allowed("10.0.0.1:5000") {
		t.Error("default admin.allow should be loopback only")
	}
}

func TestParseCommandName(t *testing.T) {
	c, err := Parse([]byte(`
[[tool]]
name = "a"
path = "/a"
command = "python"
start_timeout = "5s"
`), t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Tools[0].Command != "python" {
		t.Errorf("command without separator should be kept for PATH lookup: %q", c.Tools[0].Command)
	}
	if c.Tools[0].StartTimeout.Duration != 5*time.Second {
		t.Errorf("start_timeout = %s", c.Tools[0].StartTimeout)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key": `
[server]
adr = ":80"`,
		"duplicate name": `
[[tool]]
name = "a"
path = "/a"
command = "x"
[[tool]]
name = "a"
path = "/b"
command = "x"`,
		"is also used by": `
[[tool]]
name = "a"
path = "/a"
command = "x"
[[tool]]
name = "b"
path = "/a/"
command = "x"`,
		"reserved": `
[[tool]]
name = "a"
path = "/.yard/x"
command = "x"`,
		"must start with": `
[[tool]]
name = "a"
path = "a"
command = "x"`,
		"command is required": `
[[tool]]
name = "a"
path = "/a"`,
		"restart must be": `
[[tool]]
name = "a"
path = "/a"
command = "x"
restart = "sometimes"`,
		"not an IP address": `
[admin]
allow = ["localhost"]`,
	}
	for want, src := range cases {
		_, err := Parse([]byte(src), t.TempDir(), t.TempDir())
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}

func TestAdminAllowed(t *testing.T) {
	a := Admin{Allow: []string{"192.168.1.0/24", "10.0.0.5"}}
	if err := a.parse(); err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]bool{
		"192.168.1.20:1":      true,
		"192.168.2.20:1":      false,
		"10.0.0.5:1":          true,
		"10.0.0.6:1":          false,
		"[::ffff:10.0.0.5]:1": true,
		"127.0.0.1:1":         false,
	} {
		if got := a.Allowed(addr); got != want {
			t.Errorf("Allowed(%s) = %v", addr, got)
		}
	}

	all := Admin{Allow: []string{"*"}}
	all.parse()
	if !all.Allowed("8.8.8.8:1") {
		t.Error(`"*" should allow everyone`)
	}
}

func TestTemplateParses(t *testing.T) {
	if _, err := Parse([]byte(Template), t.TempDir(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
