# dbpivot 実装プラン

## Context

ローカル開発時に「同じアプリから接続する DB を、ローカル⇄リモートで瞬時に切り替えたい」需要に応える道具。アプリの接続文字列を書き換える運用は摩擦が大きく、再起動も発生する。アプリは常にプロキシ (`dbpivot`) に繋ぎ、CLI から上流ターゲットを切り替えることで、アプリ側の設定を一切触らずに接続先を変えられる状態を目指す。

リモートDBへの接続は、ユーザーが事前に `aws ssm start-session --document-name AWS-StartPortForwardingSessionToRemoteHost` などで `127.0.0.1:15432` のようなローカル forward 先を立てておく前提。dbpivot はそれを `forward_target` として参照するだけで、ssm セッション自体は管理しない。

```
local app  → (port 6432, dbname=appdb)  →  dbpivot  →  local DB (default)
                                                          →  ssm forward → remote DB
```

**v1 は PostgreSQL 専用**。アプリは 1 つのポート (`port:`) に対し、接続時の `database` パラメータに pool 名を書いて接続する。proxy は StartupMessage を読み、pool を選択、target の `user/password/database` で upstream に対して **代理認証** を行い (SCRAM-SHA-256)、認証完了後はバイト pipe。client → proxy は trust auth (任意 password で受け入れる)。

`database` フィールドはテンプレート (`${VAR}`) を含んで良く、`switch` コマンド時の `--var KEY=VAL` で解決される — ブランチ名やユーザー名で動的に DB を分ける運用に対応するため。

開発用ツール。HA、観測、TLS、認証セキュリティ強化等のプロダクション要件は対象外。

## 設計判断 (確定済み)

| 項目 | 決定 |
|---|---|
| 言語 | Go (1.22+, `log/slog`、`atomic.Pointer[T]`) |
| 対応プロトコル (v1) | **PostgreSQL のみ**。`protocol:` フィールドは v1 では持たない |
| 単一 listen ポート | top-level `port:` 1 つ。bind 先は `127.0.0.1` 固定。アプリは `dbname=<pool name>` で pool 選択 |
| client → proxy 認証 | **trust** (任意 password で受け入れる)。proxy は client の auth bytes を一切検証しない |
| proxy → upstream 認証 | **SCRAM-SHA-256** のみサポート (PG 14+ default)。target.user / target.password を proxy が使う |
| 切替時の既存接続 | **即時切断**。クライアント再接続で新ターゲットへ |
| 制御チャネル | Unix domain socket + 改行区切り JSON |
| CLI | `github.com/spf13/cobra`。切替コマンド名は **`switch`** |
| 設定 | YAML、`--config` 必須 (デフォルトパス無し) |
| `reload` | v1 に含める。reload 時は全 active connection 破棄 |
| SSM | スコープ外。`forward_targets` で host:port を指すだけ |
| `forward_targets` | top-level map (name → {host, port})。**省略可** (全 target が inline なら) |
| target の接続先指定 | inline (host + port) **か** `forward_to: <name>` の **XOR** |
| variables 対象 | `target.database` のみ。`${VAR}` 形式、値は `--var KEY=VAL` から (環境変数フォールバック無し) |
| variables 永続性 | 揮発。`switch` ごとに必要な変数を全て明示。不足ならエラー、状態変更なし |
| 未知 pool 名で接続 | PG ErrorResponse (`pool "foo" not configured`) を返して close |
| CancelRequest | v1 では捨てる (upstream に forward しない、即 close) |

## ファイル/パッケージ構成

```
dbpivot/
  go.mod
  cmd/dbpivot/main.go          // cobra root + サブコマンド
  internal/
    config/config.go                  // Load, Validate, ForwardTarget, Pool, Target
    config/variables.go               // RequiredVars / Resolve
    proxy/server.go                   // 単一 listener + accept + pool routing
    proxy/pool.go                     // Pool, Switch (target 解決と force close)
    proxy/pgwire.go                   // メッセージ framing、StartupMessage / Authentication* / ErrorResponse
    proxy/auth.go                     // SCRAM-SHA-256 driver (upstream 認証)
    control/protocol.go               // Req/Res 型と cmd 名
    control/server.go                 // Unix socket サーバ、dispatch
    control/client.go                 // CLI 側: dial / 1コマンド送受信
```

