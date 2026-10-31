#!/bin/bash
# リクエストの中で署名を計算した（使い回せなかった）画像を、DB と突き合わせて分類する。
# 入力はアプリの 127.0.0.1:6060/debug/image-sign-misses（profile.sh が ${base}-${host}-sign-misses.tsv に保存する）。
# 初期化のたびに記録は消えるので、次のベンチの前に取ること。
# 使い方: ./sign_misses.sh <tsv>    （省略すると isu1 から今の記録を取る）

set -eu

db="${DB_HOST:-database}"
if [ $# -ge 1 ]; then
  tsv="$1"
else
  tsv="$(mktemp)"
  ssh "${HOST:-isu1}" "curl -s 127.0.0.1:6060/debug/image-sign-misses" > "${tsv}"
fi

echo "外れ $(wc -l < "${tsv}") 件（種類別）"
cut -f1 "${tsv}" | sort | uniq -c | sort -nr
[ -s "${tsv}" ] || exit 0

# ベンチ中にできたかどうかは、最初の外れの30秒前（≒初期化）より後に作られたかで決める。
# 初期データの古さは、初期データで最も新しい投稿からの差で分ける。
{
  cat <<'SQL'
CREATE TEMP TABLE m (kind text, image_id text, at_ms bigint);
\copy m FROM STDIN
SQL
  cat "${tsv}"
  printf '\\.\n'
  cat <<'SQL'
\pset footer off
WITH t0 AS (SELECT to_timestamp(min(at_ms) / 1000.0) - interval '30 seconds' AS t FROM m),
newest AS (SELECT max(posted_at) AS p FROM posts WHERE created_at < (SELECT t FROM t0))
SELECT m.kind AS 種類, coalesce(x.cls, 'DB に見つからない') AS 分類,
       count(*) AS 外れ, count(DISTINCT m.image_id) AS 画像, count(DISTINCT x.owner) AS 持ち主
FROM m
CROSS JOIN t0
CROSS JOIN newest
LEFT JOIN LATERAL (
  SELECT CASE WHEN p.created_at >= t0.t THEN 'ベンチ中の投稿'
              ELSE '初期データ ' || CASE WHEN newest.p - p.posted_at < interval '1 day' THEN '1日以内'
                                         WHEN newest.p - p.posted_at < interval '7 days' THEN '1〜7日前'
                                         WHEN newest.p - p.posted_at < interval '30 days' THEN '7〜30日前'
                                         WHEN newest.p - p.posted_at < interval '90 days' THEN '30〜90日前'
                                         ELSE '90日より前' END END AS cls,
         p.author_id AS owner
  FROM posts p WHERE m.kind = 'post' AND p.image_id = m.image_id
  UNION ALL
  SELECT CASE WHEN r.created_at >= t0.t THEN 'ベンチ中のリアクション'
              ELSE '初期データ ' || CASE WHEN newest.p - r.created_at < interval '1 day' THEN '1日以内'
                                         WHEN newest.p - r.created_at < interval '7 days' THEN '1〜7日前'
                                         WHEN newest.p - r.created_at < interval '30 days' THEN '7〜30日前'
                                         WHEN newest.p - r.created_at < interval '90 days' THEN '30〜90日前'
                                         ELSE '90日より前' END END,
         r.reactor_id
  FROM realmojis r WHERE m.kind = 'realmoji' AND r.image_id = m.image_id
  UNION ALL
  SELECT CASE WHEN u.created_at >= t0.t THEN 'ベンチ中に登録したユーザー' ELSE '初期データのユーザー' END, u.id
  FROM users u WHERE m.kind = 'profile' AND u.profile_image_id = m.image_id
  LIMIT 1
) x ON true
GROUP BY 1, 2
ORDER BY 3 DESC;

-- 外れた時刻（初期化からの秒数）ごとの件数。負荷の山のどこで外れているか。
SELECT ((at_ms - (SELECT min(at_ms) FROM m)) / 10000) * 10 AS 秒,
       count(*) FILTER (WHERE kind = 'post') AS post,
       count(*) FILTER (WHERE kind = 'realmoji') AS realmoji,
       count(*) FILTER (WHERE kind = 'profile') AS profile
FROM m GROUP BY 1 ORDER BY 1;
SQL
} | ssh "${db}" "sudo -u postgres psql -d isureal -X -q"
