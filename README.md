# Yard

A proxy server for publishing a set of servers inside an intranet.

Programs built as web servers normally each need their own port, or have to be installed as services one by one.
Yard **starts the programs listed in its configuration file on free ports** and forwards requests under
a given URI to the matching program. Yard also takes care of starting, monitoring, and restarting them.

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
Only the logs turned on in the configuration file are written, all of them to the log directory (see [Log files](#log-files)).

See [_docs/service.md](_docs/service.md) for service behavior on each OS.

## Configuration file (yard.ini)

TOML format. Relative paths (`command` / `dir` / `cert` / `key`) are resolved against the directory containing yard.ini.
Logs only need turning on; their names are fixed and they go to the log directory (default `%TEMP%\yard`).

Unknown keys are errors (so that typos are caught).

```toml
[yard]
addr = ":8080"                    # listen address
access_log = true                 # access log, one file per day (access_20261108.log)
log = true                        # Yard's own log, one file per day (yard_20261108.log); if false, stderr (the OS log when running as a service)
log_compress_days = 7             # gzip log / access_log files older than this many days; omit to keep them as they are
log_keep_days = 90                # delete log / access_log files older than this many days; omit to keep them all
# cert = "yard.crt"               # with key: serve HTTPS on addr (PEM files)
# key = "yard.key"
# redirect_addr = ":80"           # with HTTPS: plain HTTP here is redirected to https

[admin]
allow = ["127.0.0.1", "::1", "192.168.10.0/24"]   # clients allowed to use /.yard; "*" allows all
# password_hash = "pbkdf2-sha256$..."            # require a password for /.yard; set it from the form at /.yard/config

[service]
name = "yard"                     # service name; change it to register multiple instances
# display_name = "Yard"
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
log = true                        # server stdout / stderr, one file per start ({name}-{pid}.log, e.g. memo-1234.log); discarded if false
strip_prefix = false              # if true, strip path before forwarding
local_only = false                # if true, only requests from this machine are forwarded (others get 403)
https = false                     # if true, the server speaks HTTPS (its certificate is not verified)
restart = "on-failure"            # on-failure / always / never
start_timeout = "30s"             # how long to wait for the port to open
disabled = false                  # if true, do not start
```

### Substitutions in args / env

| Placeholder | Value | Example |
|---|---|---|
| `{port}` | free port assigned by Yard | `51234` |
| `{base}` | public path (with trailing slash) | `/memo/` |
| `{path}` | public path (without trailing slash) | `/memo` |
| `{name}` | server name | `memo` |
| `{addr}` | `127.0.0.1:{port}` | `127.0.0.1:51234` |

The environment variables `YARD_PORT` / `YARD_BASE` / `YARD_NAME` are always passed as well.

### Requirements for servers

- Listen on the given port (reachable at `127.0.0.1`), with plain HTTP unless `https = true`.
  Plain HTTP is the simpler choice: the connection from Yard stays on this machine, and HTTPS for clients
  is Yard's own (`[yard] cert` / `key`). Use `https = true` only for a server that cannot turn HTTPS off;
  Yard then connects with TLS and accepts any certificate, since none names `127.0.0.1:{port}`.
- With `strip_prefix = false` (the default), work under the public path (`{base}`).
  With `strip_prefix = true` a server that runs at `/` is fine, but absolute links in HTML are not rewritten
  (only the `Location` header gets the public path prepended again).

## Forwarding

- The target is chosen by longest match on `path`. `/memo` matches `/memo` and `/memo/...`, but not `/memox`.
- The `Host` header is passed through unchanged, and `X-Forwarded-For` / `X-Forwarded-Host` / `X-Forwarded-Proto` /
  `X-Forwarded-Prefix` and `Forwarded` (RFC 7239, e.g. `for=192.0.2.43;host="example.com:8080";proto=http`) are added.
  These headers sent by the client are dropped first, so a server sees only the address Yard saw.
  `X-Real-IP` and `True-Client-IP` sent by the client are dropped too.
- WebSocket and SSE are relayed as is.
- Requests to a server that is starting wait until its port opens (up to `start_timeout`).
  If it is stopped or waiting to restart, the response is `503`.
- A server with `local_only = true` is only reachable from the machine Yard runs on (loopback).
  Requests from elsewhere get `403`. Yard itself listens on all interfaces by default (`addr = ":8080"`),
  so the other servers stay reachable from the LAN. As with `/.yard`, only the connection's address counts, not `X-Forwarded-For`.
- If no server matches, the response is `404`. `/` also returns `404` until a server with `path = "/"` is registered.

When a server exits abnormally, it is restarted after 1, 2, 4 seconds… (up to 1 minute).
A new port is picked on each start. Child processes created by the server are stopped together with it
(a job object on Windows, a process group elsewhere).

## Admin pages /.yard

| URL | Description |
|---|---|
| `GET /.yard/` | Server list (see below) |
| `GET /.yard/config` | Edit the configuration as a form (`?view=text` for the raw file). Validated and applied on save. Saving from the form rewrites yard.ini without its comments |
| `GET /.yard/servers/{name}/log` | Server output of the current or last start (last 256KB). `?raw` for plain text |
| `GET /.yard/logs/{yard\|access}` | Yard's own log / the access log: the current day's file (last 256KB). `?raw` for plain text |
| `GET /.yard/matrix` | Digital rain (just for fun; also the login page) |
| `POST /.yard/login` | Log in with the form value `password` (204, or 401 for a wrong password) |
| `POST /.yard/logout` | Log out |
| `GET /.yard/health` | Whether Yard is alive and the state of each server (**no client restriction**) |
| `GET /.yard/health/{name}` | 200 if the server is running, otherwise 503 (**no client restriction**) |
| `GET /.yard/api/servers` | Server states (JSON) |
| `POST /.yard/api/servers/{name}/{start\|stop\|restart}` | Control a server (JSON) |
| `POST /.yard/api/servers/{name}/{enable\|disable\|log-on\|log-off}` | Set the server's `disabled` / `log` in yard.ini and apply it (JSON) |
| `POST /.yard/api/reload` | Reload and apply yard.ini |
| `GET /.yard/api/config` | Contents of yard.ini |
| `PUT /.yard/api/config` | Validate, save, and apply new contents (TOML, or JSON with the same keys when `Content-Type: application/json`) |

When applying a configuration, servers whose settings did not change keep running; only changed servers are restarted.
A changed server that was stopped stays stopped.
Changes to `yard.addr` / `yard.log` / `yard.redirect_addr` / `[service]`, and turning HTTPS on or off, require restarting Yard
(`[service]` requires re-registering). A renewed certificate is picked up by a reload.

**Editing the configuration is equivalent to running arbitrary programs.** For this reason `/.yard` is only
accessible from loopback by default. If you widen `admin.allow`, keep the allowed clients narrow.
`X-Forwarded-For` is not used to identify the client. Form submissions from other sites (CSRF) are rejected using `Sec-Fetch-Site` / `Origin`.

### Server list

Each row shows a server's state, PID, port, uptime, restarts and last error, refreshed every 2 seconds.

- **Enabled** (first column) and **Log** (next to PID) are the server's `disabled` / `log`, saved to yard.ini.
  Only that one line is changed, so comments are kept. Disabling a running server stops it, and turning its
  log on or off restarts it; both ask first.
- **Start / Stop** is one button that follows the state; stopping asks first. **Restart** does not ask.
- **Port** links to the server itself (`http://127.0.0.1:{port}{base}`, bypassing Yard) when the list is opened
  on the machine Yard runs on, for telling Yard's problems from the server's. From other machines it is plain
  text, since servers only listen on loopback.
- **Log** (last column) shows the server's output of the current or last start. **Yard log** / **Access log**
  at the top show today's file of Yard's own log and the access log, when they are on.

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
- Sessions are kept in memory: restarting Yard logs everyone out, and so does changing or removing the password.
  A session ends after 24 hours without requests.
- A wrong password costs one second, and only one login is checked at a time.
- If you forget the password, remove `password_hash` from yard.ini and restart Yard.
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

Prefer this to putting a TLS proxy (nginx, Caddy, IIS...) in front of Yard. Yard decides who the client is from
the connection's address only, so behind a proxy on the same machine every client looks like loopback:
`admin.allow` and `local_only` would then let the whole network through.

## Log files

Each log is turned on with `true` and has a fixed name in the log directory:

| Setting | File | |
|---|---|---|
| `[yard] log` | `yard_20261108.log` | one file per day |
| `[yard] access_log` | `access_20261108.log` | one file per day |
| `[[server]] log` | `{name}-{pid}.log` (`memo-1234.log`) | one file per start |

The current files can be read from the server list.

Old files of Yard's own log and the access log are kept as they are unless `log_compress_days` / `log_keep_days`
are set. With `log_compress_days = 7` and `log_keep_days = 90`, today's and the previous 6 days' files stay as
`.log`, files from 7 to 89 days ago become `.log.gz`, and older ones are deleted. The age comes from the date in the
file name, and only files named exactly `yard_<date>.log` / `access_<date>.log` (or `.log.gz`) are touched.
This runs when Yard starts and when the date changes. Changing the two values applies to the access log at once
and to Yard's own log after a restart. Server output files are never compressed or deleted; that is up to you.

When Yard's own log reopens a file that already exists (Yard restarted the same day), a separator line is written to it.

Yard's own log is written with `log/slog`:

```
2026-11-08T05:42:48+09:00 [INFO  ] started server=memo pid=1234 port=50123
```

Server output (`[[server]] log`) is written as is, after one line from Yard with the port and start time.
Every start (including restarts) gets a new file, since the pid is part of the name.

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
