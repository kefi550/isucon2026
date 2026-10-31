#!/bin/bash
# ベンチ中の CPU プロファイルを取る。ベンチのボタンを押す前に起動しておく。
# /api/initialize が nginx のログに出たら、PROFILE_SECONDS 秒（既定 120）計測して手元に保存する。
# 同じあいだ、DB の待ちイベント（pg_stat_activity を SAMPLE_INTERVAL 秒ごと、既定 2）と mpstat も記録する。
# 使い方: ./profile.sh [host]   （host の既定は isu1）

set -eu

host="${1:-isu1}"
seconds="${PROFILE_SECONDS:-120}"
interval="${SAMPLE_INTERVAL:-2}"
count=$((seconds / interval))
base="profiles/$(date +%Y%m%d-%H%M%S)"
out="${base}-${host}.pprof"
waits="${base}-db-waits.txt"
cpu="${base}-db-mpstat.txt"
mkdir -p profiles

echo "${host}: ベンチの開始（/api/initialize）を待っています。ボタンを押してください"
ssh "${host}" "timeout 600 tail -n0 -F /var/log/nginx/access.log 2>/dev/null | grep -m1 -q /api/initialize"
echo "${host}: 開始を検知。${seconds} 秒計測します"

# 待ちイベントが無い active な接続は CPU で動いているとみなす。DB 関数は関数名でまとめる。
ssh database "sudo -u postgres psql -d isureal -X -At -F '|'" > "${waits}" <<SQL &
SELECT to_char(clock_timestamp(), 'HH24:MI:SS'),
       coalesce(wait_event_type, 'CPU'),
       coalesce(wait_event, '-'),
       coalesce((regexp_match(query, 'isureal_[a-z_]+'))[1], left(regexp_replace(query, '[[:space:]]+', ' ', 'g'), 80)),
       count(*)
FROM pg_stat_activity
WHERE datname = 'isureal' AND state = 'active' AND pid <> pg_backend_pid()
GROUP BY 1, 2, 3, 4
\watch i=${interval} c=${count}
SQL
ssh database "mpstat ${interval} ${count}" > "${cpu}" &

ssh "${host}" "curl -sf 'http://127.0.0.1:6060/debug/pprof/profile?seconds=${seconds}'" > "${out}"
wait
echo "保存: ${out} ${waits} ${cpu}"

go tool pprof -top -nodecount=40 "${out}" | head -n 50

echo
echo "--- DB の CPU（mpstat の平均、2コア）"
grep -E '%usr|^Average' "${cpu}" | head -n 2

echo
echo "--- DB の待ちイベント（active な接続のサンプル数の合計。CPU は待ちなしで実行中）"
awk -F'|' 'NF >= 5 { seen[$1] = 1; total += $5 } END { s = length(seen); printf "サンプル %d 回、active な接続は平均 %.1f\n", s, (s ? total / s : 0) }' "${waits}"
awk -F'|' '
NF >= 5 { k = sprintf("%-10s %-22s %s", $2, $3, $4); n[k] += $5; total += $5 }
END { for (k in n) printf "%7d %5.1f%%  %s\n", n[k], 100 * n[k] / total, k }
' "${waits}" | sort -k1,1nr | head -n 30

echo
echo "ブラウザで見る: go tool pprof -http=:10060 ${out}"
