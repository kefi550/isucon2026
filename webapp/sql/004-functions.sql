-- 初期化のたびに init.sh が流す。CREATE OR REPLACE なので何度実行してもよい。

-- 投稿の作成を1往復で行う。処理の順序とロックは Go で1文ずつ発行していたときと同じ。
-- 投稿できないときは IR409 で例外にして、画像の INSERT ごとロールバックさせる。
CREATE OR REPLACE FUNCTION isureal_create_post(
  p_post_id text, p_image_id text, p_user_id text, p_data bytea, p_posted_at timestamptz,
  p_caption text, p_visibility text, p_location_kind text,
  p_latitude numeric, p_longitude numeric, p_locality text
) RETURNS TABLE (
  user_id text, username text, secret_key_hash text, display_name text, bio text,
  profile_image_id text, region text, account_type text, status text,
  post_id text, posting_window_id text, image_id text, caption text, visibility text,
  location_kind text, latitude numeric, longitude numeric, locality text,
  posted_at timestamptz, on_time_deadline timestamptz
) LANGUAGE plpgsql AS $$
#variable_conflict use_column
DECLARE
  v_region text;
  v_status text;
  v_window_id text;
  v_local_date date;
  v_starts_at timestamptz;
  v_active int;
  v_deleted int;
  v_posted_at timestamptz := p_posted_at;
BEGIN
  -- 画像の書き込みは行ロックを取らない。スロットの確認だけ、ロックのあとで行う。
  INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at)
  VALUES (p_image_id, p_user_id, p_data, 'image/jpeg', p_posted_at);

  SELECT u.region, u.status INTO v_region, v_status FROM users u WHERE u.id = p_user_id FOR NO KEY UPDATE;
  IF NOT FOUND OR v_status <> 'active' THEN
    RAISE EXCEPTION 'user % is not active', p_user_id;
  END IF;

  -- FOR SHARE は同じ期間への投稿を並列に通す。期間を閉じる UPDATE は FOR NO KEY UPDATE なので、
  -- 終了処理とは引き続き競合し、ended_at 設定後の投稿は通らない。
  -- 期間の切り替わりと重なると1回目は行が見えないので、新しい期間を読み直す。
  SELECT w.id, w.local_date, w.starts_at INTO v_window_id, v_local_date, v_starts_at
  FROM posting_windows w WHERE w.region = v_region AND w.ended_at IS NULL FOR SHARE;
  IF NOT FOUND THEN
    SELECT w.id, w.local_date, w.starts_at INTO v_window_id, v_local_date, v_starts_at
    FROM posting_windows w WHERE w.region = v_region AND w.ended_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
      RAISE EXCEPTION USING ERRCODE = 'IR409', MESSAGE = 'post_slot_unavailable';
    END IF;
  END IF;

  -- 元の実装は投稿時刻をロックの取得後に決めていた。ロックを待つあいだに期間が切り替わったときは、
  -- 受け付けた時刻が新しい期間の開始より前になるので、ロック後の時刻を投稿時刻にする。
  -- export は画像と投稿を created_at = posted_at で結ぶので、画像の created_at も合わせる。
  IF v_posted_at < v_starts_at THEN
    v_posted_at := clock_timestamp();
    IF v_posted_at < v_starts_at THEN
      RAISE EXCEPTION USING ERRCODE = 'IR409', MESSAGE = 'post_slot_unavailable';
    END IF;
    UPDATE image_objects SET created_at = v_posted_at WHERE id = p_image_id AND created_at = p_posted_at;
  END IF;

  SELECT count(*) FILTER (WHERE p.deleted_at IS NULL), count(*) FILTER (WHERE p.deleted_at IS NOT NULL)
  INTO v_active, v_deleted
  FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.author_id = p_user_id AND w.local_date = v_local_date;
  IF v_active <> 0 OR v_deleted >= 2 THEN
    RAISE EXCEPTION USING ERRCODE = 'IR409', MESSAGE = 'post_slot_unavailable';
  END IF;

  INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility,
                     location_kind, latitude, longitude, locality, posted_at)
  VALUES (p_post_id, p_user_id, v_window_id, p_image_id, p_caption, p_visibility,
          p_location_kind, p_latitude, p_longitude, p_locality, v_posted_at);

  RETURN QUERY
  SELECT u.id::text, u.username::text, u.secret_key_hash, u.display_name, u.bio,
         u.profile_image_id::text, u.region, u.account_type, u.status,
         p.id::text, p.posting_window_id::text, p.image_id::text, p.caption, p.visibility,
         p.location_kind, p.latitude, p.longitude, p.locality, p.posted_at, w.on_time_deadline
  FROM posts p
  JOIN posting_windows w ON w.id = p.posting_window_id
  JOIN users u ON u.id = p.author_id
  WHERE p.id = p_post_id;
