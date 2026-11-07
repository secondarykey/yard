# AGENTS.md

yard の開発の概要。利用方法・設定・URL は [README.md](README.md)、
仕様の原文は [_docs/spec.md](_docs/spec.md)、サービスの OS 差は [_docs/service.md](_docs/service.md) を参照。

## 何を作っているか

イントラ内でサーバ群を公開するプロキシサーバ。設定に書いたプログラムを空きポートで起動・監視し、
URI の接頭辞で振り分けて転送する。`/.yard` が管理画面。配布物は実行ファイル1つ。

**用語:** yard が起動して公開するものは「サーバ」と呼ぶ（設定は `[[server]]`、型は `config.Server`、
URL は `/.yard/servers/...`）。登録されるのがツールとは限らないので「ツール」とは呼ばない。
yard 自身の待ち受けとログの設定は `[yard]`（`config.Yard`）。`_docs/spec.md` は原文なので書き換えない。

**Go 側の外部依存は `kardianos/service`、`BurntSushi/toml`、`golang.org/x/sys` のみ。**
ルータは `net/http.ServeMux`（メソッド付きパターン・ワイルドカード）、
画面は `html/template` + `embed` で、フロントエンドのビルドは無い。この方針は維持する。

## 構成

```
main.go                 エントリポイント、サブコマンド、サービス制御、ロガー
internal/config         yard.ini の読み込み・検証・相対パス解決・admin.allow
internal/proc           サーバのプロセス管理（Proc: 1サーバ、Manager: 全体と最長一致）
  tree_windows.go       ジョブオブジェクトでプロセスツリーをまとめる
  tree_unix.go          プロセスグループでまとめる
internal/server         HTTP（転送・アクセスログ・/.yard）
  templates/            管理画面の HTML
_cmd/hello              動作確認用のサーバ
```

## 壊しやすい決まり

- **相対パスの基準は2つ。** `command` / `dir` は yard.ini のディレクトリ（`config.Abs`）、
  `log` / `access_log` はログのディレクトリ（`config.LogPath`）。サービスの作業ディレクトリは
  System32 や `/` になるので、どちらかを通さずにパスを使わない。`command` は区切り文字を含むときだけ
  パスとして解決し、含まなければ PATH 検索に任せる。
- **既定の場所は `%APPDATA%\yard\yard.ini` と `%TEMP%\yard`。** `install` は両方を絶対パス
  （`-config` / `-logdir`）でサービスに登録する。LocalSystem で動くと `%APPDATA%` / `%TEMP%` が
  システム側に変わるため、実行時に解決し直してはいけない。
- **`/.yard` の接続元判定は `RemoteAddr` のみ。** `X-Forwarded-For` などを見ると詐称で抜けられる。
  設定の書き換えは任意のプログラムの実行と同義なので、既定はループバックのみ。
- **`/.yard` の書き込み系は `http.CrossOriginProtection` の内側に置く。** 外すと CSRF で設定を書き換えられる。
  `/.yard/health` だけは制限の外（監視用）。
- **Proc の状態遷移は `setStateLocked` を通す。** `starting` の間だけ `ready` チャネルが開いていて、
  転送側（`WaitReady`）はそれを待つ。`p.state` を直接書き換えると待ちが解けなくなる。
  `Start()` は goroutine を起こす前に同期で `starting` にする（直後のリクエストを待たせるため）。
- **サーバの出力は `*os.File` を直接渡す（パイプにしない）。** パイプにすると、孫プロセスが
  パイプを掴んだままのとき `cmd.Wait` が返らない。
- **停止はプロセスツリー単位。** Windows はジョブオブジェクト（KILL_ON_JOB_CLOSE）なので、
  yard が強制終了されてもサーバは残らない。Unix はプロセスグループへ SIGTERM → SIGKILL。
- **設定の反映（`Manager.Apply`）は差分で行う。** `config.Server` が `reflect.DeepEqual` で等しいサーバは
  動かしたまま。`config.Server` に比較できない値（関数など）を持たせない。
- **設定の保存は検証してから。** `SaveConfig` は `config.Parse` が通ったときだけ一時ファイル経由で置き換える。
- `http.Server` には `ReadHeaderTimeout` のみ設定する。SSE / WebSocket / 大きなレスポンスを切らないため。
- サービス: `Start` はブロックしない（listen までを同期で行い失敗を SCM に返す）、`Stop` は素早く返す。
  Windows サービスにシグナルは届かない。
- 管理画面に操作方法の説明文は書かない。

## ビルドとテスト

```bash
go build -o yard.exe .
go test ./...
GOOS=linux go vet ./... && GOOS=darwin go vet ./...
```

`internal/server` のテストは、テストバイナリ自身を `YARD_TEST_HELPER=1` で起動してサーバ役にしている
（`TestMain`）。実際にプロセスを起動・再起動するため数秒かかる。

## 既知の課題

- サーバのログ（`log`）にローテーションが無い。
- Windows ではサーバへ穏当な停止を伝える手段が無く、停止は即時終了になる。
- 起動直後〜ジョブオブジェクトに入れるまでの間に作られた孫プロセスは漏れうる（Windows）。
- Unix で yard が SIGKILL された場合、サーバが残る（pdeathsig を使っていない）。
- `strip_prefix` では HTML 内の絶対パスや Cookie の Path は書き換えない。
- サービス登録は Windows・Linux・macOS とも実機未確認（_docs/service.md）。