`pkg/` は作らない。外部公開 API 無し。Handler abstraction は不要 (v1 はプロトコル 1 種)。

## 設定 (YAML)

`--config` で必須指定。top-level `port` 1 つ。target は inline か `forward_to` で接続先を指定。

```yaml
port: 6432                                   # アプリが接続する単一の listen port (127.0.0.1)
control_socket: /tmp/dbpivot.sock     # 省略可、デフォルト同値

forward_targets:                             # 省略可 (リモート系の共有用)
  ssm-staging:
    host: 127.0.0.1
    port: 15432                              # 事前に立てた ssm port-forward
  ssm-prod:
    host: 127.0.0.1
    port: 15433

pools:
  - name: appdb                              # アプリは dbname=appdb で接続
    default: local
    targets:
      - name: local
        host: 127.0.0.1                      # inline
        port: 5432
        user: postgres
        password: localpass
        database: app_dev
      - name: staging
        forward_to: ssm-staging              # 名前参照
        user: app_staging_user
        password: stg_password_xxx
        database: app_${BRANCH}_staging      # switch 時に --var BRANCH=... 必須
      - name: prod
        forward_to: ssm-prod
        user: app_prod_user
        password: prod_password_yyy
        database: app_prod

  - name: analytics                          # アプリは dbname=analytics で接続
    default: local
    targets:
      - name: local
        host: 127.0.0.1
        port: 5432
        user: postgres
        password: localpass
        database: analytics_dev
```

Go 型 (`internal/config`):

```go
type ForwardTarget struct {
    Host string `yaml:"host"`
    Port int    `yaml:"port"`
}

type Target struct {
    Name      string `yaml:"name"`

    // inline 接続先 (forward_to と XOR)
    Host string `yaml:"host,omitempty"`
    Port int    `yaml:"port,omitempty"`

    // 名前参照 (host/port と XOR)
    ForwardTo string `yaml:"forward_to,omitempty"`

    User     string `yaml:"user"`
    Password string `yaml:"password"`
    Database string `yaml:"database,omitempty"` // ${VAR} 可。空なら pass-through
}

type Pool struct {
    Name    string   `yaml:"name"`
    Default string   `yaml:"default"`
    Targets []Target `yaml:"targets"`
}

type Config struct {
    Port           int                      `yaml:"port"`
    ControlSocket  string                   `yaml:"control_socket"`
    ForwardTargets map[string]ForwardTarget `yaml:"forward_targets,omitempty"`
    Pools          []Pool                   `yaml:"pools"`
}
```

ロード時のバリデーション (`config.Load`):

1. `port` が `1 ≤ port ≤ 65535`。
2. `forward_targets` (存在すれば) の各キーが非空、`host` 非空、`port` が `1 ≤ port ≤ 65535`。
3. `pools` の長さ ≥ 1。`pool.name` 非空、ファイル内で一意。pool 名は `^[A-Za-z0-9_][A-Za-z0-9_$-]{0,62}$` (クライアントが dbname として送るので PG identifier 範囲)。
4. `pool.targets` の長さ ≥ 1。`target.name` 非空、pool 内で一意。
5. **target.host/port と target.forward_to は XOR**:
   - inline: `host` と `port` 両方必須、`forward_to` は空
   - forward: `forward_to` 非空かつ `forward_targets` のキーに存在、`host`/`port` は空
6. `target.user` 非空、`target.password` 非空 (SCRAM に必須)。
7. `pool.default` が同 pool の `target.name` と一致。
8. **default target の `database` に `${VAR}` を含まないこと** (含めばエラー — daemon 起動時に解決手段が無い)。default 以外は variables 込みでよい。
9. default target の `database` が空 → WARN: "client-supplied dbname will be passed through"。
10. `target.database` の解決済み値が `^[A-Za-z0-9_][A-Za-z0-9_$-]{0,62}$` にマッチすること (default は load 時に静的検査、variables 必須 target は switch 時に検査)。

## variables (テンプレート展開)

### 構文

`${VAR}` のみ。`$VAR` (波括弧なし)、`${VAR:-default}` などのシェル拡張は非対応。`$$` は `$` リテラル。

