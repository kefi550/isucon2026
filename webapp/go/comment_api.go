package isureal

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type createCommentRequest struct {
	PostID string `param:"postId" json:"-"`
	Body   string `json:"body"`
}

type createCommentResponse struct {
	ID        string                   `json:"id"`
	Author    relationshipUserResponse `json:"author"`
	Body      string                   `json:"body"`
	CreatedAt time.Time                `json:"createdAt"`
}

func newCommentResponse(comment Comment, user User) createCommentResponse {
	return createCommentResponse{ID: comment.ID, Author: newRelationshipUserResponse(user), Body: comment.Body, CreatedAt: comment.CreatedAt}
}

func createCommentHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createCommentRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if !validUserText(input.Body, 1, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_interaction")
		}
		// 投稿の確認から権限の確認、INSERT までを、DB 関数の1往復で行う（004-functions.sql）
		var comment CommentWithAuthor
		err := h.DB.GetContext(c.Request().Context(), &comment, `
   SELECT id, post_id, author_id, body, created_at, deleted_at,
          user_id AS "author.id", username AS "author.username", secret_key_hash AS "author.secret_key_hash",
          display_name AS "author.display_name", bio AS "author.bio", profile_image_id AS "author.profile_image_id",
          region AS "author.region", account_type AS "author.account_type", status AS "author.status"
   FROM isureal_create_comment($1, $2, $3, $4)
  `, uuid.NewString(), input.PostID, GetSession(c).UserID, input.Body)
		if err != nil {
			return dbFunctionError(err)
		}
		if err := comment.Author.setImageKey(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, newCommentResponse(comment.Comment, comment.Author))
	}
}

type getCommentsRequest struct {
	PostID         string     `param:"postId"`
	AfterCreatedAt *time.Time `query:"afterCreatedAt"`
	AfterID        string     `query:"afterId"`
}

type getCommentsResponse struct {
	Items   []createCommentResponse `json:"items"`
	HasMore bool                    `json:"hasMore"`
}

func getCommentsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getCommentsRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.AfterCreatedAt == nil) != (input.AfterID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		allowed, err := canComment(ctx, h.DB, GetSession(c).UserID, input.PostID)
		if err != nil {
			return err
		}
		if !allowed {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		comments, err := selectCommentsWithAuthor(ctx, h.DB, `AND c.post_id=$1 AND ($2::timestamptz IS NULL OR (c.created_at,c.id)>($2,$3)) ORDER BY c.created_at,c.id LIMIT 51`, input.PostID, input.AfterCreatedAt, input.AfterID)
		if err != nil {
			return err
		}
		items := make([]createCommentResponse, 0, len(comments))
		for _, comment := range comments {
			items = append(items, newCommentResponse(comment.Comment, comment.Author))
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, getCommentsResponse{Items: items, HasMore: hasMore})
	}
}

type deleteCommentRequest struct {
	CommentID string `param:"commentId" json:"-"`
}

func deleteCommentHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteCommentRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if _, err := h.DB.ExecContext(c.Request().Context(), `SELECT isureal_delete_comment($1, $2)`, input.CommentID, GetSession(c).UserID); err != nil {
			return dbFunctionError(err)
		}
		return c.NoContent(http.StatusNoContent)
	}
}
