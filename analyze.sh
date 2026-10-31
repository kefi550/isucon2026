#!/bin/bash

set -eu

for i in $(seq 1 3); do
  host="isu${i}"
  echo $host
  ssh ${host} "cat /var/log/nginx/access.log | alp ltsv -m '/api/images/[^/]+','/api/users/[^/]+','/api/posts/[^/]+','/api/follows/[^/]+','/api/friend-requests/[^/]+','/api/comments/[^/]+','/api/memories/[^/]+','/api/admin/users/[^/]+/certification','/api/realmojis/[^/]+','/api/blocks/[^/]+'"
done

# pg_stat_statements の上位を total_exec_time 順に出す。件数は PGSS_TOP で変えられる。
echo database
pgss_top="${PGSS_TOP:-25}"
ssh database "sudo -u postgres psql -d isureal -X -P pager=off" <<SQL
SELECT stats_reset FROM pg_stat_statements_info;
SELECT round(total_exec_time) AS total_ms,
       calls,
       round(mean_exec_time::numeric, 1) AS mean_ms,
       round(max_exec_time) AS max_ms,
       rows,
       left(regexp_replace(query, '[[:space:]]+', ' ', 'g'), 150) AS query
FROM pg_stat_statements
ORDER BY total_exec_time DESC
LIMIT ${pgss_top};
SQL
