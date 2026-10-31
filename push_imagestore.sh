#!/bin/bash
# 初期データの画像を置く「画像置き場」を asia2（database）に用意する。push.sh / push_nginx.sh とは独立している。
#
#   ./push_imagestore.sh setup    ツールと設定を asia2 に置き、isureal-images.service（nginx、ポート 8081）を起動・自動起動にする
#   ./push_imagestore.sh export   初期画像（約14GB・約183万枚）を DB からファイルに書き出す（バックグラウンド。nice / ionice で低優先度）
#   ./push_imagestore.sh status   書き出しの進み具合（ファイル数・サイズ・ログの末尾）と、サービスの状態を見る
#   ./push_imagestore.sh verify   ランダムな画像を DB と sha256 で照合する（書き出しの後に）
#   ./push_imagestore.sh test     少数（1パーティション50件）だけ書き出して、照合する（全件の前の動作確認）
#   ./push_imagestore.sh stop     画像置き場のサービスを止める（各サーバーの nginx は DB 経由の予備に切り替わる）
#
# export は DB の IO を大きく使う。ベンチが走っていないときに実行し、終わる（status で確認）までベンチを回さない。
set -eu
cd -- "$(dirname -- "$0")"
HOST=database
OUT=/var/lib/isureal-images

build() {
  (cd webapp/go && go build -o ../../.imgexport.bin ./cmd/imgexport)
}

case "${1:-}" in
  setup)
    build
    scp -q .imgexport.bin "${HOST}:/tmp/imgexport"
    scp -q database/etc/nginx/isureal-images.conf "${HOST}:/tmp/isureal-images.conf"
    scp -q database/etc/systemd/system/isureal-images.service "${HOST}:/tmp/isureal-images.service"
    ssh "$HOST" "OUT=$OUT bash -s" <<'REMOTE'
set -eu
sudo install -m 755 /tmp/imgexport /usr/local/bin/imgexport
sudo install -d -o postgres -g postgres -m 755 "$OUT"
sudo install -m 644 /tmp/isureal-images.conf /etc/nginx/isureal-images.conf
sudo install -m 644 /tmp/isureal-images.service /etc/systemd/system/isureal-images.service
sudo nginx -t -c /etc/nginx/isureal-images.conf
sudo systemctl daemon-reload
sudo systemctl enable --now isureal-images.service
sleep 1
systemctl is-active isureal-images.service
ss -ltn | grep ':8081 ' || { echo "8081 で待ち受けていない" >&2; exit 1; }
REMOTE
    ;;
  export)
    ssh "$HOST" "OUT=$OUT bash -s" <<'REMOTE'
set -eu
if pgrep -f '/usr/local/bin/imgexport' >/dev/null; then echo "すでに実行中"; exit 0; fi
nohup sudo -u postgres nice -n 19 ionice -c3 /usr/local/bin/imgexport -out "$OUT" -workers 3 > /tmp/imgexport.log 2>&1 &
sleep 2
echo "バックグラウンドで開始した。ログ: /tmp/imgexport.log（./push_imagestore.sh status で確認）"
REMOTE
    ;;
  status)
    ssh "$HOST" "OUT=$OUT bash -s" <<'REMOTE'
echo "== サービス"; systemctl is-active isureal-images.service || true
echo "== 実行中のツール"; pgrep -af '/usr/local/bin/imgexport' || echo "(実行中ではない)"
echo "== 書き出し済み"; echo "ファイル数: $(sudo find "$OUT" -type f ! -name '*.tmp' | wc -l)  サイズ: $(sudo du -sh "$OUT" | cut -f1)  ディスクの空き: $(df -h / | awk 'NR==2{print $4}')"
echo "== ログの末尾"; tail -5 /tmp/imgexport.log 2>/dev/null || echo "(ログなし)"
REMOTE
    ;;
  verify)
    ssh "$HOST" "OUT=$OUT bash -s" <<'REMOTE'
sudo -u postgres /usr/local/bin/imgexport -out "$OUT" -verify 2000
REMOTE
    ;;
  test)
    ssh "$HOST" "OUT=$OUT bash -s" <<'REMOTE'
set -eu
sudo -u postgres nice -n 19 ionice -c3 /usr/local/bin/imgexport -out "$OUT" -limit 50 -workers 2 | tail -3
sudo -u postgres /usr/local/bin/imgexport -out "$OUT" -verify 200
REMOTE
    ;;
  stop)
    ssh "$HOST" 'sudo systemctl disable --now isureal-images.service; systemctl is-active isureal-images.service || true'
    ;;
  *)
    sed -n '2,12p' "$0"
    exit 1
    ;;
esac
