package isureal

// 友達申請は「行ロック」と「判定+書き込み」を別々の文として1往復で送る（sendBatch）。
// READ COMMITTED では文ごとにスナップショットを取るので、ロックを待っているあいだに他の申請がコミットされていても、
// 判定はその結果を見る。これを同時実行で確かめる。ISUREAL_TEST_DSN を指定したときだけ実行する。

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestFriendRequestConcurrentRequestsSerialize(t *testing.T) {
	db := openTestDB(t)
	db.SetMaxOpenConns(30) // 本番（server.go）と同じ上限。多数の同時リクエストが接続数の上限を超えないようにする
	e := newRelationServer(db, false)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("seed 失敗: %v\n%s", err, query)
		}
	}
	newUser := func(name string) string {
		id := uuid.NewString()
		exec(`INSERT INTO users (id, username, secret_key_hash, display_name, region, status) VALUES ($1,$2,$3,'u','asia','active')`, id, name, fmt.Sprintf("%064x", len(name)))
		return id
	}
	const pairs = 60
	type pair struct{ a, b string }
	var opposite, same []pair
	for i := range pairs {
		opposite = append(opposite, pair{newUser(fmt.Sprintf("opp-a-%d", i)), newUser(fmt.Sprintf("opp-b-%d", i))})
		same = append(same, pair{newUser(fmt.Sprintf("same-a-%d", i)), newUser(fmt.Sprintf("same-b-%d", i))})
	}

	results := map[string]int{}
	var mu sync.Mutex
	start := make(chan struct{})
	var wg sync.WaitGroup
	fire := func(label, actor, target string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 全部そろってから一斉に送る
			res := callRelation(e, http.MethodPost, "/api/friend-requests", actor, fmt.Sprintf(`{"recipientId":%q}`, target))
			mu.Lock()
			results[label] = res.Code
			mu.Unlock()
		}()
	}
	for i, p := range opposite { // A→B と B→A を同時に
		fire(fmt.Sprintf("opposite-%d-ab", i), p.a, p.b)
		fire(fmt.Sprintf("opposite-%d-ba", i), p.b, p.a)
	}
	for i, p := range same { // 同じ A→B を2つ同時に
		fire(fmt.Sprintf("same-%d-1", i), p.a, p.b)
		fire(fmt.Sprintf("same-%d-2", i), p.a, p.b)
	}
	close(start)
	wg.Wait()

	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.Get(&n, query, args...); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for i, p := range opposite {
		// 先に入った方が申請（pending）を作り、後の方がそれを見つけて承認する → 友達が1組、申請は承認済みの1件だけ
		ab, ba := results[fmt.Sprintf("opposite-%d-ab", i)], results[fmt.Sprintf("opposite-%d-ba", i)]
		if ab != http.StatusCreated || ba != http.StatusCreated {
			t.Fatalf("opposite[%d]: a→b=%d b→a=%d（want 201, 201）", i, ab, ba)
		}
		if n := count(`SELECT count(*) FROM friendships WHERE user_low_id=$1 AND user_high_id=$2`, min(p.a, p.b), max(p.a, p.b)); n != 1 {
			t.Fatalf("opposite[%d]: 友達関係が %d 件（want 1）。ロックの後の判定が古いスナップショットを見ている", i, n)
		}
		if n := count(`SELECT count(*) FROM friend_requests WHERE (requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1)`, p.a, p.b); n != 1 {
			t.Fatalf("opposite[%d]: 申請が %d 件（want 1）", i, n)
		}
		if n := count(`SELECT count(*) FROM friend_requests WHERE status='pending' AND ((requester_id=$1 AND recipient_id=$2) OR (requester_id=$2 AND recipient_id=$1))`, p.a, p.b); n != 0 {
			t.Fatalf("opposite[%d]: 保留中の申請が %d 件残っている（want 0）", i, n)
		}
	}
	for i, p := range same {
		codes := []int{results[fmt.Sprintf("same-%d-1", i)], results[fmt.Sprintf("same-%d-2", i)]}
		created, conflict := 0, 0
		for _, code := range codes {
			switch code {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
				conflict++
			}
		}
		if created != 1 || conflict != 1 {
			t.Fatalf("same[%d]: codes=%v（want 201 が1つと 409 が1つ）", i, codes)
		}
		if n := count(`SELECT count(*) FROM friend_requests WHERE requester_id=$1 AND recipient_id=$2 AND status='pending'`, p.a, p.b); n != 1 {
			t.Fatalf("same[%d]: 保留中の申請が %d 件（want 1）", i, n)
		}
	}
}
