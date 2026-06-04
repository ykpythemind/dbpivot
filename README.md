# dbpivot

ローカル開発時に「同じアプリから接続する DB を、ローカル ⇄ リモートで瞬時に切り替えたい」需要に応えるための、PostgreSQL / MySQL / MongoDB 対応のローカルプロキシ。

アプリの接続文字列を書き換えずに、CLI 一発で接続先を切り替えられる。

```
local app  → (postgres:6432 / mysql:3306 / mongodb:27017, dbname=appdb)  →  dbpivot  →  local DB (起動時 --target local)
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
| 対応プロトコル | PostgreSQL / MySQL / MongoDB (adapter ごとに listen port を分け、1 インスタンスで複数プロトコルを同時に serve 可能) |
| client → proxy 認証 (PostgreSQL / MySQL) | **trust** (任意 password で受理) |
| client → proxy 認証 (MongoDB) | **無し** (proxy は auth-disabled な mongod として振る舞う。client は資格情報なしで接続する。下記参照) |
| proxy → upstream 認証 (PostgreSQL) | **SCRAM-SHA-256 のみ** (PG 14+ デフォルト) |
| proxy → upstream 認証 (MySQL) | `mysql_native_password` / `caching_sha2_password` (fast-auth・TLS 経由 cleartext・RSA full-auth) |
| proxy → upstream 認証 (MongoDB) | **SCRAM-SHA-256 のみ** (`auth_source` で認証 DB を指定。既定 `admin`) |
| client → proxy TLS | 無し (PG は SSLRequest に `N`、MySQL は greeting で CLIENT_SSL を出さない、MongoDB は平文) |
| proxy → upstream TLS | `sslmode: require` で対応 (PG=SSLRequest, MySQL=in-band CLIENT_SSL。いずれも証明書検証なし)。既定は `disable`。**MongoDB は未対応 (平文のみ)** |
| CancelRequest | v1 では捨てる |
| 切替時の既存接続 | 即時切断 |

client→proxy TLS、証明書検証あり (`verify-full`)、PG の MD5/cleartext upstream auth、MongoDB upstream の TLS は v1 のスコープ外。PostgreSQL / MongoDB は client-first、MySQL は server-first なので 1 ポートは 1 プロトコルに固定される。複数プロトコルを扱いたい場合は `listen_ports` で adapter ごとにポートを割り当てる (PostgreSQL の database は postgres 用ポートへ、MySQL の database は mysql 用ポートへ、MongoDB の database は mongodb 用ポートへ、それぞれ routing される)。

### MongoDB の認証モデル

MongoDB の SCRAM はサーバ側が「パスワードから導出した ServerSignature」を返してクライアントがそれを検証する仕組みのため、PostgreSQL / MySQL のような「任意のパスワードを受理する trust 認証」を proxy 側で実装できない。そこで dbpivot は **auth-disabled な mongod として振る舞い**、client には資格情報を要求しない。実際の認証は upstream へ繋ぐ際に、target に設定した `user` / `password` / `auth_source` で SCRAM-SHA-256 を行う。

- client は **資格情報なし** で接続する (接続文字列に user/password を含めない)。
- routing は接続時の DB 名ではなく、各コマンドが運ぶ `$db` で行う (MongoDB は接続時に DB を固定しない)。`$db` が設定済みの `virtual_name` に一致したコマンドで upstream にバインドされる。
- proxy は `hello` / `ping` / `buildInfo` などのハンドシェイク系コマンドをローカルで応答し、それ以外のコマンドを upstream へそのまま (verbatim) 転送する。`$db` の書き換えは行わないので、**`virtual_name` と upstream の物理 DB 名は一致している必要がある** (PG/MySQL のような `database:` での DB 名書き換えは MongoDB では行われない)。

## 設定ファイル

カレントディレクトリの `.dbpivot.yml` を既定で読む (任意のパスにしたい場合は `--config PATH`)。

```yaml
listen_host: 127.0.0.1                       # 省略可 (既定 127.0.0.1)。docker container 等から proxy へ届かせたい場合は 0.0.0.0
listen_ports:                                # adapter ごとの listen port。使う adapter の分だけ書く
  postgres: 6432                             # postgres の database が接続するポート
  mysql: 3306                                # mysql の database が接続するポート (mysql を使わないなら省略可)
  mongodb: 27017                             # mongodb の database が接続するポート (使わないなら省略可)
