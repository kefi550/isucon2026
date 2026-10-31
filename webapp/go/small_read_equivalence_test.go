package isureal

// 検索・プロフィール・posts/me/current・ログアウトの往復削減の等価性テスト（軽量版）。
// ISUREAL_TEST_DSN を指定したときだけ実行する（指定先のスキーマは作り直される）。

import (
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func TestSmallReadAPIsEquivalence(t *testing.T) {
	db := openTestDB(t)
	rng := rand.New(rand.NewSource(10))
	data := seed(t, db, rng)

	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	session := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Set("session", &Session{UserID: c.Request().Header.Get("X-Test-User")})
			return next(c)
		}
	}
	e.GET("/old/users", refSearchUsersHandler(h), session)
	e.GET("/old/users/:userId", refGetUserHandler(h), session)
	e.GET("/old/posts/me/current", refGetMyCurrentPostHandler(h), session)
	e.GET("/api/users", searchUsersHandler(h), session)
	e.GET("/api/users/:userId", getUserHandler(h), session)
	e.GET("/api/posts/me/current", getMyCurrentPostHandler(h), session)

	codes := map[string]int{}
	compare := func(label, path, userID string) string {
		t.Helper()
		wc, wb := call(e, "/old"+path, userID)
		gc, gb := call(e, "/api"+path, userID)
		codes[label+" "+http.StatusText(wc)]++
		if wc != gc || wb != gb {
			t.Fatalf("不一致 %s user=%s\nwant(%d)=%s\n got(%d)=%s", path, userID, wc, wb, gc, gb)
		}
		return gb
	}

	active := append([]string{}, data.activeUserIDs...)
	rng.Shuffle(len(active), func(i, j int) { active[i], active[j] = active[j], active[i] })
	viewers := append(append([]string{}, active[:28]...), data.deletedUserIDs[:min(2, len(data.deletedUserIDs))]...)
	targets := append(append([]string{}, active[:30]...), data.deletedUserIDs...)
	targets = append(targets, uuid.NewString())

	for _, viewer := range viewers {
		compare("me_current", "/posts/me/current", viewer)
		for _, target := range targets {
			compare("user", "/users/"+target, viewer)
		}
		// 検索（ページ送りも含む）。"user" はほぼ全員に一致するので51人以上になりページ送りが起きる
		for _, q := range []string{"user", "User 1", "ser0", "1", "zzz", "USER00"} {
			query := url.Values{"q": {q}}
			for range 5 {
				body := compare("search", "/users?"+query.Encode(), viewer)
				var res struct {
					Items []struct {
						ID       string `json:"id"`
						Username string `json:"username"`
					} `json:"items"`
					HasMore bool `json:"hasMore"`
				}
				if json.Unmarshal([]byte(body), &res) != nil || !res.HasMore {
					break
				}
				last := res.Items[len(res.Items)-1]
				query = url.Values{"q": {q}, "beforeUsername": {last.Username}, "beforeId": {last.ID}}
			}
		}
	}
	compare("search", "/users?q=", viewers[0])
	compare("search", "/users?q=user&beforeId=x", viewers[0])

	for label, n := range codes {
		t.Logf("%-26s %5d 回", label, n)
	}
	for _, label := range []string{"me_current OK", "me_current Not Found", "user OK", "user Not Found", "search OK"} {
		if codes[label] == 0 {
			t.Errorf("%s のケースを1回も通っていない", label)
		}
	}

	// ログアウト: 指定したセッションだけが無効になり、2回目は何も起きず、存在しないユーザーはエラーになる
	userID := data.activeUserIDs[0]
	for _, token := range []string{"hash-a", "hash-b"} {
		if _, err := db.Exec(`INSERT INTO login_sessions (id, user_id, token_hash) VALUES ($1,$2,$3)`, uuid.NewString(), userID, token); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := InvalidateLoginSession(db, userID, "hash-a"); err != nil {
			t.Fatalf("ログアウトが失敗: %v", err)
		}
	}
	var states []struct {
		TokenHash string  `db:"token_hash"`
		Active    bool    `db:"active"`
		Reason    *string `db:"reason"`
		HasTime   bool    `db:"has_time"`
	}
	if err := db.Select(&states, `SELECT token_hash, active, reason, invalidated_at IS NOT NULL AS has_time FROM login_sessions WHERE user_id=$1 ORDER BY token_hash`, userID); err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0].Active || states[0].Reason == nil || *states[0].Reason != "logout" || !states[0].HasTime || !states[1].Active || states[1].Reason != nil {
		t.Fatalf("ログアウト後のセッションの状態が想定と違う: %+v", states)
	}
	newErr, refErr := InvalidateLoginSession(db, uuid.NewString(), "x"), refInvalidateLoginSession(db, uuid.NewString(), "x")
	if newErr == nil || refErr == nil || !errors.Is(newErr, errors.Unwrap(refErr)) {
		t.Fatalf("存在しないユーザーのエラーが修正前と違う: new=%v ref=%v", newErr, refErr)
	}
}
