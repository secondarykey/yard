# yard

A proxy server for publishing a set of servers inside an intranet.

Programs built as web servers normally each need their own port, or have to be installed as services one by one.
yard **starts the programs listed in its configuration file on free ports** and forwards requests under
a given URI to the matching program. yard also takes care of starting, monitoring, and restarting them.

```
http://server:8080/memo/...   →  memo.exe -port 51234 -base /memo/
http://server:8080/wiki/...   →  wiki.exe -port 51240 -base /wiki/
http://server:8080/.yard/     →  yard admin pages
```

## Usage

```bash
go build -o yard.exe .
```

```bash
yard init          # write a template yard.ini
```

```bash
yard               # run in the foreground
```

To run it as a service (requires administrator / root):

```bash
yard install
```

```bash
yard start
```

`uninstall` / `stop` / `restart` / `status` are also available. `uninstall` stops a running service before unregistering it.

### File locations

| | Default | Override |
|---|---|---|
| Configuration file | `%APPDATA%\yard\yard.ini` | `-config <path>` |
| Logs | `%TEMP%\yard\` | `-logdir <path>` |

On Linux these are `~/.config/yard/yard.ini` and `$TMPDIR/yard` (or `/tmp/yard` if unset);
on macOS, `~/Library/Application Support/yard/yard.ini` and `$TMPDIR/yard`.

`install` registers the configuration file and log locations of the user who ran it as absolute paths in the service.
Even when running as a service (LocalSystem or root), `%APPDATA%` / `%TEMP%` do not switch to the service account's locations.
Only the logs specified in the configuration file are written.

See [_docs/service.md](_docs/service.md) for service behavior on each OS.

## Configuration file (yard.ini)

TOML format. Relative paths are resolved as follows:

- `command` / `dir` / `cert` / `key`: the directory containing yard.ini
- `log` / `access_log`: the log directory (default `%TEMP%\yard`)

Unknown keys are errors (so that typos are caught).

```toml
[yard]
addr = ":8080"                    # listen address
access_log = "access.log"         # access log, one file per day (access_20261108.log); omit to disable
log = "yard.log"                  # yard's own log, one file per day (yard_20261108.log); omit for stderr (the OS log when running as a service)
# cert = "yard.crt"               # with key: serve HTTPS on addr (PEM files)
# key = "yard.key"
# redirect_addr = ":80"           # with HTTPS: plain HTTP here is redirected to https

[admin]
allow = ["127.0.0.1", "::1", "192.168.10.0/24"]   # clients allowed to use /.yard; "*" allows all
# password_hash = "pbkdf2-sha256$..."            # require a password for /.yard; set it from the form at /.yard/config

[service]
name = "yard"                     # service name; change it to register multiple instances
# display_name = "yard"
# description = "..."
# user = "yard"                   # user to run as (Linux / macOS); servers run as this user too
# user_service = false            # register as a user service (systemd --user / LaunchAgents)

