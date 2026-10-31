package isureal

// Memories の並列化の等価性テスト。ISUREAL_TEST_DSN を指定したときだけ実行する（指定先のスキーマは作り直される）。

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestListMemoriesEquivalence(t *testing.T) {
	db := openTestDB(t)
	rng := rand.New(rand.NewSource(8))
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("seed 失敗: %v\n%s", err, query)
		}
	}
	// 2026年9月1日から2か月分、毎日1つの投稿期間（最後だけ開いている）
	first := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var windows []string
	var windowDates []time.Time
	for d := 0; d < 61; d++ {
		date := first.AddDate(0, 0, d)
		var endedAt *time.Time
		if d < 60 {
			e := date.Add(23 * time.Hour)
			endedAt = &e
		}
		id := uuid.NewString()
		exec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline, ended_at) VALUES ($1,'asia',$2,$3,$4,$5)`, id, date, date, date.Add(2*time.Minute), endedAt)
		windows = append(windows, id)
		windowDates = append(windowDates, date)
	}
	var users []string
	for i := range 25 {
		id := uuid.NewString()
		key := make([]byte, 32)
		rng.Read(key)
		status := "active"
		if i >= 20 {
			status = "deleted" // 退会済みの RealMoji・コメントの作者
		}
		var profile *string
		if i%3 == 0 {
			s := uuid.NewString()
			profile = &s
		}
		exec(`INSERT INTO users (id, username, secret_key_hash, display_name, profile_image_id, region, status) VALUES ($1,$2,$3,'u',$4,'asia',$5)`, id, fmt.Sprintf("mem%02d", i), hex.EncodeToString(key), profile, status)
		users = append(users, id)
	}
	// 投稿: 先頭の3人は毎日2件投稿（1か月で50件を超えてページ送りが起きるように）、他は気まぐれに投稿。同時刻の投稿も作る
	var posts []string
	for ui, author := range users[:20] {
		for d, w := range windows {
			n := 0
			switch {
			case ui < 3:
				n = 2
			case rng.Intn(4) == 0:
				n = 1
			}
			for range n {
				postedAt := windowDates[d].Add(time.Duration(rng.Intn(3)) * time.Minute)
				var deletedAt *time.Time
				if rng.Intn(10) == 0 {
					deletedAt = &postedAt
				}
				id := uuid.NewString()
				exec(`INSERT INTO posts (id, author_id, posting_window_id, image_id, visibility, location_kind, posted_at, deleted_at) VALUES ($1,$2,$3,$4,'public','none',$5,$6)`, id, author, w, uuid.NewString(), postedAt, deletedAt)
				posts = append(posts, id)
			}
		}
	}
	// RealMoji とコメント。一部の投稿には50件を超えて付け、上限で切られることも確かめる
	base := first.Add(time.Hour)
	addReaction := func(postID, reactor string, i int) {
		var replacedAt, deletedAt *time.Time
		switch rng.Intn(6) {
		case 0:
			replacedAt = &base
		case 1:
			deletedAt = &base
		}
		at := base.Add(time.Duration(rng.Intn(50)) * time.Second)
		if replacedAt == nil && deletedAt == nil {
			// 有効な RealMoji は (post, reactor) ごとに1つまで
			var exists bool
			if err := db.Get(&exists, `SELECT EXISTS(SELECT 1 FROM realmojis WHERE post_id=$1 AND reactor_id=$2 AND replaced_at IS NULL AND deleted_at IS NULL)`, postID, reactor); err != nil {
				t.Fatal(err)
			}
			if exists {
				replacedAt = &base
			}
		}
		exec(`INSERT INTO realmojis (id, post_id, reactor_id, image_id, reaction_type, created_at, replaced_at, deleted_at) VALUES ($1,$2,$3,$4,'like',$5,$6,$7)`,
			uuid.NewString(), postID, reactor, uuid.NewString(), at, replacedAt, deletedAt)
	}
	addComment := func(postID, author string) {
		var deletedAt *time.Time
		if rng.Intn(6) == 0 {
			deletedAt = &base
		}
		at := base.Add(time.Duration(rng.Intn(50)) * time.Second)
		exec(`INSERT INTO comments (id, post_id, author_id, body, created_at, deleted_at) VALUES ($1,$2,$3,'hi',$4,$5)`, uuid.NewString(), postID, author, at, deletedAt)
	}
	for _, postID := range posts {
		for i := range rng.Intn(4) {
			addReaction(postID, users[rng.Intn(len(users))], i)
			addComment(postID, users[rng.Intn(len(users))])
		}
	}
	for _, postID := range posts[:3] {
		for i := range 70 {
			addReaction(postID, users[i%len(users)], i)
			addComment(postID, users[rng.Intn(len(users))])
		}
	}

	ctx := context.Background()
	compared, nonEmpty, paged, capped := 0, 0, 0, 0
	compare := func(userID string, start, end time.Time, beforeAt *time.Time, beforeID string) (getMemoriesResponse, bool) {
		t.Helper()
		want, wantErr := refListMemories(ctx, db, userID, start, end, beforeAt, beforeID)
		got, gotErr := listMemories(ctx, db, userID, start, end, beforeAt, beforeID)
		compared++
		if fmt.Sprint(wantErr) != fmt.Sprint(gotErr) {
			t.Fatalf("エラーが不一致 want=%v got=%v", wantErr, gotErr)
		}
		if wantErr != nil {
			return got, false
		}
		wj, _ := json.Marshal(want)
		gj, _ := json.Marshal(got)
		ws, gs := signaturePattern.ReplaceAllString(string(wj), "?SIGNED"), signaturePattern.ReplaceAllString(string(gj), "?SIGNED")
		if ws != gs {
			t.Fatalf("結果が不一致 user=%s %s〜%s before=%v/%s\nwant=%s\n got=%s", userID, start.Format(time.DateOnly), end.Format(time.DateOnly), beforeAt, beforeID, ws[:min(len(ws), 2000)], gs[:min(len(gs), 2000)])
		}
		if len(got.Items) > 0 {
			nonEmpty++
		}
		for _, item := range got.Items {
			if len(item.RealMojis) == 50 || len(item.Comments) == 50 {
				capped++
			}
		}
		return got, true
	}
	months := []time.Time{first, first.AddDate(0, 1, 0), first.AddDate(0, -1, 0)}
	for _, userID := range users {
		for _, month := range months {
			var beforeAt *time.Time
			beforeID := ""
			for range 5 {
				res, ok := compare(userID, month, month.AddDate(0, 1, 0), beforeAt, beforeID)
				if !ok || !res.HasMore {
					break
				}
				paged++
				last := res.Items[len(res.Items)-1]
				at := last.PostedAt
				beforeAt, beforeID = &at, last.ID
			}
		}
		for range 5 {
			day := windowDates[rng.Intn(len(windowDates))]
			compare(userID, day, day.AddDate(0, 0, 1), nil, "")
		}
	}
	// 不正なページ指定
	compare(users[0], first, first.AddDate(0, 1, 0), nil, "x")
	t.Logf("比較 %d 回（空でない結果 %d 回、次ページあり %d 回、50件の上限に達した投稿 %d 件）", compared, nonEmpty, paged, capped)
	if nonEmpty == 0 || paged == 0 || capped == 0 {
		t.Fatal("テストデータが偏っていて検証になっていない")
	}
}