control_socket: /tmp/dbpivot.sock            # 省略可

forward_targets:                             # 省略可。inline 派なら不要
  ssm-staging:
    host: 127.0.0.1
    port: 15432                              # 事前に立てた ssm port-forward
  ssm-prod:
    host: 127.0.0.1
    port: 15433
  ssm-staging-mongo:
    host: 127.0.0.1
    port: 15434                              # mongodb upstream への ssm port-forward

databases:
  - adapter: postgres                        # 必須。`postgres` / `mysql` / `mongodb` をサポート。使う adapter は listen_ports に対応ポートが必要
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
        sslmode: require                     # 省略可 (既定 disable)。RDS 等 SSL 必須の upstream 向け
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

  - adapter: mongodb                         # MongoDB。client は資格情報なしで mongodb://127.0.0.1:27017/events に繋ぐ
    virtual_name: events                     # MongoDB は $db を書き換えないので virtual_name = upstream の物理 DB 名
    targets:
      - name: local
        host: 127.0.0.1
        port: 27018
        user: root                           # upstream への SCRAM-SHA-256 認証に使う
        password: localpass
        auth_source: admin                   # 省略可 (既定 admin)。資格情報が住む認証 DB
      - name: staging
        forward_to: ssm-staging-mongo
        user: events_staging_user
        password: stg_password
        auth_source: admin
```

### バリデーション要点

- 各 database には `adapter` が必須。サポートされるのは `postgres` / `mysql` / `mongodb`。adapter は混在させてよいが、使う adapter には `listen_ports` の対応エントリが必須 (無いと起動時にエラー)。1 ポートは 1 プロトコル固定で、`listen_ports` のポートは重複不可。
- `listen_ports` は最低 1 つの adapter ポートを定義する。どの database にも使われていない adapter のポートは warning (接続は受けるが routing できない)。
- 全 database が同じ target 名集合を持つことを推奨。違っていても起動はする (warning) — DB が staging にまだ用意できていない、といった移行途中の状態を許容するため。`use <target>` 時にその target を持たない database は inactive 化される。
- target は inline (`host` + `port`) か `forward_to` のどちらか一方 (XOR)。
- `user`, `password` は target ごとに必須。
- `sslmode` は省略可 (既定 `disable`)。`require` を指定すると upstream へ TLS ハンドシェイクしてから接続する (証明書検証なし。PostgreSQL は SSLRequest、MySQL は in-band の CLIENT_SSL ネゴシエーション)。RDS など SSL 必須の upstream に繋ぐ場合に指定する。**MongoDB upstream の TLS は未対応** (`sslmode` は MongoDB の database では無視される)。
- `auth_source` は **MongoDB 専用** (省略可、既定 `admin`)。upstream の SCRAM-SHA-256 認証を行う認証 DB。`postgres` / `mysql` の database に書いても無視される (warning)。
- MongoDB は `$db` を書き換えないため、target の `database` は **routing に使われない**。`virtual_name` がそのまま upstream の物理 DB 名として転送される (= `virtual_name` と upstream のコレクションが属する DB 名は一致している必要がある)。`postgres` / `mysql` は従来どおり `database` で物理 DB 名を書き換える。
- `virtual_name` と target の `database` は PG 識別子規則 (`^[A-Za-z0-9_][A-Za-z0-9_$-]{0,62}$`)。

## 使い方

### インストール

[GitHub Releases](https://github.com/ykpythemind/dbpivot/releases) から OS/arch 別のビルド済みバイナリ (linux / darwin × amd64 / arm64) を取得できる。

```bash
# 例: macOS arm64
curl -sSfL https://github.com/ykpythemind/dbpivot/releases/latest/download/dbpivot_<version>_darwin_arm64.tar.gz | tar xz
./dbpivot --version
```

`checksums.txt` で `sha256` を検証できる。

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

MongoDB は資格情報なしで接続する (proxy が auth-disabled な mongod として振る舞うため)。接続文字列のパス部分 (`/events`) が `virtual_name` の selector として効き、そのコレクションは設定した upstream へ流れる:

```bash
mongosh 'mongodb://127.0.0.1:27017/events' \
        --eval 'db.items.insertOne({hello: "world"}); db.items.countDocuments()'
