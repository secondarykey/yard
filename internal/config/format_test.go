package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFormatRoundTrip(t *testing.T) {
	in := &Config{
		Yard:    Yard{Addr: ":9090", AccessLog: true, Log: true},
		Admin:   Admin{Allow: []string{"127.0.0.1", "10.0.0.0/8"}},
		Service: Service{Name: "yard", Description: `say "hi"`, UserService: true},
		Servers: []Server{
			{
				Name:         "memo",
				Path:         "/memo",
				Command:      `C:\tools\memo.exe`,
				Args:         []string{"-port", "{port}", "it's", "tab\there", "日本語"},
				Env:          []string{`DATA=D:\data`, "LINE=a\nb"},
				Log:          true,
				StripPrefix:  true,
				LocalOnly:    true,
				HTTPS:        true,
				Restart:      RestartAlways,
				StartTimeout: Duration{90 * time.Second},
				Disabled:     true,
			},
			{Name: "min", Path: "/", Command: "min"},
		},
	}
	text := Format(in)
	out, err := Decode(text)
	if err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip differs:\n%#v\n%#v\n%s", in, out, text)
	}
	if !strings.Contains(string(text), `command = 'C:\tools\memo.exe'`) {
		t.Errorf("windows path should be a literal string:\n%s", text)
	}
	if _, err := Parse(text, t.TempDir(), t.TempDir()); err != nil {
		t.Error(err)
	}
}

func TestFormatOmitsEmpty(t *testing.T) {
	got := string(Format(&Config{Admin: Admin{Allow: []string{}}, Servers: []Server{{}}}))
	if got != "[[server]]\n" {
		t.Errorf("got %q", got)
	}
}

func TestFormatTemplate(t *testing.T) {
	c, err := Decode([]byte(Template))
	if err != nil {
		t.Fatal(err)
	}
	again, err := Decode(Format(c))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, again) {
		t.Errorf("template round trip differs:\n%#v\n%#v", c, again)
	}
}

func TestLegacyLogHint(t *testing.T) {
	_, err := Decode([]byte("[yard]\nlog = \"yard.log\"\n"))
	if err == nil || !strings.Contains(err.Error(), "now true / false") {
		t.Errorf("err = %v", err)
	}
}

func TestEmptyDuration(t *testing.T) {
	c, err := Parse([]byte("[[server]]\nname = \"a\"\npath = \"/a\"\ncommand = \"a\"\nstart_timeout = \"\"\n"), t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if c.Servers[0].StartTimeout.Duration != 30*time.Second {
		t.Errorf("start_timeout = %v", c.Servers[0].StartTimeout)
	}
}
