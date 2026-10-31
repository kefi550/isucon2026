package isureal

// 通知の見回りを1本のクエリにまとめた変更と、LISTEN/NOTIFY による即時の見回りのテスト。
// ISUREAL_TEST_DSN を指定したときだけ実行する（指定先のスキーマは作り直される）。

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// refLoadRegionNotificationState は、修正前の pollRegion と同じ3本のクエリで同じ情報を取る（比較用）
func refLoadRegionNotificationState(ctx context.Context, db *sqlx.DB, region string, userIDs []string) (*openWindow, map[string]notificationUser, map[string]notificationRow, error) {
	var windows []struct {
		ID       string    `db:"id"`
		StartsAt time.Time `db:"starts_at"`
	}
	if err := db.SelectContext(ctx, &windows, `
		SELECT id, starts_at FROM posting_windows
		WHERE region = $1 AND ended_at IS NULL ORDER BY starts_at, id
	`, region); err != nil {
		return nil, nil, nil, err
	}
	if len(windows) == 0 {
		return nil, nil, nil, nil
	}
	users, err := selectNotificationUsers(ctx, db, userIDs)
	if err != nil {
		return nil, nil, nil, err
	}
	all, err := selectOpenWindowNotifications(ctx, db, windows[0].ID)
	if err != nil {
		return nil, nil, nil, err
	}
	// 修正後は、問い合わせた利用者の通知の行だけを返すので、比較のために同じ範囲に絞る
	notes := map[string]notificationRow{}
	for _, id := range userIDs {
		if note, ok := all[id]; ok {
			notes[id] = note
		}
	}
	return &openWindow{ID: windows[0].ID, StartsAt: windows[0].StartsAt}, users, notes, nil
}

func TestLoadRegionNotificationStateEquivalence(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("seed 失敗: %v\n%s", err, query)
		}
	}
	base := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	// 利用者: asia の有効・退会済み、europe へ移った人
	var ids []string
	for i := range 12 {
		id := uuid.NewString()
		region, status := "asia", "active"
		switch i % 4 {
		case 1:
			status = "deleted"
		case 2:
			region = "europe"
		}
		key := make([]byte, 32)
		exec(`INSERT INTO users (id, username, secret_key_hash, display_name, region, status) VALUES ($1,$2,$3,'u',$4,$5)`, id, fmt.Sprintf("ntf%02d", i), hex.EncodeToString(key), region, status)
		ids = append(ids, id)
	}
	query := append(append([]string{}, ids...), uuid.NewString()) // 存在しない利用者も含める

	compare := func(label, region string, userIDs []string) {
		t.Helper()
		ww, wu, wn, werr := refLoadRegionNotificationState(ctx, db, region, userIDs)
		gw, gu, gn, gerr := loadRegionNotificationState(ctx, db, region, userIDs)
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("%s: エラーが不一致 want=%v got=%v", label, werr, gerr)
		}
		if ww == nil || gw == nil {
			if (ww == nil) != (gw == nil) {
				t.Fatalf("%s: 投稿期間の有無が不一致 want=%v got=%v", label, ww, gw)
			}
			return
		}
		if ww.ID != gw.ID || !ww.StartsAt.Equal(gw.StartsAt) {
			t.Fatalf("%s: 投稿期間が不一致 want=%+v got=%+v", label, ww, gw)
		}
		if !reflect.DeepEqual(wu, gu) {
			t.Fatalf("%s: 利用者の状態が不一致\nwant=%v\n got=%v", label, wu, gu)
		}
		if len(wn) != len(gn) {
			t.Fatalf("%s: 通知の件数が不一致 want=%d got=%d", label, len(wn), len(gn))
		}
		for id, w := range wn {
			g := gn[id]
			if w.ID != g.ID || w.PostingWindowID != g.PostingWindowID || w.Region != g.Region || !w.CreatedAt.Equal(g.CreatedAt) || w.UserID != g.UserID {
				t.Fatalf("%s: 通知が不一致 user=%s want=%+v got=%+v", label, id, w, g)
			}
		}
	}

	compare("投稿期間なし", "asia", query)

	// asia に終了済みと開いている投稿期間。通知は有効な asia の利用者の一部にだけ作る
	ended, open := uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline, ended_at) VALUES ($1,'asia',$2,$3,$3,$4)`, ended, base.AddDate(0, 0, -1), base.Add(-time.Hour), base)
	exec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline) VALUES ($1,'asia',$2,$3,$3)`, open, base, base)
	for i, id := range ids {
		if i%3 != 0 {
			exec(`INSERT INTO notifications (id, user_id, posting_window_id, created_at) VALUES ($1,$2,$3,$4)`, uuid.NewString(), id, open, base)
		}
		exec(`INSERT INTO notifications (id, user_id, posting_window_id, created_at) VALUES ($1,$2,$3,$4)`, uuid.NewString(), id, ended, base.Add(-time.Hour))
	}
	compare("開いている投稿期間あり", "asia", query)
	compare("利用者なし", "asia", nil)
	compare("別リージョン（投稿期間なし）", "europe", query)
}

func TestNotificationBrokerWakesOnPostingWindowNotify(t *testing.T) {
	db := openTestDB(t)
	broker := &notificationBroker{db: db, wake: make(chan struct{}, 1), subs: map[*notificationSub]struct{}{}}
	go broker.listenPostingWindows(os.Getenv("ISUREAL_TEST_DSN"))
	time.Sleep(500 * time.Millisecond) // LISTEN が始まるのを待つ

	if _, err := db.Exec(`SELECT pg_notify($1, $2)`, postingWindowOpenedChannel, "europe"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-broker.wake:
	case <-time.After(2 * time.Second):
		t.Fatal("pg_notify を送っても broker が起きなかった")
	}
}
