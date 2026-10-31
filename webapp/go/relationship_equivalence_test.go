package isureal

// 友達関係 API の往復削減の等価性テスト。ISUREAL_TEST_DSN を指定したときだけ実行する。
// 指定した DB のスキーマを作り直し、さらに同じサーバーに isureal_relation_new という DB を作り直すので、
// 共有の DB には絶対に向けないこと。

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// openSecondTestDB は ISUREAL_TEST_DSN と同じサーバーに別の DB を作り直して開く
func openSecondTestDB(t *testing.T, base *sqlx.DB, name string) *sqlx.DB {
	t.Helper()
	if _, err := base.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
		t.Fatal(err)
	}
	if _, err := base.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("ISUREAL_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := sqlx.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema, err := os.ReadFile("../sql/001-schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("スキーマ作成に失敗: %v", err)
	}
	return db
}

// seedRelations は乱数（シード固定）で、両方の DB にまったく同じ初期データを入れる
func seedRelations(t *testing.T, dbs []*sqlx.DB, seed int64) (active, all, requestIDs []string) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	newID := func() string { return uuid.Must(uuid.NewRandomFromReader(rng)).String() }
	exec := func(query string, args ...any) {
		t.Helper()
		for _, db := range dbs {
			if _, err := db.Exec(query, args...); err != nil {
				t.Fatalf("seed 失敗: %v\n%s", err, query)
			}
		}
	}
	for i := range 40 {
		id := newID()
		key := make([]byte, 32)
		rng.Read(key)
		accountType, status := "regular", "active"
		if rng.Intn(3) == 0 {
			accountType = "certified_public"
		}
		if rng.Intn(8) == 0 {
			status = "deleted"
		} else {
			active = append(active, id)
		}
		all = append(all, id)
		exec(`INSERT INTO users (id, username, secret_key_hash, display_name, region, account_type, status) VALUES ($1,$2,$3,'u','asia',$4,$5)`,
			id, fmt.Sprintf("rel%02d", i), hex.EncodeToString(key), accountType, status)
	}
	pick := func() (string, string) {
		a, b := all[rng.Intn(len(all))], all[rng.Intn(len(all))]
		return a, b
	}
	seen := map[[2]string]bool{}
	for range 60 {
		a, b := pick()
		low, high := min(a, b), max(a, b)
		if a == b || seen[[2]string{low, high}] {
			continue
		}
		seen[[2]string{low, high}] = true
		exec(`INSERT INTO friendships (id, user_low_id, user_high_id) VALUES ($1,$2,$3)`, newID(), low, high)
	}
	for range 15 {
		a, b := pick()
		if a == b || seen[[2]string{"b" + a, b}] {
			continue
		}
		seen[[2]string{"b" + a, b}] = true
		exec(`INSERT INTO block_relations (id, blocker_id, blocked_id) VALUES ($1,$2,$3)`, newID(), a, b)
		// ブロック関係のある2人の間の保留中の申請（承認すると 409 になるケースの検証用）
		id := newID()
		requestIDs = append(requestIDs, id)
		exec(`INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at) VALUES ($1,$2,$3,'pending',now(),now())`, id, b, a)
	}
	for range 40 {
		a, b := pick()
		if a == b || seen[[2]string{"f" + a, b}] {
			continue
		}
		seen[[2]string{"f" + a, b}] = true
		exec(`INSERT INTO follows (id, follower_id, followee_id) VALUES ($1,$2,$3)`, newID(), a, b)
	}
	for range 30 {
		a, b := pick()
		if a == b {
			continue
		}
		id := newID()
		requestIDs = append(requestIDs, id)
		status := []string{"pending", "pending", "accepted", "rejected", "cancelled"}[rng.Intn(5)]
		exec(`INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at) VALUES ($1,$2,$3,$4,now(),now())`, id, a, b, status)
	}
	return active, all, requestIDs
}

// newRelationServer は本番と同じパスで、指定したハンドラー群を載せる（セッションは X-Test-User をそのまま使う）
func newRelationServer(db *sqlx.DB, reference bool) *echo.Echo {
	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	session := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Set("session", &Session{UserID: c.Request().Header.Get("X-Test-User")})
			return next(c)
		}
	}
	if reference {
		e.POST("/api/friend-requests", refCreateFriendRequestHandler(h), session)
		e.POST("/api/friend-requests/:requestId/:action", refTransitionFriendRequestHandler(h), session)
		e.DELETE("/api/friendships/:userId", refDeleteFriendshipHandler(h), session)
		e.PUT("/api/follows/:userId", refFollowHandler(h), session)
		e.DELETE("/api/follows/:userId", refUnfollowHandler(h), session)
		e.PUT("/api/blocks/:userId", refBlockHandler(h), session)
		e.DELETE("/api/blocks/:userId", refUnblockHandler(h), session)
	} else {
		e.POST("/api/friend-requests", createFriendRequestHandler(h), session)
		e.POST("/api/friend-requests/:requestId/:action", transitionFriendRequestHandler(h), session)
		e.DELETE("/api/friendships/:userId", deleteFriendshipHandler(h), session)
		e.PUT("/api/follows/:userId", followHandler(h), session)
		e.DELETE("/api/follows/:userId", unfollowHandler(h), session)
		e.PUT("/api/blocks/:userId", blockHandler(h), session)
		e.DELETE("/api/blocks/:userId", unblockHandler(h), session)
	}
	return e
}

