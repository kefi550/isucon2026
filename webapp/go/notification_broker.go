package isureal

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	notificationPollInterval     = 500 * time.Millisecond
	notificationLivenessInterval = 5 * time.Second
	notificationUserChunk        = 5000
)

type notificationDecision int

const (
	notificationKeepWaiting notificationDecision = iota
	notificationDisconnect
	notificationSend
)

type notificationUser struct {
	ID     string `db:"id"`
	Region string `db:"region"`
	Status string `db:"status"`
}

type notificationRow struct {
	UserID          string    `db:"user_id"`
	ID              string    `db:"id"`
	PostingWindowID string    `db:"posting_window_id"`
	Region          string    `db:"region"`
	CreatedAt       time.Time `db:"created_at"`
}

type notificationDelivery struct {
	send bool
	body getNotificationsResponse
}

type notificationSub struct {
	userID        string
	sessionRegion string
	connectedAt   time.Time
	ch            chan notificationDelivery
}

type notificationBroker struct {
	db   *sqlx.DB
	wake chan struct{}

	mu   sync.Mutex
	subs map[*notificationSub]struct{}
}

func (h *HandlerContext) notificationHub() *notificationBroker {
	h.notificationOnce.Do(func() {
		h.notifications = newNotificationBroker(h.DB)
	})
	return h.notifications
}

func newNotificationBroker(db *sqlx.DB) *notificationBroker {
	broker := &notificationBroker{
		db:   db,
		wake: make(chan struct{}, 1),
		subs: make(map[*notificationSub]struct{}),
	}
	go broker.loop()
	return broker
}

// windowMayNotify は、開いている投稿期間の通知がこの接続の対象になり得るかを返す。
// 通知の created_at は投稿期間の starts_at と同じ値で入る。判定は SQL の created_at > connectedAt に合わせる。
func windowMayNotify(connectedAt, startsAt time.Time) bool {
	return startsAt.After(connectedAt)
}

func userShouldDisconnect(sessionRegion string, user *notificationUser) bool {
	return user == nil || user.Status != "active" || user.Region != sessionRegion
}

// decideNotification は、1本の接続に今の投稿期間の通知を送るか、切るか、待ち続けるかを決める。
// 退会とリージョン変更は、通知行があっても送らずに切る。行が無い、または接続より前の通知なら待ち続ける。
func decideNotification(connectedAt time.Time, sessionRegion string, user *notificationUser, note *notificationRow) (notificationDecision, getNotificationsResponse) {
	if userShouldDisconnect(sessionRegion, user) {
		return notificationDisconnect, getNotificationsResponse{}
	}
	if note == nil || !note.CreatedAt.After(connectedAt) {
		return notificationKeepWaiting, getNotificationsResponse{}
	}
	return notificationSend, getNotificationsResponse{
		ID:              note.ID,
		PostingWindowID: note.PostingWindowID,
		Region:          note.Region,
		CreatedAt:       note.CreatedAt,
	}
}

func (b *notificationBroker) subscribe(userID, sessionRegion string, connectedAt time.Time) *notificationSub {
	sub := &notificationSub{
		userID:        userID,
		sessionRegion: sessionRegion,
		connectedAt:   connectedAt,
		ch:            make(chan notificationDelivery, 1),
	}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()
	b.wakePoll()
	return sub
}

func (b *notificationBroker) unsubscribe(sub *notificationSub) {
	b.mu.Lock()
	delete(b.subs, sub)
	b.mu.Unlock()
}

func (b *notificationBroker) wakePoll() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *notificationBroker) loop() {
	ticker := time.NewTicker(notificationPollInterval)
	defer ticker.Stop()
	liveness := time.NewTicker(notificationLivenessInterval)
	defer liveness.Stop()
	for {
		select {
		case <-ticker.C:
			b.pollWindows(context.Background())
		case <-b.wake:
			b.pollWindows(context.Background())
		case <-liveness.C:
			// 配信を生存確認の待ち時間の後ろに回さない。
			b.pollWindows(context.Background())
			b.pollLiveness(context.Background())
		}
	}
}

func (b *notificationBroker) snapshot() map[string][]*notificationSub {
	b.mu.Lock()
	defer b.mu.Unlock()
	groups := make(map[string][]*notificationSub)
	for sub := range b.subs {
		groups[sub.sessionRegion] = append(groups[sub.sessionRegion], sub)
	}
	return groups
}

func (b *notificationBroker) deliver(sub *notificationSub, delivery notificationDelivery) {
	b.mu.Lock()
	if _, ok := b.subs[sub]; !ok {
		b.mu.Unlock()
		return
	}
	delete(b.subs, sub)
	b.mu.Unlock()
	sub.ch <- delivery
}

