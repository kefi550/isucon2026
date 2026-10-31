#!/bin/bash
# 初期データの画像（約183万枚・13.7GB）を、isu1 のローカルディスク（/var/lib/isureal-images）に置く。
# isu1 の nginx が、アプリの検証を通ったあとに、ここのファイルをそのまま返す（nginx/isureal.conf）。
# push.sh / push_nginx.sh とは独立している。
#
#   ./push_imagestore.sh setup [1 2 3|all]   照合ツールを置き、置き場のディレクトリを作る（省略時は isu1 だけ。isu2 / isu3 は画像を DB から返すので置かない）
#   ./push_imagestore.sh copy  [1 2 3|all]   asia2（database）に書き出し済みの画像を、database から各サーバーへ直接（tar パイプ）コピーする
#   ./push_imagestore.sh status [1 2 3|all]  ファイル数・サイズ・ディスクの空き
#   ./push_imagestore.sh verify [1 2 3|all]  ランダムな 2000 枚を DB と sha256 で照合する（コピーの後に）
#
# コピーは対象サーバーの CPU・ディスクを使う。ベンチが走っていないときに実行する。
set -eu
cd -- "$(dirname -- "$0")"
SRC=database
OUT=/var/lib/isureal-images

cmd="${1:-}"; shift || true
case "${1:-1}" in all) targets="1 2 3" ;; *) targets="$*" ;; esac
ip_of() { echo "192.168.$1.10"; }

case "$cmd" in
  setup)
    (cd webapp/go && go build -o ../../.imgexport.bin ./cmd/imgexport)
    for i in $targets; do
      echo "== isu$i"
      scp -q .imgexport.bin "isu$i:/tmp/imgexport"
      ssh "isu$i" "sudo install -m 755 /tmp/imgexport /usr/local/bin/imgexport && sudo install -d -o root -g root -m 755 $OUT && echo ok"
    done
    ;;
  copy)
    # 手元は経由せず、database から各サーバーへ直接流す。ssh のエージェントを転送して使う（鍵は database に置かない）
    for i in $targets; do
      echo "== isu$i（空き: $(ssh "isu$i" "df -h / | awk 'NR==2{print \$4}'")）"
      ssh -A "$SRC" "sudo tar -C $OUT -cf - . | ssh -o StrictHostKeyChecking=accept-new isucon@$(ip_of "$i") 'sudo tar -C $OUT -xf - --no-same-owner'"
      echo "isu$i: コピー完了"
    done
    ;;
  status)
    for i in $targets; do
      echo "== isu$i"
      ssh "isu$i" "echo \"ファイル数: \$(sudo find $OUT -type f | wc -l)  サイズ: \$(sudo du -sh $OUT | cut -f1)  ディスクの空き: \$(df -h / | awk 'NR==2{print \$4}')\""
    done
    ;;
  verify)
    for i in $targets; do
      echo "== isu$i"
      ssh "isu$i" "OUT=$OUT bash -s" <<'REMOTE'
set -eu
set -a; . /home/isucon/env.sh; set +a
DSN="host=$ISUREAL_DB_HOST port=$ISUREAL_DB_PORT user=$ISUREAL_DB_USER password=$ISUREAL_DB_PASSWORD dbname=$ISUREAL_DB_NAME"
/usr/local/bin/imgexport -dsn "$DSN" -out "$OUT" -verify 2000
REMOTE
    done
    ;;
  *)
    sed -n '2,13p' "$0"
    exit 1
    ;;
esac
