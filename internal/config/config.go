// Package config reads and validates yard.ini (TOML).
//
// The default location of the configuration file is %APPDATA%\yard\yard.ini (DefaultPath),
// and the default log location is %TEMP%\yard (DefaultLogDir).
//
// Relative paths in the configuration file are resolved against the directory containing
// the configuration file for command / dir, and against the log directory for log / access_log.
// When running as a service the working directory is System32 or /, so the current
// directory at run time is never relied on.
//
// Loading deliberately does the following two things:
//
//   - Strips a UTF-8 BOM, so that editing the file by hand in Notepad etc. does not stop yard from starting.
//   - Treats unknown keys as errors. Silently ignoring a typo makes it impossible to
//     find out why "the setting has no effect".
package config

import (
	"bytes"
	"crypto/tls"
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

// FileName is the default name of the configuration file.
const FileName = "yard.ini"

// AdminPath is the URL of yard's own admin UI. It cannot be used as a server path.
const AdminPath = "/.yard"

// Restart policies.
const (
	RestartOnFailure = "on-failure" // restart only on abnormal exit (non-zero exit code)
	RestartAlways    = "always"     // always restart when it exits
	RestartNever     = "never"      // never restart
)

// Config is the whole of yard.ini.
type Config struct {
	Yard    Yard     `toml:"yard" json:"yard"`
	Admin   Admin    `toml:"admin" json:"admin"`
	Service Service  `toml:"service" json:"service"`
	Servers []Server `toml:"server" json:"server"`

	// Dir is the directory containing the configuration file (absolute). Base for relative command / dir.
	Dir string `toml:"-" json:"-"`
	// LogDir is the log directory (absolute). Base for relative log / access_log.
	LogDir string `toml:"-" json:"-"`
}

// Yard holds yard's own listen and log settings.
type Yard struct {
	Addr      string `toml:"addr" json:"addr"`             // listen address; default ":8080"
	AccessLog string `toml:"access_log" json:"access_log"` // access log destination, one file per day; disabled if empty
	Log       string `toml:"log" json:"log"`               // yard's own log destination, one file per day; stderr if empty (the OS log when running as a service)

	// LogCompressDays and LogKeepDays apply to log and access_log: files older than this many days
	// are gzipped / deleted. 0 (the default) keeps them all as they are.
	LogCompressDays int `toml:"log_compress_days" json:"log_compress_days"`
	LogKeepDays     int `toml:"log_keep_days" json:"log_keep_days"`

	// Cert and Key are PEM files for HTTPS. With both set, addr serves HTTPS instead of HTTP.
	// Relative paths are resolved against the configuration file's directory.
	Cert string `toml:"cert" json:"cert"`
	Key  string `toml:"key" json:"key"`
	// RedirectAddr, with HTTPS on, is a plain-HTTP listen address that redirects every request to https.
	RedirectAddr string `toml:"redirect_addr" json:"redirect_addr"`

	certificate *tls.Certificate // loaded from Cert / Key by Parse
}

// TLS reports whether addr serves HTTPS.
func (y *Yard) TLS() bool { return y.Cert != "" }

// Certificate returns the certificate loaded by Parse, or nil without HTTPS.
func (y *Yard) Certificate() *tls.Certificate { return y.certificate }

// Admin holds the /.yard settings.
type Admin struct {
	// Allow lists the clients (IP or CIDR) allowed to use /.yard.
	// Editing the configuration is equivalent to running programs, so the default is loopback only.
	// "*" allows everything. /.yard/health is not subject to this restriction.
	Allow []string `toml:"allow" json:"allow"`
	// PasswordHash turns on login for /.yard when set (see HashPassword). It is kept out of JSON
	// so that the hash never goes to the browser; the form editor sends a new password instead.
	PasswordHash string `toml:"password_hash" json:"-"`

	prefixes []netip.Prefix
	any      bool
}

// Service holds the service registration information. Used only by install.
type Service struct {
	Name        string `toml:"name" json:"name"`
	DisplayName string `toml:"display_name" json:"display_name"`
	Description string `toml:"description" json:"description"`
	User        string `toml:"user" json:"user"` // user to run as (Linux/macOS); OS default if empty
	// UserService registers it as a service of the logged-in user (Linux: systemd --user,
	// macOS: ~/Library/LaunchAgents). Ignored on Windows.
	UserService bool `toml:"user_service" json:"user_service"`
}

// Server is one server (program) that yard starts and publishes.
type Server struct {
	Name    string   `toml:"name" json:"name"`       // identifier (required, unique)
	Path    string   `toml:"path" json:"path"`       // public URI prefix (required, unique), e.g. "/memo"
	Command string   `toml:"command" json:"command"` // executable (required)
	Args    []string `toml:"args" json:"args"`       // arguments; {port}, {base}, etc. are substituted
	Dir     string   `toml:"dir" json:"dir"`         // working directory; defaults to the configuration file's directory
	Env     []string `toml:"env" json:"env"`         // extra environment variables "KEY=VALUE"; substituted
	Log     string   `toml:"log" json:"log"`         // stdout / stderr destination; discarded if empty

	StripPrefix  bool     `toml:"strip_prefix" json:"strip_prefix"`   // strip path before forwarding
	LocalOnly    bool     `toml:"local_only" json:"local_only"`       // forward only requests from this machine (loopback)
	Restart      string   `toml:"restart" json:"restart"`             // on-failure (default) / always / never
	StartTimeout Duration `toml:"start_timeout" json:"start_timeout"` // how long to wait for the port to open; default 30s
	Disabled     bool     `toml:"disabled" json:"disabled"`           // do not start
}

// Duration accepts TOML strings such as "30s".
type Duration struct{ time.Duration }

// An empty string is the zero value (the default).
func (d *Duration) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	if d.Duration == 0 {
		return nil, nil
	}
	return []byte(d.String()), nil
}

