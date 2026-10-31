package isureal

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type createFriendRequestRequest struct {
	RecipientID string `json:"recipientId"`
}

type createFriendRequestResponse struct {
	RelationID string    `json:"relationId" db:"relation_id"`
	Status     string    `json:"status" db:"status"`
	UpdatedAt  time.Time `json:"updatedAt" db:"updated_at"`
}

func createFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createFriendRequestRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		tx, err := beginRelationshipTx(ctx, h.DB, actor, input.RecipientID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// ブロック・友達関係・保留中の申請の確認を1往復で行う（判定の順番は従来どおり）
		var state struct {
			Blocked            bool    `db:"blocked"`
			Friends            bool    `db:"friends"`
			PendingID          *string `db:"pending_id"`
			PendingRequesterID *string `db:"pending_requester_id"`
		}
		if err := tx.GetContext(ctx, &state, `
   SELECT
     EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1)) AS blocked,
     EXISTS(SELECT 1 FROM friendships WHERE user_low_id=$3 AND user_high_id=$4) AS friends,
     pending.id AS pending_id, pending.requester_id AS pending_requester_id
   FROM (SELECT 1) one
   LEFT JOIN LATERAL (
     SELECT id, requester_id FROM friend_requests WHERE status='pending' AND
      ((requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1))
     LIMIT 1
   ) pending ON true
  `, actor, input.RecipientID, min(actor, input.RecipientID), max(actor, input.RecipientID)); err != nil {
			return err
		}
		if state.Blocked || state.Friends {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		var response createFriendRequestResponse
		if state.PendingID != nil {
			if *state.PendingRequesterID == actor {
				return NewAPIError(http.StatusConflict, "relationship_conflict")
			}
			// 相手からの保留中の申請があれば、それを承認して友達になる
			response, err = updateFriendRequestStatus(ctx, tx, *state.PendingID, "accepted", true, actor, input.RecipientID)
			if err != nil {
				return err
			}
		} else {
			if err := tx.GetContext(ctx, &response, `
    INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at)
    VALUES ($1,$2,$3,'pending',clock_timestamp(),clock_timestamp())
    RETURNING id AS relation_id, status, updated_at
   `, uuid.NewString(), actor, input.RecipientID); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, response)
	}
}

type transitionFriendRequestRequest struct {
	RequestID string `param:"requestId" json:"-"`
	Action    string `param:"action" json:"-"`
}

type transitionFriendRequestResponse createFriendRequestResponse

func transitionFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
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
		tx, err := beginRelationshipTx(ctx, h.DB, request.RequesterID, request.RecipientID)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// ロック後の最新状態と、承認時に必要なブロック確認を1往復で取得する
		var current struct {
			FriendRequest
			Blocked bool `db:"blocked"`
		}
		if err := tx.GetContext(ctx, &current, `
   SELECT fr.id, fr.requester_id, fr.recipient_id, fr.status, fr.created_at, fr.updated_at,
     EXISTS(SELECT 1 FROM block_relations b
            WHERE (b.blocker_id=fr.requester_id AND b.blocked_id=fr.recipient_id)
               OR (b.blocker_id=fr.recipient_id AND b.blocked_id=fr.requester_id)) AS blocked
   FROM friend_requests fr WHERE fr.id=$1
  `, input.RequestID); err != nil {
			return err
		}
		if current.Status != "pending" {
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		if status == "accepted" && current.Blocked {
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		response, err := updateFriendRequestStatus(ctx, tx, current.ID, status, status == "accepted", current.RequesterID, current.RecipientID)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, transitionFriendRequestResponse(response))
	}
}
