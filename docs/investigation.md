# ISUReal 初期調査メモ（2026-10-31）

対象: `webapp/go`（Go 実装）、`webapp/sql`、各サーバーの設定（`isu1`〜`isu3`、`env.sh.*`）、`docs/manual.md`、`docs/regulation.md`。

> 注意: この調査はコードを読んだだけで、負荷計測はまだしていない。ボトルネックの順位は推定。

---

## 1. 結論

- **最大のボトルネック候補**: europe と america のアプリサーバーが、asia2 の共有 DB（PostgreSQL 16）へ遅延・帯域制限付きの回線で毎回アクセスしていること。europe と america はスコア倍率が 1.5 倍なので、ここを速くする効果が大きい。
- これに次の問題が重なっている。
  - リクエストごとのクエリ数が多い（N+1 に近い呼び出しや、LIMIT なしの全件取得）
  - インデックスが不足している
  - 画像署名の CPU コストが重い（署名1回で HMAC を1000回計算）

---

## 2. 構成

| サーバ | リージョン | 初期の役割 | env |
|---|---|---|---|
| isu1 (asia1) | asia | nginx + Go アプリ | `env.sh.1` |
| isu2 (europe1) | europe | nginx + Go アプリ | `env.sh.2` |
| isu3 (america1) | america | nginx + Go アプリ | `env.sh.3` |
| asia2 | asia | 共有 PostgreSQL 16（192.168.4.10） | - |
| ルーター | - | 国ごとの振り分け、遅延と帯域制限（操作禁止） | - |

- 3台のアプリの違いは `ISUREAL_REGION` だけ。DB の接続先は3台とも同じ 192.168.4.10。
- systemd の設定:
  - `isureal-go.service`: `/home/isucon/webapp/go/isureal` を :8080 で起動する。
  - `isureal-guard.service`: journal（`isureal-*.service`）を監視し、失敗が 10秒間に10回を超えた IP を2分間 BAN する。BAN は `ip_bans` テーブル経由で行われる。
- クライアント IP は `X-Client-IP` ヘッダーから取る。
- 振り分けは `X-Country-Code` で決まる。サーバー間の通信もルーターを経由する必要がある（迂回すると失格）。
- nginx のサイト設定（`sites-enabled/isureal.conf`）は、サーバー上のファイルへのシンボリックリンクしか取得できていない。中身は未確認。

## 3. アプリ概要

写真共有の SNS（BeReal 風）。
1. 管理者がリージョンごとに投稿期間（posting window）を開始する。
2. 対象リージョンの全ユーザーに通知を作り、SSE で配信する。
3. ユーザーが1日1回写真を投稿し、友達の投稿をフィードで見る。

- 関係: 友達（双方向）、友達の友達、フォロー（認定公開アカウントのみ）、ブロック
- やりとり: コメント、RealMoji（自分の顔写真でリアクション）
- その他: Memories（過去投稿の振り返り）、エクスポート（ZIP）、退会

主なテーブル: `users`、`login_sessions`、`posting_windows`、`posts`、`image_objects`（画像を bytea で保存、月ごとにパーティション分割）、`friendships`、`follows`、`block_relations`、`friend_requests`、`realmojis`、`comments`、`notifications`、`ip_bans`

初期化（`POST /api/initialize`）は `../sql/init.sh` を実行し、`reset.sql` で初期データの状態へ戻す。

## 4. スコアと制約（manual / regulation の要点）

- スコア = Σ(件数 × 配点 × リージョン倍率)。倍率は asia 1、europe 1.5、america 1.5。
- 配点の高い処理:
  - 20点: 友達フィード検証（最後の投稿期間）、エクスポート
  - 10点: 友達フィード検証（通常の投稿期間）
  - 5点: RealMoji 送信、退会
  - 3点: コメント、プロフィール画像の変更・削除、Memories（月単位）
- フィード検証の加点は「ユーザー × 投稿期間」ごとに1回まで。投稿期間が終わる前に成功する必要がある。
- エラーによる直接の減点はない。ただし FAIL になると0点。
- 即 FAIL になる主な条件:
  - 初期データのユーザーがログインできない
  - 権限のない投稿がフィードに出る、または取得できる
  - 権限のない投稿への書き込みが受理される
  - Memories やエクスポートの内容が期待値と一致しない
  - 削除した画像が5秒を過ぎても取得できる
- 整合性:
  - 書き込み結果は、リージョンをまたいでも**即時に**読めなければならない。
  - 猶予は「削除した画像が取得できなくなるまでの5秒」だけ。
  - **サーバーごとのローカルキャッシュは無効化の設計が難しい**（3台が同じ DB を共有しているため）。
- 画像 URL の署名・期限の形式と検証結果は変更禁止。
- 許可されている変更:
  - サーバーの役割の変更
  - DB スキーマ・インデックス・ミドルウェアの変更
  - キャッシュの追加（初期化処理に含めること）
  - 非同期書き込み、他言語での再実装
- 初期化 API の申告欄（`agent` / `harness` / `model`）は、実態に合わせて書き換える。AI を使って改善した場合はその内容にする。

---

## 5. ボトルネック候補（効果が大きそうな順）