`internal/config/variables.go`:

```go
// RequiredVars は s 中の "${VAR}" を抽出し、出現順・重複排除して返す。
func RequiredVars(s string) []string

// Resolve は s 中の "${VAR}" を vars[VAR] で置換する。
// vars に欠けるキーがあれば missing にそれを列挙してエラーを返す。
func Resolve(s string, vars map[string]string) (resolved string, missing []string, err error)
```

実装は自前パーサ ~20 LOC (`${` を見たら `}` までを VAR とし、未閉じはエラー)。

### 値ソース

- `--var KEY=VAL` を複数回、または `--var KEY1=VAL1,KEY2=VAL2` のカンマ区切り。
- **環境変数フォールバックなし** (意図しない展開や、CLI と daemon の env 差異からくる事故を避けるため)。
- 過剰 var (target が必要としていない KEY) は許容、warn なし。
- 不足 var は error、状態変更なし。

### daemon 側

各 `switch` リクエストで:
1. target を引く。未知ならエラー。
2. `RequiredVars(target.Database)` で必要キーを得る。
3. `Resolve(target.Database, req.Variables)`。missing が空でなければエラー。
4. 解決済み値が identifier 正規表現にマッチしなければエラー。
5. ここで初めて `pool.current.Store` と接続強制 close。

Pool は `current` として「解決済み Target スナップショット」を保持:

```go
type ResolvedTarget struct {
    Name     string     // 元の target 名
    Host     string     // inline か forward_target から解決済み
    Port     int
    User     string
    Password string
    Database string     // 解決済み database (空なら pass-through)
}
```

## ランタイム構造

```go
type Server struct {
    listener net.Listener            // top-level port 1 つ
    pools    map[string]*Pool        // pool name -> *Pool
    control  net.Listener
    mu       sync.Mutex
    closed   bool
}

type Pool struct {
    name    string
    targets []Target                 // config 由来 (テンプレート保持)
    byName  map[string]*Target
    fwd     map[string]ForwardTarget // forward_targets スナップショット (resolve用)
    current atomic.Pointer[ResolvedTarget]

    mu    sync.Mutex
    conns map[*Conn]struct{}         // active
}

type Conn struct {
    client, upstream net.Conn
    resolved         ResolvedTarget  // accept 時のスナップショット
    closeOnce        sync.Once
}
```

- Server は 1 つの accept goroutine。conn ごとに handler goroutine を起動。
- handler は startup 解析 → pool 引き → upstream dial + SCRAM 認証 → 登録 → bidi pipe。
- どちらか終了で `closeOnce.Do(close both + deregister)`。
- control の dispatch goroutine は別。シリアル処理。
- シグナル待ち goroutine 1本 (`SIGINT`/`SIGTERM` → `Server.Shutdown`)。

## PostgreSQL 接続処理フロー

`*bufio.Reader` で `client` を包む。全 int は big-endian。

定数:
- `MaxStartupLen = 64 * 1024`
- `sslReq    = 0x04D2162F` (80877103)
- `gssReq    = 0x04D21630` (80877104)
- `cancelReq = 0x04D2162E` (80877102)
- `protoV3   = 0x00030000` (196608)

### 1. Startup 状態遷移

ループ最大 2 回 (SSL/GSS preamble 1 回 → 本番 StartupMessage 1 回):

```
for attempt := 0; attempt < 2; attempt++ {
    msgLen := readUint32(br)
    if msgLen < 8 || msgLen > MaxStartupLen { return errMalformed }
    code   := readUint32(br)

    switch code {
    case sslReq, gssReq:
        client.Write([]byte{'N'})         // 拒否、クライアントは平文で再送
        continue
    case cancelReq:
        io.CopyN(io.Discard, br, 8)       // 残り 8 バイト drain
        return nil                         // v1: 捨てる
    case protoV3:
        return handleStartup(...)
    default:
        return fmt.Errorf("unsupported pg protocol code 0x%08x", code)
    }
}
return errors.New("client did not send StartupMessage after preamble")
```

### 2. `handleStartup`

