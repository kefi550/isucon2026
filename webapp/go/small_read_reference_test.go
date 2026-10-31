package isureal

// このファイルは、検索・プロフィール・posts/me/current・ログアウトの往復削減の前後で
// 結果が変わらないことを確かめるための参照実装（修正前のコードそのまま）。関数名だけを ref 接頭辞に置き換えている。

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

func refSearchUsersHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input searchUsersRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if !validUserText(input.Q, 1, 100) {
			return NewAPIError(http.StatusBadRequest, "invalid_relationship")
		}
		if (input.BeforeUsername == "") != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		var users []User
		if err := h.DB.SelectContext(ctx, &users, `SELECT `+strings.Join(userColumns, ", ")+` FROM users WHERE status='active' AND id<>$1 AND ($2='' OR (username,id)>($2,$3)) ORDER BY username, id`, viewer, input.BeforeUsername, input.BeforeID); err != nil {
			return err
		}
		blocked, err := getBlockedUserIDs(ctx, h.DB, viewer)
		if err != nil {
			return err
		}
		query := normalizeSearchText(input.Q)
		items := make([]relationshipUserResponse, 0)
		for _, user := range users {
			if !strings.Contains(normalizeSearchText(user.Username), query) && !strings.Contains(normalizeSearchText(user.DisplayName), query) {
				continue
			}
			if blocked[user.ID] {
				continue
			}
			if err := user.setImageKey(); err != nil {
				return err
			}
			if user.AccountType != "certified_public" {
				user.Bio = ""
			}
			items = append(items, newRelationshipUserResponse(user))
			if len(items) == 51 {
				break
			}
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, searchUsersResponse{Items: items, HasMore: hasMore})
	}
}
func refGetUserHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getUserRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		user, err := getUser(ctx, h.DB, input.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err != nil {
			return err
		}
		blocked, err := isBlocked(ctx, h.DB, viewer, user.ID)
		if err != nil {
			return err
		}
		if blocked {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		friends, err := getFriendIDs(ctx, h.DB, viewer)
		if err != nil {
			return err
		}
		if user.AccountType != "certified_public" && !slices.Contains(friends, user.ID) {
			user.Bio = ""
		}
		return c.JSON(http.StatusOK, getUserResponse(newRelationshipUserResponse(user)))
	}
}

func refGetMyCurrentPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		var postID string
		err := h.DB.GetContext(ctx, &postID, `
   SELECT p.id FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   JOIN users u ON u.id = p.author_id
   JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                              AND current.local_date = w.local_date
   WHERE p.author_id = $1 AND p.deleted_at IS NULL
  `, userID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		post, err := getPost(ctx, h.DB, postID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		user, err := getUser(ctx, h.DB, userID)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, getMyCurrentPostResponse(newPostResponse(post, user)))
	}
}

func refInvalidateLoginSession(db *sqlx.DB, userID string, tokenHash string) error {
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	var lockedUserID string
	if err := tx.Get(&lockedUserID, `SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE`, userID); err != nil {
		return fmt.Errorf("failed to lock user: %w", err)
	}

	if _, err := tx.Exec(`
		UPDATE login_sessions
		SET active = false, invalidated_at = clock_timestamp(), reason = 'logout'
		WHERE user_id = $1 AND active = true AND token_hash = $2	
	`, userID, tokenHash); err != nil {
		return fmt.Errorf("failed to invalidate sessions: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}
