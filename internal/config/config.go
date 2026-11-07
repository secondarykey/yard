// Package config は yard.ini（TOML）を読み、検証する。
//
// 設定ファイルの既定の場所は %APPDATA%\yard\yard.ini（DefaultPath）、
// ログの既定の場所は %TEMP%\yard（DefaultLogDir）。
//
// 設定ファイル中の相対パスは、command / dir は設定ファイルのあるディレクトリ、
// log / access_log はログのディレクトリを基準に解決する。サービスとして
// 動くときの作業ディレクトリは System32 や / になるため、実行時の
// カレントディレクトリには頼らない。
//
// 読み込みでは次の2点を意図的に行っている。
//
//   - UTF-8 BOM を取り除く。メモ帳などで手編集した時点で起動しなくなるのを防ぐ。
//   - 未知の項目をエラーにする。綴り間違いを黙って無視すると
//     「設定が効かない」原因に辿り着けない。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// FileName は設定ファイルの既定の名前。
const FileName = "yard.ini"

// AdminPath は yard 自身の管理画面の URL。サーバの path には使えない。
const AdminPath = "/.yard"

// 再起動ポリシー。
const (
	RestartOnFailure = "on-failure" // 異常終了（終了コード非0）のときだけ再起動する
	RestartAlways    = "always"     // 終了したら必ず再起動する
	RestartNever     = "never"      // 再起動しない
)

// Config は yard.ini 全体。
type Config struct {
	Yard    Yard     `toml:"yard"`
	Admin   Admin    `toml:"admin"`
	Service Service  `toml:"service"`
	Servers []Server `toml:"server"`

	// Dir は設定ファイルのあるディレクトリ（絶対パス）。command / dir の相対パスの基準。
	Dir string `toml:"-"`
	// LogDir はログのディレクトリ（絶対パス）。log / access_log の相対パスの基準。
	LogDir string `toml:"-"`
}

// Yard は yard 自身の待ち受けとログの設定。
type Yard struct {
	Addr      string `toml:"addr"`       // 待ち受けアドレス。既定 ":8080"
	AccessLog string `toml:"access_log"` // アクセスログの出力先。空なら出力しない
	Log       string `toml:"log"`        // yard 自身のログの出力先。空なら標準エラー（サービス時はOSのログ）
}

// Admin は /.yard の設定。
type Admin struct {
	// Allow は /.yard を操作できる接続元（IP または CIDR）。
	// 設定の書き換えはプログラムの実行と同義なので、既定はループバックのみ。
	// "*" ですべてを許可する。/.yard/health はこの制限を受けない。
	Allow []string `toml:"allow"`

	prefixes []netip.Prefix
	any      bool
}

// Service はサービス登録時の情報。install 時にだけ使う。
type Service struct {
	Name        string `toml:"name"`
	DisplayName string `toml:"display_name"`
	Description string `toml:"description"`
	User        string `toml:"user"` // 実行ユーザ（Linux/macOS）。空なら OS の既定
	// UserService はログインユーザのサービスとして登録する（Linux: systemd --user、
	// macOS: ~/Library/LaunchAgents）。Windows では無視される。
	UserService bool `toml:"user_service"`
}

// Server は yard が起動して公開する1つのサーバ（プログラム）。
type Server struct {
	Name    string   `toml:"name"`    // 識別名（必須・一意）
	Path    string   `toml:"path"`    // 公開する URI の接頭辞（必須・一意）。例 "/memo"
	Command string   `toml:"command"` // 実行ファイル（必須）
	Args    []string `toml:"args"`    // 引数。{port} {base} などを置換する
	Dir     string   `toml:"dir"`     // 作業ディレクトリ。既定は設定ファイルのディレクトリ
	Env     []string `toml:"env"`     // 追加の環境変数 "KEY=VALUE"。置換あり
	Log     string   `toml:"log"`     // 標準出力・標準エラーの出力先。空なら捨てる

	StripPrefix  bool     `toml:"strip_prefix"`  // path を取り除いてから転送する
	Restart      string   `toml:"restart"`       // on-failure（既定）/ always / never
	StartTimeout Duration `toml:"start_timeout"` // ポートが開くまでの待ち時間。既定 30s
	Disabled     bool     `toml:"disabled"`      // 起動しない
}

