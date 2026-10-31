-- 初期化は 001-schema.sql を再実行しない。既存 DB へは init.sh がこのファイルを流す。
-- 2回目以降の CREATE INDEX IF NOT EXISTS は何もしない。

CREATE INDEX IF NOT EXISTS image_objects_id ON image_objects (id);
-- ユーザー名の一意性は register の LOCK TABLE ではなく UNIQUE で保証する。
-- 既存の非ユニーク index は UNIQUE の作成に成功してから消す。2回目以降はどちらも何もしない。
CREATE UNIQUE INDEX IF NOT EXISTS users_username_key ON users (username);
DROP INDEX IF EXISTS users_username;
-- 窓の開始が、リージョンの active なユーザーを読むときに使う。
CREATE INDEX IF NOT EXISTS users_region_active ON users (region) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS login_sessions_token_hash ON login_sessions (token_hash);
CREATE INDEX IF NOT EXISTS friendships_user_high_id ON friendships (user_high_id);
CREATE INDEX IF NOT EXISTS follows_followee_id ON follows (followee_id);
CREATE INDEX IF NOT EXISTS block_relations_blocked_id ON block_relations (blocked_id);
CREATE INDEX IF NOT EXISTS friend_requests_requester_id_status ON friend_requests (requester_id, status);
CREATE INDEX IF NOT EXISTS friend_requests_recipient_id_status ON friend_requests (recipient_id, status);
CREATE INDEX IF NOT EXISTS posts_image_id ON posts (image_id);
CREATE INDEX IF NOT EXISTS posts_posted_at_id ON posts (posted_at, id);
CREATE INDEX IF NOT EXISTS realmojis_image_id ON realmojis (image_id);
CREATE INDEX IF NOT EXISTS comments_post_id_created_at_id ON comments (post_id, created_at, id);
-- notifications は、窓の開始で全ユーザー分（約1700行）を一括 INSERT する。行ごとに更新する構造を最小にする:
--   * 読むのは「窓の通知をまとめて」（broker）と「窓×ユーザーの存在確認」（窓の開始の補正）だけなので、
--     UNIQUE(窓, ユーザー) が両方を賄う。1つの窓の行がこの索引の連続した範囲に入るので、挿入が散らばらない。
--     旧 UNIQUE(ユーザー, 窓)・(ユーザー, created_at)・(窓) は、ユーザー ID の順に散らばって書き込むうえ、
--     (ユーザー, created_at) はどこからも使われていない。
--   * id は参照されないので、PK（ランダムな UUID で散らばって書く）は持たない。
--   * 外部キーは、挿入のたびに users の行ロック（KEY SHARE）を取って WAL を出すので外す。
--     ユーザーは削除されず（status で表す）、初期化は session_replication_role = replica で消すので、整合性は保たれる。
-- 新しい索引の作成に成功してから古いものを消す。2回目以降はすべて何もしない。
CREATE UNIQUE INDEX IF NOT EXISTS notifications_posting_window_id_user_id_key ON notifications (posting_window_id, user_id);
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_user_id_posting_window_id_key;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_pkey;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_user_id_fkey;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_posting_window_id_fkey;
DROP INDEX IF EXISTS notifications_user_id_created_at;
DROP INDEX IF EXISTS notifications_posting_window_id;

-- (post_id) だけの index は (post_id, created_at, id) に含まれる。
DROP INDEX IF EXISTS comments_post_id;