[[server]]
name = "memo"                     # identifier (required, unique)
path = "/memo"                    # public URI (required, unique)
command = "bin/memo.exe"          # executable (required); looked up in PATH if it has no separator
args = ["-port", "{port}", "-base", "{base}"]
dir = "bin"                       # working directory; defaults to the yard.ini directory
env = ["MEMO_DATA=data"]          # extra environment variables
log = "memo.log"                  # server stdout / stderr, to this one file (not split by day); discarded if omitted
strip_prefix = false              # if true, strip path before forwarding
local_only = false                # if true, only requests from this machine are forwarded (others get 403)
restart = "on-failure"            # on-failure / always / never
start_timeout = "30s"             # how long to wait for the port to open
disabled = false                  # if true, do not start
```

### Substitutions in args / env

| Placeholder | Value | Example |
|---|---|---|
| `{port}` | free port assigned by yard | `51234` |
| `{base}` | public path (with trailing slash) | `/memo/` |
| `{path}` | public path (without trailing slash) | `/memo` |
| `{name}` | server name | `memo` |
| `{addr}` | `127.0.0.1:{port}` | `127.0.0.1:51234` |

The environment variables `YARD_PORT` / `YARD_BASE` / `YARD_NAME` are always passed as well.

### Requirements for servers

- Listen on the given port (reachable at `127.0.0.1`).
- With `strip_prefix = false` (the default), work under the public path (`{base}`).
  With `strip_prefix = true` a server that runs at `/` is fine, but absolute links in HTML are not rewritten
  (only the `Location` header gets the public path prepended again).

## Forwarding

- The target is chosen by longest match on `path`. `/memo` matches `/memo` and `/memo/...`, but not `/memox`.
- The `Host` header is passed through unchanged, and `X-Forwarded-For` / `X-Forwarded-Host` / `X-Forwarded-Proto` /
  `X-Forwarded-Prefix` are added.
- WebSocket and SSE are relayed as is.
- Requests to a server that is starting wait until its port opens (up to `start_timeout`).
  If it is stopped or waiting to restart, the response is `503`.
- A server with `local_only = true` is only reachable from the machine yard runs on (loopback).
  Requests from elsewhere get `403`. yard itself listens on all interfaces by default (`addr = ":8080"`),
  so the other servers stay reachable from the LAN. As with `/.yard`, only the connection's address counts, not `X-Forwarded-For`.
- If no server matches, the response is `404`. `/` also returns `404` until a server with `path = "/"` is registered.

When a server exits abnormally, it is restarted after 1, 2, 4 seconds… (up to 1 minute).
A new port is picked on each start. Child processes created by the server are stopped together with it
(a job object on Windows, a process group elsewhere).

## Admin pages /.yard

| URL | Description |
|---|---|
| `GET /.yard/` | Server list (state, PID, port, uptime; start / stop / restart) |
| `GET /.yard/config` | Edit the configuration as a form (`?view=text` for the raw file). Validated and applied on save. Saving from the form rewrites yard.ini without its comments |
| `GET /.yard/servers/{name}/log` | Server output (last 256KB). `?raw` for plain text |
| `GET /.yard/matrix` | Digital rain (just for fun; also the login page) |
| `POST /.yard/login` | Log in with the form value `password` (204, or 401 for a wrong password) |
| `POST /.yard/logout` | Log out |
| `GET /.yard/health` | Whether yard is alive and the state of each server (**no client restriction**) |
| `GET /.yard/health/{name}` | 200 if the server is running, otherwise 503 (**no client restriction**) |
| `GET /.yard/api/servers` | Server states (JSON) |
| `POST /.yard/api/servers/{name}/{start\|stop\|restart}` | Control a server (JSON) |
| `POST /.yard/api/reload` | Reload and apply yard.ini |
| `GET /.yard/api/config` | Contents of yard.ini |
| `PUT /.yard/api/config` | Validate, save, and apply new contents (TOML, or JSON with the same keys when `Content-Type: application/json`) |

When applying a configuration, servers whose settings did not change keep running; only changed servers are restarted.
Changes to `yard.addr` / `yard.log` / `yard.redirect_addr` / `[service]`, and turning HTTPS on or off, require restarting yard
(`[service]` requires re-registering). A renewed certificate is picked up by a reload.

**Editing the configuration is equivalent to running arbitrary programs.** For this reason `/.yard` is only
accessible from loopback by default. If you widen `admin.allow`, keep the allowed clients narrow.
`X-Forwarded-For` is not used to identify the client. Form submissions from other sites (CSRF) are rejected using `Sec-Fetch-Site` / `Origin`.

### Password

Turning on "Require password" in the form at `/.yard/config` makes `/.yard` require logging in, on top of
`admin.allow`. Only a PBKDF2-SHA256 hash is stored (`admin.password_hash`); the form never shows it.
Without a session, browsers get the digital rain, and clicking it brings up the password box.
The API answers `401`; scripts can log in first and keep the cookie:

```bash
curl -c jar -d password=... http://localhost:8080/.yard/login
curl -b jar http://localhost:8080/.yard/api/servers
```

- `/.yard/health` stays open.
- Sessions are kept in memory: restarting yard logs everyone out, and so does changing or removing the password.
  A session ends after 24 hours without requests.
- A wrong password costs one second, and only one login is checked at a time.
- If you forget the password, remove `password_hash` from yard.ini and restart yard.
- Without HTTPS (see below) the password travels in plain text on the network.

## HTTPS

Set `cert` and `key` under `[yard]` and `addr` serves HTTPS instead of HTTP.

```toml
[yard]
addr = ":443"
cert = "yard.crt"          # PEM; the chain (server certificate first) if there are intermediates
key = "yard.key"           # PEM, unencrypted
redirect_addr = ":80"      # optional: http://host/... → https://host/...
```

- The certificate is checked when the configuration is loaded or saved, so a broken one never gets saved.
- To renew it, replace the files and reload (`/.yard` → reload, or save the configuration). No restart is needed.
- Inside an intranet, public CAs are hard to use. Make a certificate from a local CA (for example with
  [mkcert](https://github.com/FiloSottile/mkcert)) for the names clients use, and make the clients trust that CA.
- With HTTPS the login cookie is `Secure`, and servers get `X-Forwarded-Proto: https`.
- HTTP/2 is used where the client supports it. WebSocket still runs over HTTP/1.1.

Prefer this to putting a TLS proxy (nginx, Caddy, IIS...) in front of yard. yard decides who the client is from
the connection's address only, so behind a proxy on the same machine every client looks like loopback:
`admin.allow` and `local_only` would then let the whole network through.

## Log files

yard's own log (`[yard] log`) and the access log (`access_log`) switch files every day: the date goes before
the extension, which becomes `.log` (`access.log` → `access_20261108.log`). Old files are kept; deleting them is up to you.
When yard's own log reopens a file that already exists (yard restarted the same day), a separator line is written to it.

yard's own log is written with `log/slog`:

```
2026-11-08T05:42:48+09:00 [INFO  ] started server=memo pid=1234 port=50123
```

Server output (`[[server]] log`) goes to the configured file as is.

## Access log format

Combined Log Format, followed by the processing time (milliseconds) and the name of the target server.

```
192.168.10.5 - - [07/Nov/2026:17:30:38 +0900] "GET /memo/ HTTP/1.1" 200 1234 "-" "Mozilla/5.0 ..." 12 memo
```

## Test server

`_cmd/hello` is a small web server that accepts `-port` and `-base`.

```bash
go build -o hello.exe ./_cmd/hello
```

```toml
[[server]]
name = "hello"
path = "/hello"
command = "./hello.exe"
args = ["-port", "{port}", "-base", "{base}"]
```
