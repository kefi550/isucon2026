#!/bin/bash

set -eu

for i in $(seq 1 3); do
  host="isu${i}"
  echo $host
  ssh ${host} "cat /var/log/nginx/access.log | alp ltsv -m '/api/images/[^/]+','/api/users/[^/]+','/api/posts/[^/]+','/api/follows/[^/]+','/api/friend-requests/[^/]+','/api/comments/[^/]+','/api/memories/[^/]+','/api/admin/users/[^/]+/certification','/api/realmojis/[^/]+','/api/blocks/[^/]+'"
done

# DB の集計は、計測を止めている間（pg_stat_statements と log_lock_waits を無効にしている）は使えない。
# 計測を戻したときだけ ANALYZE_DB=1 で実行する。
if [ "${ANALYZE_DB:-0}" = 1 ]; then

# pg_stat_statements の上位。件数は PGSS_TOP で変えられる。
# hit_pct は共有バッファに載っていた割合、read_blks と read_ms はディスクから読んだブロック数と時間。
echo database
pgss_top="${PGSS_TOP:-25}"
ssh database "sudo -u postgres psql -d isureal -X -P pager=off" <<SQL
SELECT stats_reset FROM pg_stat_statements_info;

\echo '--- total_exec_time 順'
SELECT round(total_exec_time) AS total_ms,
       calls,
       round(mean_exec_time::numeric, 1) AS mean_ms,
       round(max_exec_time) AS max_ms,
       round(rows::numeric / nullif(calls, 0), 1) AS rows_per_call,
       round(100.0 * shared_blks_hit / nullif(shared_blks_hit + shared_blks_read, 0), 1) AS hit_pct,
       shared_blks_read AS read_blks,
       round(blk_read_time) AS read_ms,
       left(regexp_replace(query, '[[:space:]]+', ' ', 'g'), 150) AS query
FROM pg_stat_statements
ORDER BY total_exec_time DESC
LIMIT ${pgss_top};

\echo '--- mean_exec_time 順（10回以上呼ばれたもの）'
SELECT round(mean_exec_time::numeric, 1) AS mean_ms,
       calls,
       round(total_exec_time) AS total_ms,
       round(max_exec_time) AS max_ms,
       round(rows::numeric / nullif(calls, 0), 1) AS rows_per_call,
       round(100.0 * shared_blks_hit / nullif(shared_blks_hit + shared_blks_read, 0), 1) AS hit_pct,
       left(regexp_replace(query, '[[:space:]]+', ' ', 'g'), 150) AS query
FROM pg_stat_statements
WHERE calls >= 10
ORDER BY mean_exec_time DESC
LIMIT 10;
SQL

# ロック待ち（log_lock_waits、deadlock_timeout 以上待ったもの）を、待ったロックと文ごとに集計する。
# pg_stat_statements をリセットした時刻より後のログだけを見る（LOCK_SINCE に UTC の 'YYYY-MM-DD HH:MM:SS' を渡すと変えられる）。
# LOCK_BYTES / LOCK_TOP でも変えられる。
echo "--- ロック待ち（合計待ち時間順）"
lock_bytes="${LOCK_BYTES:-50000000}"
lock_top="${LOCK_TOP:-20}"
since="${LOCK_SINCE:-$(ssh database "sudo -u postgres psql -d isureal -X -At -c \"SELECT to_char(stats_reset AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') FROM pg_stat_statements_info;\"")}"
relmap=$(mktemp)
trap 'rm -f "$relmap"' EXIT
ssh database "sudo -u postgres psql -d isureal -X -At -F ' ' -c \"SELECT oid, relname FROM pg_class WHERE relkind IN ('r', 'p');\"" > "$relmap"
ssh database "sudo tail -c ${lock_bytes} /var/log/postgresql/postgresql-16-main.log | grep -v '^[[:space:]]' | grep -E 'acquired .* after [0-9.]+ ms| CONTEXT:  | STATEMENT:  '" |
awk -v since="$since" '
FNR == NR { rel[$1] = $2; next }
$1 " " substr($2, 1, 8) < since { next }
{
  pid = $4
  gsub(/[\[\]]/, "", pid)
}
match($0, /acquired .* after [0-9.]+ ms/) {
  if (pid in lock) emit(pid)
  s = substr($0, RSTART + 9, RLENGTH - 9)
  ms = s
  sub(/^.* after /, "", ms)
  sub(/ ms$/, "", ms)
  sub(/ after [0-9.]+ ms$/, "", s)
  sub(/ on transaction [0-9]+/, " on transaction", s)
  sub(/ on advisory lock \[[0-9,]+\]/, " on advisory lock", s)
  gsub(/ of database [0-9]+/, "", s)
  gsub(/ \([0-9]+,[0-9]+\)/, "", s)
  if (match(s, /relation [0-9]+/)) {
    oid = substr(s, RSTART + 9, RLENGTH - 9)
    s = substr(s, 1, RSTART - 1) "\"" (oid in rel ? rel[oid] : oid) "\"" substr(s, RSTART + RLENGTH)
  }
  lock[pid] = s
  wait[pid] = ms + 0
  next
}
!(pid in lock) { next }
/ CONTEXT:  / {
  c = $0
  sub(/^.* CONTEXT:  /, "", c)
  gsub(/ \([0-9]+,[0-9]+\)/, "", c)
  gsub(/\([^)]*\) line [0-9]+/, "", c)
  ctx[pid] = c
  next
}
/ STATEMENT:  / {
  q = $0
  sub(/^.* STATEMENT:  /, "", q)
  stmt[pid] = q
  emit(pid)
}
function emit(p,   k) {
  q = stmt[p]
  gsub(/[[:space:]]+/, " ", q)
  gsub(/\$[0-9]+/, "$?", q)
  k = lock[p] " | " ctx[p] " | " substr(q, 1, 120)
  total[k] += wait[p]
  cnt[k]++
  if (wait[p] > mx[k]) mx[k] = wait[p]
  delete lock[p]; delete wait[p]; delete ctx[p]; delete stmt[p]
}
END {
  for (p in lock) emit(p)
  for (k in total)
    printf "%8.0f ms  n=%-5d max=%6.0f  %s\n", total[k], cnt[k], mx[k], k
}
' "$relmap" - | sort -nr | head -n "$lock_top"

fi