END
$$;

-- ログインを1往復で行う。資格情報が違う、または退会済みなら何も書かずに0行を返す（呼び出し側は 401）。
-- 処理の順序とロックは Go で1文ずつ発行していたときと同じ。
CREATE OR REPLACE FUNCTION isureal_login(
  p_username text, p_secret_key_hash text, p_session_id text, p_token_hash text
) RETURNS TABLE (
  id text, username text, display_name text, bio text, region text, account_type text, status text
) LANGUAGE plpgsql AS $$
#variable_conflict use_column
DECLARE
  v_user users%ROWTYPE;
BEGIN
  SELECT * INTO v_user FROM users u WHERE u.username = p_username;
  IF NOT FOUND OR v_user.status <> 'active' OR v_user.secret_key_hash <> p_secret_key_hash THEN
    RETURN;
  END IF;

  PERFORM 1 FROM users u WHERE u.id = v_user.id AND u.status = 'active' FOR NO KEY UPDATE;
  IF NOT FOUND THEN
    RETURN;
  END IF;

  UPDATE login_sessions s
  SET active = false, invalidated_at = clock_timestamp(), reason = 'replaced'
  WHERE s.user_id = v_user.id AND s.active = true;

  INSERT INTO login_sessions (id, user_id, token_hash) VALUES (p_session_id, v_user.id, p_token_hash);

  RETURN QUERY SELECT v_user.id::text, v_user.username::text, v_user.display_name, v_user.bio,
                      v_user.region, v_user.account_type, v_user.status;
END
$$;

-- 投稿の閲覧可否とコメント可否。Go の canViewPost（viewerContext.canView）と canComment と同じ判定。
-- 投稿が無い・削除済み・投稿者が退会済み、または閲覧者が退会済みなら両方 false。
CREATE OR REPLACE FUNCTION isureal_post_access(p_viewer_id text, p_post_id text)
RETURNS TABLE (can_view boolean, can_comment boolean) LANGUAGE plpgsql AS $$
DECLARE
  v_author_id text;
  v_visibility text;
  v_window_ended_at timestamptz;
  v_region text;
  v_posted_today boolean;
  v_friend_ids text[];
  v_friend boolean;
  v_can_view boolean;
BEGIN
  SELECT p.author_id, p.visibility, w.ended_at INTO v_author_id, v_visibility, v_window_ended_at
  FROM posts p
  JOIN posting_windows w ON w.id = p.posting_window_id
  JOIN users u ON u.id = p.author_id AND u.status = 'active'
  WHERE p.id = p_post_id AND p.deleted_at IS NULL;
  IF NOT FOUND THEN
    RETURN QUERY SELECT false, false;
    RETURN;
  END IF;

  SELECT u.region INTO v_region FROM users u WHERE u.id = p_viewer_id AND u.status = 'active';
  IF NOT FOUND THEN
    RETURN QUERY SELECT false, false;
    RETURN;
  END IF;

  SELECT EXISTS(
    SELECT 1 FROM posts p
    JOIN posting_windows posted ON posted.id = p.posting_window_id
    JOIN posting_windows current ON current.region = v_region AND current.ended_at IS NULL
                                AND current.local_date = posted.local_date
    WHERE p.author_id = p_viewer_id AND p.deleted_at IS NULL
  ) INTO v_posted_today;

  IF v_author_id = p_viewer_id THEN
    RETURN QUERY SELECT true, v_window_ended_at IS NULL AND v_posted_today;
    RETURN;
  END IF;

  IF v_window_ended_at IS NOT NULL OR NOT v_posted_today OR EXISTS(
    SELECT 1 FROM block_relations b
    WHERE (b.blocker_id = p_viewer_id AND b.blocked_id = v_author_id)
       OR (b.blocker_id = v_author_id AND b.blocked_id = p_viewer_id)
  ) THEN
    RETURN QUERY SELECT false, false;
    RETURN;
  END IF;

  IF v_visibility = 'public' THEN
    RETURN QUERY SELECT true, true;
    RETURN;
  END IF;

  v_friend_ids := ARRAY(
    SELECT CASE WHEN f.user_low_id = p_viewer_id THEN f.user_high_id ELSE f.user_low_id END
    FROM friendships f WHERE f.user_low_id = p_viewer_id OR f.user_high_id = p_viewer_id
  );
  v_friend := v_author_id = ANY(v_friend_ids);
  IF v_visibility = 'friends' THEN
    v_can_view := v_friend;
  ELSIF v_visibility = 'friends_of_friends' THEN
    v_can_view := v_friend OR EXISTS(
      SELECT 1 FROM friendships f
      WHERE (f.user_low_id = v_author_id AND f.user_high_id = ANY(v_friend_ids))
         OR (f.user_high_id = v_author_id AND f.user_low_id = ANY(v_friend_ids))
    );
  ELSE
    v_can_view := false;
  END IF;
  RETURN QUERY SELECT v_can_view, v_can_view AND v_friend;
