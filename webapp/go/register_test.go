package isureal

// 登録は「membership のロック → INSERT → COMMIT」を1文で実行する。
// 窓の開始（openPostingWindow）は一括 INSERT の後で同じロックを取り、その時点までにコミットされた登録・退会・リージョン変更を通知対象へ反映する。
// 登録側の変更が、この協調を壊していないことを、順序を固定して確かめる。ISUREAL_TEST_DSN を指定したときだけ実行する。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

func newRegisterServer(t *testing.T) (*sqlx.DB, func(method, path, body string) (int, []byte)) {
	t.Helper()
	db := openTestDB(t)
	db.SetMaxOpenConns(30) // 本番（server.go）と同じ上限
	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.POST("/api/auth/register", registerHandler(h))
	e.POST("/api/admin/posting-window", openPostingWindowHandler(h))
	do := func(method, path, body string) (int, []byte) {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	return db, do
}

func TestRegister(t *testing.T) {
	_, do := newRegisterServer(t)
	code, body := do(http.MethodPost, "/api/auth/register", `{"username":"alice_1","displayName":"Alice","region":"europe"}`)
	if code != http.StatusCreated {
		t.Fatalf("登録: status=%d body=%s", code, body)
	}
	var created struct {
		SecretKey string `json:"secretKey"`
		User      struct {
			ID          string `json:"id"`
			Username    string `json:"username"`
			DisplayName string `json:"displayName"`
			Region      string `json:"region"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.SecretKey == "" || created.User.ID == "" || created.User.Username != "alice_1" || created.User.DisplayName != "Alice" || created.User.Region != "europe" {
		t.Fatalf("登録のレスポンスが不正: %s", body)
	}
	// ユーザー名の重複は 409
	if code, body := do(http.MethodPost, "/api/auth/register", `{"username":"alice_1","displayName":"Other","region":"asia"}`); code != http.StatusConflict {
		t.Fatalf("重複: status=%d body=%s（want 409）", code, body)
	}
	// 不正な入力は 400
	for _, in := range []string{
		`{"username":"Bad Name","displayName":"x","region":"asia"}`,
		`{"username":"ok_name","displayName":"","region":"asia"}`,
		`{"username":"ok_name","displayName":"x","region":"mars"}`,
	} {
		if code, body := do(http.MethodPost, "/api/auth/register", in); code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s（want 400）", in, code, body)
		}
	}
	// 重複で失敗した登録がロックを握り続けていない（続けて登録できる）
	if code, body := do(http.MethodPost, "/api/auth/register", `{"username":"bob_1","displayName":"Bob","region":"asia"}`); code != http.StatusCreated {
		t.Fatalf("重複の後の登録: status=%d body=%s", code, body)
	}
}

const membershipLockSQL = `SELECT pg_advisory_xact_lock(hashtext('user_membership'), 1)`

// 誰かが membership のロックを持っているあいだ、登録はコミットされずに待つ（窓の開始の補正と競合させるため）。
func TestRegisterWaitsForMembershipLock(t *testing.T) {
	db, do := newRegisterServer(t)
	tx := db.MustBegin()
	defer tx.Rollback()
	if _, err := tx.Exec(membershipLockSQL); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := do(http.MethodPost, "/api/auth/register", `{"username":"waiter","displayName":"w","region":"asia"}`)
		done <- code
	}()
	select {
	case code := <-done:
		t.Fatalf("membership のロックを持たれているのに登録が完了した（status=%d）", code)
	case <-time.After(400 * time.Millisecond):
	}
	var visible int
	if err := db.Get(&visible, `SELECT count(*) FROM users WHERE username='waiter'`); err != nil || visible != 0 {
		t.Fatalf("待っているあいだにユーザーが見えている: %d err=%v", visible, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != http.StatusCreated {
			t.Fatalf("ロック解放後の登録: status=%d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ロックを解放しても登録が終わらない")
	}
}

// 登録の途中（ロックを持ち、INSERT 済みでまだ未コミット）に窓の開始が来ると、窓の開始はその登録のコミットを待ち、
// コミット後のユーザーにも通知を作る。
func TestOpenPostingWindowWaitsForInFlightRegistration(t *testing.T) {
	db, do := newRegisterServer(t)
	if code, body := do(http.MethodPost, "/api/auth/register", `{"username":"existing","displayName":"e","region":"asia"}`); code != http.StatusCreated {
		t.Fatalf("事前の登録: %d %s", code, body)
	}
	tx := db.MustBegin()
	defer tx.Rollback()
	if _, err := tx.Exec(membershipLockSQL); err != nil {
		t.Fatal(err)
	}
	lateID := uuid.NewString()
	if _, err := tx.Exec(`INSERT INTO users (id, username, secret_key_hash, display_name, region) VALUES ($1,'inflight',$2,'i','asia')`, lateID, fmt.Sprintf("%064x", 1)); err != nil {
		t.Fatal(err)
	}
	type result struct {
		code int
		body []byte
	}
	done := make(chan result, 1)
	go func() {
		code, body := do(http.MethodPost, "/api/admin/posting-window", `{"region":"asia","localDate":"2026-11-01"}`)
		done <- result{code, body}
	}()
	select {
	case r := <-done:
		t.Fatalf("登録が未コミットなのに窓の開始が完了した（status=%d）", r.code)
	case <-time.After(400 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("登録をコミットしても窓の開始が終わらない")
	}
	if r.code != http.StatusCreated {
		t.Fatalf("窓の開始: status=%d body=%s", r.code, r.body)
	}
	var window struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(r.body, &window); err != nil {
		t.Fatal(err)
	}
	var missing int
	if err := db.Get(&missing, `SELECT count(*) FROM users u WHERE u.region='asia' AND u.status='active' AND NOT EXISTS (SELECT 1 FROM notifications n WHERE n.user_id=u.id AND n.posting_window_id=$1)`, window.ID); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("通知のないユーザーが %d 人いる（途中の登録 %s も含めて全員に作られるはず）", missing, lateID)
	}
}

// 窓の開始と登録が同時に来ても、エラーやデッドロックにならない（本番と同じ接続数の上限で）。
func TestRegisterConcurrentWithOpenPostingWindowNoErrors(t *testing.T) {
	_, do := newRegisterServer(t)
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	const iterations, registrations = 20, 12
	for iter := range iterations {
		var wg sync.WaitGroup
		start := make(chan struct{})
		codes := make(chan int, registrations+1)
		for k := range registrations {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				code, _ := do(http.MethodPost, "/api/auth/register", fmt.Sprintf(`{"username":"u_%d_%d","displayName":"u","region":"asia"}`, iter, k))
				codes <- code
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			code, _ := do(http.MethodPost, "/api/admin/posting-window", fmt.Sprintf(`{"region":"asia","localDate":%q}`, base.AddDate(0, 0, iter).Format(time.DateOnly)))
			codes <- code
		}()
		close(start)
		wg.Wait()
		close(codes)
		for code := range codes {
			if code != http.StatusCreated {
				t.Fatalf("iter %d: status=%d（want 201）", iter, code)
			}
		}
	}
}
