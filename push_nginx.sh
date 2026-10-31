#!/bin/bash
# nginx の設定（nginx/ 配下）を isu サーバーに配布する。push.sh（アプリの配布）とは独立している。
#
#   ./push_nginx.sh              isu1 だけに配布する（最初はここで試す）
#   ./push_nginx.sh 1 3          指定したサーバーだけに配布する
#   ./push_nginx.sh all          isu1〜isu3 に配布する
#   ./push_nginx.sh --rollback [1 2 3|all]   画像置き場を使う前の設定（nginx/original/）に戻す
#
# nginx -t に失敗したら、元の設定に戻して reload せずに止まる。
set -eu
cd -- "$(dirname -- "$0")"

mode=deploy
if [ "${1:-}" = "--rollback" ]; then
  mode=rollback
  shift
fi

case "${1:-}" in
  "") targets="1" ;;
  all) targets="1 2 3" ;;
  *) targets="$*" ;;
esac

for i in $targets; do
  host="isu${i}"
  echo "== ${host} (${mode})"
  if [ "$mode" = deploy ]; then
    rsync -a nginx/isureal.conf "${host}:/tmp/isureal.conf.new"
  else
    rsync -a nginx/original/isureal.conf "${host}:/tmp/isureal.conf.new"
  fi
  ssh "${host}" "MODE=${mode} bash -s" <<'REMOTE'
set -eu
site=/etc/nginx/sites-available/isureal.conf
store=/etc/nginx/conf.d/isureal-image-store.conf

# 失敗したときに戻せるよう、いまの設定を退避する
sudo cp -p "$site" /tmp/isureal.conf.bak
if [ -f "$store" ]; then sudo cp -p "$store" /tmp/isureal-image-store.conf.bak; else sudo rm -f /tmp/isureal-image-store.conf.bak; fi

restore() {
  sudo cp -p /tmp/isureal.conf.bak "$site"
  if [ -f /tmp/isureal-image-store.conf.bak ]; then sudo cp -p /tmp/isureal-image-store.conf.bak "$store"; else sudo rm -f "$store"; fi
}

# 画像は全サーバーのローカルのファイルから返すので、画像置き場の upstream の設定は置かない
sudo rm -f "$store"
sudo install -m 644 /tmp/isureal.conf.new "$site"

if sudo nginx -t; then
  sudo systemctl reload nginx
  echo "reloaded nginx ($MODE)"
else
  echo "nginx -t に失敗したので元の設定に戻す" >&2
  restore
  sudo nginx -t
  exit 1
fi
REMOTE
done
