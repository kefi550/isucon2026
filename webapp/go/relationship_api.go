package isureal

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type deleteFriendshipRequest struct {
	UserID string `param:"userId" json:"-"`
}

func deleteFriendshipHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteFriendshipRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.UserID)
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

type followRequest struct {
	UserID string `param:"userId" json:"-"`
}

func followHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input followRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// 「認定公開アカウントでない」「ブロック関係がある」「フォロー済み」はどれも同じ 409 なので、
		// 条件付き INSERT の1往復にまとめ、追加されなければ 409 を返す
		result, err := tx.ExecContext(ctx, `
   INSERT INTO follows (id, follower_id, followee_id)
   SELECT $1::text, $2::text, $3::text
   WHERE EXISTS(SELECT 1 FROM users WHERE id=$3::text AND status='active' AND account_type='certified_public')
     AND NOT EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$2::text AND blocked_id=$3::text) OR (blocker_id=$3::text AND blocked_id=$2::text))
     AND NOT EXISTS(SELECT 1 FROM follows WHERE follower_id=$2::text AND followee_id=$3::text)
  `, uuid.NewString(), actor, input.UserID)
		if err := requireRelationshipChange(result, err, http.StatusConflict, "relationship_conflict"); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

type unfollowRequest struct {
	UserID string `param:"userId" json:"-"`
}

func unfollowHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input unfollowRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.UserID)
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

type blockRequest struct {
	UserID string `param:"userId" json:"-"`
}

func blockHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input blockRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.UserID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// ブロックの追加と後片付け（友達関係・フォローの削除、保留中の申請の取り消し）を1往復で行う。
		// すでにブロック済みなら追加件数が0になり 409 を返す（後片付けはロールバックで取り消される）。
		var inserted int
		if err := tx.GetContext(ctx, &inserted, `
   WITH inserted AS (
     INSERT INTO block_relations (id, blocker_id, blocked_id)
     SELECT $1::text, $2::text, $3::text
     WHERE NOT EXISTS(SELECT 1 FROM block_relations WHERE blocker_id=$2::text AND blocked_id=$3::text)
     RETURNING 1
   ), deleted_friendships AS (
     DELETE FROM friendships WHERE user_low_id=$4::text AND user_high_id=$5::text
   ), deleted_follows AS (
     DELETE FROM follows WHERE (follower_id=$2::text AND followee_id=$3::text) OR (follower_id=$3::text AND followee_id=$2::text)
   ), cancelled_requests AS (
     UPDATE friend_requests SET status='cancelled', updated_at=clock_timestamp()
     WHERE status='pending' AND ((requester_id=$2::text AND recipient_id=$3::text) OR (requester_id=$3::text AND recipient_id=$2::text))
   )
   SELECT count(*) FROM inserted
  `, uuid.NewString(), actor, input.UserID, min(actor, input.UserID), max(actor, input.UserID)); err != nil {
			return err
		}
		if inserted == 0 {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

type unblockRequest struct {
	UserID string `param:"userId" json:"-"`
}

func unblockHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input unblockRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.UserID)
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
