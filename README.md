# dbpivot

ローカル開発時に「同じアプリから接続する DB を、ローカル ⇄ リモートで瞬時に切り替えたい」需要に応えるための、PostgreSQL 専用のローカルプロキシ。

アプリの接続文字列を書き換えずに、CLI 一発で接続先を切り替えられる。

```
local app  → (port 6432, dbname=appdb)  →  dbpivot  →  local DB (default)
                                                          →  ssm forward → remote DB
```

## 何ができる

- **1 ポートで複数 database を多重化**: アプリは `dbname=<virtual_name>` で接続するだけ。proxy が StartupMessage を読んで database ごとの上流に振り分ける。
- **`database` の動的書き換え**: アプリは常に同じ dbname で繋ぐが、現在の target に設定された実 DB 名で StartupMessage を書き換えて upstream へ送る。
- **テンプレート変数**: `app_${BRANCH}_staging` のように書いておき、`switch` 時に `--var BRANCH=main` で展開。
- **既存接続の即時切断**: 切替時に該当 database の全接続を force-close。クライアントは再接続するだけで新ターゲットへ。
- **SCRAM-SHA-256 代理認証**: client → proxy は trust auth、proxy → upstream は SCRAM-SHA-256 で本物の認証。target ごとに user/password を持つ。
- **ssm port-forward と相性◎**: `aws ssm start-session --document-name AWS-StartPortForwardingSessionToRemoteHost` 等でローカルに立てた forward 先を `forward_targets` として参照するだけ。ssm 自体は管理しない。

## v1 のスコープ

| | |
|---|---|
| 対応プロトコル | PostgreSQL のみ |
| client → proxy 認証 | **trust** (任意 password で受理) |
| proxy → upstream 認証 | **SCRAM-SHA-256 のみ** (PG 14+ デフォルト) |
| TLS | 無し (SSLRequest には `N` を返す) |
| CancelRequest | v1 では捨てる (BackendKeyData のルーティング未実装) |
| 切替時の既存接続 | 即時切断 |

MySQL / MongoDB、TLS、CancelRequest ルーティング、MD5/cleartext upstream auth は v1 のスコープ外。

## 設定例

```yaml
port: 6432                                   # アプリが接続する単一の listen port (127.0.0.1)
control_socket: /tmp/dbpivot.sock     # 省略可

forward_targets:                             # 省略可。inline 派なら不要
  ssm-staging:
    host: 127.0.0.1
    port: 15432                              # 事前に立てた ssm port-forward
  ssm-prod:
    host: 127.0.0.1
    port: 15433

databases:
  - virtual_name: appdb                      # アプリは dbname=appdb で接続 (= 論理名)
    default: local
    targets:
      - name: local
        host: 127.0.0.1                      # inline
        port: 5432
        user: postgres
        password: localpass
        database: app_dev                    # 物理 DB 名 (default は variables 不可)
      - name: staging
        forward_to: ssm-staging              # 名前参照
        user: app_staging_user
        password: stg_password
        database: app_${BRANCH}_staging      # switch 時に --var BRANCH=... 必須
      - name: prod
        forward_to: ssm-prod
        user: app_prod_user
        password: prod_password
        database: app_${USER}_prod

  - virtual_name: analytics
    default: local
    targets:
      - name: local
        host: 127.0.0.1
        port: 5432
        user: postgres
        password: localpass
        database: analytics_dev
```

### バリデーション要点

- `default` target の `database` は `${VAR}` を含めない (起動時に解決手段がないため)。
- target は inline (`host` + `port`) か `forward_to` のどちらか一方 (XOR)。
- `user`, `password` は target ごとに必須 (SCRAM 代理認証で使う)。
- `virtual_name` と target の `database` は PG 識別子規則 (`^[A-Za-z0-9_][A-Za-z0-9_$-]{0,62}$`)。

## 使い方

### ビルド

```bash
go build -o dbpivot ./cmd/dbpivot
```

### 起動

```bash
dbpivot serve --config ./config.yaml
```

### 接続

```bash
psql 'host=127.0.0.1 port=6432 user=anyuser dbname=appdb password=anything sslmode=disable' \
     -c 'select current_database()'
# → app_dev   (target.database で書き換えられた値)
```

`user` と `password` は何でもよい (client → proxy は trust)。`dbname` だけが database selector として効く。

### CLI

```
dbpivot serve   --config PATH [--socket PATH] [--log-level info|debug]
dbpivot switch  <database> <target> [--var KEY=VAL]... [--socket PATH] [--json]
dbpivot status  [<database>]                            [--socket PATH] [--json]
dbpivot list                                        [--socket PATH] [--json]
dbpivot reload                                      [--socket PATH] [--json]
```

例:

```bash
# 確認
dbpivot status
# appdb -> local (db=app_dev upstream=127.0.0.1:5432 active=0)

# 切替 (variables 必須の target)
dbpivot switch appdb staging --var BRANCH=main
# appdb: local (db=app_dev) -> staging (db=app_main_staging) (closed 0 connection(s))

# 戻す
dbpivot switch appdb local

# 設定を再読込 (port 変更は再起動必須)
dbpivot reload
```

## アーキテクチャ

```
cmd/dbpivot/main.go        // cobra CLI
internal/
  config/    config.go             // YAML ロード + バリデーション
             variables.go          // ${VAR} 展開
  proxy/     server.go             // 単一 TCP listener + accept + 振り分け
             database.go               // Database, Switch, conn registry
             pgwire.go             // PG メッセージ framing と各種 encode/decode
             auth.go               // upstream SCRAM-SHA-256 driver
  control/   protocol.go           // Req/Res 型
             server.go             // Unix socket サーバ
             client.go             // CLI 側 dial
scenario/                          // testcontainers ベースの E2E (build tag = scenario)
```

依存:

- `gopkg.in/yaml.v3`
- `github.com/spf13/cobra`
- `github.com/xdg-go/scram`

DB ドライバは抱えない (PG wire は自前)。

## テスト

通常のユニットテスト:

```bash
go test ./...
```

実 PostgreSQL を立てて行う E2E (Docker 必須):

```bash
go test -tags=scenario ./scenario/...
```

`scenario/` 配下は `//go:build scenario` で守られているので通常実行では走らない。`testcontainers-go` で Postgres 16 を立て、SCRAM 認証込みで:

- dbname → database ルーティングと `database` 書き換え
- switch による既存接続の force-close
- 同一スキーマ + 別データの切替検証
- 未知 database での PG ErrorResponse
- control plane (`status` / `list` / `switch`)

を verify する。

## 設計の詳細

実装プランは [docs/plans/2026-05-12-dbpivot-implementation.md](docs/plans/2026-05-12-dbpivot-implementation.md) に。
