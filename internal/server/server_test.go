package server

import (
	"fmt"
	"io"
	"log"
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

// テストバイナリ自身をツールとして起動する。YARD_TEST_HELPER=1 のときは
// テストを走らせず、YARD_PORT で待ち受ける小さなサーバになる。
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
		fmt.Fprintf(w, "path=%s raw=%s host=%s prefix=%s xff=%s arg=%s base=%s",
			r.URL.Path, r.URL.RawPath, r.Host, r.Header.Get("X-Forwarded-Prefix"),
			r.Header.Get("X-Forwarded-For"), strings.Join(os.Args[1:], ","), os.Getenv("YARD_BASE"))
	})
	fmt.Println("helper up")
	if err := http.ListenAndServe("127.0.0.1:"+os.Getenv("YARD_PORT"), mux); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func newTestServer(t *testing.T, tools string) (*Server, *httptest.Server, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src := strings.ReplaceAll(tools, "EXE", strings.ReplaceAll(exe, `\`, `\\`))
	src = "[server]\naccess_log = \"access.log\"\n" + src
	path := filepath.Join(dir, config.FileName)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(path, cfg, log.New(io.Discard, "", 0))
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

const echoTool = `
[[tool]]
name = "echo"
path = "/echo"
command = "EXE"
args = ["-p", "{port}", "{base}"]
env = ["YARD_TEST_HELPER=1"]
log = "echo.log"
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
	s, ts, dir := newTestServer(t, echoTool)

	// 起動中でも最初のリクエストはポートが開くまで待たされる。
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
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q does not contain %q", body, want)
		}
	}

	// 一致しないパス
	if code, _, _ := get(t, ts.URL+"/echox"); code != 404 {
		t.Errorf("/echox = %d", code)
	}
	// ルートはポータル
	if code, body, _ := get(t, ts.URL+"/"); code != 200 || !strings.Contains(body, `href="/echo/"`) {
		t.Errorf("portal = %d %s", code, body)
	}

	// ツールの出力とアクセスログ
	if b, _ := os.ReadFile(filepath.Join(dir, "echo.log")); !strings.Contains(string(b), "helper up") {
		t.Errorf("tool log = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "access.log")); !strings.Contains(string(b), `"GET /echo/a/b?q=1 HTTP/1.1" 200`) ||
		!strings.Contains(string(b), " echo\n") {
		t.Errorf("access log = %q", b)
	}
}

func TestStripPrefix(t *testing.T) {
	_, ts, _ := newTestServer(t, strings.Replace(echoTool, "start_timeout", "strip_prefix = true\nstart_timeout", 1))

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
	s, ts, _ := newTestServer(t, echoTool)
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

func TestAdmin(t *testing.T) {
	s, ts, dir := newTestServer(t, echoTool)
	p, _ := s.mgr.Get("echo")
	waitState(t, p, proc.StateRunning)

	// 停止すると 503、生死確認も 503
	res, err := http.Post(ts.URL+"/.yard/api/tools/echo/stop", "", nil)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("stop: %v %v", res, err)
	}
	if code, _, _ := get(t, ts.URL+"/echo/"); code != http.StatusServiceUnavailable {
		t.Errorf("stopped tool = %d", code)
	}
	if code, _, _ := get(t, ts.URL+"/.yard/health/echo"); code != http.StatusServiceUnavailable {
		t.Errorf("health of stopped tool = %d", code)
	}
	if code, body, _ := get(t, ts.URL+"/.yard/health"); code != 200 || !strings.Contains(body, `"stopped"`) {
		t.Errorf("health = %d %s", code, body)
	}

	// 画面
	for _, u := range []string{"/.yard/", "/.yard/config", "/.yard/tools/echo/log"} {
		if code, body, _ := get(t, ts.URL+u); code != 200 || !strings.Contains(body, "<html") {
			t.Errorf("%s = %d", u, code)
		}
	}
	if code, _, h := get(t, ts.URL+"/.yard"); code/100 != 3 || h.Get("Location") != "/.yard/" {
		t.Errorf("/.yard = %d %q", code, h.Get("Location"))
	}

	// 不正な設定は保存しない
	path := filepath.Join(dir, config.FileName)
	before, _ := os.ReadFile(path)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/.yard/api/config", strings.NewReader("[[tool]]\nname = 1\n"))
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid config: %v %v", res, err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("invalid config was written")
	}

	// 正しい設定は保存して反映する（ツールが消える）
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/.yard/api/config", strings.NewReader("[server]\n"))
	res, err = http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("valid config: %v %v", res, err)
	}
	if _, ok := s.mgr.Get("echo"); ok {
		t.Error("tool should be removed")
	}

	// 別サイトからのフォーム送信は拒否する
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
		"/.yard/":          http.StatusForbidden,
		"/.yard/api/tools": http.StatusForbidden,
		"/.yard/health":    http.StatusOK,
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