// Load reads the configuration file at path, fills in defaults, and validates it.
// logDir is the base for relative log paths.
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

// Parse interprets the configuration text. dir is the base for relative command / dir,
// and logDir the base for relative log paths.
func Parse(data []byte, dir, logDir string) (*Config, error) {
	c, err := Decode(data)
	if err != nil {
		return nil, err
	}
	c.Dir = dir
	c.LogDir = logDir
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// Decode reads the configuration text as written: no defaults, no path resolution, and no checks
// beyond TOML syntax and unknown keys. This is what the form editor shows and what Format writes back.
func Decode(data []byte) (*Config, error) {
	data = bytes.TrimPrefix(data, []byte("ï»¿"))

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
	return &c, nil
}

var legacySection = regexp.MustCompile(`(?m)^\s*(\[server\]|\[\[tool\]\])`)

// withLegacyHint adds rewrite instructions if the configuration uses the old format ([server] and [[tool]]).
// The old [server] conflicts in type with the new [[server]], and the TOML error alone does not reveal the cause.
func withLegacyHint(err error, data []byte) error {
	if !legacySection.Match(data) {
		return err
	}
	return fmt.Errorf("%w\n(this is the old configuration format: rename [server] to [yard] and [[tool]] to [[server]])", err)
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
	if c.Yard.LogCompressDays < 0 {
		errs = append(errs, errors.New("yard.log_compress_days must not be negative"))
	}
	if c.Yard.LogKeepDays < 0 {
		errs = append(errs, errors.New("yard.log_keep_days must not be negative"))
	}
	if err := c.Yard.loadTLS(c); err != nil {
		errs = append(errs, err)
	}

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
			// With a separator it is a file path; without one it is a command name looked up in PATH.
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

// loadTLS resolves cert / key and loads them, so that a broken certificate is caught
// before the configuration is saved rather than when yard next starts.
func (y *Yard) loadTLS(c *Config) error {
	y.certificate = nil
	switch {
	case y.Cert == "" && y.Key == "":
		if y.RedirectAddr != "" {
			return errors.New("yard.redirect_addr needs yard.cert and yard.key")
		}
		return nil
	case y.Cert == "" || y.Key == "":
		return errors.New("yard.cert and yard.key must be set together")
	}
	y.Cert = c.Abs(y.Cert)
	y.Key = c.Abs(y.Key)
	cert, err := tls.LoadX509KeyPair(y.Cert, y.Key)
	if err != nil {
		return fmt.Errorf("yard.cert / yard.key: %w", err)
	}
	y.certificate = &cert
	if y.RedirectAddr != "" {
		if _, _, err := net.SplitHostPort(y.RedirectAddr); err != nil {
			return fmt.Errorf("yard.redirect_addr: %w", err)
		}
	}
	return nil
}

// cleanPath normalizes path to start with "/" and have no trailing slash (the root is "/").
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

// Abs makes p absolute relative to the configuration file's directory. Empty stays empty.
func (c *Config) Abs(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// LogPath makes p absolute relative to the log directory. Empty stays empty.
func (c *Config) LogPath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.LogDir, p)
}

// Server looks up a server's configuration by name.
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
	if a.PasswordHash != "" {
		if _, err := parsePasswordHash(a.PasswordHash); err != nil {
			errs = append(errs, err)
		}
	}
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

// Allowed reports whether the client remoteAddr ("host:port") may use the admin UI.
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

// IsLoopback reports whether the client remoteAddr ("host:port") is this machine.
// Only RemoteAddr counts; forwarding headers can be spoofed.
func IsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.Unmap().IsLoopback()
}

// DefaultPath returns the default configuration file location, %APPDATA%\yard\yard.ini.
// On Linux it is ~/.config/yard/yard.ini, on macOS ~/Library/Application Support/yard/yard.ini.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return FileName
	}
	return filepath.Join(dir, "yard", FileName)
}

// DefaultLogDir returns the default log location, %TEMP%\yard.
func DefaultLogDir() string {
	return filepath.Join(os.TempDir(), "yard")
}

// Template is the configuration file template written by install / init.
const Template = `# yard configuration file (TOML)
# Relative paths are resolved against the directory of this file for command / dir,
# and against the log directory (default %TEMP%\yard) for log / access_log.
# Only the logs specified here are written. access_log and yard's log switch files every day
# (access.log → access_20261108.log); a server's log is a single file.

[yard]
addr = ":8080"
# access_log = "access.log"
# log = "yard.log"
# Days after which those files are gzipped / deleted (0 or omitted: keep them all as they are).
# log_compress_days = 7
# log_keep_days = 90
# Serve HTTPS on addr (PEM files, relative to this file). redirect_addr redirects plain HTTP to it.
# cert = "yard.crt"
# key = "yard.key"
# redirect_addr = ":80"

[admin]
# Clients allowed to use /.yard (IP / CIDR / "*"). Loopback only by default.
allow = ["127.0.0.1", "::1"]
# Require a password for /.yard. Set it from the form at /.yard/config.
# password_hash = ""

[service]
name = "yard"
# display_name = "yard"
# description = "yard - intranet server proxy"
# user = ""
# user_service = false

# Write one [[server]] per server.
# The following placeholders in args and env are substituted at startup:
#   {port}  free port assigned by yard
#   {base}  public path (with trailing slash, e.g. "/memo/")
#   {path}  public path (without trailing slash, e.g. "/memo")
#   {name}  server name
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
# local_only = false
# restart = "on-failure"
# start_timeout = "30s"
# disabled = false
`
