package isureal

// このファイルは、友達関係 API の往復削減の前後で結果が変わらないことを確かめるための参照実装（修正前のコードそのまま）。
// 関数名だけを ref 接頭辞に置き換えている。

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

func refBeginRelationshipTx(ctx context.Context, db *sqlx.DB, actor, target string) (*sqlx.Tx, error) {
	if actor == target || target == "" {
		return nil, NewAPIError(http.StatusBadRequest, "invalid_relationship")
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, id := range []string{min(actor, target), max(actor, target)} {
		var lockedID string
		if err := tx.GetContext(ctx, &lockedID, `SELECT id FROM users WHERE id=$1 AND status='active' FOR NO KEY UPDATE`, id); err != nil {
			tx.Rollback()
			if errors.Is(err, sql.ErrNoRows) {
				return nil, NewAPIError(http.StatusNotFound, "relationship_not_found")
			}
			return nil, err
		}
	}
	return tx, nil
}

func refCreateFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createFriendRequestRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := refBeginRelationshipTx(ctx, h.DB, actor, input.RecipientID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		blocked, err := isBlocked(ctx, tx, actor, input.RecipientID)
		if err != nil {
			return err
		}
		if blocked {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		friends, err := getFriendIDs(ctx, tx, actor)
		if err != nil {
			return err
		}
		for _, id := range friends {
			if id == input.RecipientID {
				return NewAPIError(http.StatusConflict, "relationship_conflict")
			}
		}
		var existingID string
		err = tx.GetContext(ctx, &existingID, `
   SELECT id FROM friend_requests WHERE status='pending' AND
    ((requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1))
  `, actor, input.RecipientID)
		var requestID string
		if err == nil {
			request, err := getFriendRequest(ctx, tx, existingID)
			if err != nil {
				return err
			}
			if request.RequesterID == actor {
				return NewAPIError(http.StatusConflict, "relationship_conflict")
			}
			if _, err := tx.ExecContext(ctx, `UPDATE friend_requests SET status='accepted', updated_at=clock_timestamp() WHERE id=$1`, request.ID); err != nil {
				return err
			}
			if err := insertFriendship(ctx, tx, request); err != nil {
				return err
			}
			requestID = request.ID
		} else if errors.Is(err, sql.ErrNoRows) {
			requestID = uuid.NewString()
			if _, err := tx.ExecContext(ctx, `INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at) VALUES ($1,$2,$3,'pending',clock_timestamp(),clock_timestamp())`, requestID, actor, input.RecipientID); err != nil {
				return err
			}
		} else {
			return err
		}
		request, err := getFriendRequest(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, createFriendRequestResponse{RelationID: request.ID, Status: request.Status, UpdatedAt: request.UpdatedAt})
	}
}

func refTransitionFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input transitionFriendRequestRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		var status string
		switch input.Action {
		case "accept":
			status = "accepted"
		case "reject":
			status = "rejected"
		case "cancel":
			status = "cancelled"
		default:
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		request, err := getFriendRequest(ctx, h.DB, input.RequestID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err != nil {
			return err
		}
		if (status == "cancelled" && actor != request.RequesterID) || (status != "cancelled" && actor != request.RecipientID) {
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		tx, err := refBeginRelationshipTx(ctx, h.DB, request.RequesterID, request.RecipientID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		request, err = getFriendRequest(ctx, tx, input.RequestID)
		if err != nil {
			return err
		}
		if request.Status != "pending" {
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		if status == "accepted" {
			blocked, err := isBlocked(ctx, tx, request.RequesterID, request.RecipientID)
			if err != nil {
				return err
			}
			if blocked {
				return NewAPIError(http.StatusConflict, "relationship_conflict")
			}
			if err := insertFriendship(ctx, tx, request); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE friend_requests SET status=$2, updated_at=clock_timestamp() WHERE id=$1`, request.ID, status); err != nil {
			return err
		}
		request, err = getFriendRequest(ctx, tx, request.ID)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, transitionFriendRequestResponse{RelationID: request.ID, Status: request.Status, UpdatedAt: request.UpdatedAt})
	}
}

func refDeleteFriendshipHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteFriendshipRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := refBeginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(ctx, `DELETE FROM friendships WHERE user_low_id=$1 AND user_high_id=$2`, min(actor, input.UserID), max(actor, input.UserID))
		if err := requireRelationshipChange(result, err, http.StatusConflict, "relationship_conflict"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

func refFollowHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input followRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := refBeginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		target, err := getUser(ctx, tx, input.UserID)
		if err != nil {
			return err
		}
		if target.AccountType != "certified_public" {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		blocked, err := isBlocked(ctx, tx, actor, target.ID)
		if err != nil {
			return err
		}
		if blocked {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		var exists bool
		if err := tx.GetContext(ctx, &exists, `SELECT EXISTS(SELECT 1 FROM follows WHERE follower_id=$1 AND followee_id=$2)`, actor, target.ID); err != nil {
			return err
		}
		if exists {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO follows (id, follower_id, followee_id) VALUES ($1,$2,$3)`, uuid.NewString(), actor, target.ID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

func refUnfollowHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input unfollowRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := refBeginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(ctx, `DELETE FROM follows WHERE follower_id=$1 AND followee_id=$2`, actor, input.UserID)
		if err := requireRelationshipChange(result, err, http.StatusConflict, "relationship_conflict"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

func refBlockHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input blockRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := refBeginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var exists bool
		if err := tx.GetContext(ctx, &exists, `SELECT EXISTS(SELECT 1 FROM block_relations WHERE blocker_id=$1 AND blocked_id=$2)`, actor, input.UserID); err != nil {
			return err
		}
		if exists {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO block_relations (id, blocker_id, blocked_id) VALUES ($1,$2,$3)`, uuid.NewString(), actor, input.UserID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM friendships WHERE user_low_id=$1 AND user_high_id=$2`, min(actor, input.UserID), max(actor, input.UserID)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM follows WHERE (follower_id=$1 AND followee_id=$2) OR (follower_id=$2 AND followee_id=$1)`, actor, input.UserID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE friend_requests SET status='cancelled', updated_at=clock_timestamp() WHERE status='pending' AND ((requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1))`, actor, input.UserID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

func refUnblockHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input unblockRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := refBeginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(ctx, `DELETE FROM block_relations WHERE blocker_id=$1 AND blocked_id=$2`, actor, input.UserID)
		if err := requireRelationshipChange(result, err, http.StatusForbidden, "relationship_action_forbidden"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

// insertFriendship は修正前の友達関係 INSERT（本体からは不要になったので参照実装側へ移した）
func insertFriendship(ctx context.Context, tx *sqlx.Tx, request FriendRequest) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id) VALUES ($1,$2,$3,$4)`, uuid.NewString(), min(request.RequesterID, request.RecipientID), max(request.RequesterID, request.RecipientID), request.ID)
	return err
}
