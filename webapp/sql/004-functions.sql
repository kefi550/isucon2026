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

  SELECT count(*) FILTER (WHERE p.deleted_at IS NULL), count(*) FILTER (WHERE p.deleted_at IS NOT NULL)
  INTO v_active, v_deleted
  FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.author_id = p_user_id AND w.local_date = v_local_date;
  IF v_active <> 0 OR v_deleted >= 2 OR p_posted_at < v_starts_at THEN
    RAISE EXCEPTION USING ERRCODE = 'IR409', MESSAGE = 'post_slot_unavailable';
  END IF;

  INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility,
                     location_kind, latitude, longitude, locality, posted_at)
  VALUES (p_post_id, p_user_id, v_window_id, p_image_id, p_caption, p_visibility,
          p_location_kind, p_latitude, p_longitude, p_locality, p_posted_at);

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
