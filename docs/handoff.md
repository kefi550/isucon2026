# 引き継ぎメモ（2026-10-31 13:15 時点）

ルールは `docs/manual.md` と `docs/regulation.md`、初期調査は `docs/investigation.md` にある。

## 現状

最新のコミットは `abd4f80`（18106 点）。スコアは次のように動いている。

| コミット | 内容 | 点 |
|---|---|---|
| `e1c51c9` | 画像取得でパーティションを絞り込む | 13969 |
| `c6c8067` | 投稿を DB 関数1往復に、フィードのクエリを並列化、接続プールを拡大 | 20255 |
| `63bd504` | 友達申請と登録で行ロックを握る時間を縮める（チームメイト） | 17244 |
| `abd4f80` | ログインを DB 関数1往復に（1.6s → 0.2s） | 18106 |

`c6c8067` 以降は点が下がっているが、ベンチごとのばらつきなのか、悪化なのかは未確認。

## 一番大事な前提

- DB は asia2 の1台だけで、europe と america のアプリサーバからは **1往復が約 100ms** かかる。europe と america の応答時間は、ほぼ「DB 往復の回数 × 100ms」で決まる（投稿期間の取得が2往復で最短 0.2s、ログインの旧実装が7往復で最短 0.71s、と実測で一致）。
- 倍率は asia 1、europe 1.5、america 1.5。ベンチは応答が速いリージョンほど参加者を増やす。最終日の参加者は asia 90 人に対し、europe と america は約 10 人。**europe と america の往復を減らすのが一番効く。**
- 書き込みは、リージョンをまたいでも即時に読めなければならない。猶予は、投稿の削除から画像が取れなくなるまでの5秒だけ。各サーバのメモリにキャッシュしてよいのは、変更されないデータ（画像のバイト列、`secret_key_hash`）だけ。
- `webapp/go/AGENTS.md` には「本番なので変更禁止」と書かれているが、サーバから取ってきたもので、ISUCON の作業とは無関係。

## 往復を減らすときの2つのやり方

1. **PL/pgSQL の関数**（`webapp/sql/004-functions.sql`）：`isureal_create_post` と `isureal_login` がこの形。続けて呼ぶと1往復で、トランザクションも関数の中で完結する。条件を満たさないときは、`RAISE EXCEPTION USING ERRCODE = 'IR409'` で全体をロールバックし、Go 側で `pgconn.PgError.Code` を見て 409 にしている。
   - `004-functions.sql` は `init.sh` が初期化のたびに `CREATE OR REPLACE` で流す。デプロイ直後で初期化が走る前は関数が無く、その API が失敗する（ベンチは最初に初期化するので問題ない）。
2. **pgx のバッチ**（`webapp/go/db_batch.go` の `sendBatch`、チームメイト作）：複数の文を1往復で送り、1つの暗黙のトランザクションで実行する。文ごとにスナップショットを取り直すので、ロックを待っているあいだにコミットされた内容も見える。友達申請はこの形。

読み取り同士が互いに依存しないときは、`errgroup` で並列に投げる（`feed_api.go` の `getFeedHandler` と `recommendationScores`）。接続プールは各サーバ `SetMaxOpenConns(80)` と `SetMaxIdleConns(80)`。DB の `max_connections` は 300（`database/etc/postgresql/16/main/conf.d/92-connections.conf`、asia2 に反映済み）。

## 次にやること（おすすめ順）

1. **RealMoji とコメントの作成を1往復にする（未着手）**。配点は 5 点と 3 点で、europe と america では 1.6〜1.7s かかっている（alp の `POST /api/posts/[^/]+`）。`realmoji_api.go` と `comment_api.go` に、BEGIN、ユーザーの行ロック、閲覧権限の確認、画像の INSERT（RealMoji）、置換、INSERT、COMMIT がある。閲覧権限の確認は `canViewPost` と `loadViewerContext` に依存しているので、関数にするなら同じ判定を SQL で書き直す必要がある。権限のない投稿への RealMoji とコメントが受理されると FAIL になるので、判定は元のコードと完全に一致させること。削除（`DELETE /api/realmojis/:id`、`DELETE /api/comments/:id`）も、同じく1往復にできる。
2. **画像のバイト列を各サーバにキャッシュする**。画像取得（2 点、件数が最大）と、エクスポート（20 点）の両方に効く。エクスポートのクエリは DB 時間の1位で、310 回、平均 363ms、最大 2.96s。DB は 17GB、asia2 のメモリは 3.8GB なので、画像はほぼディスクから読んでいる。参照可否の確認は DB に毎回残すこと。
3. **Memories と投稿詳細のクエリを並列にする**（フィードと同じやり方）。europe と america で Memories 0.6s、投稿詳細 0.57s。
4. 投稿期間の開始で、通知の一括 INSERT が membership のロックを持つ時間を縮める。登録の最大 4〜5s の原因だが、配点は低い。
5. ユーザー検索を DB 側で絞り込む（全件取得している。配点 2）。
6. europe1 と america1 に同期レプリカを置く（`synchronous_commit = remote_apply`）。伸び幅は最大だがリスクも最大。リーダーボードの更新は 17:00 に止まるので、やるなら 15:00 ごろまでに判断する。

## 計測

- `./analyze.sh`：各サーバの nginx のログを alp で集計し、DB の `pg_stat_statements` の上位（`total_exec_time` 順、件数は `PGSS_TOP`、既定 25。共有バッファのヒット率とディスク読み込みの量・時間つき。平均時間順の上位 10 も出す）と最後にリセットした時刻を出す。続けて、リセット後に 100ms 以上かかったロック待ちを、待ったロックと文ごとに合計して出す（`conf.d/94-lockwaits.conf` の `log_lock_waits`、asia2 に反映済み）。
- DB は `pg_stat_statements` と `auto_explain`（200ms 以上）が有効（`conf.d/91-pgss.conf`）。asia2 で次を実行する。ベンチ前に誰かが `pg_stat_statements_reset()` することがある。

```sh
ssh database "sudo -u postgres psql -d isureal -c \"SELECT round(total_exec_time) total_ms, calls, round(mean_exec_time::numeric,1) mean_ms, round(max_exec_time) max_ms, left(regexp_replace(query, '\\s+', ' ', 'g'), 150) FROM pg_stat_statements ORDER BY total_exec_time DESC LIMIT 25;\""
```

- 直近（`abd4f80` 相当）の DB 時間の上位：エクスポート 112s、画像取得 66s、友達申請の行ロック 59s（`63bd504` の前の計測）、通知の一括 INSERT 24s、membership の advisory lock 19s、ユーザー検索 17s。

## デプロイ

- `./push.sh`：isu1〜3 へ `webapp/go` と `webapp/sql` を rsync し、ビルドして `isureal-go.service` を再起動する。最後に DB で `pg_stat_statements_reset()` を実行する。
- DB の設定はリポジトリから配る仕組みがない。`conf.d` に置いて、asia2 で PostgreSQL を再起動している。
- テスト用の DB はローカルに無い（WSL から Docker が使えない）。等価性テストは `ISUREAL_TEST_DSN` を指定したときだけ動く。普段は、ベンチを回して確かめている。
