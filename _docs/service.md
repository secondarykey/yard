# Service behavior per OS (kardianos/service)

Findings from reading the source of `github.com/kardianos/service` v1.3.0.
yard is registered with `yard install`, which embeds `-config <absolute path to yard.ini> -logdir <absolute log path>`
as arguments. By default these are `%APPDATA%\yard\yard.ini` and `%TEMP%\yard` of the user who ran install.

## Common

| Item | Details |
|---|---|
| What is registered | Path of the executable + absolute `-config` / `-logdir` paths. **Re-register after moving the executable** |
| Working directory | OS dependent (see below). yard resolves relative paths against the yard.ini or log directory, so it does not matter |
| `%APPDATA%` / `%TEMP%` | Under LocalSystem these become `C:\Windows\System32\config\systemprofile\...` and `C:\Windows\Temp`. yard uses the absolute paths resolved at install time and never reads them |
| Privileges | Registering / removing a system service requires administrator / root |
| Logs | Without `yard.log`, yard's own log goes to the OS log (see below) |
| Stopping | Servers are stopped inside `Stop` (immediate termination on Windows, otherwise SIGTERM → SIGKILL after 5 seconds) |

Changing `name` under `[service]` lets you register several yard instances on the same machine.
Changes to `[service]` take effect after `yard uninstall` → `yard install` (a restart is not enough).

### uninstall

On Windows and systemd, kardianos/service's `Uninstall` **does not stop a running service**.

- Windows: the service is only "marked for deletion"; it keeps running and disappears once it stops.
- systemd: it only runs `disable` and deletes the unit file; the process keeps running.
- launchd: it stops the service, then deletes the plist.

So if the status is running, `yard uninstall` first calls `Stop`, confirms that the service has stopped (up to 30 seconds),
and only then unregisters it. If it cannot be stopped, it returns an error without unregistering.

## Windows (SCM)

- Registered with: the Service Control Manager. Check with `sc query <name>` / `services.msc`.
- Run as: LocalSystem by default. `[service] user` is not used on Windows
  (it would require passing a password). Change it in services.msc if needed.
- Working directory: `C:\Windows\System32`.
- **Environment:** under LocalSystem only the system (machine) environment variables apply. Anything set
  only for the user is missing, and servers inherit yard's environment:
  - A `command` found only through the user's `PATH` fails with `exec: "go": executable file not found in %PATH%`.
  - Tool settings are lost too. For `go run`: a user-level `GOPATH`, `go env -w` settings
    (`%APPDATA%\go\env`: `GOFLAGS`, `CGO_*`, ...), and the build / module caches go to
    `C:\Windows\System32\config\systemprofile\...` instead, so builds can differ from the ones run by hand.
  - Fixes: set the service's Log On account to that user in services.msc (the user's environment then applies),
    or point `command` at a prebuilt executable. An absolute path to `go.exe` alone fixes only the lookup.
    Adding the directory to the system `PATH` may need a reboot before services see it.
- Start type: automatic (`StartType` defaults to automatic).
- Recovery options: yard sets `OnFailure = restart` and `OnFailureDelayDuration = 5s`.
  The SCM restarts yard if yard itself exits abnormally.
- Logs: the Event Log (source name = service name) if `yard.log` is not set.
- Stop timeout: the time before the SCM forcibly terminates a service is
  `HKLM\SYSTEM\CurrentControlSet\Control\WaitToKillServiceTimeout` (20 seconds by default).
- Signals are not delivered. Stopping is done via control requests from the SCM.
- It runs in session 0, so windows opened by servers are not visible.
- **Server process tree:** yard puts servers in a job object (KILL_ON_JOB_CLOSE).
  Even if yard is killed, the OS terminates the servers and their grandchildren (verified on a real machine).
- Network: per-program rules in Windows Firewall match on the executable path.
  If the development build and the service binary have different paths, only one of them gets through.

## Linux

kardianos/service detects the init system automatically (priority: systemd → upstart → OpenRC →
procd → sysv). The kind used by `yard status` etc. can be found with `service.Platform()`.

### systemd (most distributions)

- Registered at: `/etc/systemd/system/<name>.service`
  (with `user_service = true`, `~/.config/systemd/user/<name>.service`, controlled with `systemctl --user`).
- Run as: `[service] user` goes into `User=`. Empty means root.
  **Servers run as the same user**, so always set it if you want to avoid root.
- Working directory: `/`.
- Restart: yard sets `Restart=on-failure`. The template hard-codes `RestartSec=120`
  (waits 2 minutes before restarting), `StartLimitInterval=5` / `StartLimitBurst=10`.
- Environment: `/etc/sysconfig/<name>` is read as an `EnvironmentFile` if it exists.
  **systemd's PATH is minimal**, so it is safest to write server commands as absolute paths.
- Logs: syslog → journald (`journalctl -u <name>`) if `yard.log` is not set.
- Stopping: with the default `KillMode=control-group`, systemd sends SIGTERM to yard and the servers (same cgroup)
  at the same time. yard also stops the servers, but stopping them twice is harmless.

### upstart / OpenRC / procd / sysv

- A script for each init system is generated (`/etc/init/*.conf`, `/etc/init.d/<name>`, etc.).
- Handling of `[service] user` and the restart policy differ per template;
  sysv does not restart automatically on abnormal exit.
- yard puts servers in a separate process group with Setpgid and sends SIGTERM/SIGKILL to the whole group when stopping.
  If yard itself is killed with SIGKILL, the servers are left running (Linux pdeathsig is not used).

## macOS (launchd)

- Registered at: `/Library/LaunchDaemons/<name>.plist` (registered as root).
  With `user_service = true`, `~/Library/LaunchAgents/<name>.plist` (runs only while logged in).
- Run as: `[service] user` goes into `UserName`. Empty means root.
- yard sets `KeepAlive = true` and `RunAtLoad = true` (launchd restarts it if it dies).
- Working directory: `/` unless specified.
- Logs: syslog (`log show --predicate 'process == "yard"'`) if `yard.log` is not set.
- The executable may run into Gatekeeper / notarization issues. Locally built binaries are less likely to be affected.
- Stopping: stopping via `launchctl` delivers SIGTERM. launchd also terminates the job's process group by default.

## FreeBSD / others

- FreeBSD: an rc script at `/usr/local/etc/rc.d/<name>`. `sysrc <name>_enable=YES` may be required.
- Solaris (SMF) and AIX are also supported but unverified.

## Verification status

| OS | Build | Real machine |
|---|---|---|
| Windows 11 | ✓ | Verified foreground run, server start/stop/restart, and cleanup via job object. Service registration unverified |
| Linux | ✓ (`GOOS=linux go vet`) | Unverified |
| macOS | ✓ (`GOOS=darwin go vet`) | Unverified |
| FreeBSD | ✓ (`GOOS=freebsd go build`) | Unverified |