END
$$;

-- RealMoji の置き換えを1往復で行う。処理の順序とロックは Go で1文ずつ発行していたときと同じ。
-- 受理できないときは IR403 / IR404（MESSAGE がエラーコード）で例外にして、画像の INSERT ごとロールバックさせる。
CREATE OR REPLACE FUNCTION isureal_replace_realmoji(
  p_id text, p_image_id text, p_post_id text, p_actor_id text, p_data bytea, p_reaction_type text
) RETURNS TABLE (
  id text, post_id text, reactor_id text, image_id text, reaction_type text,
  created_at timestamptz, replaced_at timestamptz, deleted_at timestamptz,
  user_id text, username text, secret_key_hash text, display_name text, bio text,
  profile_image_id text, region text, account_type text, status text
) LANGUAGE plpgsql AS $$
#variable_conflict use_column
DECLARE
  v_author_id text;
  v_locked int;
BEGIN
  SELECT p.author_id INTO v_author_id
  FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.id = p_post_id AND p.deleted_at IS NULL;
  IF NOT FOUND OR v_author_id = p_actor_id THEN
    RAISE EXCEPTION USING ERRCODE = 'IR403', MESSAGE = 'interaction_forbidden';
  END IF;

  -- 画像の書き込みが終わるまで、2人分の行ロックは取らない。
  INSERT INTO image_objects (id, owner_user_id, data, media_type)
  VALUES (p_image_id, p_actor_id, p_data, 'image/jpeg');

  -- lockRelationshipUsers と同じく、id のバイト順でロックする。
  SELECT count(*) INTO v_locked FROM (
    SELECT u.id FROM users u
    WHERE u.id = ANY(ARRAY[p_actor_id, v_author_id]) AND u.status = 'active'
    ORDER BY u.id COLLATE "C" FOR NO KEY UPDATE
  ) locked;
  IF v_locked <> 2 THEN
    RAISE EXCEPTION USING ERRCODE = 'IR404', MESSAGE = 'relationship_not_found';
  END IF;

  IF NOT (SELECT a.can_view FROM isureal_post_access(p_actor_id, p_post_id) a) THEN
    RAISE EXCEPTION USING ERRCODE = 'IR403', MESSAGE = 'interaction_forbidden';
  END IF;

  UPDATE realmojis r SET replaced_at = clock_timestamp()
  WHERE r.post_id = p_post_id AND r.reactor_id = p_actor_id AND r.replaced_at IS NULL AND r.deleted_at IS NULL;

  INSERT INTO realmojis (id, post_id, reactor_id, image_id, reaction_type, created_at)
  VALUES (p_id, p_post_id, p_actor_id, p_image_id, p_reaction_type, clock_timestamp());

  RETURN QUERY
  SELECT r.id::text, r.post_id::text, r.reactor_id::text, r.image_id::text, r.reaction_type,
         r.created_at, r.replaced_at, r.deleted_at,
         u.id::text, u.username::text, u.secret_key_hash, u.display_name, u.bio,
         u.profile_image_id::text, u.region, u.account_type, u.status
  FROM realmojis r JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
  WHERE r.id = p_id;
END
$$;

