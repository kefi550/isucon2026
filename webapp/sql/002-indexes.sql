-- 初期化は 001-schema.sql を再実行しない。既存 DB へは init.sh がこのファイルを流す。
-- 2回目以降の CREATE INDEX IF NOT EXISTS は何もしない。

CREATE INDEX IF NOT EXISTS image_objects_id ON image_objects (id);
CREATE INDEX IF NOT EXISTS users_username ON users (username);
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
CREATE INDEX IF NOT EXISTS notifications_user_id_created_at ON notifications (user_id, created_at);

-- (post_id) だけの index は (post_id, created_at, id) に含まれる。
DROP INDEX IF EXISTS comments_post_id;