```go
body := make([]byte, msgLen-8)
io.ReadFull(br, body)

params, err := parseStartupBody(body)        // []kv、順序保持
if err != nil { return err }

dbname := lookupParam(params, "database")
if dbname == "" { dbname = lookupParam(params, "user") }  // PG 慣習

pool, ok := server.pools[dbname]
if !ok {
    writeErrorResponse(client, "FATAL", "3D000",
        fmt.Sprintf("pool %q not configured", dbname))
    return nil
}

rt := pool.current.Load()

// upstream に渡す StartupMessage を組み立て
upParams := []kv{
    {K: "user", V: rt.User},
}
if rt.Database != "" {
    upParams = append(upParams, kv{K: "database", V: rt.Database})
} else {
    // pass-through: client が送ってきた database をそのまま使う
    if d := lookupParam(params, "database"); d != "" {
        upParams = append(upParams, kv{K: "database", V: d})
    }
}
// application_name 等の追加パラメータは client のまま転送 (user/database 除く)
for _, p := range params {
    if p.K == "user" || p.K == "database" { continue }
    upParams = append(upParams, p)
}
upStartup := encodeStartup(upParams)

up, err := net.DialTimeout("tcp", net.JoinHostPort(rt.Host, strconv.Itoa(rt.Port)), dialTimeout)
if err != nil {
    writeErrorResponse(client, "FATAL", "08006",
        fmt.Sprintf("upstream dial failed: %v", err))
    return err
}

if _, err := up.Write(upStartup); err != nil { up.Close(); return err }

// upstream と SCRAM 認証
if err := authenticateUpstream(up, rt.User, rt.Password); err != nil {
    writeErrorResponse(client, "FATAL", "28P01",
        fmt.Sprintf("upstream auth failed: %v", err))
    up.Close()
    return err
}

// client に AuthenticationOk を返す (trust auth)
writeAuthenticationOk(client)
// 以降は bidi pipe で upstream の ParameterStatus / BackendKeyData / ReadyForQuery が
// 自然に client へ流れる。

return runConn(pool, client, up, *rt)        // pool.conns 登録 + PipeBidi
```

### 3. `authenticateUpstream` (SCRAM-SHA-256)

`internal/proxy/auth.go`。upstream からのメッセージ ('R' = Authentication) を読み、種別に応じて処理:

| code | meaning | v1 動作 |
|---|---|---|
| `0` (AuthenticationOk) | 認証不要 | そのまま return nil |
| `10` (AuthenticationSASL) | SCRAM 開始要求 | SCRAM-SHA-256 を選び、client-first-message を送信 |
| `11` (AuthenticationSASLContinue) | server-first-message | client-final-message を送信 |
| `12` (AuthenticationSASLFinal) | server-final-message | verify、続けて AuthenticationOk を待つ |
| `3` (CleartextPassword) | 旧式 | エラー: "v1 supports SCRAM-SHA-256 only" |
| `5` (MD5) | 旧式 | エラー: "v1 supports SCRAM-SHA-256 only" |
| その他 | | エラー |

SCRAM 実装は `github.com/xdg-go/scram` の Client を使う (~20 LOC で接続):

```go
client, _ := scram.SHA256.NewClient(user, password, "")
conv := client.NewConversation()
clientFirst, _ := conv.Step("")                       // "n,,n=user,r=nonce"

// → SASLInitialResponse (PasswordMessage 'p' に SCRAM mech 名と clientFirst を入れて送信)
writeSASLInitialResponse(up, "SCRAM-SHA-256", clientFirst)

// upstream から AuthenticationSASLContinue を受信
serverFirst, _ := readSASLContinueData(up)
clientFinal, _ := conv.Step(serverFirst)              // "c=biws,r=nonce,p=proof"

// → SASLResponse (PasswordMessage 'p' に clientFinal をそのまま入れて送信)
writeSASLResponse(up, clientFinal)

// upstream から AuthenticationSASLFinal を受信
serverFinal, _ := readSASLFinalData(up)
if _, err := conv.Step(serverFinal); err != nil { return err }  // verify

// upstream から AuthenticationOk を受信
return readAuthenticationOk(up)
```

### 4. `pgwire.go` の関数

手書き ~150 LOC:

```go
type kv struct{ K, V string }

// Startup
func parseStartupBody(b []byte) ([]kv, error)
func lookupParam(p []kv, key string) string
func encodeStartup(params []kv) []byte         // [len|0x00030000|key\0val\0...\0]

// 一般メッセージ (type byte + length + body)
func readMessage(r io.Reader) (typ byte, body []byte, err error)
func writeMessage(w io.Writer, typ byte, body []byte) error

// 特殊メッセージ
func writeAuthenticationOk(w io.Writer) error                 // 'R' + 8 + 0
func writeSASLInitialResponse(w io.Writer, mech, data string) error  // 'p' + ... mech\0 lenInt4 data
func writeSASLResponse(w io.Writer, data string) error        // 'p' + ... data
func parseAuthenticationMessage(body []byte) (code int32, data []byte) // body[0:4]=code, body[4:]=data
func writeErrorResponse(w io.Writer, severity, sqlstate, msg string) error
```

PG ErrorResponse は auth 前でも psql が適切にハンドル。`pgproto3` は使わない (Parameters が map で encode 順非決定、テストが書きにくい)。

### 5. エラー処理

| ケース | 動作 |
|---|---|
| length/code の短読み | WARN `pg startup: short read`、close |
| `msgLen` 範囲外 | WARN `pg startup: length=%d out of range`、close |
| `parseStartupBody` 失敗 | WARN `malformed params`、close |
| 未知 protocol code | WARN `unknown code 0x%08x`、close |
| 未知 pool 名 | INFO `unknown pool %q from %s`、PG ErrorResponse 後 close |
| 上流 `DialTimeout` 失敗 | ERROR `upstream dial`、PG ErrorResponse 後 close |
| upstream が SCRAM 以外を要求 | ERROR `unsupported upstream auth method (%d)`、PG ErrorResponse 後 close |
| SCRAM 認証失敗 (server-final verify) | ERROR `auth failed`、PG ErrorResponse 後 close |

全ての (client, upstream) は `pool.conns` に登録され `closeOnce` 規律下。`switch`/`shutdown` は force-close。

### 6. v1 で **やらない** こと

- listen 側 TLS (SSLRequest には `N` を返す)
- 上流 TLS (ローカル ssm forward は平文前提)
- client → proxy の password 検証 (trust auth)
- SCRAM-SHA-256 以外の upstream auth (MD5、cleartext、GSSAPI、Kerberos)
- SCRAM channel binding (`tls-server-end-point`) — `n,,` prefix で常に no binding
- Startup 以降のメッセージ解釈 — bidi pipe
- CancelRequest のルーティング (BackendKeyData の追跡が必要、追加実装あり)
- `protocol: tcp` (MySQL/Mongo 用)

## 切替アルゴリズム (`switch`)

1. `pool.byName[name]` を引く。未知なら error、状態変更なし。
2. target の host/port を解決 (inline ならそのまま、forward_to なら `pool.fwd[name]` を引く)。
3. `RequiredVars(target.Database)` を取得、variables で全て埋まるか検査。不足ならエラー。
4. Resolve した database が identifier 正規表現を満たすか検査。
5. `ResolvedTarget{Name, Host, Port, User, Password, Database}` を組み立て `pool.current.Store(rt)`。
6. `pool.mu` 取って `conns` のスナップショット (slice) を取り、ロック解放。
7. snapshot 内の各 `Conn.client/upstream` を Close。残処理 (deregister) は `closeOnce` に任せる。

snapshot 後にロックを離してから close (デッドロック回避)。CLI へは「`previous → current (db=app_main_staging)`、closed N」が確定した段階で応答 (実 close は非同期完了、許容)。

## 制御プロトコル (Unix socket / 改行区切り JSON)

socket は `0600`、1接続1コマンド (応答後にサーバ側からclose)。

### `switch`
```json
// req
{"cmd":"switch","pool":"appdb","target":"staging","variables":{"BRANCH":"main"}}
// res
{"ok":true,"pool":"appdb","previous":"local","previous_database":"app_dev",
 "current":"staging","current_database":"app_main_staging","closed_conns":3}
// err
{"ok":false,"error":"unknown target \"qa\" for pool \"appdb\""}
{"ok":false,"error":"target \"staging\" requires variables: BRANCH","missing":["BRANCH"]}
{"ok":false,"error":"resolved database \"app foo\" contains invalid characters"}
```