# → 1   (上流 mongod の events DB に書き込まれる)
```

接続文字列に user/password を含めない (含めても proxy 側は認証を要求しない)。実際の認証は target の `user` / `password` / `auth_source` で upstream に対して行われる。

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
# listening postgres=127.0.0.1:6432  current target: local
#   appdb     [postgres] -> local (db=app_dev upstream=127.0.0.1:5432 active=0)
#   analytics [postgres] -> local (db=analytics_dev upstream=127.0.0.1:5432 active=0)

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
  proxy/     server.go             // adapter ごとの TCP listener + accept + 振り分け + SwitchAll
             database.go           // Database, ResolveTarget / Apply, conn registry
             pgwire.go             // PG メッセージ framing と各種 encode/decode
             auth.go               // upstream SCRAM-SHA-256 driver
             mysqlwire.go / mysqlserver.go   // MySQL framing + dispatch
             bson.go / mongowire.go          // BSON codec + MongoDB メッセージ framing (OP_MSG/OP_QUERY)
             mongohandshake.go / mongoadmin.go  // client→proxy hello + admin コマンドのローカル応答
             mongoauth.go / mongoserver.go   // upstream hello+SCRAM auth + dispatch ($db deferred routing)
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

DB ドライバは抱えない (PG / MySQL / MongoDB の wire protocol と BSON codec は自前。MongoDB の SCRAM も `xdg-go/scram` を再利用)。integration test のみ testcontainers / 公式 mongo-driver を使う (build tag `integration_test`)。

## テスト

通常のユニットテスト:

```bash
go test ./...
```

実 DB (PostgreSQL / MySQL / MongoDB) を testcontainers で立てて行う integration test (Docker 必須):

```bash
go test -tags=integration_test ./integration_test/...
```

MongoDB の integration test は実 `mongo:7.0` (auth 有効) を立て、公式 Go driver で proxy 経由の CRUD と未設定 database のエラーを検証する。

CLI レイヤを含む e2e は `e2e/run.sh` から走らせる (docker / psql / jq / go 必須):

```bash
./e2e/run.sh
```

## リリース

[GoReleaser](https://goreleaser.com) で linux / darwin × amd64 / arm64 のバイナリをビルドし、GitHub Releases に公開する。**バージョンタグ (`v*`) を push するだけ**で `.github/workflows/release.yml` が発火し、自動でリリースされる。

```bash
# main にマージ済みの状態でタグを打って push
git tag v1.2.3
git push origin v1.2.3
```

push をトリガーに GoReleaser が `dbpivot_<version>_<os>_<arch>.tar.gz` と `checksums.txt` を Releases に公開する。

- changelog は GitHub のコミットから自動生成 (`docs:` / `test:` / `ci:` / `chore:` / Merge 系は除外)
- `v1.2.3-rc1` のような pre-release タグは `prerelease: auto` で自動的に prerelease 扱いになる
- バージョンは `-X main.version={{.Version}}` で埋め込まれ、`dbpivot --version` で確認できる

タグを打つ前のローカル確認 (公開はされない):

```bash
goreleaser check                       # .goreleaser.yaml の検証
goreleaser release --snapshot --clean  # dist/ にビルドして中身を確認
```

設定は [.goreleaser.yaml](.goreleaser.yaml)、CI は [.github/workflows/release.yml](.github/workflows/release.yml) を参照。

## 設計の詳細

実装プランは [docs/plans/2026-05-12-dbpivot-implementation.md](docs/plans/2026-05-12-dbpivot-implementation.md) に。