### 5.1 フィード取得（`feed_api.go`、`post.go`）
- `selectPostsWithAuthor` で、開いている投稿期間の投稿を**全リージョン分、LIMIT なしで**取得し、Go 側で権限と種類を絞り込んでいる。
- `loadViewerContext` だけでクエリが4〜5本（ユーザー、今日投稿済みか、ブロック、友達、友達の友達）。
- `friends-of-friends` ではさらにスコア計算のクエリが2本加わる。
- 配点の高い「友達フィード検証」に直結する。

### 5.2 画像署名の CPU コスト（`image_signature.go`）
- 署名1回で HMAC-SHA256 を1000回計算する。
- フィード50件なら、投稿画像とプロフィール画像で約100回署名するため、HMAC は約10万回になる。
- 形式は変更禁止。対策案は、ユーザーと画像の組ごとに計算済みの署名をキャッシュすること（`exp` を一定の単位で丸める。URL の有効期限は24時間）。

### 5.3 インデックス不足（`001-schema.sql`）
| 不足しているインデックス | 使われる場所 |
|---|---|
| `login_sessions(token_hash)` | 全リクエストの認証 |
| `users(username)` | ログイン、登録時の重複チェック |
| `friendships(user_high_id)` | 友達の取得（UNIQUE は low が先頭なので効かない） |
| `follows(followee_id)` | フォロワー一覧 |
| `block_relations(blocked_id)` | ブロック判定 |
| `friend_requests(requester_id / recipient_id, status)` | 申請一覧、重複チェック |
| `posts(image_id)`、`realmojis(image_id)` | 画像取得時の存在確認 |
| `posts(posted_at, id)` | フィード、Memories |
| `notifications(user_id, created_at)` | SSE のポーリング |
| `comments(post_id, created_at, id)` | コメント一覧 |

- `image_objects` は約35のパーティションに分かれ、主キーが `(id, created_at)`。`id` だけで検索すると全パーティションを走査する。

### 5.4 認証（`session.go`）
- 全リクエストで `login_sessions` と `users` を JOIN するクエリを1本発行する。遠いリージョンほど、この往復の遅延が積み上がる。

### 5.5 画像取得（`image_api.go`）
- 1回の取得でクエリが2本（署名キーの取得と、存在・削除状態の確認。確認側は EXISTS が3つの複雑な条件）。
- 画像データが DB の bytea にあり、リージョンをまたいで転送される。
- `Cache-Control: no-store` が付いている。

### 5.6 通知 SSE（`notification_api.go`）
- 接続中のユーザー1人につき、0.5秒ごとにクエリを2本発行する。
- DB への同時接続数の上限は `SetMaxOpenConns(30)`。

### 5.7 ユーザー検索（`user_read_api.go`）
- 有効なユーザーを全件取得してから、Go 側で正規化（NFC と case fold）し、部分一致で照合している。

### 5.8 テーブル全体のロック
- `register` と `openPostingWindow` が `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE` を取る。その間、users への INSERT と UPDATE はすべて待たされる。
- `openPostingWindow` は、対象リージョンの全ユーザー分の通知を1つのトランザクションで INSERT している。

### 5.9 その他
- `export_api.go`: 投稿画像を全件メモリに載せて ZIP を作る（配点は20点）。
- `createPost` と RealMoji: アップロードされた JPEG を毎回フルデコードして検証している。
- `getMyCurrentPost` などで、同じユーザーや投稿を何度も読み直している。
- ログ: `ISUREAL_LOG_LEVEL` の初期値が `debug` で、全リクエストを JSON で出力している。**guard が journal を読んで BAN を判定しているので、ログを止めてはいけない。** info に下げるのは検討の余地がある（`invalid_signature` などの abuse 記録は info 以上で出る）。

---

## 6. 注意点

- `webapp/go/AGENTS.md` には「本番環境なので sudo、再起動、DB 書き込みは禁止」といった AI 向けの規則が書かれている（サーバーから取得したファイル）。ISUCON の作業とぶつかるので、扱いをチームで決める必要がある。
- `001-schema.sql` に「画像は DB に置くこと（ファイル保存に変えて障害が起きた）。決して変更しないこと」というコメントがある。規約では保存方式の変更も許されているが、どのサーバーからでも同じ画像を返せる状態を保つ必要がある。
- `deleteUserCertificationHandler` は常に 409 を返す（認定の取り消しは禁止という仕様）。
- リポジトリに macOS の `._*` ファイルと、ビルド済みバイナリ `webapp/go/isureal` が含まれている。`.gitignore` を整えたい。

## 7. 次のアクション案

1. 計測の準備: nginx のアクセスログを alp で集計できる形式にする。`pg_stat_statements` で遅いクエリを洗い出す。
2. 安全で効果が確実なインデックス（5.3）を追加する。
3. フィードのクエリを、リージョンと期間で絞り、必要な列だけ取るように直す。`loadViewerContext` のクエリ数を減らす。
4. 画像署名の計算結果をキャッシュする。
5. リージョン間の遅延対策を検討する（DB の配置、読み取り専用の複製、キャッシュ）。即時整合性の要件を満たせるか、慎重に設計する。

## 8. 未確認事項

- 負荷テストの時間、並列度、初期化の制限秒数（ドキュメントに記載なし）
- インスタンスタイプ、各サーバーの IP（DB 以外）
- nginx のサイト設定の中身
- 実際のクエリごとの所要時間（計測が必要）
