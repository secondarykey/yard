# AGENTS.md

Development overview of yard. For usage, configuration, and URLs see [README.md](README.md);
for the original specification see [_docs/spec.md](_docs/spec.md); for OS differences in service behavior see [_docs/service.md](_docs/service.md).

## What this is

A proxy server that publishes a set of servers inside an intranet. It starts the programs listed in the
configuration on free ports, monitors them, and forwards requests by URI prefix. `/.yard` is the admin UI.
It ships as a single executable.

**Terminology:** what yard starts and publishes is called a "server" (`[[server]]` in the configuration,
`config.Server` as a type, `/.yard/servers/...` in URLs). Do not call them "tools", since what gets registered
is not necessarily a tool. yard's own listen and log settings are `[yard]` (`config.Yard`).

**Language:** everything in the repository (code, comments, messages, UI, docs) is written in English,
except `_docs/spec.md`, which is the original Japanese specification and must not be rewritten.

**The only external Go dependencies are `kardianos/service`, `BurntSushi/toml`, and `golang.org/x/sys`.**
Routing uses `net/http.ServeMux` (method patterns and wildcards), and the UI uses `html/template` + `embed`
with no frontend build. Keep it that way.

## Layout

```
main.go                 entry point, subcommands, service control, logger
internal/config         loading / validating yard.ini, resolving relative paths, admin.allow
  format.go             writing a Config back as yard.ini (for the form editor)
internal/proc           server process management (Proc: one server, Manager: all of them and longest match)
  tree_windows.go       groups the process tree with a job object
  tree_unix.go          groups it with a process group
internal/server         HTTP (forwarding, access log, /.yard)
  templates/            admin UI HTML
_cmd/hello              server for manual testing
```

## Rules that are easy to break

- **Relative paths have two bases.** `command` / `dir` are relative to the yard.ini directory (`config.Abs`),
  `log` / `access_log` to the log directory (`config.LogPath`). A service's working directory is
  System32 or `/`, so never use a path without going through one of them. `command` is resolved as a path
  only when it contains a separator; otherwise it is left to PATH lookup.
- **The default locations are `%APPDATA%\yard\yard.ini` and `%TEMP%\yard`.** `install` registers both
  as absolute paths (`-config` / `-logdir`) in the service. Under LocalSystem `%APPDATA%` / `%TEMP%`
  change to the system's locations, so do not resolve them again at run time.
- **`/.yard` decides the client from `RemoteAddr` only.** Looking at `X-Forwarded-For` or similar allows spoofing.
  Editing the configuration is equivalent to running arbitrary programs, so the default is loopback only.
- **Keep the write endpoints of `/.yard` inside `http.CrossOriginProtection`.** Without it the configuration
  can be rewritten via CSRF. Only `/.yard/health` is outside the restrictions (for monitoring).
- **Proc state transitions go through `setStateLocked`.** The `ready` channel is open only while `starting`,
  and the forwarding side (`WaitReady`) waits on it. Writing `p.state` directly leaves waiters stuck.
  `Start()` sets `starting` synchronously before spawning the goroutine (so that immediate requests wait).
- **Pass `*os.File` directly as server output (not a pipe).** With a pipe, `cmd.Wait` does not return
  while a grandchild process still holds the pipe.
- **Stopping works on the whole process tree.** On Windows it is a job object (KILL_ON_JOB_CLOSE), so
  servers do not survive even if yard is killed. On Unix, SIGTERM → SIGKILL to the process group.
- **Applying the configuration (`Manager.Apply`) is diff-based.** Servers whose `config.Server` is equal under
  `reflect.DeepEqual` keep running. Do not give `config.Server` non-comparable values (functions, etc.).
- **The form editor works on `config.Decode`, not `config.Parse`.** It must show and write back the values
  as written (relative paths, empty = default), never the normalized ones. `config.Format` writes them out;
  a key added to `config.Server` etc. needs a json tag, a line in `Format`, and a field in `config.html`.
- **Validate before saving the configuration.** `SaveConfig` replaces the file via a temporary file only when `config.Parse` succeeds.
- Only `ReadHeaderTimeout` is set on `http.Server`, so that SSE / WebSocket / large responses are not cut off.
- Service: `Start` does not block (it does everything up to listen synchronously and reports failure to the SCM), and `Stop` returns quickly.
  Windows services do not receive signals.
- Do not put usage instructions in the admin UI.

## Build and test

```bash
go build -o yard.exe .
go test ./...
GOOS=linux go vet ./... && GOOS=darwin go vet ./...
```

The `internal/server` tests start the test binary itself with `YARD_TEST_HELPER=1` to act as a server
(`TestMain`). They really start and restart processes, so they take a few seconds.

## Known issues

- Server logs (`log`) are not rotated.
- Saving from the form editor rewrites yard.ini without its comments.
- On Windows there is no way to ask a server to stop gracefully; stopping terminates it immediately.
- Grandchild processes created between start and being put in the job object can leak (Windows).
- On Unix, if yard is killed with SIGKILL, servers are left running (pdeathsig is not used).
- `strip_prefix` does not rewrite absolute paths in HTML or Cookie paths.
- Service registration is unverified on real Windows, Linux, and macOS machines (_docs/service.md).
