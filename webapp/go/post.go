package isureal

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

type PostLocation struct {
	Kind      string   `json:"kind" db:"location_kind"`
	Latitude  *float64 `json:"latitude,omitempty" db:"latitude"`
	Longitude *float64 `json:"longitude,omitempty" db:"longitude"`
	Locality  *string  `json:"locality,omitempty" db:"locality"`
}

func (location PostLocation) valid() bool {
	switch location.Kind {
	case "none":
		return location.Latitude == nil && location.Longitude == nil && location.Locality == nil
	case "exact":
		return location.Latitude != nil && location.Longitude != nil && location.Locality == nil &&
			*location.Latitude >= -90 && *location.Latitude <= 90 && *location.Longitude >= -180 && *location.Longitude <= 180
	case "approximate":
		return location.Latitude == nil && location.Longitude == nil && location.Locality != nil && validUserText(*location.Locality, 1, 100)
	default:
		return false
	}
}

type Post struct {
	ID              string `db:"id"`
	AuthorID        string `db:"author_id"`
	PostingWindowID string `db:"posting_window_id"`
	ImageID         string `db:"image_id"`
	Caption         string `db:"caption"`
	Visibility      string `db:"visibility"`
	PostLocation
	PostedAt       time.Time  `db:"posted_at"`
	OnTimeDeadline time.Time  `db:"on_time_deadline"`
	WindowEndedAt  *time.Time `db:"window_ended_at"`
}

const postColumns = `p.id, p.author_id, p.posting_window_id, p.image_id, p.caption, p.visibility,
  p.location_kind, p.latitude, p.longitude, p.locality, p.posted_at,
  w.on_time_deadline, w.ended_at AS window_ended_at`

func getPost(ctx context.Context, db sqlx.QueryerContext, postID string) (Post, error) {
	var post Post
	err := sqlx.GetContext(ctx, db, &post, `
  SELECT `+postColumns+`
  FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.id = $1 AND p.deleted_at IS NULL
 `, postID)
	return post, err
}

type PostWithAuthor struct {
	Post
	Author User `db:"author"`
}

func selectPostsWithAuthor(ctx context.Context, db sqlx.QueryerContext, conditions string, args ...any) ([]PostWithAuthor, error) {
	var posts []PostWithAuthor
	if err := sqlx.SelectContext(ctx, db, &posts, `
  SELECT `+postColumns+`, `+userColumnsAs("u", "author")+`
  FROM posts p
  JOIN posting_windows w ON w.id = p.posting_window_id
  JOIN users u ON u.id = p.author_id AND u.status = 'active'
  WHERE p.deleted_at IS NULL `+conditions, args...); err != nil {
		return nil, err
	}
	for i := range posts {
		if err := posts[i].Author.setImageKey(); err != nil {
			return nil, err
		}
	}
	return posts, nil
}

func getPostWithAuthor(ctx context.Context, db sqlx.QueryerContext, postID string) (PostWithAuthor, error) {
	posts, err := selectPostsWithAuthor(ctx, db, `AND p.id = $1`, postID)
	if err != nil {
		return PostWithAuthor{}, err
	}
	if len(posts) == 0 {
		return PostWithAuthor{}, sql.ErrNoRows
	}
	return posts[0], nil
}

type viewerContext struct {
	User
	HasPostedToday   bool
	Friends          map[string]bool
	FriendsOfFriends map[string]bool
	MutualFriends    map[string]int
	Blocked          map[string]bool
}

// viewerUserColumns は users を別名 u で参照するときの列リスト（列名は User 構造体のタグと一致させる）
var viewerUserColumns = func() string {
	columns := make([]string, len(userColumns))
	for i, column := range userColumns {
		columns[i] = "u." + column
	}
	return strings.Join(columns, ", ")
}()

func loadViewerContext(ctx context.Context, db sqlx.QueryerContext, viewerID string) (viewerContext, error) {
	row, err := selectViewerRow(ctx, db, viewerID)
	if err != nil {
		return viewerContext{}, err
	}
	relations, err := selectViewerRelations(ctx, db, viewerID)
	if err != nil {
		return viewerContext{}, err
	}
	return buildViewerContext(row, relations)
}

type viewerRow struct {
	User
	HasPostedToday bool `db:"has_posted_today"`
}

// kind='b' は相手のユーザーID（first_id）、kind='f' は friendships の1行（low, high）を表す。
type viewerRelation struct {
	Kind     string `db:"kind"`
	FirstID  string `db:"first_id"`
	SecondID string `db:"second_id"`
}