// Duration は TOML の "30s" 形式の文字列を受け付ける。
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Load は path の設定ファイルを読み、既定値を補って検証する。
// logDir はログの相対パスの基準。
func Load(path, logDir string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	return Parse(data, filepath.Dir(abs), logDir)
}

// Parse は設定の本文を解釈する。dir は command / dir の、logDir はログの相対パスの基準。
func Parse(data []byte, dir, logDir string) (*Config, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	var c Config
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return nil, withLegacyHint(err, data)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return nil, withLegacyHint(fmt.Errorf("unknown keys: %s", strings.Join(keys, ", ")), data)
	}
	c.Dir = dir
	c.LogDir = logDir
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

var legacySection = regexp.MustCompile(`(?m)^\s*(\[server\]|\[\[tool\]\])`)

// withLegacyHint は、旧形式（[server] と [[tool]]）の設定ファイルなら書き換え方を添える。
// 旧形式の [server] は新形式の [[server]] と型が衝突し、TOML のエラーだけでは原因が分からない。
func withLegacyHint(err error, data []byte) error {
	if !legacySection.Match(data) {
		return err
	}
	return fmt.Errorf("%w\n(旧形式の設定です。[server] は [yard] に、[[tool]] は [[server]] に書き換えてください)", err)
}

func (c *Config) normalize() error {
	var errs []error

	if c.Yard.Addr == "" {
		c.Yard.Addr = ":8080"
	}
	if _, _, err := net.SplitHostPort(c.Yard.Addr); err != nil {
		errs = append(errs, fmt.Errorf("yard.addr: %w", err))
	}
	c.Yard.AccessLog = c.LogPath(c.Yard.AccessLog)
	c.Yard.Log = c.LogPath(c.Yard.Log)

	if err := c.Admin.parse(); err != nil {
		errs = append(errs, err)
	}

	if c.Service.Name == "" {
		c.Service.Name = "yard"
	}
	if c.Service.DisplayName == "" {
		c.Service.DisplayName = c.Service.Name
	}
	if c.Service.Description == "" {
		c.Service.Description = "yard - intranet server proxy"
	}

	names := map[string]bool{}
	paths := map[string]string{}
	for i := range c.Servers {
		t := &c.Servers[i]
		label := fmt.Sprintf("server[%d]", i)
		if t.Name != "" {
			label = fmt.Sprintf("server %q", t.Name)
		}

		switch {
		case t.Name == "":
			errs = append(errs, fmt.Errorf("%s: name is required", label))
		case strings.ContainsAny(t.Name, "/\\ \t?#%"):
			errs = append(errs, fmt.Errorf("%s: name must not contain '/', spaces, '?', '#' or '%%'", label))
		case names[t.Name]:
			errs = append(errs, fmt.Errorf("%s: duplicate name", label))
		}
		names[t.Name] = true

		p, err := cleanPath(t.Path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: path: %w", label, err))
		} else if other, ok := paths[p]; ok {
			errs = append(errs, fmt.Errorf("%s: path %q is also used by %q", label, p, other))
		} else {
			paths[p] = t.Name
		}
		t.Path = p

		if t.Command == "" {
			errs = append(errs, fmt.Errorf("%s: command is required", label))
		} else if strings.ContainsAny(t.Command, `/\`) {
			// 区切りを含むものはファイルパス、含まないものは PATH から探すコマンド名。
			t.Command = c.Abs(t.Command)
		}
		if t.Dir == "" {
			t.Dir = c.Dir
		} else {
			t.Dir = c.Abs(t.Dir)
		}
		t.Log = c.LogPath(t.Log)

		for _, e := range t.Env {
			if k, _, ok := strings.Cut(e, "="); !ok || k == "" {
				errs = append(errs, fmt.Errorf("%s: env %q must be KEY=VALUE", label, e))
			}
		}

		switch t.Restart {
		case "":
			t.Restart = RestartOnFailure
		case RestartOnFailure, RestartAlways, RestartNever:
		default:
			errs = append(errs, fmt.Errorf("%s: restart must be %q, %q or %q", label, RestartOnFailure, RestartAlways, RestartNever))
		}
		if t.StartTimeout.Duration <= 0 {
			t.StartTimeout.Duration = 30 * time.Second
		}
	}
	return errors.Join(errs...)
}

// cleanPath は path を "/" 始まり・末尾スラッシュ無しに揃える（ルートは "/"）。
func cleanPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("is required")
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%q must start with '/'", p)
	}
	if strings.ContainsAny(p, "?#%") {
		return "", fmt.Errorf("%q must not contain '?', '#' or '%%'", p)
	}
	p = path.Clean(p)
	if p == AdminPath || strings.HasPrefix(p, AdminPath+"/") {
		return "", fmt.Errorf("%q is reserved for yard", AdminPath)
	}
	return p, nil
}

// Abs は設定ファイルのディレクトリを基準に p を絶対パスにする。空ならそのまま。
func (c *Config) Abs(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// LogPath はログのディレクトリを基準に p を絶対パスにする。空ならそのまま。
func (c *Config) LogPath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.LogDir, p)
}

// Server は名前でサーバの設定を探す。
func (c *Config) Server(name string) (Server, bool) {
	for _, t := range c.Servers {
		if t.Name == name {
			return t, true
		}
	}
	return Server{}, false
}

func (a *Admin) parse() error {
	if a.Allow == nil {
		a.Allow = []string{"127.0.0.1", "::1"}
	}
	a.prefixes = nil
	a.any = false
	var errs []error
	for _, s := range a.Allow {
		if s == "*" {
			a.any = true
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			a.prefixes = append(a.prefixes, p.Masked())
			continue
		}
		ip, err := netip.ParseAddr(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("admin.allow: %q is not an IP address or CIDR", s))
			continue
		}
		a.prefixes = append(a.prefixes, netip.PrefixFrom(ip, ip.BitLen()))
	}
	return errors.Join(errs...)
}

// Allowed は接続元 remoteAddr（"host:port"）が管理画面を使えるかを返す。
func (a *Admin) Allowed(remoteAddr string) bool {
	if a.any {
		return true
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap().WithZone("")
	for _, p := range a.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// DefaultPath は設定ファイルの既定の場所 %APPDATA%\yard\yard.ini を返す。
// Linux は ~/.config/yard/yard.ini、macOS は ~/Library/Application Support/yard/yard.ini。
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return FileName
	}
	return filepath.Join(dir, "yard", FileName)
}

// DefaultLogDir はログの既定の場所 %TEMP%\yard を返す。
func DefaultLogDir() string {
	return filepath.Join(os.TempDir(), "yard")
}

// Template は install / init で書き出す設定ファイルの雛形。
const Template = `# yard の設定ファイル（TOML）
# 相対パスは、command / dir はこのファイルのあるディレクトリ、
# log / access_log はログのディレクトリ（既定 %TEMP%\yard）を基準に解決される。
# ログは書いたものだけが出力される。

[yard]
addr = ":8080"
# access_log = "access.log"
# log = "yard.log"

[admin]
# /.yard を操作できる接続元（IP / CIDR / "*"）。既定はループバックのみ。
allow = ["127.0.0.1", "::1"]

[service]
name = "yard"
# display_name = "yard"
# description = "yard - intranet server proxy"
# user = ""
# user_service = false

# サーバごとに [[server]] を書く。
# args と env の中の次の文字列は起動時に置換される。
#   {port}  yard が割り当てた空きポート
#   {base}  公開パス（末尾スラッシュあり。例 "/memo/"）
#   {path}  公開パス（末尾スラッシュなし。例 "/memo"）
#   {name}  サーバ名
#   {addr}  "127.0.0.1:{port}"
#
# [[server]]
# name = "memo"
# path = "/memo"
# command = "bin/memo.exe"
# args = ["-port", "{port}", "-base", "{base}"]
# dir = "bin"
# env = ["MEMO_DATA=data/memo"]
# log = "memo.log"
# strip_prefix = false
# restart = "on-failure"
# start_timeout = "30s"
# disabled = false
`