### `status`
```json
{"cmd":"status"}                                 // 全部
{"cmd":"status","pool":"appdb"}                  // 個別
// res
{"ok":true,"port":6432,"pools":[
  {"name":"appdb","current":"local","current_database":"app_dev",
   "current_host":"127.0.0.1","current_port":5432,"active_conns":2},
  {"name":"analytics","current":"local","current_database":"analytics_dev",
   "current_host":"127.0.0.1","current_port":5432,"active_conns":0}
]}
```

### `list`
```json
{"cmd":"list"}
// res — password は出力しない
{"ok":true,"port":6432,
 "forward_targets":{
   "ssm-staging":{"host":"127.0.0.1","port":15432}
 },
 "pools":[
  {"name":"appdb","default":"local","current":"local",
   "targets":[
     {"name":"local","host":"127.0.0.1","port":5432,"user":"postgres",
      "database_template":"app_dev","required_variables":[]},
     {"name":"staging","forward_to":"ssm-staging","user":"app_staging_user",
      "database_template":"app_${BRANCH}_staging","required_variables":["BRANCH"]}
   ]}
 ]}
```

### `reload`

`--config` を再読み込み、Server の `pools` / `forward_targets` を差し替え。**全 active connections を破棄**。

- `port` 変更は **再起動を要求** (listener のホットスワップは扱わない、warning に列挙)。
- pool の追加 / 削除 / target の追加・変更は受け付ける。
- `current` の target 名が新configから消えていれば `default` にフォールバック。
- 新 config の default target が validation 違反なら `ok:false`、running state は無変更。

```json
{"cmd":"reload"}
// res
{"ok":true,"pools_updated":2,"dropped_conns":5,"warnings":["port 変更は再起動が必要 (running=6432, config=6433)"]}
// err
{"ok":false,"error":"validation: pool 'appdb' default 'foo' not in targets"}
```

### 未知コマンド
```json
{"ok":false,"error":"unknown command: foo"}
```

## CLI

```
dbpivot serve   --config PATH [--socket PATH] [--log-level info|debug]
dbpivot switch  <pool> <target> [--var KEY=VAL]... [--socket PATH] [--json]
dbpivot status  [<pool>]                            [--socket PATH] [--json]
dbpivot list                                        [--socket PATH] [--json]
dbpivot reload                                      [--socket PATH] [--json]
```

- `--config` は `serve` 専用、かつ必須。
- `--socket` デフォルトは `/tmp/dbpivot.sock`。
- `--var` は `KEY=VAL` を複数回 or `KEY1=VAL1,KEY2=VAL2` 形式 (cobra `StringSlice`)。control プロトコル上の JSON キーは `variables`。
- `--json` で生 JSON 応答をそのまま stdout。未指定なら人間向け整形 (例: `appdb: local (db=app_dev) -> staging (db=app_main_staging) (closed 3 connection(s))`)。
- exit code: `0` 成功 / `1` daemon応答 `ok:false` / `2` クライアント側エラー (socket dial 失敗、引数不正)。

## ライフサイクル

**起動 (`serve`)**:
1. config load + validate。default target の variables 必須違反等あれば exit 1。
2. `net.Dial("unix", socket)` 試行 — 成功すれば別 daemon あり、`status` を取得して出力し exit 1。
3. `ENOENT` は無視、`ECONNREFUSED` なら stale → `os.Remove` してから進む。
4. 各 pool の default target を解決して `ResolvedTarget` を `pool.current` に store。
5. `net.Listen("tcp", "127.0.0.1:"+port)`。失敗なら exit 1。
6. control socket bind (0600)。
7. accept goroutine 起動。

**新規接続で上流に届かない / SCRAM 失敗**: PG ErrorResponse 返却、client close、warn/error ログ。

**切替先未知 / variables 不足**: control が `ok:false`、状態変更なし、debug ログ。

**shutdown (SIGINT/SIGTERM)**: `Server.Shutdown(ctx, 5s)`:
1. 主 listener close (accept 停止)。
2. 全 pool の active conn を switch と同じ snapshot-then-close で close。
3. control listener close。
4. `os.Remove(socket)`。
5. 戻る。main exit 0。
- 5秒で io.Copy 群が抜けなくても return (ソケット閉じてるので個別に死ぬ)。

