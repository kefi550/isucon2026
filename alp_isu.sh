#!/bin/bash

set -eu

for i in $(seq 1 3); do
  host="isu${i}"
  echo $host
  ssh ${host} "cat /var/log/nginx/access.log | alp ltsv -m '/api/images/[^/]+','/api/users/[^/]+','/api/posts/[^/]+','/api/follows/[^/]+','/api/friend-requests/[^/]+','/api/comments/[^/]+','/api/memories/[^/]+','/api/admin/users/[^/]+/certification','/api/realmojis/[^/]+','/api/blocks/[^/]+'"
done
