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
		viewer, err := loadViewerContext(ctx, h.DB, GetSession(c).UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return c.JSON(http.StatusOK, getFeedResponse{Items: []feedItemResponse{}})
		}
		if err != nil {
			return err
		}
		// 今日まだ投稿していない閲覧者は、自分以外の投稿を一切見られない（canView 参照）
		if !viewer.HasPostedToday {
			return c.JSON(http.StatusOK, getFeedResponse{Items: []feedItemResponse{}})
		}
		beforePostedAt, beforeID := input.BeforePostedAt, input.BeforeID
		if ranked {
			beforePostedAt, beforeID = nil, ""
		}
		posts, following, err := selectFeedCandidates(ctx, h.DB, viewer, input.Feed, beforePostedAt, beforeID)
		if err != nil {
			return err
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

// selectFeedCandidates は canView と matchesFeed の条件を SQL に展開して、フィード候補の投稿を取得する。
// 条件は canView / matchesFeed と完全に一致させること（広いと LIMIT 後に件数が欠け、狭いと投稿が漏れる）。
// 呼び出し側でも canView / matchesFeed で再確認し、権限のない投稿を返さないようにしている。
// following フィードでは、取得した投稿の作者（=フォロー中のユーザー）を following として返す。
func selectFeedCandidates(ctx context.Context, db sqlx.QueryerContext, viewer viewerContext, feed string, beforePostedAt *time.Time, beforeID string) ([]PostWithAuthor, map[string]bool, error) {
	friends := userIDList(viewer.Friends, nil)
	args := []any{viewer.ID, beforePostedAt, beforeID, userIDList(viewer.Blocked, nil)}
	var condition string
	switch feed {
	case "public":
		condition = `AND p.visibility = 'public'`
	case "friends":
		args = append(args, friends)
		condition = `AND p.author_id = ANY($5::text[]) AND p.visibility IN ('public', 'friends', 'friends_of_friends')`
	case "following":
		args = append(args, friends, userIDList(viewer.FriendsOfFriends, nil))
		condition = `AND p.author_id IN (SELECT followee_id FROM follows WHERE follower_id = $1)
   AND (p.visibility = 'public'
     OR (p.visibility IN ('friends', 'friends_of_friends') AND p.author_id = ANY($5::text[]))
     OR (p.visibility = 'friends_of_friends' AND p.author_id = ANY($6::text[])))`
	case "friends-of-friends":
		// 友達の友達のうち、友達本人と閲覧者自身を除いたユーザー
		args = append(args, userIDList(viewer.FriendsOfFriends, func(id string) bool { return !viewer.Friends[id] && id != viewer.ID }))
		condition = `AND p.author_id = ANY($5::text[]) AND p.visibility IN ('public', 'friends_of_friends')`
	default:
		return nil, nil, nil
	}
	limit := ` LIMIT 51`
	if feed == "friends-of-friends" {
		// スコア順に並べ替えるため候補を全件取る
		limit = ``
	}
	posts, err := selectPostsWithAuthor(ctx, db, `
   AND p.author_id <> $1 AND w.ended_at IS NULL
   AND NOT (p.author_id = ANY($4::text[]))
   AND ($2::timestamptz IS NULL OR (p.posted_at, p.id) < ($2, $3))
   `+condition+`
   ORDER BY p.posted_at DESC, p.id DESC`+limit, args...)
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

// userIDList は集合を SQL の配列パラメータ用のスライスに変換する。
// nil スライスは SQL の NULL になり ANY / NOT ANY の結果が壊れるため、必ず空でない（非 nil の）スライスを返す。
func userIDList(set map[string]bool, keep func(string) bool) []string {
	ids := make([]string, 0, len(set))
	for id, ok := range set {
		if ok && (keep == nil || keep(id)) {
			ids = append(ids, id)
		}
	}
	return ids
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

	query, args, err := sqlx.In(`SELECT user_low_id, user_high_id FROM friendships WHERE user_low_id IN (?) OR user_high_id IN (?)`, authorIDs, authorIDs)
	if err != nil {
		return nil, err
	}
	var friendships []struct {
		UserLowID  string `db:"user_low_id"`
		UserHighID string `db:"user_high_id"`
	}
	if err := sqlx.SelectContext(ctx, db, &friendships, sqlx.Rebind(sqlx.DOLLAR, query), args...); err != nil {
		return nil, err
	}
	connections := map[string]int64{}
	for _, friendship := range friendships {
		connections[friendship.UserLowID] += int64(viewer.MutualFriends[friendship.UserHighID])
		connections[friendship.UserHighID] += int64(viewer.MutualFriends[friendship.UserLowID])
	}

	query, args, err = sqlx.In(`
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
	var engagements []struct {
		PostID string `db:"post_id"`
		UserID string `db:"user_id"`
	}
	if err := sqlx.SelectContext(ctx, db, &engagements, sqlx.Rebind(sqlx.DOLLAR, query), args...); err != nil {
		return nil, err
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
