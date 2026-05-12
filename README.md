# dbpivot

ローカル開発時に「同じアプリから接続する DB を、ローカル ⇄ リモートで瞬時に切り替えたい」需要に応えるための、PostgreSQL 専用のローカルプロキシ。

アプリの接続文字列を書き換えずに、CLI 一発で接続先を切り替えられる。

```
local app  → (port 6432, dbname=appdb)  →  dbpivot  →  local DB (起動時 --target local)
                                                    →  ssm forward → remote DB (use staging で全 DB 同時切替)
```

## 何ができる

- **dbname だけで複数 DB を多重化**: アプリは `dbname=<virtual_name>` で 1 ポートに繋ぐだけ。proxy がどの上流に流すかを決める。
- **target 単位で一括切替**: `local` / `staging` / `prod` などの target を持っておき、`dbpivot use <target>` で全 database を同時に切り替える。
- **接続先 DB 名のテンプレ化**: `database: app_${BRANCH}_staging` のように書いておき、`use` / `serve` 時に `--var BRANCH=main` で展開。
- **ssm port-forward と相性◎**: 事前に立てた `127.0.0.1:15432` のようなローカル forward 先を `forward_targets` として参照するだけ。ssm 自体は管理しない。
- **切替時は既存接続を即時切断**: クライアントは再接続で新しい上流に自然に乗り換わる。

## v1 のスコープ

| | |
|---|---|
| 対応プロトコル | PostgreSQL のみ |
| client → proxy 認証 | **trust** (任意 password で受理) |
| proxy → upstream 認証 | **SCRAM-SHA-256 のみ** (PG 14+ デフォルト) |
| TLS | 無し (SSLRequest には `N` を返す) |
| CancelRequest | v1 では捨てる |
| 切替時の既存接続 | 即時切断 |

MySQL / MongoDB、TLS、MD5/cleartext upstream auth は v1 のスコープ外。

## 設定ファイル

カレントディレクトリの `.dbpivot.yml` を既定で読む (任意のパスにしたい場合は `--config PATH`)。

```yaml
port: 6432                                   # アプリが接続する単一の listen port (127.0.0.1)
control_socket: /tmp/dbpivot.sock            # 省略可

forward_targets:                             # 省略可。inline 派なら不要
  ssm-staging:
    host: 127.0.0.1
    port: 15432                              # 事前に立てた ssm port-forward
  ssm-prod:
    host: 127.0.0.1
    port: 15433

databases:
  - adapter: postgres                        # 必須。v1 では `postgres` のみサポート
    virtual_name: appdb                      # アプリは dbname=appdb で接続 (= 論理名)
    targets:
      - name: local                          # target 名は全 database で共通推奨
        host: 127.0.0.1
        port: 5432
        user: postgres
        password: localpass
        database: app_dev                    # 物理 DB 名
      - name: staging
        forward_to: ssm-staging
        user: app_staging_user
        password: stg_password
        database: app_${BRANCH}_staging      # use 時に --var BRANCH=... 必須
      - name: prod
        forward_to: ssm-prod
        user: app_prod_user
        password: prod_password
        database: app_${USER}_prod

  - adapter: postgres
    virtual_name: analytics
    targets:
      - name: local                          # 同じ target 名集合 {local, staging, prod}
        host: 127.0.0.1
        port: 5432
        user: postgres
        password: localpass
        database: analytics_dev
      - name: staging
        forward_to: ssm-staging
        user: analytics_staging_user
        password: stg_password
        database: analytics_staging
      - name: prod
        forward_to: ssm-prod
        user: analytics_prod_user
        password: prod_password
        database: analytics_prod
```

### バリデーション要点

- 各 database には `adapter` が必須。v1 でサポートされるのは `postgres` のみ。
- 全 database が同じ target 名集合を持つことを推奨。違っていても起動はする (warning) — DB が staging にまだ用意できていない、といった移行途中の状態を許容するため。`use <target>` 時にその target を持たない database は inactive 化される。
- target は inline (`host` + `port`) か `forward_to` のどちらか一方 (XOR)。
- `user`, `password` は target ごとに必須。
- `virtual_name` と target の `database` は PG 識別子規則 (`^[A-Za-z0-9_][A-Za-z0-9_$-]{0,62}$`)。

## 使い方

### ビルド

```bash
go build -o dbpivot ./cmd/dbpivot
```

### 起動

`--target` で全 database に適用する初期 target を指定。テンプレート変数が必要なら `--var KEY=VAL` も併用。

```bash
# シンプル: ローカル DB に向ける (./.dbpivot.yml を読む)
dbpivot serve --target local

# 起動時から staging に繋ぎたい (BRANCH 必須)
dbpivot serve --target staging --var BRANCH=main

# 別パスの config を使う
dbpivot serve --config ~/my-dbpivot.yml --target local
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
dbpivot serve  --target TARGET [--var KEY=VAL]... [--config PATH] [--log-level LVL]
dbpivot use    <target> [--var KEY=VAL]... [--config PATH] [--json]
dbpivot status [--config PATH] [--json]
```

`--config` は省略時 `./.dbpivot.yml`。`use` / `status` は config の `control_socket` を読んで daemon に接続する (config が無ければ `/tmp/dbpivot.sock`)。

例:

```bash
# 確認
dbpivot status
# listening on 127.0.0.1:6432  current target: local
#   appdb     -> local (db=app_dev upstream=127.0.0.1:5432 active=0)
#   analytics -> local (db=analytics_dev upstream=127.0.0.1:5432 active=0)

# 全 database を staging に同時切替
dbpivot use staging --var BRANCH=main
# switched to target "staging":
#   appdb:     local (db=app_dev) -> staging (db=app_main_staging) (closed 0 connection(s))
#   analytics: local (db=analytics_dev) -> staging (db=analytics_staging) (closed 0 connection(s))

# 戻す
dbpivot use local
```

## アーキテクチャ

```
cmd/dbpivot/main.go        // cobra CLI
internal/
  config/    config.go             // YAML ロード + バリデーション
             variables.go          // ${VAR} 展開
  proxy/     server.go             // 単一 TCP listener + accept + 振り分け + SwitchAll
             database.go           // Database, ResolveTarget / Apply, conn registry
             pgwire.go             // PG メッセージ framing と各種 encode/decode
             auth.go               // upstream SCRAM-SHA-256 driver
  control/   protocol.go           // Req/Res 型
             server.go             // Unix socket サーバ
             client.go             // CLI 側 dial
integration_test/                  // testcontainers ベースの integration test (build tag = integration_test)
e2e/                               // docker compose + tiny web server による手動 e2e スクリプト
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

実 PostgreSQL を立てて行う integration test (Docker 必須):

```bash
go test -tags=integration_test ./integration_test/...
```

CLI レイヤを含む e2e は `e2e/run.sh` から走らせる (docker / psql / jq / go 必須):

```bash
./e2e/run.sh
```

## 設計の詳細

実装プランは [docs/plans/2026-05-12-dbpivot-implementation.md](docs/plans/2026-05-12-dbpivot-implementation.md) に。