type relationResult struct {
	Code       int
	RelationID string
	Status     string
	Error      string
	HasTime    bool
}

func callRelation(e *echo.Echo, method, path, actor, body string) relationResult {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("X-Test-User", actor)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	var parsed struct {
		RelationID string `json:"relationId"`
		Status     string `json:"status"`
		UpdatedAt  string `json:"updatedAt"`
		Error      string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return relationResult{Code: rec.Code, RelationID: parsed.RelationID, Status: parsed.Status, Error: parsed.Error, HasTime: parsed.UpdatedAt != ""}
}

// relationState は ID と時刻を除いた、業務上の状態のスナップショット
func relationState(t *testing.T, db *sqlx.DB, idMap map[string]string) map[string][]string {
	t.Helper()
	state := map[string][]string{}
	queries := map[string]string{
		"friendships": `SELECT user_low_id || ':' || user_high_id FROM friendships`,
		"follows":     `SELECT follower_id || ':' || followee_id FROM follows`,
		"blocks":      `SELECT blocker_id || ':' || blocked_id FROM block_relations`,
	}
	for name, query := range queries {
		var rows []string
		if err := db.Select(&rows, query); err != nil {
			t.Fatal(err)
		}
		sort.Strings(rows)
		state[name] = rows
	}
	// 申請は、旧 DB の ID に読み替えて状態まで比べる（新 DB は idMap で旧 ID に対応付ける）
	var requests []struct {
		ID, RequesterID, RecipientID, Status string
	}
	if err := db.Select(&requests, `SELECT id AS "id", requester_id AS "requesterid", recipient_id AS "recipientid", status AS "status" FROM friend_requests`); err != nil {
		t.Fatal(err)
	}
	rows := make([]string, 0, len(requests))
	for _, r := range requests {
		id := r.ID
		if mapped, ok := idMap[id]; ok {
			id = mapped
		}
		rows = append(rows, strings.Join([]string{id, r.RequesterID, r.RecipientID, r.Status}, ":"))
	}
	sort.Strings(rows)
	state["friend_requests"] = rows
	// 友達関係の作成元の申請も対応付けて比べる
	var sources []string
	if err := db.Select(&sources, `SELECT coalesce(source_request_id, '') FROM friendships`); err != nil {
		t.Fatal(err)
	}
	for i, s := range sources {
		if mapped, ok := idMap[s]; ok {
			sources[i] = mapped
		}
	}
	sort.Strings(sources)
	state["friendship_sources"] = sources
	return state
}

func TestRelationshipEquivalence(t *testing.T) {
	oldDB := openTestDB(t)
	newDB := openSecondTestDB(t, oldDB, "isureal_relation_new")
	active, all, seededRequests := seedRelations(t, []*sqlx.DB{oldDB, newDB}, 4)
	oldServer, newServer := newRelationServer(oldDB, true), newRelationServer(newDB, false)

	// 旧 DB の申請 ID → 新 DB の申請 ID（初期データは同じ ID）。newToOld はその逆引き。
	oldToNew, newToOld := map[string]string{}, map[string]string{}
	knownOldIDs := []string{}
	for _, id := range seededRequests {
		oldToNew[id], newToOld[id] = id, id
		knownOldIDs = append(knownOldIDs, id)
	}
	rng := rand.New(rand.NewSource(5))
	target := func() string {
		switch rng.Intn(12) {
		case 0:
			return uuid.NewString() // 存在しないユーザー
		case 1:
			return "" // 空
		default:
			return all[rng.Intn(len(all))] // 退会済みや自分自身も含む
		}
	}
	codes := map[string]int{}
	const operations = 4000
	for i := range operations {
		actor := active[rng.Intn(len(active))]
		var method, oldPath, newPath, body, label string
		switch op := rng.Intn(10); op {
		case 0, 1, 2:
			label = "create_request"
			method, oldPath = http.MethodPost, "/api/friend-requests"
			newPath = oldPath
			body = fmt.Sprintf(`{"recipientId":%q}`, target())
		case 3, 4:
			label = "transition"
			oldID := uuid.NewString()
			if rng.Intn(10) != 0 {
				oldID = knownOldIDs[rng.Intn(len(knownOldIDs))]
			}
			newID := oldID
			if mapped, ok := oldToNew[oldID]; ok {
				newID = mapped
			}
			// 申請の当事者が操作することが多くなるようにする
			var request FriendRequest
			if err := oldDB.Get(&request, `SELECT id, requester_id, recipient_id, status, created_at, updated_at FROM friend_requests WHERE id=$1`, oldID); err == nil && rng.Intn(4) != 0 {
				actor = []string{request.RequesterID, request.RecipientID}[rng.Intn(2)]
			}
			action := []string{"accept", "reject", "cancel", "accept", "bogus"}[rng.Intn(5)]
			method = http.MethodPost
			oldPath, newPath = "/api/friend-requests/"+oldID+"/"+action, "/api/friend-requests/"+newID+"/"+action
		default:
			kinds := []struct{ m, p, l string }{
				{http.MethodDelete, "/api/friendships/", "unfriend"},
				{http.MethodPut, "/api/follows/", "follow"},
				{http.MethodPut, "/api/follows/", "follow"},
				{http.MethodDelete, "/api/follows/", "unfollow"},
				{http.MethodPut, "/api/blocks/", "block"},
				{http.MethodDelete, "/api/blocks/", "unblock"},
			}
			k := kinds[rng.Intn(len(kinds))]
			label = k.l
			method, oldPath = k.m, k.p+url.PathEscape(target())
			newPath = oldPath
		}
		want := callRelation(oldServer, method, oldPath, actor, body)
		got := callRelation(newServer, method, newPath, actor, body)
		codes[fmt.Sprintf("%s %d", label, want.Code)]++
		if want.Code != got.Code || want.Status != got.Status || want.Error != got.Error || want.HasTime != got.HasTime || (want.RelationID == "") != (got.RelationID == "") {
			t.Fatalf("操作 %d (%s %s actor=%s body=%s) の結果が不一致\nwant=%+v\n got=%+v", i, method, oldPath, actor, body, want, got)
		}
		if want.RelationID != "" {
			if mapped, ok := oldToNew[want.RelationID]; ok {
				if mapped != got.RelationID {
					t.Fatalf("操作 %d: 申請 ID の対応が不一致 want(new)=%s got=%s", i, mapped, got.RelationID)
				}
			} else {
				if _, ok := newToOld[got.RelationID]; ok {
					t.Fatalf("操作 %d: 新しい申請のはずが既存の ID を返した", i)
				}
				oldToNew[want.RelationID], newToOld[got.RelationID] = got.RelationID, want.RelationID
				knownOldIDs = append(knownOldIDs, want.RelationID)
			}
		}
		if i%500 == 499 || i == operations-1 {
			if w, g := relationState(t, oldDB, nil), relationState(t, newDB, newToOld); !reflect.DeepEqual(w, g) {
				for name := range w {
					if !reflect.DeepEqual(w[name], g[name]) {
						t.Errorf("操作 %d 時点で %s の状態が不一致\nwant=%v\n got=%v", i, name, w[name], g[name])
					}
				}
				t.FailNow()
			}
		}
	}
	keys := make([]string, 0, len(codes))
	for k := range codes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%-22s %4d 回", k, codes[k])
	}
	// どの API でも成功と失敗の両方を通っていること
	for _, label := range []string{"create_request", "transition", "unfriend", "follow", "unfollow", "block", "unblock"} {
		success, failure := 0, 0
		for k, n := range codes {
			if strings.HasPrefix(k, label+" ") {
				if strings.HasSuffix(k, " 200") || strings.HasSuffix(k, " 201") || strings.HasSuffix(k, " 204") {
					success += n
				} else {
					failure += n
				}
			}
		}
		if success == 0 || failure == 0 {
			t.Errorf("%s の成功(%d)・失敗(%d)の片方しか通っていない", label, success, failure)
		}
	}
}

