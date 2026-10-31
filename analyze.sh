#!/bin/bash

set -eu

for i in $(seq 1 3); do
  host="isu${i}"
  echo $host
  ssh ${host} "cat /var/log/nginx/access.log | alp ltsv -m '/api/images/[^/]+','/api/users/[^/]+','/api/posts/[^/]+','/api/follows/[^/]+','/api/friend-requests/[^/]+','/api/comments/[^/]+','/api/memories/[^/]+','/api/admin/users/[^/]+/certification','/api/realmojis/[^/]+','/api/blocks/[^/]+'"
done

# ベンチ中に全ログを読まない。末尾だけ見る。SLOW_BYTES / SLOW_TOP で変えられる。
echo database
slow_bytes="${SLOW_BYTES:-3000000}"
slow_top="${SLOW_TOP:-20}"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
ssh database "sudo tail -c ${slow_bytes} /var/log/postgresql/postgresql-16-main.log" > "$tmp"
awk '
/^[0-9]{4}-[0-9]{2}-[0-9]{2} / {
  if (dur != "" && q !~ /^plan:/) emit()
  dur = ""
  q = ""
  if (match($0, /duration: [0-9.]+ ms/)) {
    dur = substr($0, RSTART + 10, RLENGTH - 13) + 0
    line = $0
    sub(/^.*duration: [0-9.]+ ms[[:space:]]+/, "", line)
    sub(/^(bind|execute|parse|statement)[^:]*:[[:space:]]*/, "", line)
    q = line
  }
  next
}
dur != "" { q = q " " $0 }
function emit() {
  gsub(/[[:space:]]+/, " ", q)
  sub(/^ /, "", q)
  sub(/ $/, "", q)
  gsub(/\$[0-9]+/, "$?", q)
  key[q] += dur
  cnt[q]++
  if (dur > mx[q]) mx[q] = dur
}
END {
  if (dur != "" && q !~ /^plan:/) emit()
  for (k in key)
    printf "%8.0f ms  n=%-5d max=%7.0f  %s\n", key[k], cnt[k], mx[k], substr(k, 1, 160)
}
' "$tmp" | sort -nr | head -n "$slow_top"
