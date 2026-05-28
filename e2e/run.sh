#!/usr/bin/env bash
# End-to-end smoke test for dbpivot.
#
#   1. docker で postgres を 2 つ + mysql を 2 つ起動 (local / staging を模擬)
#   2. 同じスキーマを流し、別データを入れる
#   3. dbpivot を build → 1 インスタンスで postgres+mysql の両方を serve
#   4. tiny Go web サーバを起動 (proxy 経由で SELECT, postgres 側を確認)
#   5. mysql CLI を proxy 経由で実行し、mysql 側も同じデータが見えることを確認
#   6. dbpivot use staging → PG/MySQL 両方が同時に staging に切替わることを確認
#   7. dbpivot use local で戻り、両方が local に戻ることを確認
#
# Cleanup (containers / processes) は trap で全部行う。
#
# 必要: docker, psql, jq
# (mysql クライアントは container 内蔵の mysql:8 を docker exec 経由で使うので
#  ホスト側へのインストールは不要。Homebrew の Oracle mysql 9 client は
#  mysql_native_password plugin を除去済みで proxy と handshake できない。)

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

DBPIVOT_PORT=${DBPIVOT_PORT:-6432}
DBPIVOT_MYSQL_PORT=${DBPIVOT_MYSQL_PORT:-63306}
WEB_PORT=${WEB_PORT:-18080}
PG_LOCAL_PORT=${PG_LOCAL_PORT:-55432}
PG_STAGING_PORT=${PG_STAGING_PORT:-55433}
MY_LOCAL_PORT=${MY_LOCAL_PORT:-53306}
MY_STAGING_PORT=${MY_STAGING_PORT:-53307}

PG_LOCAL_NAME=dbpivot-e2e-pg-local
PG_STAGING_NAME=dbpivot-e2e-pg-staging
MY_LOCAL_NAME=dbpivot-e2e-my-local
MY_STAGING_NAME=dbpivot-e2e-my-staging

PG_USER=appuser
PG_PASS=apppass
PG_DB=appdb

MY_USER=appuser
MY_PASS=apppass
MY_DB=appdb
MY_ROOT_PASS=rootpass

WORK="$(mktemp -d -t dbpivot-e2e.XXXXXX)"
echo "[e2e] workdir: $WORK"

# Verify required CLIs up front so the failure mode is obvious rather than the
# script getting most of the way through and then a `mysql -e ...` returning 127.
for bin in docker psql jq; do
    if ! command -v "$bin" >/dev/null 2>&1; then
        echo "[e2e] required command not found: $bin" >&2
        exit 1
    fi
done

cleanup() {
    set +e
    echo "[e2e] cleanup..."
    [[ -n "${WEB_PID:-}" ]]   && kill "$WEB_PID"   2>/dev/null && wait "$WEB_PID"   2>/dev/null
    [[ -n "${PIVOT_PID:-}" ]] && kill "$PIVOT_PID" 2>/dev/null && wait "$PIVOT_PID" 2>/dev/null
    docker rm -f "$PG_LOCAL_NAME" "$PG_STAGING_NAME" "$MY_LOCAL_NAME" "$MY_STAGING_NAME" >/dev/null 2>&1
    rm -rf "$WORK"
}
trap cleanup EXIT

wait_tcp() {
    local host=$1 port=$2 name=$3
    for _ in $(seq 1 100); do
        if (exec 3<>/dev/tcp/"$host"/"$port") 2>/dev/null; then
            exec 3>&- 3<&-
            return 0
        fi
        sleep 0.2
    done
    echo "[e2e] timeout waiting for $name ($host:$port)" >&2
    return 1
}

wait_pg_ready() {
    local port=$1
    for _ in $(seq 1 60); do
        if PGPASSWORD=$PG_PASS psql -h 127.0.0.1 -p "$port" -U $PG_USER -d $PG_DB -tAc 'select 1' >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    echo "[e2e] timeout waiting for postgres on port $port" >&2
    return 1
}

# wait_my_ready blocks until the non-root MYSQL_USER is reachable via the
# container's own mysql client. Because MYSQL_USER is created only after the
# initial bootstrap finishes, a successful select on that user implies the
# container is fully ready (handshake + auth + DB exists).
wait_my_ready() {
    local name=$1
    for _ in $(seq 1 120); do
        if docker exec "$name" mysql -u "$MY_USER" -p"$MY_PASS" -D "$MY_DB" \
                --connect-timeout=2 -N -e 'SELECT 1' >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    echo "[e2e] timeout waiting for mysql container $name" >&2
    return 1
}

# my_exec runs a SQL command directly against a mysql container's own server
# (used for seeding the upstream containers, before dbpivot is involved).
my_exec() {
    local name=$1 sql=$2
    docker exec "$name" mysql -u "$MY_USER" -p"$MY_PASS" -D "$MY_DB" -e "$sql"
}

