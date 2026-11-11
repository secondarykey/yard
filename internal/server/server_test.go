package server

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yard/internal/config"
	"yard/internal/proc"
)

// The test binary itself is started as a server. With YARD_TEST_HELPER=1 it runs
// no tests and becomes a small server listening on YARD_PORT.
func TestMain(m *testing.M) {
	if os.Getenv("YARD_TEST_HELPER") == "1" {
		helper()
		return
	}
	os.Exit(m.Run())
}

func helper() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/exit") {
			go func() { time.Sleep(50 * time.Millisecond); os.Exit(3) }()
			return
		}
		if strings.HasSuffix(r.URL.Path, "/redirect") {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		fmt.Fprintf(w, "path=%s raw=%s host=%s prefix=%s xff=%s proto=%s arg=%s base=%s fwd=%s realip=[%s] ae=[%s]",
			r.URL.Path, r.URL.RawPath, r.Host, r.Header.Get("X-Forwarded-Prefix"),
			r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Proto"),
			strings.Join(os.Args[1:], ","), os.Getenv("YARD_BASE"),
			strings.Join(r.Header.Values("Forwarded"), "|"),
			r.Header.Get("X-Real-Ip")+r.Header.Get("True-Client-Ip"), r.Header.Get("Accept-Encoding"))
	})
	fmt.Println("helper up")
	addr := "127.0.0.1:" + os.Getenv("YARD_PORT")
	if os.Getenv("YARD_TEST_TLS") == "1" {
		// HTTPS with httptest's self-signed certificate.
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		ts := httptest.NewUnstartedServer(mux)
		ts.Listener.Close()
		ts.Listener = ln
		ts.StartTLS()
		select {}
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func newTestServer(t *testing.T, servers string) (*Server, *httptest.Server, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src := strings.ReplaceAll(servers, "EXE", strings.ReplaceAll(exe, `\`, `\\`))
	src = "[yard]\naccess_log = true\n" + src
	path := filepath.Join(dir, config.FileName)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(path, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		s.Close()
	})
	return s, ts, dir
}

const echoServer = `
[[server]]
name = "echo"
path = "/echo"
command = "EXE"
args = ["-p", "{port}", "{base}"]
env = ["YARD_TEST_HELPER=1"]
log = true
start_timeout = "10s"
`

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header
}

func waitState(t *testing.T, p *proc.Proc, want proc.State) proc.Status {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := p.Status()
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %s (%s), want %s", st.State, st.Error, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestProxy(t *testing.T) {
	s, ts, dir := newTestServer(t, echoServer)

	// Even while starting, the first request waits until the port opens.
	code, body, _ := get(t, ts.URL+"/echo/a/b?q=1")
	if code != 200 {
		t.Fatalf("code = %d, body = %s", code, body)
	}
	p, _ := s.mgr.Get("echo")
	st := p.Status()
	for _, want := range []string{
		"path=/echo/a/b ",
		"prefix=/echo ",
		"xff=127.0.0.1",
		fmt.Sprintf("arg=-p,%d,/echo/", st.Port),
		"base=/echo/",
		"host=" + strings.TrimPrefix(ts.URL, "http://"),
		`fwd=for=127.0.0.1;host="` + strings.TrimPrefix(ts.URL, "http://") + `";proto=http`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q does not contain %q", body, want)
		}
	}

	// Forwarding and client IP headers sent by the client are replaced or dropped, not passed on
	// or appended to, and Yard does not ask for gzip on its own when the client did not.
	req, _ := http.NewRequest("GET", ts.URL+"/echo/", nil)
	req.Header.Set("Forwarded", "for=192.0.2.1")
	req.Header.Set("X-Forwarded-For", "192.0.2.1")
	req.Header.Set("X-Real-IP", "192.0.2.1")
	req.Header.Set("True-Client-IP", "192.0.2.1")
	res, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if body := string(b); strings.Contains(body, "192.0.2.1") || !strings.Contains(body, "fwd=for=127.0.0.1;") {
		t.Errorf("forged forwarding headers: body %q", body)
	}
	for _, want := range []string{"realip=[]", "ae=[]", "xff=127.0.0.1 "} {
		if !strings.Contains(string(b), want) {
			t.Errorf("body %q does not contain %q", b, want)
		}
	}

	// Non-matching path
	if code, _, _ := get(t, ts.URL+"/echox"); code != 404 {
		t.Errorf("/echox = %d", code)
	}
	// Without a "/" server, the root is 404 too
	if code, _, _ := get(t, ts.URL+"/"); code != 404 {
		t.Errorf("/ = %d", code)
	}

	// Server output ({name}-{pid}.log) and access log
	logName := filepath.Join(dir, fmt.Sprintf("echo-%d.log", st.PID))
	if p.LogFile() != logName {
		t.Errorf("log file = %q, want %q", p.LogFile(), logName)
	}
	if b, _ := os.ReadFile(logName); !strings.Contains(string(b), "helper up") || !strings.HasPrefix(string(b), "=== yard: echo starting port=") {
		t.Errorf("server log = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "echo-starting.log")); !os.IsNotExist(err) {
		t.Errorf("temporary log name left behind: %v", err)
	}

	// From this machine the port links to the server itself.
	direct := fmt.Sprintf("http://127.0.0.1:%d/echo/", st.Port)
	if st.Direct != direct {
		t.Errorf("direct = %q, want %q", st.Direct, direct)
	}
	if _, body, _ := get(t, ts.URL+"/.yard/"); !strings.Contains(body, `href="`+direct+`"`) {
		t.Error("the port should link to the server itself")
	}
	if code, body, _ := get(t, direct+"a"); code != 200 || !strings.Contains(body, "path=/echo/a ") {
		t.Errorf("direct = %d %s", code, body)
	}

	// The access log and Yard's own log can be read from /.yard/logs.
	if _, body, _ := get(t, ts.URL+"/.yard/logs/access?raw"); !strings.Contains(body, `"GET /echo/a/b?q=1 HTTP/1.1" 200`) {
		t.Errorf("access log page = %q", body)
	}
	if _, body, _ := get(t, ts.URL+"/.yard/"); !strings.Contains(body, "/.yard/logs/access") || strings.Contains(body, "/.yard/logs/yard") {
		t.Error("the server list should link the access log only")
	}
	yardLog := filepath.Join(dir, "yard_test.log")
	os.WriteFile(yardLog, []byte("hello from yard\n"), 0o644)
	s.SetYardLog(func() string { return yardLog })
	if _, body, _ := get(t, ts.URL+"/.yard/logs/yard?raw"); body != "hello from yard\n" {
		t.Errorf("yard log page = %q", body)
	}
	if code, _, _ := get(t, ts.URL+"/.yard/logs/other"); code != 404 {
		t.Errorf("/.yard/logs/other = %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "access_"+time.Now().Format("20060102")+".log")); !strings.Contains(string(b), `"GET /echo/a/b?q=1 HTTP/1.1" 200`) ||
		!strings.Contains(string(b), " echo\n") {
		t.Errorf("access log = %q", b)
	}
}

func TestStripPrefix(t *testing.T) {
	_, ts, _ := newTestServer(t, strings.Replace(echoServer, "start_timeout", "strip_prefix = true\nstart_timeout", 1))

	code, _, h := get(t, ts.URL+"/echo?x=1")
	if code != http.StatusMovedPermanently || h.Get("Location") != "/echo/?x=1" {
		t.Errorf("redirect = %d %q", code, h.Get("Location"))
	}
	if _, body, _ := get(t, ts.URL+"/echo/a%2Fb"); !strings.Contains(body, "path=/a/b raw=/a%2Fb ") {
		t.Errorf("body = %q", body)
	}
	if code, _, h := get(t, ts.URL+"/echo/redirect"); code != http.StatusFound || h.Get("Location") != "/echo/target" {
		t.Errorf("location = %d %q", code, h.Get("Location"))
	}
}

func TestRestartOnFailure(t *testing.T) {
	s, ts, _ := newTestServer(t, echoServer)
	p, _ := s.mgr.Get("echo")
	first := waitState(t, p, proc.StateRunning)

	get(t, ts.URL+"/echo/exit")
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := p.Status()
		if st.State == proc.StateRunning && st.PID != first.PID {
			if st.Restarts != 1 {
				t.Errorf("restarts = %d", st.Restarts)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not restarted: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code, _, _ := get(t, ts.URL+"/echo/"); code != 200 {
		t.Errorf("after restart = %d", code)
	}
}

// enable / disable / log-on / log-off change yard.ini and apply it.
func TestServerSettings(t *testing.T) {
	s, ts, dir := newTestServer(t, "# my comment\n"+echoServer)
	post := func(action string) proc.Status {
		t.Helper()
		res, err := http.Post(ts.URL+"/.yard/api/servers/echo/"+action, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			b, _ := io.ReadAll(res.Body)
			t.Fatalf("%s = %d %s", action, res.StatusCode, b)
		}
		p, _ := s.mgr.Get("echo")
		return p.Status()
	}
	ini := func() string {
		b, _ := os.ReadFile(filepath.Join(dir, config.FileName))
		return string(b)
	}
	p, _ := s.mgr.Get("echo")
	waitState(t, p, proc.StateRunning)

	// A stopped server stays stopped when its log is turned off.
	post("stop")
	if st := post("log-off"); st.State != proc.StateStopped || st.Log {
		t.Errorf("after log-off: %+v", st)
	}
	if c := ini(); !strings.Contains(c, "log = false") || !strings.Contains(c, "# my comment") {
		t.Errorf("yard.ini =\n%s", c)
	}

	if st := post("disable"); st.State != proc.StateDisabled {
		t.Errorf("after disable: %+v", st)
	}
	post("enable")
	p, _ = s.mgr.Get("echo")
	first := waitState(t, p, proc.StateRunning)

	// Turning the log on restarts a running server, which then writes {name}-{pid}.log.
	post("log-on")
	p, _ = s.mgr.Get("echo")
	st := waitState(t, p, proc.StateRunning)
	if st.PID == first.PID || !st.Log || p.LogFile() != filepath.Join(dir, fmt.Sprintf("echo-%d.log", st.PID)) {
		t.Errorf("after log-on: %+v, %s", st, p.LogFile())
	}
	if c := ini(); !strings.Contains(c, "log = true") || strings.Contains(c, "disabled = true") {
		t.Errorf("yard.ini =\n%s", c)
	}
}

func TestAdmin(t *testing.T) {
	s, ts, dir := newTestServer(t, echoServer)
	p, _ := s.mgr.Get("echo")
	waitState(t, p, proc.StateRunning)

	// After stopping: 503, and the health check is 503 too
	res, err := http.Post(ts.URL+"/.yard/api/servers/echo/stop", "", nil)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("stop: %v %v", res, err)
	}
	if code, _, _ := get(t, ts.URL+"/echo/"); code != http.StatusServiceUnavailable {
		t.Errorf("stopped server = %d", code)
	}
	if code, _, _ := get(t, ts.URL+"/.yard/health/echo"); code != http.StatusServiceUnavailable {
		t.Errorf("health of stopped server = %d", code)
	}
	if code, body, _ := get(t, ts.URL+"/.yard/health"); code != 200 || !strings.Contains(body, `"stopped"`) {
		t.Errorf("health = %d %s", code, body)
	}

	// Pages
	for _, u := range []string{"/.yard/", "/.yard/config", "/.yard/config?view=text", "/.yard/servers/echo/log", "/.yard/matrix"} {
		if code, body, _ := get(t, ts.URL+u); code != 200 || !strings.Contains(body, "<html") {
			t.Errorf("%s = %d", u, code)
		}
	}
	if code, _, h := get(t, ts.URL+"/.yard"); code/100 != 3 || h.Get("Location") != "/.yard/" {
		t.Errorf("/.yard = %d %q", code, h.Get("Location"))
	}

	// An invalid configuration is not saved
	path := filepath.Join(dir, config.FileName)
	before, _ := os.ReadFile(path)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/.yard/api/config", strings.NewReader("[[server]]\nname = 1\n"))
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid config: %v %v", res, err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("invalid config was written")
	}

	// The form editor sends JSON, which is written out as TOML
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/.yard/api/config",
		strings.NewReader(`{"yard":{"addr":":0"},"admin":{"allow":[]},"server":[{"name":"x","path":"/x","command":"x","start_timeout":"1m","disabled":true}]}`))
	req.Header.Set("Content-Type", "application/json")
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("json config: %v %v", res, err)
	}
	want := `[yard]
addr = ":0"

[[server]]
name = "x"
path = "/x"
command = "x"
start_timeout = "1m0s"
disabled = true
`
	if after, _ := os.ReadFile(path); string(after) != want {
		t.Errorf("json config written as:\n%s", after)
	}
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/.yard/api/config", strings.NewReader(`{"server":[{"nme":"x"}]}`))
	req.Header.Set("Content-Type", "application/json")
	if res, err = http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown json field: %v %v", res, err)
	}

	// A valid configuration is saved and applied (the server goes away)
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/.yard/api/config", strings.NewReader("[yard]\n"))
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("valid config: %v %v", res, err)
	}
	if _, ok := s.mgr.Get("echo"); ok {
		t.Error("server should be removed")
	}

	// Form submissions from other sites are rejected
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/.yard/reload", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site post: %v %v", res, err)
	}
}

