package isureal

import (
	"context"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
	"golang.org/x/sync/errgroup"
)

type getMemoriesRequest struct {
	Month          string     `query:"month"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type memoryPostResponse struct {
	createPostResponse
	RealMojis []replaceRealMojiResponse `json:"realmojis"`
	Comments  []createCommentResponse   `json:"comments"`
}

type getMemoriesResponse struct {
	Items   []memoryPostResponse `json:"items"`
	HasMore bool                 `json:"hasMore"`
}

type getMemoriesByDateRequest struct {
	Date           string     `param:"date"`
	BeforePostedAt *time.Time `query:"beforePostedAt"`
	BeforeID       string     `query:"beforeId"`
}

type getMemoriesByDateResponse getMemoriesResponse

func getMemoriesHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getMemoriesRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		start, err := time.Parse("2006-01", input.Month)
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_memory_period")
		}
		response, err := listMemories(c.Request().Context(), h.DB, GetSession(c).UserID, start, start.AddDate(0, 1, 0), input.BeforePostedAt, input.BeforeID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, response)
	}
}

func getMemoriesByDateHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getMemoriesByDateRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		start, err := time.Parse(time.DateOnly, input.Date)
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_memory_period")
		}
		response, err := listMemories(c.Request().Context(), h.DB, GetSession(c).UserID, start, start.AddDate(0, 0, 1), input.BeforePostedAt, input.BeforeID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, getMemoriesByDateResponse(response))
	}
}

// memoryPostIDs は、Memories で返す先頭50件の投稿 ID を求めるサブクエリ。
// listMemories の投稿クエリ（selectPostsWithAuthor）と同じ条件・同じ並び順にすること。
// 引数は $1=userID, $2=開始日, $3=終了日, $4=カーソル時刻, $5=カーソルID。
const memoryPostIDs = `SELECT p.id FROM posts p
    JOIN posting_windows w ON w.id = p.posting_window_id
    JOIN users u ON u.id = p.author_id AND u.status = 'active'
    WHERE p.deleted_at IS NULL AND p.author_id = $1 AND w.local_date >= $2 AND w.local_date < $3
      AND ($4::timestamptz IS NULL OR (p.posted_at, p.id) < ($4, $5))
    ORDER BY p.posted_at DESC, p.id DESC LIMIT 50`

func listMemories(ctx context.Context, db sqlx.QueryerContext, userID string, start, end time.Time, beforeAt *time.Time, beforeID string) (getMemoriesResponse, error) {
	var response getMemoriesResponse
	if (beforeAt == nil) != (beforeID == "") {
		return response, NewAPIError(http.StatusBadRequest, "invalid_pagination")
	}
	// 投稿・RealMoji・コメントは互いの結果を待たなくてよいので（対象の投稿 ID はサブクエリで求める）、
	// 3本を並列に投げて往復1回分の待ちにする
	args := []any{userID, start, end, beforeAt, beforeID}
	var (
		posts     []PostWithAuthor
		reactions []RealMojiWithReactor
		comments  []CommentWithAuthor
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		posts, err = selectPostsWithAuthor(gctx, db, `
  AND p.author_id = $1 AND w.local_date >= $2 AND w.local_date < $3
  AND ($4::timestamptz IS NULL OR (p.posted_at, p.id) < ($4, $5))
  ORDER BY p.posted_at DESC, p.id DESC LIMIT 51
 `, args...)
		return err
	})
	g.Go(func() (err error) {
		reactions, err = selectCurrentRealMojis(gctx, db, `AND r.post_id IN (`+memoryPostIDs+`) ORDER BY r.created_at DESC, r.id DESC`, args...)
		return err
	})
	g.Go(func() (err error) {
		comments, err = selectCommentsWithAuthor(gctx, db, `AND c.post_id IN (`+memoryPostIDs+`) ORDER BY c.created_at, c.id`, args...)
		return err
	})
	if err := g.Wait(); err != nil {
		return response, err
	}

	items := make([]memoryPostResponse, 0, len(posts))
	for _, post := range posts {
		items = append(items, memoryPostResponse{
			createPostResponse: newPostResponse(post.Post, post.Author),
			RealMojis:          make([]replaceRealMojiResponse, 0),
			Comments:           make([]createCommentResponse, 0),
		})
	}
	response.HasMore = len(items) > 50
	if response.HasMore {
		items = items[:50]
	}
	itemByPostID := make(map[string]*memoryPostResponse, len(items))
	for i := range items {
		itemByPostID[items[i].ID] = &items[i]
	}
	// 並列に読んでいるので、ごくまれに投稿クエリに無い投稿の行が混ざりうる。その行は捨てる。
	for _, reaction := range reactions {
		if item := itemByPostID[reaction.PostID]; item != nil && len(item.RealMojis) < 50 {
			item.RealMojis = append(item.RealMojis, newRealMojiResponse(reaction.RealMoji, reaction.Reactor))
		}
	}
	for _, comment := range comments {
		if item := itemByPostID[comment.PostID]; item != nil && len(item.Comments) < 50 {
			item.Comments = append(item.Comments, newCommentResponse(comment.Comment, comment.Author))
		}
	}
	response.Items = items
	return response, nil
}
