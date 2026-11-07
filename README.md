# yard

イントラ内でツール群を公開するためのプロキシサーバ。

Web サーバとして作ったツールは、本来それぞれ別のポートで起動したりサービス化したりする必要がある。
yard は設定ファイルに書いたプログラムを**空きポートで起動し**、指定した URI 以下への
リクエストをそのプログラムへ転送する。プログラムの起動・監視・再起動まで yard が受け持つ。

```
http://server:8080/memo/...   →  memo.exe -port 51234 -base /memo/
http://server:8080/wiki/...   →  wiki.exe -port 51240 -base /wiki/
http://server:8080/.yard/     →  yard の管理画面
```

## 使い方

```bash
go build -o yard.exe .
```

```bash
yard init          # yard.ini の雛形を書き出す
```

```bash
yard               # 前面で起動する
```

サービスとして常駐させる場合（管理者 / root 権限が必要）:

```bash
yard install
```

```bash
yard start
```

`uninstall` / `stop` / `restart` / `status` もある。設定ファイルは `-config` で指定でき、
省略時はカレントディレクトリの `yard.ini`、無ければ実行ファイルと同じ場所の `yard.ini` を使う。
`install` は設定ファイルの絶対パスをサービスに登録する。

OS ごとのサービスの挙動は [_docs/service.md](_docs/service.md) を参照。

## 設定ファイル（yard.ini）

TOML 形式。**相対パスはすべて yard.ini のあるディレクトリが基準**。
未知のキーはエラーになる（綴り間違いに気づけるように）。

```toml
[server]
addr = ":8080"                    # 待ち受けアドレス
access_log = "logs/access.log"    # アクセスログ。省略で出力しない
log = "logs/yard.log"             # yard 自身のログ。省略で標準エラー（サービス時は OS のログ）

[admin]
allow = ["127.0.0.1", "::1", "192.168.10.0/24"]   # /.yard を操作できる接続元。"*" で全許可

[service]
name = "yard"                     # サービス名。変えれば複数インスタンスを登録できる
# display_name = "yard"
# description = "..."
# user = "yard"                   # 実行ユーザ（Linux / macOS）。ツールもこのユーザで動く
# user_service = false            # ユーザサービスとして登録（systemd --user / LaunchAgents）

[[tool]]
name = "memo"                     # 識別名（必須・一意）
path = "/memo"                    # 公開する URI（必須・一意）
command = "tools/memo.exe"        # 実行ファイル（必須）。区切り文字が無ければ PATH から探す
args = ["-port", "{port}", "-base", "{base}"]
dir = "tools"                     # 作業ディレクトリ。省略で yard.ini の場所
env = ["MEMO_DATA=data"]          # 追加の環境変数
log = "logs/memo.log"             # ツールの標準出力・標準エラー。省略で捨てる
strip_prefix = false              # true なら path を取り除いて転送する
restart = "on-failure"            # on-failure / always / never
start_timeout = "30s"             # ポートが開くまで待つ時間
disabled = false                  # true なら起動しない
```

### args / env の置換

| 文字列 | 内容 | 例 |
|---|---|---|
| `{port}` | yard が割り当てた空きポート | `51234` |
| `{base}` | 公開パス（末尾スラッシュあり） | `/memo/` |
| `{path}` | 公開パス（末尾スラッシュなし） | `/memo` |
| `{name}` | ツール名 | `memo` |
| `{addr}` | `127.0.0.1:{port}` | `127.0.0.1:51234` |

環境変数 `YARD_PORT` / `YARD_BASE` / `YARD_NAME` も常に渡す。

### ツール側に求めること

- 指定されたポート（`127.0.0.1` で接続できること）で待ち受ける。
- `strip_prefix = false`（既定）のときは、公開パス（`{base}`）以下で動ける。
  `strip_prefix = true` なら `/` で動くツールでもよいが、HTML 内の絶対パスのリンクは書き換えない
  （`Location` ヘッダだけは公開パスを付け直す）。

## 転送

- `path` の最長一致で転送先を決める。`/memo` は `/memo` と `/memo/...` に一致し、`/memox` には一致しない。
- `Host` ヘッダは元のまま渡し、`X-Forwarded-For` / `X-Forwarded-Host` / `X-Forwarded-Proto` /
  `X-Forwarded-Prefix` を付ける。
- WebSocket と SSE はそのまま中継する。
- 起動中のツールへのリクエストは、ポートが開くまで（`start_timeout` まで）待つ。
  停止中・再起動待ちなら `503`。
- `path = "/"` のツールが無ければ、`/` にツールの一覧を出す。

ツールは異常終了すると 1 秒・2 秒・4 秒…（最大 1 分）と間を空けて再起動する。
ポートは起動のたびに取り直す。ツールが作った子プロセスもまとめて止める
（Windows はジョブオブジェクト、それ以外はプロセスグループ）。

## 管理画面 /.yard

| URL | 内容 |
|---|---|
| `GET /.yard/` | ツール一覧（状態・PID・ポート・稼働時間、起動 / 停止 / 再起動） |
| `GET /.yard/config` | 設定ファイルの編集。保存時に検証し、反映する |
| `GET /.yard/tools/{name}/log` | ツールの出力（末尾 256KB）。`?raw` でテキスト |
| `GET /.yard/health` | yard の生死と各ツールの状態（**接続元制限なし**） |
| `GET /.yard/health/{name}` | ツールが running なら 200、それ以外は 503（**接続元制限なし**） |
| `GET /.yard/api/tools` | ツールの状態（JSON） |
| `POST /.yard/api/tools/{name}/{start\|stop\|restart}` | 操作（JSON） |
| `POST /.yard/api/reload` | yard.ini を読み直して反映 |
| `GET /.yard/api/config` | yard.ini の本文 |
| `PUT /.yard/api/config` | 本文を検証して保存し、反映 |

設定の反映では、内容が変わっていないツールは動かしたまま、変わったツールだけ再起動する。
`server.addr` / `server.log` / `[service]` の変更は yard の再起動（`[service]` は再登録）が必要。

**設定の書き換えは任意のプログラムの実行と同義。** そのため `/.yard` は既定でループバックからしか
操作できない。`admin.allow` を広げるときは接続元を絞ること。接続元の判定には
`X-Forwarded-For` を使わない。別サイトからのフォーム送信（CSRF）は `Sec-Fetch-Site` / `Origin` で拒否する。

## アクセスログの形式

Combined Log Format の後ろに、処理時間（ミリ秒）と転送先のツール名を付ける。

```
192.168.10.5 - - [07/Nov/2026:17:30:38 +0900] "GET /memo/ HTTP/1.1" 200 1234 "-" "Mozilla/5.0 ..." 12 memo
```

## 動作確認用のツール

`_cmd/hello` は `-port` と `-base` を受け取る小さな Web サーバ。

```bash
go build -o hello.exe ./_cmd/hello
```

```toml
[[tool]]
name = "hello"
path = "/hello"
command = "./hello.exe"
args = ["-port", "{port}", "-base", "{base}"]
```
