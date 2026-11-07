package config

import (
	"fmt"
	"strings"
)

// Format writes c back as configuration text. Pass a Config from Decode (the values as written),
// not from Parse, or defaults and resolved absolute paths end up in the file.
//
// Empty values are left out so that their defaults apply. An empty admin.allow is left out too,
// which means loopback only rather than nobody. Comments in the original file are not kept.
func Format(c *Config) []byte {
	var w formatter
	w.table("[yard]", false,
		str("addr", c.Yard.Addr),
		flag("access_log", c.Yard.AccessLog),
		flag("log", c.Yard.Log),
		str("cert", c.Yard.Cert),
		str("key", c.Yard.Key),
		str("redirect_addr", c.Yard.RedirectAddr))
	w.table("[admin]", false,
		list("allow", c.Admin.Allow),
		str("password_hash", c.Admin.PasswordHash))
	w.table("[service]", false,
		str("name", c.Service.Name),
		str("display_name", c.Service.DisplayName),
		str("description", c.Service.Description),
		str("user", c.Service.User),
		flag("user_service", c.Service.UserService))
	for _, s := range c.Servers {
		timeout := ""
		if s.StartTimeout.Duration != 0 {
			timeout = s.StartTimeout.String()
		}
		w.table("[[server]]", true,
			str("name", s.Name),
			str("path", s.Path),
			str("command", s.Command),
			list("args", s.Args),
			str("dir", s.Dir),
			list("env", s.Env),
			flag("log", s.Log),
			flag("strip_prefix", s.StripPrefix),
			flag("local_only", s.LocalOnly),
			flag("https", s.HTTPS),
			str("restart", s.Restart),
			str("start_timeout", timeout),
			flag("disabled", s.Disabled))
	}
	return []byte(w.String())
}

// keyValue is one "key = value" line. An empty value means the key is left out.
type keyValue struct{ key, value string }

func str(key, v string) keyValue {
	if v == "" {
		return keyValue{}
	}
	return keyValue{key, quote(v)}
}

func list(key string, vs []string) keyValue {
	if len(vs) == 0 {
		return keyValue{}
	}
	q := make([]string, len(vs))
	for i, v := range vs {
		q[i] = quote(v)
	}
	return keyValue{key, "[" + strings.Join(q, ", ") + "]"}
}

func flag(key string, v bool) keyValue {
	if !v {
		return keyValue{}
	}
	return keyValue{key, "true"}
}

type formatter struct{ strings.Builder }

// table writes a table header and its keys. A table with no keys is written only if always is set.
func (w *formatter) table(header string, always bool, kvs ...keyValue) {
	var body strings.Builder
	for _, kv := range kvs {
		if kv.value != "" {
			fmt.Fprintf(&body, "%s = %s\n", kv.key, kv.value)
		}
	}
	if body.Len() == 0 && !always {
		return
	}
	if w.Len() > 0 {
		w.WriteString("\n")
	}
	w.WriteString(header + "\n")
	w.WriteString(body.String())
}

// quote returns s as a TOML string. Values with backslashes (Windows paths) become literal
// strings ('C:\bin') when possible, so they read the same as they were typed.
func quote(s string) string {
	if strings.Contains(s, `\`) && !strings.ContainsFunc(s, func(r rune) bool {
		return r == '\'' || isControl(r)
	}) {
		return "'" + s + "'"
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if isControl(r) {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }
