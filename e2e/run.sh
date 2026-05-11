#!/usr/bin/env bash
# End-to-end smoke test for dbpivot.
#
#   1. docker で postgres を 2 つ起動 (local / staging を模擬)
#   2. 同じスキーマを流し、別データを入れる
#   3. dbpivot を build → serve --target local で起動
#   4. tiny Go web サーバを起動 (proxy 経由で SELECT)
#   5. curl /items → local データが返ることを確認
#   6. dbpivot use staging で切替
#   7. curl /items → staging データに変わることを確認
#
# Cleanup (containers / processes) は trap で全部行う。

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

DBPIVOT_PORT=${DBPIVOT_PORT:-6432}
WEB_PORT=${WEB_PORT:-18080}
PG_LOCAL_PORT=${PG_LOCAL_PORT:-55432}
PG_STAGING_PORT=${PG_STAGING_PORT:-55433}

PG_LOCAL_NAME=dbpivot-e2e-local
PG_STAGING_NAME=dbpivot-e2e-staging
PG_USER=appuser
PG_PASS=apppass
PG_DB=appdb

WORK="$(mktemp -d -t dbpivot-e2e.XXXXXX)"
echo "[e2e] workdir: $WORK"

cleanup() {
    set +e
    echo "[e2e] cleanup..."
    [[ -n "${WEB_PID:-}" ]]   && kill "$WEB_PID"   2>/dev/null && wait "$WEB_PID"   2>/dev/null
    [[ -n "${PIVOT_PID:-}" ]] && kill "$PIVOT_PID" 2>/dev/null && wait "$PIVOT_PID" 2>/dev/null
    docker rm -f "$PG_LOCAL_NAME" "$PG_STAGING_NAME" >/dev/null 2>&1
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

echo "[e2e] starting postgres containers..."
start_pg "$PG_LOCAL_NAME"   "$PG_LOCAL_PORT"
start_pg "$PG_STAGING_NAME" "$PG_STAGING_PORT"
wait_pg_ready "$PG_LOCAL_PORT"
wait_pg_ready "$PG_STAGING_PORT"

echo "[e2e] applying schema + seed data..."
SCHEMA="CREATE TABLE items (id int PRIMARY KEY, name text NOT NULL);"
PGPASSWORD=$PG_PASS psql -h 127.0.0.1 -p "$PG_LOCAL_PORT"   -U $PG_USER -d $PG_DB -v ON_ERROR_STOP=1 -c "$SCHEMA" >/dev/null
PGPASSWORD=$PG_PASS psql -h 127.0.0.1 -p "$PG_STAGING_PORT" -U $PG_USER -d $PG_DB -v ON_ERROR_STOP=1 -c "$SCHEMA" >/dev/null
PGPASSWORD=$PG_PASS psql -h 127.0.0.1 -p "$PG_LOCAL_PORT"   -U $PG_USER -d $PG_DB -v ON_ERROR_STOP=1 \
    -c "INSERT INTO items VALUES (1, 'local-alpha'), (2, 'local-beta');" >/dev/null
PGPASSWORD=$PG_PASS psql -h 127.0.0.1 -p "$PG_STAGING_PORT" -U $PG_USER -d $PG_DB -v ON_ERROR_STOP=1 \
    -c "INSERT INTO items VALUES (10, 'staging-gamma'), (20, 'staging-delta'), (30, 'staging-epsilon');" >/dev/null

echo "[e2e] building dbpivot..."
cd "$ROOT"
go build -o "$WORK/dbpivot" ./cmd/dbpivot

cat > "$WORK/dbpivot.yml" <<EOF
port: $DBPIVOT_PORT
control_socket: $WORK/dbpivot.sock

databases:
  - virtual_name: appdb
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
EOF

echo "[e2e] starting dbpivot (target=local)..."
"$WORK/dbpivot" serve --config "$WORK/dbpivot.yml" --target local --log-level warn &
PIVOT_PID=$!
wait_tcp 127.0.0.1 "$DBPIVOT_PORT" "dbpivot"

echo "[e2e] starting web server..."
DSN="postgres://anyone:any@127.0.0.1:$DBPIVOT_PORT/appdb?sslmode=disable" \
ADDR="127.0.0.1:$WEB_PORT" \
    go run ./e2e/server &
WEB_PID=$!
wait_tcp 127.0.0.1 "$WEB_PORT" "web server"

assert_first_name() {
    local want=$1 phase=$2
    local resp
    resp=$(curl -fsS "http://127.0.0.1:$WEB_PORT/items")
    echo "[e2e] $phase: $resp"
    local got
    got=$(echo "$resp" | jq -r '.[0].name')
    if [[ "$got" != "$want" ]]; then
        echo "[e2e] FAIL ($phase): first item name = $got, want $want" >&2
        exit 1
    fi
}

echo "[e2e] --- request #1 (expect local data) ---"
assert_first_name "local-alpha" "before-switch"

echo "[e2e] --- dbpivot use staging ---"
"$WORK/dbpivot" use staging --config "$WORK/dbpivot.yml"

echo "[e2e] --- request #2 (expect staging data) ---"
assert_first_name "staging-gamma" "after-switch"

echo "[e2e] --- dbpivot use local (switch back) ---"
"$WORK/dbpivot" use local --config "$WORK/dbpivot.yml"

echo "[e2e] --- request #3 (expect local data again) ---"
assert_first_name "local-alpha" "after-switch-back"

echo "[e2e] OK"
