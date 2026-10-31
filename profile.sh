#!/bin/bash
# ベンチ中の負荷と待ちを、アプリと DB の両方で記録する。ベンチのボタンを押す前に起動しておく。
# /api/initialize が nginx のログに出たら、PROFILE_SECONDS 秒（既定 120）計測して profiles/ に保存する。
#
# アプリ（host）:
#   - CPU プロファイル（pprof）
#   - CPU 使用率（mpstat、コアごと）
#   - ゴルーチンがどこで止まっているか（goroutine?debug=2 を GOROUTINE_INTERVAL 秒ごと、既定 5）
#   - DB の接続プールの使用数と空き待ち（/debug/vars の db_pools）
# DB（database）:
#   - CPU 使用率（mpstat）とディスク（iostat、DB_DISK の既定は nvme0n1）
#   - 待ちイベント（pg_stat_activity を SAMPLE_INTERVAL 秒ごと、既定 2。idle 以外の接続）
# 使い方: ./profile.sh [host]   （host の既定は isu1）

set -eu

host="${1:-isu1}"
db="${DB_HOST:-database}"
disk="${DB_DISK:-nvme0n1}"
seconds="${PROFILE_SECONDS:-120}"
interval="${SAMPLE_INTERVAL:-2}"
ginterval="${GOROUTINE_INTERVAL:-5}"
count=$((seconds / interval))
gcount=$((seconds / ginterval))
base="profiles/$(date +%Y%m%d-%H%M%S)"
out="${base}-${host}.pprof"
app_cpu="${base}-${host}-mpstat.txt"
app_samples="${base}-${host}-samples.txt"
waits="${base}-db-waits.txt"
cpu="${base}-db-mpstat.txt"
io="${base}-db-iostat.txt"
mkdir -p profiles

echo "${host}: ベンチの開始（/api/initialize）を待っています。ボタンを押してください"
ssh "${host}" "timeout 600 tail -n0 -F /var/log/nginx/access.log 2>/dev/null | grep -m1 -q /api/initialize"
echo "${host}: 開始を検知。${seconds} 秒計測します"

# --- DB ---
# 待ちイベントが無い active な接続は CPU で動いているとみなす。DB 関数は関数名でまとめる。
ssh "${db}" "sudo -u postgres psql -d isureal -X -At -F '|'" > "${waits}" <<SQL &
SELECT to_char(clock_timestamp(), 'HH24:MI:SS'),
       state,
       coalesce(wait_event_type, 'CPU'),
       coalesce(wait_event, '-'),
       coalesce((regexp_match(query, 'isureal_[a-z_]+'))[1], left(regexp_replace(query, '[[:space:]]+', ' ', 'g'), 80)),
       count(*)
FROM pg_stat_activity
WHERE datname = 'isureal' AND state <> 'idle' AND pid <> pg_backend_pid()
GROUP BY 1, 2, 3, 4, 5
\watch i=${interval} c=${count}
SQL
ssh "${db}" "mpstat ${interval} ${count}" > "${cpu}" &
ssh "${db}" "iostat -dxy ${interval} ${count}" > "${io}" &

# --- アプリ ---
ssh "${host}" "mpstat -P ALL ${interval} ${count}" > "${app_cpu}" &
# 1行ずつ「pool|時刻|プール名|使用中|アイドル|空き待ちの累計回数|空き待ちの累計 ns」、
# 「sign|時刻|署名付き URL を使い回せた回数|リクエスト中に計算した回数|初期化後に先に計算した件数」（初期化からの累計）と
# 「gor|時刻|状態|runtime 以外で最初の関数|最初の isureal の関数|本数」を出す。
ssh "${host}" bash -s -- "${ginterval}" "${gcount}" > "${app_samples}" <<'REMOTE' &
for _ in $(seq 1 "$2"); do
  ts=$(date +%H:%M:%S)
  curl -s 127.0.0.1:6060/debug/vars |
    jq -r --arg ts "$ts" '((.db_pools // {}) | to_entries[] | ["pool", $ts, .key, .value.InUse, .value.Idle, .value.WaitCount, .value.WaitDuration]),
      (select(.image_sign_hits != null) | ["sign", $ts, .image_sign_hits, .image_sign_misses, .image_presigned])
      | map(tostring) | join("|")' 2>/dev/null
  curl -s '127.0.0.1:6060/debug/pprof/goroutine?debug=2' | awk -v ts="$ts" '
    function flush(  k) {
      if (state != "") { k = state "|" (frame == "" ? "-" : frame) "|" (app == "" ? "-" : app); n[k]++ }
      state = ""; frame = ""; app = ""
    }
    /^goroutine / { flush(); state = $3; sub(/^\[/, "", state); sub(/[],].*$/, "", state); next }
    state != "" && /^[^\t]/ && !/^created by/ {
      f = $0; sub(/\([^()]*\)$/, "", f); sub(/^example\.com\/isureal-company\//, "", f)
      if (frame == "" && f !~ /^(runtime\.|internal\/|sync\.runtime_|syscall\.)/) frame = f
      if (app == "" && f ~ /isureal\./) { app = f; sub(/^.*isureal\./, "", app) }
    }
    END { flush(); for (k in n) print "gor|" ts "|" k "|" n[k] }'
  sleep "$1"
done
REMOTE

ssh "${host}" "curl -sf 'http://127.0.0.1:6060/debug/pprof/profile?seconds=${seconds}'" > "${out}"
wait
ssh "${host}" "curl -s 127.0.0.1:6060/debug/image-sign-misses" > "${base}-${host}-sign-misses.tsv" || true
echo "保存: ${base}-*"

echo
echo "===== アプリ（${host}） ====="
go tool pprof -top -nodecount=30 "${out}" | head -n 38

echo
echo "--- CPU（mpstat の平均。all と各コア）"
grep -E '^Average' "${app_cpu}" | head -n 4

echo
echo "--- DB の接続プール（ベンチ中の空き待ちの増分と、使用中の本数）"
awk -F'|' '$1 == "pool" {
  p = $3
  if (!(p in first)) { fc[p] = $6; fd[p] = $7; first[p] = 1 }
  lc[p] = $6; ld[p] = $7; s[p] += $4; m[p] = ($4 > m[p] ? $4 : m[p]); c[p]++
}
END {
  if (length(c) == 0) print "（/debug/vars に db_pools がない。アプリが古い）"
  for (p in c) printf "%-6s 空き待ち %d 回 計 %.0f ms、使用中 平均 %.1f 最大 %d\n", p, lc[p] - fc[p], (ld[p] - fd[p]) / 1e6, s[p] / c[p], m[p]
}' "${app_samples}"