// TestRelationshipConcurrentNoDeadlock は、少人数のユーザー間で友達関係の操作を並行に大量に流し、
// デッドロックなどで 500 にならないことを確かめる（ロック順の検証）
func TestRelationshipConcurrentNoDeadlock(t *testing.T) {
	db := openTestDB(t)
	db.SetMaxOpenConns(40)
	active, _, _ := seedRelations(t, []*sqlx.DB{db}, 6)
	users := active[:8] // 衝突を起こしやすくするため少人数に絞る
	server := newRelationServer(db, false)
	var wg sync.WaitGroup
	var failures atomic.Int64
	for worker := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(100 + worker)))
			for range 150 {
				actor, target := users[rng.Intn(len(users))], users[rng.Intn(len(users))]
				var result relationResult
				switch rng.Intn(6) {
				case 0, 1:
					result = callRelation(server, http.MethodPost, "/api/friend-requests", actor, fmt.Sprintf(`{"recipientId":%q}`, target))
				case 2:
					result = callRelation(server, http.MethodPut, "/api/blocks/"+target, actor, "")
				case 3:
					result = callRelation(server, http.MethodDelete, "/api/blocks/"+target, actor, "")
				case 4:
					result = callRelation(server, http.MethodPut, "/api/follows/"+target, actor, "")
				default:
					result = callRelation(server, http.MethodDelete, "/api/friendships/"+target, actor, "")
				}
				if result.Code >= 500 {
					failures.Add(1)
					t.Errorf("500 系エラー: %+v", result)
				}
			}
		}()
	}
	wg.Wait()
	if failures.Load() > 0 {
		t.Fatalf("並行実行で %d 件の 500 系エラー", failures.Load())
	}
}
