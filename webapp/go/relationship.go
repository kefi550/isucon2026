package isureal

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type FriendRequest struct {
	ID          string    `db:"id"`
	RequesterID string    `db:"requester_id"`
	RecipientID string    `db:"recipient_id"`
	Status      string    `db:"status"`
	CreatedAt   time.Time `db:"created_at"`
	UpdatedAt   time.Time `db:"updated_at"`
}

func getFriendRequest(ctx context.Context, db sqlx.QueryerContext, id string) (FriendRequest, error) {
	var request FriendRequest
	err := sqlx.GetContext(ctx, db, &request, `SELECT id, requester_id, recipient_id, status, created_at, updated_at FROM friend_requests WHERE id=$1`, id)
	return request, err
}

func beginRelationshipTx(ctx context.Context, db *sqlx.DB, actor, target string) (*sqlx.Tx, error) {
	if actor == target || target == "" {
		return nil, NewAPIError(http.StatusBadRequest, "invalid_relationship")
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := lockRelationshipUsers(ctx, tx, actor, target); err != nil {
		tx.Rollback()
		return nil, err
	}
	return tx, nil
}

// 2人分のロックを1往復で取る。ORDER BY で並べた順に行がロックされるので、
// 従来どおり id の昇順でロックすることになり、デッドロックを避ける性質は変わらない。
// COLLATE "C" はバイト順の比較で、Go の min/max（他の処理のロック順）と一致させるために必要。
func lockRelationshipUsers(ctx context.Context, tx *sqlx.Tx, actor, target string) error {
	if actor == target || target == "" {
		return NewAPIError(http.StatusBadRequest, "invalid_relationship")
	}
	var lockedIDs []string
	if err := tx.SelectContext(ctx, &lockedIDs, `SELECT id FROM users WHERE id = ANY($1::text[]) AND status='active' ORDER BY id COLLATE "C" FOR NO KEY UPDATE`, []string{actor, target}); err != nil {
		return err
	}
	if len(lockedIDs) != 2 {
		// どちらかが存在しないか退会済み
		return NewAPIError(http.StatusNotFound, "relationship_not_found")
	}
	return nil
}

func isBlocked(ctx context.Context, db sqlx.QueryerContext, actor, target string) (bool, error) {
	var blocked bool
	err := sqlx.GetContext(ctx, db, &blocked, `SELECT EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=$2) OR (blocker_id=$2 AND blocked_id=$1))`, actor, target)
	return blocked, err
}

// updateFriendRequestStatus は友達申請の状態を更新し、makeFriends のときは同じ1往復で友達関係も作る。
// userA / userB は申請の2人（順不同）。友達関係の low / high は Go のバイト順比較で決める（従来と同じ）。
func updateFriendRequestStatus(ctx context.Context, tx *sqlx.Tx, requestID, status string, makeFriends bool, userA, userB string) (createFriendRequestResponse, error) {
	var response createFriendRequestResponse
	err := tx.GetContext(ctx, &response, `
  WITH updated AS (
    UPDATE friend_requests SET status=$2::text, updated_at=clock_timestamp() WHERE id=$1::text
    RETURNING id, status, updated_at
  ), friendship AS (
    INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id)
    SELECT $4::text, $5::text, $6::text, $1::text WHERE $3::boolean
  )
  SELECT id AS relation_id, status, updated_at FROM updated
 `, requestID, status, makeFriends, uuid.NewString(), min(userA, userB), max(userA, userB))
	return response, err
}

func requireRelationshipChange(result sql.Result, err error, status int, code string) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return NewAPIError(status, code)
	}
	return nil
}