echo
echo "--- 画像の署名付き URL（初期化からの累計。最後のサンプル時点）"
awk -F'|' '$1 == "sign" { last = $0; h = $3; m = $4; p = $5 }
END {
  if (last == "") { print "（/debug/vars に image_sign_hits がない。アプリが古い）"; exit }
  t = h + m
  printf "使い回し %d 回、リクエスト中に計算 %d 回（%.1f%%）、先に計算 %d 件\n", h, m, (t ? 100 * m / t : 0), p
}' "${app_samples}"
awk -F'|' '$1 == "sign" { printf "  %s 使い回し %d 計算 %d 先に %d\n", $2, $3, $4, $5 }' "${app_samples}" | awk 'NR % 3 == 1'

echo
echo "--- 使い回せなかった画像の分類（sign_misses.sh）"
DB_HOST="${db}" ./sign_misses.sh "${base}-${host}-sign-misses.tsv" || true

echo
echo "--- ゴルーチン（1回あたりの平均本数。状態 | runtime 以外で最初の関数 | 最初の isureal の関数）"
awk -F'|' '$1 == "gor" { seen[$2] = 1; n[$3 "|" $4 "|" $5] += $6 }
END { s = length(seen); for (k in n) printf "%7.1f  %s\n", n[k] / s, k }' "${app_samples}" | sort -k1,1nr | head -n 20

echo
echo "===== DB（${db}） ====="
echo "--- CPU（mpstat の平均、2コア）"
grep -E '%usr|^Average' "${cpu}" | head -n 2
awk '$NF + 0 < 5 && $1 != "Average:" && $NF ~ /^[0-9.]+$/ { lo++ } $NF ~ /^[0-9.]+$/ && $1 != "Average:" { n++ } END { printf "空きが 5%% 未満: %d / %d 回\n", lo, n }' "${cpu}"

echo
echo "--- ディスク（${disk}、iostat の平均）"
awk -v disk="${disk}" '
/^Device/ { for (i = 1; i <= NF; i++) col[$i] = i; next }
$1 == disk { n++; for (k in col) sum[k] += $(col[k]) }
END {
  if (!n) { print "（データなし）"; exit }
  split("r/s rkB/s r_await w/s wkB/s w_await aqu-sz %util", keys, " ")
  for (i = 1; i in keys; i++) if (keys[i] in sum) printf "%s=%.1f  ", keys[i], sum[keys[i]] / n
  print ""
}' "${io}"

echo
echo "--- 待ちイベント（idle 以外の接続のサンプル数の合計。CPU は待ちなしで実行中）"
awk -F'|' 'NF >= 6 && $2 == "active" { seen[$1] = 1; total += $6 } END { s = length(seen); printf "サンプル %d 回、active な接続は平均 %.1f\n", s, (s ? total / s : 0) }' "${waits}"
awk -F'|' '
NF >= 6 { k = sprintf("%-20s %-8s %-20s %s", $2, $3, $4, $5); n[k] += $6; total += $6 }
END { for (k in n) printf "%7d %5.1f%%  %s\n", n[k], 100 * n[k] / total, k }
' "${waits}" | sort -k1,1nr | head -n 30

echo
echo "ブラウザで見る: go tool pprof -http=:10060 ${out}"