func (b *notificationBroker) pollWindows(ctx context.Context) {
	for region, subs := range b.snapshot() {
		if err := b.pollRegion(ctx, region, subs); err != nil {
			slog.Error("failed to get notification", "error", err, "region", region)
		}
	}
}

func (b *notificationBroker) pollRegion(ctx context.Context, region string, subs []*notificationSub) error {
	var windows []struct {
		ID       string    `db:"id"`
		StartsAt time.Time `db:"starts_at"`
	}
	if err := b.db.SelectContext(ctx, &windows, `
		SELECT id, starts_at
		FROM posting_windows
		WHERE region = $1 AND ended_at IS NULL
		ORDER BY starts_at, id
	`, region); err != nil {
		return err
	}
	if len(windows) == 0 {
		return nil
	}
	window := windows[0]
	eligible := make([]*notificationSub, 0, len(subs))
	for _, sub := range subs {
		if windowMayNotify(sub.connectedAt, window.StartsAt) {
			eligible = append(eligible, sub)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	users, err := selectNotificationUsers(ctx, b.db, subscriberUserIDs(eligible))
	if err != nil {
		return err
	}
	notes, err := selectOpenWindowNotifications(ctx, b.db, window.ID)
	if err != nil {
		return err
	}
	for _, sub := range eligible {
		user, ok := users[sub.userID]
		var userPtr *notificationUser
		if ok {
			userPtr = &user
		}
		note, ok := notes[sub.userID]
		var notePtr *notificationRow
		if ok {
			notePtr = &note
		}
		decision, body := decideNotification(sub.connectedAt, sub.sessionRegion, userPtr, notePtr)
		switch decision {
		case notificationDisconnect:
			b.deliver(sub, notificationDelivery{})
		case notificationSend:
			b.deliver(sub, notificationDelivery{send: true, body: body})
		}
	}
	return nil
}

func (b *notificationBroker) pollLiveness(ctx context.Context) {
	for region, subs := range b.snapshot() {
		users, err := selectNotificationUsers(ctx, b.db, subscriberUserIDs(subs))
		if err != nil {
			slog.Error("failed to get notification recipient", "error", err, "region", region)
			continue
		}
		for _, sub := range subs {
			user, ok := users[sub.userID]
			var userPtr *notificationUser
			if ok {
				userPtr = &user
			}
			if userShouldDisconnect(sub.sessionRegion, userPtr) {
				b.deliver(sub, notificationDelivery{})
			}
		}
	}
}

func subscriberUserIDs(subs []*notificationSub) []string {
	ids := make([]string, 0, len(subs))
	seen := make(map[string]struct{}, len(subs))
	for _, sub := range subs {
		if _, ok := seen[sub.userID]; ok {
			continue
		}
		seen[sub.userID] = struct{}{}
		ids = append(ids, sub.userID)
	}
	return ids
}

func selectNotificationUsers(ctx context.Context, db sqlx.QueryerContext, ids []string) (map[string]notificationUser, error) {
	users := make(map[string]notificationUser, len(ids))
	for start := 0; start < len(ids); start += notificationUserChunk {
		end := min(start+notificationUserChunk, len(ids))
		query, args, err := sqlx.In(`SELECT id, region, status FROM users WHERE id IN (?)`, ids[start:end])
		if err != nil {
			return nil, err
		}
		var rows []notificationUser
		if err := sqlx.SelectContext(ctx, db, &rows, sqlx.Rebind(sqlx.DOLLAR, query), args...); err != nil {
			return nil, err
		}
		for _, row := range rows {
			users[row.ID] = row
		}
	}
	return users, nil
}

func selectOpenWindowNotifications(ctx context.Context, db sqlx.QueryerContext, windowID string) (map[string]notificationRow, error) {
	var rows []notificationRow
	if err := sqlx.SelectContext(ctx, db, &rows, `
		SELECT n.user_id, n.id, n.posting_window_id, w.region, n.created_at
		FROM notifications n
		JOIN posting_windows w ON w.id = n.posting_window_id
		WHERE n.posting_window_id = $1 AND w.ended_at IS NULL
	`, windowID); err != nil {
		return nil, err
	}
	notes := make(map[string]notificationRow, len(rows))
	for _, row := range rows {
		existing, ok := notes[row.UserID]
		if ok && (row.CreatedAt.After(existing.CreatedAt) || (row.CreatedAt.Equal(existing.CreatedAt) && row.ID >= existing.ID)) {
			continue
		}
		notes[row.UserID] = row
	}
	return notes, nil
}