# my_via_proxy runs a SQL command via the dbpivot mysql port, using the local
# mysql container as a sidecar client (the image still ships the
# mysql_native_password plugin that the proxy advertises in its greeting).
# host.docker.internal routes back to the host where dbpivot listens.
my_via_proxy() {
    local sql=$1
    docker exec "$MY_LOCAL_NAME" mysql -h host.docker.internal -P "$DBPIVOT_MYSQL_PORT" \
        -u anyone -panypass --ssl-mode=DISABLED -D mydb -BN -e "$sql"
}

start_pg() {
    local name=$1 hostport=$2
    docker rm -f "$name" >/dev/null 2>&1 || true
    docker run -d --rm --name "$name" \
        -e POSTGRES_USER=$PG_USER \
        -e POSTGRES_PASSWORD=$PG_PASS \
        -e POSTGRES_DB=$PG_DB \
        -e POSTGRES_HOST_AUTH_METHOD=scram-sha-256 \
        -e POSTGRES_INITDB_ARGS="--auth-host=scram-sha-256 --auth-local=scram-sha-256" \
        -e POSTGRES_PASSWORD_ENCRYPTION=scram-sha-256 \
        -p "$hostport:5432" \
        postgres:16 >/dev/null
}

# start_mysql brings up a mysql:8.0.36 container with a non-root MYSQL_USER on
# MYSQL_DATABASE. host.docker.internal is mapped to the host gateway so the
# container can reach the dbpivot proxy listening on the host (the proxy →
# upstream leg uses 127.0.0.1:$hostport, the container → proxy leg uses
# host.docker.internal:$DBPIVOT_MYSQL_PORT).
start_mysql() {
    local name=$1 hostport=$2
    docker rm -f "$name" >/dev/null 2>&1 || true
    docker run -d --rm --name "$name" \
        --add-host=host.docker.internal:host-gateway \
        -e MYSQL_ROOT_PASSWORD=$MY_ROOT_PASS \
        -e MYSQL_USER=$MY_USER \
        -e MYSQL_PASSWORD=$MY_PASS \
        -e MYSQL_DATABASE=$MY_DB \
        -p "$hostport:3306" \
        mysql:8.0.36 >/dev/null
}

echo "[e2e] starting postgres + mysql containers..."
start_pg    "$PG_LOCAL_NAME"   "$PG_LOCAL_PORT"
start_pg    "$PG_STAGING_NAME" "$PG_STAGING_PORT"
start_mysql "$MY_LOCAL_NAME"   "$MY_LOCAL_PORT"
start_mysql "$MY_STAGING_NAME" "$MY_STAGING_PORT"

echo "[e2e] waiting for postgres readiness..."
wait_pg_ready "$PG_LOCAL_PORT"
wait_pg_ready "$PG_STAGING_PORT"
echo "[e2e] waiting for mysql readiness (slower; can take ~30s each)..."
wait_my_ready "$MY_LOCAL_NAME"
wait_my_ready "$MY_STAGING_NAME"

echo "[e2e] applying schema + seed data..."
PG_SCHEMA="CREATE TABLE items (id int PRIMARY KEY, name text NOT NULL);"
MY_SCHEMA="CREATE TABLE items (id INT PRIMARY KEY, name VARCHAR(64) NOT NULL);"

PGPASSWORD=$PG_PASS psql -h 127.0.0.1 -p "$PG_LOCAL_PORT"   -U $PG_USER -d $PG_DB -v ON_ERROR_STOP=1 -c "$PG_SCHEMA" >/dev/null
PGPASSWORD=$PG_PASS psql -h 127.0.0.1 -p "$PG_STAGING_PORT" -U $PG_USER -d $PG_DB -v ON_ERROR_STOP=1 -c "$PG_SCHEMA" >/dev/null
PGPASSWORD=$PG_PASS psql -h 127.0.0.1 -p "$PG_LOCAL_PORT"   -U $PG_USER -d $PG_DB -v ON_ERROR_STOP=1 \
    -c "INSERT INTO items VALUES (1, 'local-alpha'), (2, 'local-beta');" >/dev/null
PGPASSWORD=$PG_PASS psql -h 127.0.0.1 -p "$PG_STAGING_PORT" -U $PG_USER -d $PG_DB -v ON_ERROR_STOP=1 \
    -c "INSERT INTO items VALUES (10, 'staging-gamma'), (20, 'staging-delta'), (30, 'staging-epsilon');" >/dev/null

# Seed MySQL with a recognisably different "first row" so a misroute would
# show up as a name like local-alpha (PG) leaking into the MySQL channel.
my_exec "$MY_LOCAL_NAME"   "$MY_SCHEMA"
my_exec "$MY_STAGING_NAME" "$MY_SCHEMA"
my_exec "$MY_LOCAL_NAME"   "INSERT INTO items VALUES (1, 'mylocal-alpha'), (2, 'mylocal-beta');"
my_exec "$MY_STAGING_NAME" "INSERT INTO items VALUES (10, 'mystaging-gamma'), (20, 'mystaging-delta'), (30, 'mystaging-epsilon');"

