package isureal

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
	"golang.org/x/sync/errgroup"
)

type getFeedRequest struct {
	Feed           string     `param:"feed"`
	BeforeScore    *int64     `query:"beforeScore"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type feedItemResponse struct {
	createPostResponse
	Score *int64 `json:"score,omitempty"`
}

type getFeedResponse struct {
	Items   []feedItemResponse `json:"items"`
	HasMore bool               `json:"hasMore"`
}

func getFeedHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getFeedRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		switch input.Feed {
		case "friends", "friends-of-friends", "public", "following":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if (input.BeforePostedAt == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ranked := input.Feed == "friends-of-friends"
		if ranked && (input.BeforeScore == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		viewerID := GetSession(c).UserID
		beforePostedAt, beforeID := input.BeforePostedAt, input.BeforeID
		if ranked {
			beforePostedAt, beforeID = nil, ""
		}
		// 閲覧者の情報・関係・投稿候補は互いに依存しないので、並列に投げて往復1回分の待ちにする。
		// 投稿候補の SQL が可視性の条件をすべて見ているので、ここで読む関係は、その結果を確認し直すための最小限でよい:
		// 公開フィードはブロックだけ、友達フィードは直接の友達とブロックだけ（友達の友達は読まない）。
		selectRelations := selectViewerRelations
		switch input.Feed {
		case "public":
			selectRelations = selectViewerBlockedRelations
		case "friends":
			selectRelations = selectViewerDirectRelations
		}
		var (
			row       viewerRow
			relations []viewerRelation
			posts     []PostWithAuthor
			following map[string]bool
		)
		g, gctx := errgroup.WithContext(ctx)
		g.Go(func() (err error) {
			row, err = selectViewerRow(gctx, h.DB, viewerID)
			return err
		})
		g.Go(func() (err error) {
			relations, err = selectRelations(gctx, h.DB, viewerID)
			return err
		})
		g.Go(func() (err error) {
			posts, following, err = selectFeedCandidates(gctx, h.DB, viewerID, input.Feed, beforePostedAt, beforeID)
			return err
		})
		err := g.Wait()
		if errors.Is(err, sql.ErrNoRows) {
			return c.JSON(http.StatusOK, getFeedResponse{Items: []feedItemResponse{}})
		}
		if err != nil {
			return err
		}
		viewer, err := buildViewerContext(row, relations)
		if err != nil {
			return err
		}
		// 今日まだ投稿していない閲覧者は、自分以外の投稿を一切見られない（canView 参照）
		if !viewer.HasPostedToday {
			return c.JSON(http.StatusOK, getFeedResponse{Items: []feedItemResponse{}})
		}
		candidates := make([]PostWithAuthor, 0)
		for _, post := range posts {
			if viewer.canView(post.Post) && matchesFeed(viewer, following, post.Post, input.Feed) {
				candidates = append(candidates, post)
			}
		}
		var scores map[string]int64
		if ranked {
			if scores, err = recommendationScores(ctx, h.DB, viewer, candidates); err != nil {
				return err
			}
			sort.SliceStable(candidates, func(i, j int) bool {
				return scores[candidates[i].ID] > scores[candidates[j].ID]
			})
		}
		items := make([]feedItemResponse, 0)
		for _, post := range candidates {
			score := scores[post.ID]
			if ranked && input.BeforeScore != nil && !rankedBefore(score, post.Post, *input.BeforeScore, *input.BeforePostedAt, input.BeforeID) {
				continue
			}
			item := feedItemResponse{createPostResponse: newPostResponse(post.Post, post.Author)}
			if ranked {
				item.Score = &score
			}
			items = append(items, item)
			if len(items) == 51 {
				break
			}
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, getFeedResponse{Items: items, HasMore: hasMore})
	}
}

// フィード候補の SQL で使う、閲覧者（$1）の友達・ブロック相手・友達の友達の ID 配列。
// 定義は loadViewerContext の Friends / Blocked / FriendsOfFriends と一致させること。
// IN (SELECT ...) だと全件走査の計画になるため、配列化してインデックスを使わせる。
const (
	feedFriendIDs = `ARRAY(SELECT CASE WHEN f.user_low_id = $1 THEN f.user_high_id ELSE f.user_low_id END
     FROM friendships f WHERE f.user_low_id = $1 OR f.user_high_id = $1)`
	feedBlockedIDs = `ARRAY(SELECT CASE WHEN b.blocker_id = $1 THEN b.blocked_id ELSE b.blocker_id END
     FROM block_relations b WHERE b.blocker_id = $1 OR b.blocked_id = $1)`
	feedFriendsOfFriendsIDs = `ARRAY(
     SELECT f.user_high_id FROM friendships f WHERE f.user_low_id = ANY(` + feedFriendIDs + `)
     UNION
     SELECT f.user_low_id FROM friendships f WHERE f.user_high_id = ANY(` + feedFriendIDs + `))`
)

// selectFeedCandidates は canView と matchesFeed の条件を SQL に展開して、フィード候補の投稿を取得する。
// 条件は canView / matchesFeed と完全に一致させること（広いと LIMIT 後に件数が欠け、狭いと投稿が漏れる）。
// 閲覧者の関係も SQL の中で求めるので、loadViewerContext の結果を待たずに並列で実行できる。
// 呼び出し側でも canView / matchesFeed で再確認し、権限のない投稿を返さないようにしている。
// following フィードでは、取得した投稿の作者（=フォロー中のユーザー）を following として返す。
func selectFeedCandidates(ctx context.Context, db sqlx.QueryerContext, viewerID string, feed string, beforePostedAt *time.Time, beforeID string) ([]PostWithAuthor, map[string]bool, error) {
	var condition string
	switch feed {
	case "public":
		condition = `AND p.visibility = 'public'`
	case "friends":
		condition = `AND p.author_id = ANY(` + feedFriendIDs + `) AND p.visibility IN ('public', 'friends', 'friends_of_friends')`
	case "following":
		condition = `AND p.author_id IN (SELECT followee_id FROM follows WHERE follower_id = $1)
   AND (p.visibility = 'public'
     OR (p.visibility IN ('friends', 'friends_of_friends') AND p.author_id = ANY(` + feedFriendIDs + `))
     OR (p.visibility = 'friends_of_friends' AND p.author_id = ANY(` + feedFriendsOfFriendsIDs + `)))`
	case "friends-of-friends":
		// 友達の友達のうち、友達本人と閲覧者自身を除いたユーザー（閲覧者自身は共通条件で除く）
		condition = `AND p.author_id = ANY(` + feedFriendsOfFriendsIDs + `)
   AND NOT (p.author_id = ANY(` + feedFriendIDs + `))
   AND p.visibility IN ('public', 'friends_of_friends')`
	default:
		return nil, nil, nil
	}
	limit := ` LIMIT 51`
	if feed == "friends-of-friends" {
		// スコア順に並べ替えるため候補を全件取る
		limit = ``
	}
	// 退会済み、または今日まだ投稿していない閲覧者には何も返さない（呼び出し側は空のフィードを返す）
	posts, err := selectPostsWithAuthor(ctx, db, `
   AND EXISTS (
     SELECT 1 FROM users vu WHERE vu.id = $1 AND vu.status = 'active'
       AND EXISTS (
         SELECT 1 FROM posts vp
         JOIN posting_windows posted ON posted.id = vp.posting_window_id
         JOIN posting_windows cur ON cur.region = vu.region AND cur.ended_at IS NULL
                                 AND cur.local_date = posted.local_date
         WHERE vp.author_id = vu.id AND vp.deleted_at IS NULL
       )
   )
   AND p.author_id <> $1 AND w.ended_at IS NULL
   AND NOT (p.author_id = ANY(`+feedBlockedIDs+`))
   AND ($2::timestamptz IS NULL OR (p.posted_at, p.id) < ($2, $3))
   `+condition+`
   ORDER BY p.posted_at DESC, p.id DESC`+limit, viewerID, beforePostedAt, beforeID)
	if err != nil {
		return nil, nil, err
	}
	following := map[string]bool{}
	if feed == "following" {
		for _, post := range posts {
			following[post.AuthorID] = true
		}
	}
	return posts, following, nil
}

func rankedBefore(score int64, post Post, beforeScore int64, beforePostedAt time.Time, beforeID string) bool {
	if score != beforeScore {
		return score < beforeScore
	}
	if !post.PostedAt.Equal(beforePostedAt) {
		return post.PostedAt.Before(beforePostedAt)
	}
	return post.ID < beforeID
}

func recommendationScores(ctx context.Context, db sqlx.QueryerContext, viewer viewerContext, posts []PostWithAuthor) (map[string]int64, error) {
	scores := make(map[string]int64, len(posts))
	if len(posts) == 0 {
		return scores, nil
	}
	authorIDs := make([]string, 0, len(posts))
	postIDs := make([]string, 0, len(posts))
	for _, post := range posts {
		authorIDs = append(authorIDs, post.AuthorID)
		postIDs = append(postIDs, post.ID)
	}

	friendshipsQuery, friendshipsArgs, err := sqlx.In(`SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id IN (?) OR user_high_id IN (?)`, authorIDs, authorIDs)
	if err != nil {
		return nil, err
	}
	engagementsQuery, engagementsArgs, err := sqlx.In(`
  SELECT r.post_id, r.reactor_id AS user_id FROM realmojis r
  JOIN users u ON u.id = r.reactor_id AND u.status = 'active'
  WHERE r.post_id IN (?) AND r.replaced_at IS NULL AND r.deleted_at IS NULL
  UNION
  SELECT c.post_id, c.author_id AS user_id FROM comments c
  JOIN users u ON u.id = c.author_id AND u.status = 'active'
  WHERE c.post_id IN (?) AND c.deleted_at IS NULL
 `, postIDs, postIDs)
	if err != nil {
		return nil, err
	}
	var friendships []struct {
		UserLowID  string `db:"user_low_id"`
		UserHighID string `db:"user_high_id"`
	}
	var engagements []struct {
		PostID string `db:"post_id"`
		UserID string `db:"user_id"`
	}
	// 2本は互いに依存しないので並列に投げる（db はトランザクションではなく接続プールであること）
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return sqlx.SelectContext(gctx, db, &friendships, sqlx.Rebind(sqlx.DOLLAR, friendshipsQuery), friendshipsArgs...)
	})
	g.Go(func() error {
		return sqlx.SelectContext(gctx, db, &engagements, sqlx.Rebind(sqlx.DOLLAR, engagementsQuery), engagementsArgs...)
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	connections := map[string]int64{}
	for _, friendship := range friendships {
		connections[friendship.UserLowID] += int64(viewer.MutualFriends[friendship.UserHighID])
		connections[friendship.UserHighID] += int64(viewer.MutualFriends[friendship.UserLowID])
	}

	friendEngagements := map[string]int64{}
	for _, engagement := range engagements {
		if viewer.Friends[engagement.UserID] {
			friendEngagements[engagement.PostID]++
		}
	}

	for _, post := range posts {
		scores[post.ID] = 10*friendEngagements[post.ID] + 3*int64(viewer.MutualFriends[post.AuthorID]) + min(connections[post.AuthorID], 30)
	}
	return scores, nil
}

func matchesFeed(viewer viewerContext, following map[string]bool, post Post, feed string) bool {
	switch feed {
	case "public":
		return post.Visibility == "public"
	case "following":
		return following[post.AuthorID]
	case "friends":
		return viewer.Friends[post.AuthorID]
	case "friends-of-friends":
		return !viewer.Friends[post.AuthorID] && viewer.FriendsOfFriends[post.AuthorID]
	default:
		return false
	}
}