// ユーザー情報と「今日投稿済みか」を1往復で取得する（リージョン間の往復回数を減らすため）
func selectViewerRow(ctx context.Context, db sqlx.QueryerContext, viewerID string) (viewerRow, error) {
	var row viewerRow
	err := sqlx.GetContext(ctx, db, &row, `
  SELECT `+viewerUserColumns+`,
    EXISTS(
      SELECT 1 FROM posts p
      JOIN posting_windows posted ON posted.id = p.posting_window_id
      JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                                  AND current.local_date = posted.local_date
      WHERE p.author_id = u.id AND p.deleted_at IS NULL
    ) AS has_posted_today
  FROM users u WHERE u.id = $1 AND u.status = 'active'
 `, viewerID)
	return row, err
}

// ブロック関係と、友達・友達の友達の算出に必要な友達関係を1往復で取得する。
// 友達の友達関係には viewer 自身との関係も必ず含まれるため、そこから友達一覧も復元できる。
// selectViewerDirectRelations は、ブロックと直接の友達だけを読む（友達の友達は読まない）。
// 友達フィードの確認（canView / matchesFeed）に必要なのは Friends と Blocked だけで、
// 友達の友達（selectViewerRelations の2ホップ目）は判定に使われない。
// buildViewerContext に渡すと FriendsOfFriends は「自分と友達」だけになるが、友達フィードでは参照されない。
func selectViewerDirectRelations(ctx context.Context, db sqlx.QueryerContext, viewerID string) ([]viewerRelation, error) {
	var relations []viewerRelation
	err := sqlx.SelectContext(ctx, db, &relations, `
  SELECT 'b' AS kind, CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END AS first_id, '' AS second_id
  FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
  UNION ALL
  SELECT 'f' AS kind, user_low_id AS first_id, user_high_id AS second_id
  FROM friendships WHERE user_low_id = $1 OR user_high_id = $1
 `, viewerID)
	return relations, err
}

// selectViewerBlockedRelations は、ブロックだけを読む。
// 公開フィードの確認（canView / matchesFeed）に必要なのは Blocked だけで、公開の投稿は友達かどうかに関係なく見える。
func selectViewerBlockedRelations(ctx context.Context, db sqlx.QueryerContext, viewerID string) ([]viewerRelation, error) {
	var relations []viewerRelation
	err := sqlx.SelectContext(ctx, db, &relations, `
  SELECT 'b' AS kind, CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END AS first_id, '' AS second_id
  FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
 `, viewerID)
	return relations, err
}

func selectViewerRelations(ctx context.Context, db sqlx.QueryerContext, viewerID string) ([]viewerRelation, error) {
	var relations []viewerRelation
	err := sqlx.SelectContext(ctx, db, &relations, `
  WITH friend_ids AS (
    SELECT CASE WHEN user_low_id = $1 THEN user_high_id ELSE user_low_id END AS id
    FROM friendships WHERE user_low_id = $1 OR user_high_id = $1
  )
  SELECT 'b' AS kind, CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END AS first_id, '' AS second_id
  FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
  UNION ALL
  SELECT 'f' AS kind, user_low_id AS first_id, user_high_id AS second_id
  FROM friendships
  -- IN (SELECT ...) だと全件走査の計画になるため、配列化してインデックス（low 側の UNIQUE と friendships_user_high_id）を使わせる
  WHERE user_low_id = ANY(ARRAY(SELECT id FROM friend_ids)) OR user_high_id = ANY(ARRAY(SELECT id FROM friend_ids))
 `, viewerID)
	return relations, err
}

func buildViewerContext(row viewerRow, relations []viewerRelation) (viewerContext, error) {
	viewer := viewerContext{Friends: map[string]bool{}, FriendsOfFriends: map[string]bool{}, MutualFriends: map[string]int{}, Blocked: map[string]bool{}}
	viewer.User = row.User
	viewer.HasPostedToday = row.HasPostedToday
	if err := viewer.User.setImageKey(); err != nil {
		return viewer, err
	}
	type friendshipPair struct{ UserLowID, UserHighID string }
	friendships := make([]friendshipPair, 0, len(relations))
	for _, relation := range relations {
		if relation.Kind == "b" {
			viewer.Blocked[relation.FirstID] = true
			continue
		}
		friendships = append(friendships, friendshipPair{UserLowID: relation.FirstID, UserHighID: relation.SecondID})
		if relation.FirstID == viewer.ID {
			viewer.Friends[relation.SecondID] = true
		} else if relation.SecondID == viewer.ID {
			viewer.Friends[relation.FirstID] = true
		}
	}
	for _, friendship := range friendships {
		if viewer.Friends[friendship.UserLowID] {
			viewer.FriendsOfFriends[friendship.UserHighID] = true
			if friendship.UserHighID != viewer.ID {
				viewer.MutualFriends[friendship.UserHighID]++
			}
		}
		if viewer.Friends[friendship.UserHighID] {
			viewer.FriendsOfFriends[friendship.UserLowID] = true
			if friendship.UserLowID != viewer.ID {
				viewer.MutualFriends[friendship.UserLowID]++
			}
		}
	}
	return viewer, nil
}