func TestAdminRestrictedByRemoteAddr(t *testing.T) {
	s, _, _ := newTestServer(t, "")
	h := s.Handler()

	for path, want := range map[string]int{
		"/.yard/":            http.StatusForbidden,
		"/.yard/api/servers": http.StatusForbidden,
		"/.yard/health":      http.StatusOK,
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "10.1.2.3:4567"
		r.Header.Set("X-Forwarded-For", "127.0.0.1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s = %d, want %d", path, w.Code, want)
		}
	}
}

func TestCleanPathRedirect(t *testing.T) {
	s, _, _ := newTestServer(t, "")
	r := httptest.NewRequest(http.MethodGet, "/a/../.yard/", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/.yard/" {
		t.Errorf("= %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestLocalOnly(t *testing.T) {
	s, ts, _ := newTestServer(t, strings.Replace(echoServer, "log = true", "local_only = true", 1))
	h := s.Handler()

	// From this machine it is forwarded as usual (httptest listens on loopback).
	if code, body, _ := get(t, ts.URL+"/echo/"); code != 200 || !strings.Contains(body, "path=/echo/") {
		t.Errorf("loopback = %d %s", code, body)
	}

	// From the LAN it is refused, even with a forged X-Forwarded-For.
	r := httptest.NewRequest(http.MethodGet, "/echo/", nil)
	r.RemoteAddr = "192.168.1.5:4567"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("lan = %d %s", w.Code, w.Body)
	}

	p, _ := s.mgr.Get("echo")
	if !p.Status().LocalOnly {
		t.Error("status should report local_only")
	}
}

func TestHTTPSServer(t *testing.T) {
	const tlsServer = `
[[server]]
name = "echo"
path = "/echo"
command = "EXE"
env = ["YARD_TEST_HELPER=1", "YARD_TEST_TLS=1"]
log = true
start_timeout = "10s"
https = true
`
	s, ts, _ := newTestServer(t, tlsServer)
	code, body, _ := get(t, ts.URL+"/echo/a")
	if code != 200 || !strings.Contains(body, "path=/echo/a ") ||
		!strings.Contains(body, "host="+strings.TrimPrefix(ts.URL, "http://")) {
		p, _ := s.mgr.Get("echo")
		b, _ := os.ReadFile(p.LogFile())
		t.Fatalf("code = %d, body = %s, log = %s", code, body, b)
	}

	// Without https the same server cannot be reached.
	_, ts, _ = newTestServer(t, strings.Replace(tlsServer, "https = true", "", 1))
	if code, body, _ := get(t, ts.URL+"/echo/a"); code == 200 && strings.Contains(body, "path=") {
		t.Errorf("plain HTTP to an HTTPS server = %d %s", code, body)
	}
}

// A request that is in flight while a save closes the access log must not write to the closed
// log: that would open its file again, and nothing would close it.
func TestAccessLogClosedDuringRequest(t *testing.T) {
	s, _, _ := newTestServer(t, "")
	file := s.accessLogFile()
	if file == "" {
		t.Fatal("access log is off")
	}
	h := s.logAccess(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.SaveConfig([]byte("[yard]\naccess_log = false\n")); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/during-save", nil))

	if b, _ := os.ReadFile(file); strings.Contains(string(b), "/during-save") {
		t.Errorf("written to the closed access log: %q", b)
	}
	// On Windows an open handle would keep the file from being removed.
	if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
		t.Errorf("access log is still open: %v", err)
	}
}

func TestForwarded(t *testing.T) {
	for _, c := range []struct {
		remote, host string
		tls          bool
		want         string
	}{
		{"192.0.2.43:5000", "example.com", false, "for=192.0.2.43;host=example.com;proto=http"},
		{"192.0.2.43:5000", "example.com:8443", true, `for=192.0.2.43;host="example.com:8443";proto=https`},
		{"[2001:db8::1]:4711", "[2001:db8::2]", false, `for="[2001:db8::1]";host="[2001:db8::2]";proto=http`},
		{"pipe", "", false, "proto=http"},
	} {
		target := "http://x/"
		if c.tls {
			target = "https://x/" // httptest sets r.TLS
		}
		r := httptest.NewRequest("GET", target, nil)
		r.RemoteAddr, r.Host = c.remote, c.host
		if got := forwarded(r); got != c.want {
			t.Errorf("forwarded(%s, %s, tls=%v) = %s, want %s", c.remote, c.host, c.tls, got, c.want)
		}
	}
	if got := forwardedValue(`a"b\c`); got != `"a\"b\\c"` {
		t.Errorf("forwardedValue = %s", got)
	}
}

// The header shows the version, and the name links to the server list (there is no Servers tab).
func TestHeader(t *testing.T) {
	old := Version
	Version = "9.8.7"
	t.Cleanup(func() { Version = old })
	_, ts, _ := newTestServer(t, "")
	_, body, _ := get(t, ts.URL+"/.yard/config")
	if !strings.Contains(body, `<a class="brand" href="/.yard/">Yard<span class="ver">v9.8.7</span></a>`) {
		t.Errorf("the name should link to the server list and show the version: %s", body)
	}
	if strings.Contains(body, ">Servers</a>") {
		t.Error("the Servers tab is still there")
	}
}