**並行 shutdown + control**: `Server.closed` フラグを mutex で守り、control dispatch は冒頭でこれをチェック → 真なら `{"ok":false,"error":"shutting down"}`。

## 依存

- YAML: `gopkg.in/yaml.v3`
- CLI: `github.com/spf13/cobra` (viper 無し)
- SCRAM: `github.com/xdg-go/scram` (SHA-256 client、~3 関数だけ使う)
- ログ: stdlib `log/slog`、text handler を stderr、`--log-level` で切替
- テスト: stdlib `testing`、必要に応じて `testify/require`
- **PG wire は自前** (`internal/proxy/pgwire.go`、~150 LOC)。`pgproto3` は使わない
- DB ドライバなし

## 検証

### E2E (手動)

ローカル Postgres を 2 つ立てる。インスタンス A (5432) に `app_dev` DB、インスタンス B (5433) に `app_main_staging` DB を作成。両方 SCRAM-SHA-256 で認証。

```yaml
port: 6432
pools:
  - name: appdb
    default: local
    targets:
      - name: local
        host: 127.0.0.1
        port: 5432
        user: postgres
        password: localpass
        database: app_dev
      - name: staging
        host: 127.0.0.1
        port: 5433
        user: postgres
        password: stgpass
        database: app_${BRANCH}_staging
```

手順:
1. `dbpivot serve --config ./config.yaml` → `listening 127.0.0.1:6432`、`pool appdb current=local (db=app_dev)`
2. `psql 'host=127.0.0.1 port=6432 user=anyuser dbname=appdb password=anything sslmode=disable' -At -c 'select current_database()'` → `app_dev` (client password は何でもよい = trust)
3. `sslmode=prefer` でも同様 (SSLRequest → 'N' 経路)
4. `psql ... dbname=nonexistent_pool ...` → `psql: error: ... FATAL: pool "nonexistent_pool" not configured`
5. `dbpivot status` → `current=local, current_database=app_dev`
6. `dbpivot switch appdb staging` → ERROR `missing variables: BRANCH`
7. `dbpivot switch appdb staging --var BRANCH=main` → `appdb: local (db=app_dev) -> staging (db=app_main_staging)`
8. 別タブの psql が切断、再接続で `select current_database()` が `app_main_staging`、`SELECT current_user;` が `postgres` (target.user)
9. `dbpivot switch appdb staging --var BRANCH=feat-x` → 再切替 (DB が無ければ PG が `database does not exist` を返す。期待挙動)
10. `dbpivot switch appdb local` → `app_dev` に戻る (variables 不要)
11. `dbpivot reload` → 対象 pool 全切断、再接続で新設定
12. `SIGINT` でクリーン終了、socket ファイル消える

### 単体テスト

`internal/config/config_test.go`:
- table-driven: port 未設定 / port 範囲外 / pool 名重複 / pool 名が PG 識別子規則違反 / pools 空 / target.name 重複 / **inline と forward_to 併用 (error)** / **inline で host 欠落 (error)** / forward_to 未定義 / target.user 欠落 / target.password 欠落 / **default target.database に `${VAR}` (error)** / default target.database 空 (warn のみ) / target.database 解決済み値が正規表現にマッチしない

`internal/config/variables_test.go`:
- `RequiredVars`: `"app_${BRANCH}_${USER}"` → `["BRANCH","USER"]`、重複排除、出現順
- `Resolve`: 正常展開 / missing 列挙 / 余剰 var 許容 / `$$` リテラル / 未閉じ `${BRANCH` でエラー

`internal/proxy/pool_test.go`:
- **TestSwitchUpdatesCurrent**: `Switch("staging", {BRANCH:"main"})` で `Current().Database == "app_main_staging"`
- **TestSwitchResolvesForwardTo**: forward_to の target を switch すると Current().Host/Port が `forward_targets` の値
- **TestSwitchClosesExistingConns**: ダミー conn を 3 つ登録 → `Switch` で全て Close される
- **TestSwitchUnknownTarget**: error、`Current()` 不変
- **TestSwitchMissingVariables**: variables 必須 target に空 variables → error、`Current()` 不変
- **TestSwitchInvalidResolvedDatabase**: `app foo` のような空白入り → error
- **TestRegistryNoLeak**: 100 conn を順次登録/解除、終了時 `len(pool.conns)==0`

