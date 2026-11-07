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
[[server]]
name = "memo"
path = "/memo/"
command = "bin/memo"
args = ["-port", "{port}"]
log = "logs/memo.log"
`), dir, logDir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Yard.Addr != ":8080" {
		t.Errorf("addr = %q", c.Yard.Addr)
	}
	sv := c.Servers[0]
	if sv.Path != "/memo" {
		t.Errorf("path = %q", sv.Path)
	}
	if sv.Command != filepath.Join(dir, "bin/memo") {
		t.Errorf("command = %q", sv.Command)
	}
	if sv.Dir != dir {
		t.Errorf("dir = %q", sv.Dir)
	}
	if sv.Log != filepath.Join(logDir, "logs/memo.log") {
		t.Errorf("log = %q", sv.Log)
	}
	if sv.Restart != RestartOnFailure || sv.StartTimeout.Duration != 30*time.Second {
		t.Errorf("restart = %q, timeout = %s", sv.Restart, sv.StartTimeout)
	}
	if !c.Admin.Allowed("127.0.0.1:5000") || !c.Admin.Allowed("[::1]:5000") || c.Admin.Allowed("10.0.0.1:5000") {
		t.Error("default admin.allow should be loopback only")
	}
}

func TestParseCommandName(t *testing.T) {
	c, err := Parse([]byte(`
[[server]]
name = "a"
path = "/a"
command = "python"
start_timeout = "5s"
`), t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Servers[0].Command != "python" {
		t.Errorf("command without separator should be kept for PATH lookup: %q", c.Servers[0].Command)
	}
	if c.Servers[0].StartTimeout.Duration != 5*time.Second {
		t.Errorf("start_timeout = %s", c.Servers[0].StartTimeout)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown key": `
[yard]
adr = ":80"`,
		"rename [server] to [yard]": `
[server]
addr = ":80"`,
		"[[tool]] to [[server]]": `
[[tool]]
name = "a"`,
		"duplicate name": `
[[server]]
name = "a"
path = "/a"
command = "x"
[[server]]
name = "a"
path = "/b"
command = "x"`,
		"is also used by": `
[[server]]
name = "a"
path = "/a"
command = "x"
[[server]]
name = "b"
path = "/a/"
command = "x"`,
		"reserved": `
[[server]]
name = "a"
path = "/.yard/x"
command = "x"`,
		"must start with": `
[[server]]
name = "a"
path = "a"
command = "x"`,
		"command is required": `
[[server]]
name = "a"
path = "/a"`,
		"restart must be": `
[[server]]
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

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:1234":        true,
		"127.10.0.1:1234":       true,
		"[::1]:1234":            true,
		"[::ffff:127.0.0.1]:80": true,
		"192.168.1.5:1234":      false,
		"[fe80::1%eth0]:1234":   false,
		"10.0.0.1":              false,
		"garbage":               false,
	} {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
