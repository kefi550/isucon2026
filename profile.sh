#!/bin/bash
# ベンチ中の CPU プロファイルを取る。ベンチのボタンを押す前に起動しておく。
# /api/initialize が nginx のログに出たら、PROFILE_SECONDS 秒（既定 90）計測して手元に保存する。
# 使い方: ./profile.sh [host]   （host の既定は isu1）

set -eu

host="${1:-isu1}"
seconds="${PROFILE_SECONDS:-90}"
out="profiles/$(date +%Y%m%d-%H%M%S)-${host}.pprof"
mkdir -p profiles

echo "${host}: ベンチの開始（/api/initialize）を待っています。ボタンを押してください"
ssh "${host}" "timeout 600 tail -n0 -F /var/log/nginx/access.log 2>/dev/null | grep -m1 -q /api/initialize"
echo "${host}: 開始を検知。${seconds} 秒計測します"
ssh "${host}" "curl -sf 'http://127.0.0.1:6060/debug/pprof/profile?seconds=${seconds}'" > "${out}"
echo "保存: ${out}"

go tool pprof -top -nodecount=40 "${out}" | head -n 50
echo
echo "ブラウザで見る: go tool pprof -http=:10060 ${out}"
