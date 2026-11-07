package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yard/internal/config"
)

// do sends a request as a browser would (Accept: text/html) with an optional session cookie.
func do(t *testing.T, method, u, cookie, contentType, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func sessionOf(res *http.Response) string {
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie && c.MaxAge >= 0 {
			return c.Value
		}
	}
	return ""
}

func login(t *testing.T, ts *httptest.Server, password string) (*http.Response, string) {
	t.Helper()
	return do(t, http.MethodPost, ts.URL+"/.yard/login", "", "application/x-www-form-urlencoded",
		url.Values{"password": {password}}.Encode())
}

func TestAuth(t *testing.T) {
	s, ts, dir := newTestServer(t, "")
	path := filepath.Join(dir, config.FileName)
	putJSON := func(cookie, body string) *http.Response {
		res, b := do(t, http.MethodPut, ts.URL+"/.yard/api/config", cookie, "application/json", body)
		if res.StatusCode != 200 {
			t.Fatalf("put %s = %d %s", body, res.StatusCode, b)
		}
		return res
	}

	// Off by default: logging in is a no-op and pages are open.
	if res, _ := login(t, ts, "x"); res.StatusCode != http.StatusNoContent {
		t.Errorf("login with auth off = %d", res.StatusCode)
	}
	if res, body := do(t, http.MethodGet, ts.URL+"/.yard/", "", "", ""); res.StatusCode != 200 || strings.Contains(body, "Log out") {
		t.Errorf("dashboard with auth off = %d", res.StatusCode)
	}

	// Setting a password from the form stores a hash and logs in whoever set it.
	res := putJSON("", `{"yard":{"access_log":"access.log"},"password":"s3cret"}`)
	first := sessionOf(res)
	if first == "" {
		t.Fatal("no session after setting the password")
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "password_hash = \"pbkdf2-sha256$") || strings.Contains(string(b), "s3cret") {
		t.Fatalf("config written as:\n%s", b)
	}
	hash := s.Config().Admin.PasswordHash
	if hash == "" {
		t.Fatal("password not applied")
	}

	// Without a session: the digital rain with a password box, 401 for the API, health stays open.
	res, body := do(t, http.MethodGet, ts.URL+"/.yard/config", "", "", "")
	if res.StatusCode != http.StatusUnauthorized || !strings.Contains(body, `id="login"`) || !strings.Contains(body, `"/.yard/config"`) {
		t.Errorf("page without session = %d\n%s", res.StatusCode, body)
	}
	if res, _ := do(t, http.MethodGet, ts.URL+"/.yard/api/servers", "", "", ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("api without session = %d", res.StatusCode)
	}
	if res, _ := do(t, http.MethodPost, ts.URL+"/.yard/reload", "", "", ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("post without session = %d", res.StatusCode)
	}
	if res, _ := do(t, http.MethodGet, ts.URL+"/.yard/health", "", "", ""); res.StatusCode != 200 {
		t.Errorf("health = %d", res.StatusCode)
	}
	if res, _ := do(t, http.MethodGet, ts.URL+"/.yard/", "forged", "", ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown session = %d", res.StatusCode)
	}

	// With the session
	if res, body := do(t, http.MethodGet, ts.URL+"/.yard/", first, "", ""); res.StatusCode != 200 || !strings.Contains(body, "Log out") {
		t.Errorf("dashboard with session = %d", res.StatusCode)
	}
	if res, body := do(t, http.MethodGet, ts.URL+"/.yard/matrix", first, "", ""); res.StatusCode != 200 || strings.Contains(body, `id="login"`) {
		t.Errorf("matrix with session = %d", res.StatusCode)
	}

	// Logging in
	if res, _ := login(t, ts, "wrong"); res.StatusCode != http.StatusUnauthorized || sessionOf(res) != "" {
		t.Errorf("wrong password = %d", res.StatusCode)
	}
	res, _ = login(t, ts, "s3cret")
	second := sessionOf(res)
	if res.StatusCode != http.StatusNoContent || second == "" {
		t.Fatalf("login = %d", res.StatusCode)
	}

	// Logging in from another site is CSRF
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/.yard/login", strings.NewReader("password=s3cret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site login: %v %v", res, err)
	}

	// Saving the form without "password" keeps it
	putJSON(second, `{"yard":{"access_log":"access.log"},"server":[]}`)
	if s.Config().Admin.PasswordHash != hash {
		t.Error("password changed by a save that did not touch it")
	}

	// Logging out ends that session only
	if res, _ := do(t, http.MethodPost, ts.URL+"/.yard/logout", second, "", ""); res.StatusCode != http.StatusSeeOther {
		t.Errorf("logout = %d", res.StatusCode)
	}
	if res, _ := do(t, http.MethodGet, ts.URL+"/.yard/", second, "", ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("after logout = %d", res.StatusCode)
	}

	// Changing the password ends the other sessions
	res = putJSON(first, `{"password":"other"}`)
	third := sessionOf(res)
	if res, _ := do(t, http.MethodGet, ts.URL+"/.yard/", first, "", ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("old session after password change = %d", res.StatusCode)
	}
	if res, _ := do(t, http.MethodGet, ts.URL+"/.yard/", third, "", ""); res.StatusCode != 200 {
		t.Errorf("new session after password change = %d", res.StatusCode)
	}

	// Turning it off
	putJSON(third, `{"password":""}`)
	if res, _ := do(t, http.MethodGet, ts.URL+"/.yard/", "", "", ""); res.StatusCode != 200 {
		t.Errorf("after turning auth off = %d", res.StatusCode)
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "password_hash") {
		t.Errorf("password_hash left in:\n%s", b)
	}
}