-- コメントの作成を1往復で行う。処理の順序とロックは Go で1文ずつ発行していたときと同じ。
CREATE OR REPLACE FUNCTION isureal_create_comment(
  p_id text, p_post_id text, p_actor_id text, p_body text
) RETURNS TABLE (
  id text, post_id text, author_id text, body text, created_at timestamptz, deleted_at timestamptz,
  user_id text, username text, secret_key_hash text, display_name text, bio text,
  profile_image_id text, region text, account_type text, status text
) LANGUAGE plpgsql AS $$
#variable_conflict use_column
DECLARE
  v_author_id text;
  v_locked int;
BEGIN
  SELECT p.author_id INTO v_author_id
  FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.id = p_post_id AND p.deleted_at IS NULL;
  IF NOT FOUND THEN
    RAISE EXCEPTION USING ERRCODE = 'IR403', MESSAGE = 'interaction_forbidden';
  END IF;

  -- 自分の投稿へのコメントなら1人分だけロックする。退会済みでもロックは取る（従来どおり）。
  SELECT count(*) INTO v_locked FROM (
    SELECT u.id FROM users u
    WHERE u.id = ANY(ARRAY[p_actor_id, v_author_id])
    ORDER BY u.id COLLATE "C" FOR NO KEY UPDATE
  ) locked;
  IF (p_actor_id = v_author_id AND v_locked <> 1) OR (p_actor_id <> v_author_id AND v_locked <> 2) THEN
    RAISE EXCEPTION 'user % or % not found', p_actor_id, v_author_id;
  END IF;

  IF NOT (SELECT a.can_comment FROM isureal_post_access(p_actor_id, p_post_id) a) THEN
    RAISE EXCEPTION USING ERRCODE = 'IR403', MESSAGE = 'interaction_forbidden';
  END IF;

  INSERT INTO comments (id, post_id, author_id, body, created_at)
  VALUES (p_id, p_post_id, p_actor_id, p_body, clock_timestamp());

  RETURN QUERY
  SELECT c.id::text, c.post_id::text, c.author_id::text, c.body, c.created_at, c.deleted_at,
         u.id::text, u.username::text, u.secret_key_hash, u.display_name, u.bio,
         u.profile_image_id::text, u.region, u.account_type, u.status
  FROM comments c JOIN users u ON u.id = c.author_id AND u.status = 'active'
  WHERE c.id = p_id;
END
$$;

-- RealMoji の削除を1往復で行う。削除できるのは、付けた本人か投稿者。
CREATE OR REPLACE FUNCTION isureal_delete_realmoji(p_id text, p_actor_id text)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_reactor_id text;
  v_author_id text;
BEGIN
  PERFORM 1 FROM users u WHERE u.id = p_actor_id FOR NO KEY UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'user % not found', p_actor_id;
  END IF;

  SELECT r.reactor_id, p.author_id INTO v_reactor_id, v_author_id
  FROM realmojis r JOIN posts p ON p.id = r.post_id
  WHERE r.id = p_id AND r.deleted_at IS NULL FOR UPDATE OF r;
  IF NOT FOUND THEN
    RAISE EXCEPTION USING ERRCODE = 'IR404', MESSAGE = 'interaction_not_found';
  END IF;
  IF p_actor_id <> v_reactor_id AND p_actor_id <> v_author_id THEN
    RAISE EXCEPTION USING ERRCODE = 'IR403', MESSAGE = 'interaction_forbidden';
  END IF;

  UPDATE realmojis SET deleted_at = clock_timestamp() WHERE id = p_id;
END
$$;

-- コメントの削除を1往復で行う。削除できるのは、書いた本人か投稿者。
CREATE OR REPLACE FUNCTION isureal_delete_comment(p_id text, p_actor_id text)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_comment_author_id text;
  v_post_author_id text;
BEGIN
  PERFORM 1 FROM users u WHERE u.id = p_actor_id FOR NO KEY UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'user % not found', p_actor_id;
  END IF;

  SELECT c.author_id, p.author_id INTO v_comment_author_id, v_post_author_id
  FROM comments c JOIN posts p ON p.id = c.post_id
  WHERE c.id = p_id AND c.deleted_at IS NULL FOR UPDATE OF c;
  IF NOT FOUND THEN
    RAISE EXCEPTION USING ERRCODE = 'IR404', MESSAGE = 'interaction_not_found';
  END IF;
  IF p_actor_id <> v_comment_author_id AND p_actor_id <> v_post_author_id THEN
    RAISE EXCEPTION USING ERRCODE = 'IR403', MESSAGE = 'interaction_forbidden';
  END IF;

  UPDATE comments SET deleted_at = clock_timestamp() WHERE id = p_id;
END
$$;