echo "[e2e] building dbpivot + e2e server..."
cd "$ROOT"
go build -o "$WORK/dbpivot"   ./cmd/dbpivot
go build -o "$WORK/e2eserver" ./e2e/server

# One dbpivot serving BOTH adapters from one process — the mixed-adapter
# capability the multi-port refactor enabled. PG clients hit DBPIVOT_PORT,
# MySQL clients hit DBPIVOT_MYSQL_PORT; `use <target>` flips both at once.
cat > "$WORK/dbpivot.yml" <<EOF
# Bind on all interfaces so the sidecar mysql container can reach the proxy
# across both Linux (host.docker.internal == bridge gateway IP) and macOS
# Docker Desktop (host.docker.internal == host loopback). Production should
# keep the default 127.0.0.1.
listen_host: 0.0.0.0
listen_ports:
  postgres: $DBPIVOT_PORT
  mysql: $DBPIVOT_MYSQL_PORT
control_socket: $WORK/dbpivot.sock

databases:
  - adapter: postgres
    virtual_name: appdb
    targets:
      - name: local
        host: 127.0.0.1
        port: $PG_LOCAL_PORT
        user: $PG_USER
        password: $PG_PASS
        database: $PG_DB
      - name: staging
        host: 127.0.0.1
        port: $PG_STAGING_PORT
        user: $PG_USER
        password: $PG_PASS
        database: $PG_DB

  - adapter: mysql
    virtual_name: mydb
    targets:
      - name: local
        host: 127.0.0.1
        port: $MY_LOCAL_PORT
        user: $MY_USER
        password: $MY_PASS
        database: $MY_DB
      - name: staging
        host: 127.0.0.1
        port: $MY_STAGING_PORT
        user: $MY_USER
        password: $MY_PASS
        database: $MY_DB
EOF

echo "[e2e] starting dbpivot (target=local, serving postgres+mysql)..."
"$WORK/dbpivot" serve --config "$WORK/dbpivot.yml" --target local --log-level warn &
PIVOT_PID=$!
wait_tcp 127.0.0.1 "$DBPIVOT_PORT"       "dbpivot (postgres)"
wait_tcp 127.0.0.1 "$DBPIVOT_MYSQL_PORT" "dbpivot (mysql)"

echo "[e2e] starting web server (postgres path)..."
# Run the prebuilt binary directly rather than `go run` so the cleanup trap can
# actually kill it — `go run` spawns the compiled child as a grandchild, which
# survives kill $WEB_PID and keeps the port held / stdout open.
DSN="postgres://anyone:any@127.0.0.1:$DBPIVOT_PORT/appdb?sslmode=disable" \
ADDR="127.0.0.1:$WEB_PORT" \
    "$WORK/e2eserver" &
WEB_PID=$!
wait_tcp 127.0.0.1 "$WEB_PORT" "web server"

assert_first_pg_name() {
    local want=$1 phase=$2
    local resp
    resp=$(curl -fsS "http://127.0.0.1:$WEB_PORT/items")
    echo "[e2e]  pg  $phase: $resp"
    local got
    got=$(echo "$resp" | jq -r '.[0].name')
    if [[ "$got" != "$want" ]]; then
        echo "[e2e] FAIL pg ($phase): first item name = $got, want $want" >&2
        exit 1
    fi
}

# Query mydb via the dbpivot mysql port (using a mysql container as sidecar
# client). Client→proxy is trust auth so user/password are arbitrary; the
# dbname (-D mydb) is what selects the database. --ssl-mode=DISABLED matches
# the proxy's no-CLIENT_SSL greeting.
assert_first_my_name() {
    local want=$1 phase=$2
    local got
    got=$(my_via_proxy 'SELECT name FROM items ORDER BY id LIMIT 1' | tr -d '\r')
    echo "[e2e] mysql $phase: $got"
    if [[ "$got" != "$want" ]]; then
        echo "[e2e] FAIL mysql ($phase): first item name = $got, want $want" >&2
        exit 1
    fi
}

echo "[e2e] --- request #1 (expect local data on BOTH adapters) ---"
assert_first_pg_name "local-alpha"   "before-switch"
assert_first_my_name "mylocal-alpha" "before-switch"

echo "[e2e] --- dbpivot use staging (flips PG + MySQL in one shot) ---"
"$WORK/dbpivot" use staging --config "$WORK/dbpivot.yml"

echo "[e2e] --- request #2 (expect staging data on BOTH adapters) ---"
assert_first_pg_name "staging-gamma"    "after-switch"
assert_first_my_name "mystaging-gamma"  "after-switch"

echo "[e2e] --- dbpivot use local (switch back) ---"
"$WORK/dbpivot" use local --config "$WORK/dbpivot.yml"

echo "[e2e] --- request #3 (expect local data again on BOTH adapters) ---"
assert_first_pg_name "local-alpha"   "after-switch-back"
assert_first_my_name "mylocal-alpha" "after-switch-back"

echo "[e2e] OK"
