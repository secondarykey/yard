# サービス化（kardianos/service）の OS ごとの挙動

`github.com/kardianos/service` v1.3.0 のソースを読んで確認した内容。
yard は `yard install` で登録し、登録時に `-config <yard.ini の絶対パス>` を引数として埋め込む。

## 共通

| 項目 | 内容 |
|---|---|
| 登録内容 | 実行ファイルのパス + `-config <絶対パス>`。**実行ファイルを移動したら再登録が必要** |
| 作業ディレクトリ | OS 依存（下記）。yard は相対パスをすべて yard.ini の場所基準で解決するので影響しない |
| 権限 | システムサービスの登録・削除は管理者 / root が必要 |
| ログ | `server.log` が無いとき、yard 自身のログは OS のログへ（下記） |
| 停止 | `Stop` の中でツールを止める（Windows は即時終了、他は SIGTERM → 5 秒後 SIGKILL） |

`[service]` の `name` を変えると、同じマシンに yard を複数登録できる。
`[service]` の変更は `yard uninstall` → `yard install` で反映される（再起動では反映されない）。

## Windows（SCM）

- 登録先: サービスコントロールマネージャ。`sc query <name>` / `services.msc` で確認できる。
- 実行ユーザ: 既定は LocalSystem。`[service] user` は Windows では使っていない
  （パスワードの受け渡しが要るため）。必要なら services.msc で変更する。
- 作業ディレクトリ: `C:\Windows\System32`。
- 起動種別: 自動（`StartType` 既定 automatic）。
- 回復オプション: yard は `OnFailure = restart`、`OnFailureDelayDuration = 5s` を指定している。
  yard 自身が異常終了したときに SCM が再起動する。
- ログ: `server.log` が無ければイベントログ（ソース名 = サービス名）。
- 停止待ち: SCM がサービスを強制終了するまでの時間は
  `HKLM\SYSTEM\CurrentControlSet\Control\WaitToKillServiceTimeout`（既定 20 秒）。
- シグナルは届かない。停止は SCM からの制御要求で行われる。
- セッション 0 で動くため、ツールがウィンドウを出しても見えない。
- **ツールのプロセスツリー:** yard はツールをジョブオブジェクト（KILL_ON_JOB_CLOSE）に入れる。
  yard が強制終了されてもツールと孫プロセスは OS が終了させる（実機で確認済み）。
- ネットワーク: Windows ファイアウォールのプログラム単位の許可は実行ファイルのパスで一致する。
  開発用ビルドとサービスの実体でパスが違うと片方だけ通らない。

## Linux

kardianos/service は init システムを自動判別する（優先順: systemd → upstart → OpenRC →
procd → sysv）。`yard status` などで使われる種別は `service.Platform()` で分かる。

### systemd（大半のディストリビューション）

- 登録先: `/etc/systemd/system/<name>.service`
  （`user_service = true` なら `~/.config/systemd/user/<name>.service`、`systemctl --user` で操作）。
- 実行ユーザ: `[service] user` を書くと `User=` に入る。空なら root。
  **ツールも同じユーザで動く**ので、root を避けたいなら必ず指定する。
- 作業ディレクトリ: `/`。
- 再起動: yard は `Restart=on-failure` を指定。テンプレート固定で `RestartSec=120`
  （再起動まで 2 分待つ）、`StartLimitInterval=5` / `StartLimitBurst=10`。
- 環境変数: `/etc/sysconfig/<name>` があれば `EnvironmentFile` として読む。
  **systemd の PATH は最小限**なので、ツールの command は絶対パスで書くのが安全。
- ログ: `server.log` が無ければ syslog → journald（`journalctl -u <name>`）。
- 停止: 既定の `KillMode=control-group` により、systemd が yard とツール（同じ cgroup）に
  同時に SIGTERM を送る。yard 側もツールを止めに行くが、二重に止めても問題はない。

### upstart / OpenRC / procd / sysv

- 各 init のスクリプト（`/etc/init/*.conf`、`/etc/init.d/<name>` など）が生成される。
- `[service] user` の扱いや再起動ポリシーはテンプレートごとに異なり、
  sysv では異常終了時の自動再起動は無い。
- yard は Setpgid でツールを別プロセスグループにしており、停止時はグループごと SIGTERM/SIGKILL する。
  yard 自身が SIGKILL された場合はツールが残る（Linux の pdeathsig は使っていない）。

## macOS（launchd）

- 登録先: `/Library/LaunchDaemons/<name>.plist`（root で登録）。
  `user_service = true` なら `~/Library/LaunchAgents/<name>.plist`（ログイン中のみ動く）。
- 実行ユーザ: `[service] user` が `UserName` に入る。空なら root。
- yard は `KeepAlive = true`、`RunAtLoad = true` を指定している（落ちたら launchd が起こし直す）。
- 作業ディレクトリ: 指定しなければ `/`。
- ログ: `server.log` が無ければ syslog（`log show --predicate 'process == "yard"'`）。
- 実行ファイルには Gatekeeper / 公証の問題が出うる。手元でビルドしたものは問題になりにくい。
- 停止: `launchctl` からの停止で SIGTERM が届く。launchd は既定でジョブのプロセスグループも終了させる。

## FreeBSD / その他

- FreeBSD: `/usr/local/etc/rc.d/<name>` の rc スクリプト。`sysrc <name>_enable=YES` が必要な場合がある。
- Solaris（SMF）、AIX にも対応しているが未確認。

## 動作確認の状況

| OS | ビルド | 実機 |
|---|---|---|
| Windows 11 | ○ | 前面起動・ツールの起動/停止/再起動・ジョブオブジェクトによる後始末を確認。サービス登録は未確認 |
| Linux | ○（`GOOS=linux go vet`） | 未確認 |
| macOS | ○（`GOOS=darwin go vet`） | 未確認 |
| FreeBSD | ○（`GOOS=freebsd go build`） | 未確認 |