func (viewer viewerContext) canView(post Post) bool {
	if post.AuthorID == viewer.ID {
		return true
	}
	if post.WindowEndedAt != nil || !viewer.HasPostedToday || viewer.Blocked[post.AuthorID] {
		return false
	}
	switch post.Visibility {
	case "public":
		return true
	case "friends":
		return viewer.Friends[post.AuthorID]
	case "friends_of_friends":
		return viewer.Friends[post.AuthorID] || viewer.FriendsOfFriends[post.AuthorID]
	default:
		return false
	}
}

// postAndViewer は、投稿と閲覧者の情報を並列に取得した結果。エラーはそれぞれ別に持つ。
type postAndViewer struct {
	Post      PostWithAuthor
	PostErr   error
	Viewer    viewerContext
	ViewerErr error
}

// loadPostAndViewer は、投稿（getPostWithAuthor）と閲覧者の情報（loadViewerContext と同じ2本）を並列に取得する。
// 呼び出し側は、従来の逐次処理と同じ順番（PostErr → ViewerErr）でエラーを判定すること。
// errgroup を使わないのは、片方のエラー（ErrNoRows など）で他方が取り消され、従来とは違うエラーになるのを防ぐため。
func loadPostAndViewer(ctx context.Context, db *sqlx.DB, postID, viewerID string) postAndViewer {
	var (
		result    postAndViewer
		row       viewerRow
		rowErr    error
		relations []viewerRelation
		relErr    error
		wg        sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		result.Post, result.PostErr = getPostWithAuthor(ctx, db, postID)
	}()
	go func() {
		defer wg.Done()
		row, rowErr = selectViewerRow(ctx, db, viewerID)
	}()
	go func() {
		defer wg.Done()
		relations, relErr = selectViewerRelations(ctx, db, viewerID)
	}()
	wg.Wait()
	// loadViewerContext と同じ順番（行 → 関係 → 組み立て）でエラーを決める
	switch {
	case rowErr != nil:
		result.ViewerErr = rowErr
	case relErr != nil:
		result.ViewerErr = relErr
	default:
		result.Viewer, result.ViewerErr = buildViewerContext(row, relations)
	}
	return result
}

func canViewPost(ctx context.Context, db *sqlx.DB, viewerID, postID string) (bool, error) {
	loaded := loadPostAndViewer(ctx, db, postID, viewerID)
	if errors.Is(loaded.PostErr, sql.ErrNoRows) {
		return false, nil
	}
	if loaded.PostErr != nil {
		return false, loaded.PostErr
	}
	if errors.Is(loaded.ViewerErr, sql.ErrNoRows) {
		return false, nil
	}
	if loaded.ViewerErr != nil {
		return false, loaded.ViewerErr
	}
	return loaded.Viewer.canView(loaded.Post.Post), nil
}

func getFriendIDs(ctx context.Context, db sqlx.QueryerContext, userID string) ([]string, error) {
	var friendships []struct {
		UserLowID  string `db:"user_low_id"`
		UserHighID string `db:"user_high_id"`
	}
	if err := sqlx.SelectContext(ctx, db, &friendships, `SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id = $1 OR user_high_id = $1`, userID); err != nil {
		return nil, err
	}
	var ids []string
	for _, friendship := range friendships {
		if friendship.UserLowID == userID {
			ids = append(ids, friendship.UserHighID)
		} else {
			ids = append(ids, friendship.UserLowID)
		}
	}
	return ids, nil
}

func getBlockedUserIDs(ctx context.Context, db sqlx.QueryerContext, userID string) (map[string]bool, error) {
	var ids []string
	if err := sqlx.SelectContext(ctx, db, &ids, `
  SELECT CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END
  FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1
 `, userID); err != nil {
		return nil, err
	}
	blocked := make(map[string]bool, len(ids))
	for _, id := range ids {
		blocked[id] = true
	}
	return blocked, nil
}
