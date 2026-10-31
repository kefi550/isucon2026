package isureal

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// createFriendRequestSQL は、ロックを取った後の判定と書き込みを1文で行う（判定の順番は従来どおり）。
// ロック（lockRelationshipUsersSQL）とは別の文として送るので、ロックを待っているあいだにコミットされた内容も見える。
//
//	$1 = 2人の ID の配列、$2 = 申請する人、$3 = 相手、$4/$5 = 友達関係の low/high（バイト順）、
//	$6 = 友達関係の ID、$7 = 申請の ID
//
// action は、not_found（どちらかが存在しない・退会済み）／conflict（ブロック・友達・自分が申請済み）／
// accept（相手からの保留中の申請を承認して友達になった）／create（新しく申請した）。
const createFriendRequestSQL = `
  WITH locked AS (
    SELECT count(*) AS n FROM users WHERE id = ANY($1::text[]) AND status = 'active'
  ), state AS (
    SELECT
      (SELECT n FROM locked) = 2 AS users_ok,
      EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$2::text AND blocked_id=$3::text) OR (blocker_id=$3::text AND blocked_id=$2::text)) AS blocked,
      EXISTS(SELECT 1 FROM friendships WHERE user_low_id=$4::text AND user_high_id=$5::text) AS friends,
      pending.id AS pending_id, pending.requester_id AS pending_requester_id
    FROM (SELECT 1) one
    LEFT JOIN LATERAL (
      SELECT id, requester_id FROM friend_requests WHERE status='pending' AND
       ((requester_id=$2::text AND recipient_id=$3::text) OR (requester_id=$3::text AND recipient_id=$2::text))
      LIMIT 1
    ) pending ON true
  ), plan AS (
    SELECT pending_id, CASE
      WHEN NOT users_ok THEN 'not_found'
      WHEN blocked OR friends THEN 'conflict'
      WHEN pending_id IS NOT NULL AND pending_requester_id = $2::text THEN 'conflict'
      WHEN pending_id IS NOT NULL THEN 'accept'
      ELSE 'create' END AS action
    FROM state
  ), accepted AS (
    UPDATE friend_requests SET status='accepted', updated_at=clock_timestamp()
    WHERE id = (SELECT pending_id FROM plan WHERE action='accept')
    RETURNING id, status, updated_at
  ), friendship AS (
    INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id)
    SELECT $6::text, $4::text, $5::text, id FROM accepted
  ), created AS (
    INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at)
    SELECT $7::text, $2::text, $3::text, 'pending', clock_timestamp(), clock_timestamp() FROM plan WHERE action='create'
    RETURNING id, status, updated_at
  )
  SELECT plan.action, r.id AS relation_id, r.status, r.updated_at
  FROM plan LEFT JOIN (SELECT * FROM accepted UNION ALL SELECT * FROM created) r ON true
`

// lockRelationshipUsersSQL は2人分の行ロックを id の昇順で取る（lockRelationshipUsers と同じ）。バッチの先頭に置く。
const lockRelationshipUsersSQL = `SELECT id FROM users WHERE id = ANY($1::text[]) AND status='active' ORDER BY id COLLATE "C" FOR NO KEY UPDATE`

func createFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createFriendRequestRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		if actor == input.RecipientID || input.RecipientID == "" {
			return NewAPIError(http.StatusBadRequest, "invalid_relationship")
		}
		// 行ロックから COMMIT までを1往復で送る。遅延の大きいリージョンでも、ロックを握るのはサーバー内の実行時間だけになる。
		batch := &pgx.Batch{}
		batch.Queue(lockRelationshipUsersSQL, []string{actor, input.RecipientID})
		batch.Queue(createFriendRequestSQL, []string{actor, input.RecipientID}, actor, input.RecipientID,
			min(actor, input.RecipientID), max(actor, input.RecipientID), uuid.NewString(), uuid.NewString())
		var (
			action     string
			relationID *string
			status     *string
			updatedAt  *time.Time
		)
		if err := sendBatch(ctx, h.DB, batch, func(results pgx.BatchResults) error {
			if _, err := results.Exec(); err != nil {
				return err
			}
			return results.QueryRow().Scan(&action, &relationID, &status, &updatedAt)
		}); err != nil {
			return err
		}
		switch action {
		case "not_found":
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		case "conflict":
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		return c.JSON(http.StatusCreated, createFriendRequestResponse{RelationID: *relationID, Status: *status, UpdatedAt: *updatedAt})
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
		// 行ロックから COMMIT までを1往復で送る（createFriendRequestHandler と同じ）。
		batch := &pgx.Batch{}
		batch.Queue(lockRelationshipUsersSQL, []string{request.RequesterID, request.RecipientID})
		batch.Queue(transitionFriendRequestSQL, []string{request.RequesterID, request.RecipientID}, input.RequestID, status,
			uuid.NewString(), min(request.RequesterID, request.RecipientID), max(request.RequesterID, request.RecipientID))
		var (
			action     string
			relationID *string
			newStatus  *string
			updatedAt  *time.Time
		)
		if err := sendBatch(ctx, h.DB, batch, func(results pgx.BatchResults) error {
			if _, err := results.Exec(); err != nil {
				return err
			}
			return results.QueryRow().Scan(&action, &relationID, &newStatus, &updatedAt)
		}); err != nil {
			return err
		}
		switch action {
		case "not_found":
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		case "missing":
			return sql.ErrNoRows
		case "forbidden":
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		case "conflict":
			return NewAPIError(http.StatusConflict, "relationship_conflict")
		}
		return c.JSON(http.StatusOK, transitionFriendRequestResponse{RelationID: *relationID, Status: *newStatus, UpdatedAt: *updatedAt})
	}
}

// transitionFriendRequestSQL は、ロックを取った後の最新の状態を見て、申請の状態の更新（承認なら友達関係の作成も）を1文で行う。
//
//	$1 = 2人の ID の配列、$2 = 申請の ID、$3 = 新しい状態、$4 = 友達関係の ID、$5/$6 = 友達関係の low/high（バイト順）
//
// action は、not_found（どちらかが存在しない・退会済み）／missing（申請がない）／forbidden（保留中でない）／
// conflict（承認しようとしたが、ブロックがある）／ok（更新した）。
const transitionFriendRequestSQL = `
  WITH locked AS (
    SELECT count(*) AS n FROM users WHERE id = ANY($1::text[]) AND status = 'active'
  ), cur AS (
    SELECT fr.id, fr.status,
      EXISTS(SELECT 1 FROM block_relations b
             WHERE (b.blocker_id=fr.requester_id AND b.blocked_id=fr.recipient_id)
                OR (b.blocker_id=fr.recipient_id AND b.blocked_id=fr.requester_id)) AS blocked
    FROM friend_requests fr WHERE fr.id=$2::text
  ), plan AS (
    SELECT CASE
      WHEN (SELECT n FROM locked) <> 2 THEN 'not_found'
      WHEN NOT EXISTS (SELECT 1 FROM cur) THEN 'missing'
      WHEN (SELECT status FROM cur) <> 'pending' THEN 'forbidden'
      WHEN $3::text = 'accepted' AND (SELECT blocked FROM cur) THEN 'conflict'
      ELSE 'ok' END AS action
  ), updated AS (
    UPDATE friend_requests SET status=$3::text, updated_at=clock_timestamp()
    WHERE id=$2::text AND (SELECT action FROM plan) = 'ok'
    RETURNING id, status, updated_at
  ), friendship AS (
    INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id)
    SELECT $4::text, $5::text, $6::text, id FROM updated WHERE $3::text = 'accepted'
  )
  SELECT plan.action, u.id AS relation_id, u.status, u.updated_at
  FROM plan LEFT JOIN updated u ON true
`