`internal/proxy/pgwire_test.go`:
- **TestParseEncodeRoundtrip**: 既知 StartupMessage を parse → encode で bytes 一致
- **TestEncodeUpstreamStartup**: client params が `[user=alice, database=appdb, application_name=psql]` で target.User=postgres, target.Database=app_dev → 結果に user=postgres、database=app_dev、application_name=psql が含まれる
- **TestParseMalformed**: 末尾 NUL 欠落、奇数 NUL、長さ不整合 — それぞれエラー
- **TestErrorResponseFormat**: `writeErrorResponse` の bytes 出力を手作りパーサで読み戻して FATAL/3D000/メッセージが取り出せる
- **TestAuthenticationOkFormat**: `writeAuthenticationOk` が `'R' + 0x00000008 + 0x00000000`

`internal/proxy/auth_test.go`:
- **TestSCRAMHappyPath**: モック upstream を立て、AuthenticationSASL → server-first → server-final → AuthenticationOk のシーケンスを返す。`authenticateUpstream` が正しく client-first/client-final を送信し、AuthenticationOk まで完了する。`xdg-go/scram` をテストで参考実装として使う
- **TestSCRAMBadPassword**: server-final で verify 失敗 → error
- **TestSCRAMUnsupportedMethod**: AuthenticationCleartextPassword (3) や MD5 (5) を返す → "unsupported upstream auth method"

`internal/proxy/server_test.go` (`net.Pipe` or `net.Listen("tcp",":0")` で client/upstream 模擬):
- **TestRouteByDbname**: 2 つの fake upstream (各々モック SCRAM 応答) を立て、pool A/B を dbname=A/B で接続 → 正しい upstream に届く、それぞれ user/database が target の値に書き換えられている
- **TestSSLRequestThenStartup**: `[0x00000008, 0x04D2162F]` → client に `'N'`、続けて StartupMessage が処理される
- **TestUnknownPoolReturnsErrorResponse**: dbname=foo → client が PG ErrorResponse を受け取り conn close
- **TestEmptyTargetDatabasePassthrough**: target.Database 空 → upstream に届く database が client 指定値のまま
- **TestCancelRequestDropped**: `[0x00000010, 0x04D2162E, pid, secret]` → 何も upstream に送られず、client conn close
- **TestPreambleLoopBound**: SSLRequest 2 連続 → 2 回目で error

`internal/control/server_test.go`:
- `t.TempDir()` 配下の socket で各 cmd の正常/異常系 JSON を検証 (`switch`/`status`/`list`/`reload` × 成功/失敗、特に variables 不足の error 形)

実 DB を使う統合テストは v1 では作らない (E2E 手順で代替)。

## 実装対象ファイル

- `/Users/ykpythemind/git/github.com/ykpythemind/dbpivot/go.mod`
- `/Users/ykpythemind/git/github.com/ykpythemind/dbpivot/cmd/dbpivot/main.go`
- `/Users/ykpythemind/git/github.com/ykpythemind/dbpivot/internal/config/config.go`
- `/Users/ykpythemind/git/github.com/ykpythemind/dbpivot/internal/config/variables.go`
- `/Users/ykpythemind/git/github.com/ykpythemind/dbpivot/internal/proxy/server.go`
- `/Users/ykpythemind/git/github.com/ykpythemind/dbpivot/internal/proxy/pool.go`
- `/Users/ykpythemind/git/github.com/ykpythemind/dbpivot/internal/proxy/pgwire.go`
- `/Users/ykpythemind/git/github.com/ykpythemind/dbpivot/internal/proxy/auth.go`
- `/Users/ykpythemind/git/github.com/ykpythemind/dbpivot/internal/control/protocol.go`
- `/Users/ykpythemind/git/github.com/ykpythemind/dbpivot/internal/control/server.go`
- `/Users/ykpythemind/git/github.com/ykpythemind/dbpivot/internal/control/client.go`
- 各 `*_test.go`
