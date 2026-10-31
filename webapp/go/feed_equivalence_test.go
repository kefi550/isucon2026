package isureal

// フィード高速化の等価性テスト。
// ISUREAL_TEST_DSN（例: postgres://isureal:isureal@127.0.0.1:55432/isureal）を指定したときだけ実行する。
// 指定した DB のスキーマは作り直されるので、本番や共有の DB には絶対に向けないこと。

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// signaturePattern は画像 URL の署名部分（exp は時刻依存なので比較対象外にする）
var signaturePattern = regexp.MustCompile(`\?exp=\d+\\u0026sig=[A-Za-z0-9_-]+|\?exp=\d+&sig=[A-Za-z0-9_-]+`)

type seededData struct {
	activeUserIDs  []string
	deletedUserIDs []string
	postedAts      []time.Time
}

func openTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN が未設定なのでスキップ")
	}
	db, err := sqlx.Open("pgx", dsn)
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

// seed は乱数（シード固定）でユーザー・関係・投稿期間・投稿・リアクションを作る
func seed(t *testing.T, db *sqlx.DB, rng *rand.Rand) seededData {
	t.Helper()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("seed 失敗: %v\n%s", err, query)
		}
	}
	regions := []string{"asia", "europe", "america"}
	var data seededData
	const userCount = 160
	userIDs := make([]string, userCount)
	userRegion := map[string]string{}
	for i := range userIDs {
		id := uuid.NewString()
		userIDs[i] = id
		key := make([]byte, 32)
		rng.Read(key)
		region := regions[rng.Intn(3)]
		userRegion[id] = region
		accountType := "regular"
		if rng.Intn(5) == 0 {
			accountType = "certified_public"
		}
		status := "active"
		if rng.Intn(15) == 0 {
			status = "deleted"
			data.deletedUserIDs = append(data.deletedUserIDs, id)
		} else {
			data.activeUserIDs = append(data.activeUserIDs, id)
		}
		var profileImageID *string
		if rng.Intn(2) == 0 {
			s := uuid.NewString()
			profileImageID = &s
		}
		exec(`INSERT INTO users (id, username, secret_key_hash, display_name, bio, profile_image_id, region, account_type, status) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			id, fmt.Sprintf("user%03d", i), hex.EncodeToString(key), fmt.Sprintf("User %d", i), fmt.Sprintf("bio %d", i), profileImageID, region, accountType, status)
	}

	// 友達関係（友達の友達が十分にできる密度）
	pairSeen := map[[2]string]bool{}
	for range userCount * 4 {
		a, b := userIDs[rng.Intn(userCount)], userIDs[rng.Intn(userCount)]
		if a == b {
			continue
		}
		low, high := min(a, b), max(a, b)
		if pairSeen[[2]string{low, high}] {
			continue
		}
		pairSeen[[2]string{low, high}] = true
		exec(`INSERT INTO friendships (id, user_low_id, user_high_id) VALUES ($1,$2,$3)`, uuid.NewString(), low, high)
	}
	// ブロック（友達関係と重なるものも含める）
	blockSeen := map[[2]string]bool{}
	for range userCount / 2 {
		a, b := userIDs[rng.Intn(userCount)], userIDs[rng.Intn(userCount)]
		if a == b || blockSeen[[2]string{a, b}] {
			continue
		}
		blockSeen[[2]string{a, b}] = true
		exec(`INSERT INTO block_relations (id, blocker_id, blocked_id) VALUES ($1,$2,$3)`, uuid.NewString(), a, b)
	}
	// フォロー
	followSeen := map[[2]string]bool{}
	for range userCount * 2 {
		a, b := userIDs[rng.Intn(userCount)], userIDs[rng.Intn(userCount)]
		if a == b || followSeen[[2]string{a, b}] {
			continue
		}
		followSeen[[2]string{a, b}] = true
		exec(`INSERT INTO follows (id, follower_id, followee_id) VALUES ($1,$2,$3)`, uuid.NewString(), a, b)
	}

	// 投稿期間: 各リージョンに終了済み1つと開いている1つ。america だけ開いている期間の日付をずらす。
	base := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	currentWindow := map[string]string{}
	endedWindow := map[string]string{}
	windowDate := map[string]time.Time{}
	for _, region := range regions {
		date, endedDate := base, base.AddDate(0, 0, -1)
		if region == "america" {
			// america の開いている期間は他リージョンの終了済み期間と同じ日付にする（HasPostedToday の境界検証用）
			date, endedDate = base.AddDate(0, 0, -1), base.AddDate(0, 0, -2)
		}
		ended := uuid.NewString()
		exec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline, ended_at) VALUES ($1,$2,$3,$4,$5,$6)`,
			ended, region, endedDate, base.Add(-24*time.Hour), base.Add(-23*time.Hour), base)
		endedWindow[region] = ended
		current := uuid.NewString()
		exec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline) VALUES ($1,$2,$3,$4,$5)`,
			current, region, date, base, base.Add(2*time.Minute))
		currentWindow[region] = current
		windowDate[current] = date
	}

	// 投稿: 同時刻の投稿をわざと作り、(posted_at, id) の並びも検証する
	var postIDs []string
	visibilities := []string{"public", "friends", "friends_of_friends"}
	for _, id := range userIDs {
		for range 1 + rng.Intn(2) {
			region := userRegion[id]
			if rng.Intn(6) == 0 {
				// リージョン変更後のユーザーを模して、別リージョンの期間に投稿させる
				region = regions[rng.Intn(3)]
			}
			window := currentWindow[region]
			if rng.Intn(4) == 0 {
				window = endedWindow[region]
			}
			postedAt := base.Add(time.Duration(rng.Intn(40)) * 10 * time.Second)
			data.postedAts = append(data.postedAts, postedAt)
			var deletedAt *time.Time
			if rng.Intn(8) == 0 {
				d := postedAt.Add(time.Minute)
				deletedAt = &d
			}
			postID := uuid.NewString()
			postIDs = append(postIDs, postID)
			exec(`INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility, location_kind, posted_at, deleted_at) VALUES ($1,$2,$3,$4,$5,$6,'none',$7,$8)`,
				postID, id, window, uuid.NewString(), "caption", visibilities[rng.Intn(3)], postedAt, deletedAt)
		}
	}
	// friends-of-friends のスコア計算に効く RealMoji とコメント（削除・置換済みも混ぜる）
	activeRealMoji := map[[2]string]bool{}
	for range len(postIDs) * 3 {
		postID, userID := postIDs[rng.Intn(len(postIDs))], userIDs[rng.Intn(userCount)]
		at := base.Add(time.Duration(rng.Intn(3600)) * time.Second)
		if rng.Intn(2) == 0 {
			var deletedAt *time.Time
			if rng.Intn(5) == 0 {
				deletedAt = &at
			}
			exec(`INSERT INTO comments (id, post_id, author_id, body, created_at, deleted_at) VALUES ($1,$2,$3,'hi',$4,$5)`, uuid.NewString(), postID, userID, at, deletedAt)
		} else {
			var replacedAt *time.Time
			pair := [2]string{postID, userID}
			// 有効な RealMoji は (post, reactor) ごとに1つまで（部分 UNIQUE インデックス）なので、2件目以降は置換済みにする
			if rng.Intn(5) == 0 || activeRealMoji[pair] {
				replacedAt = &at
			} else {
				activeRealMoji[pair] = true
			}
			exec(`INSERT INTO realmojis (id, post_id, reactor_id, image_id, reaction_type, created_at, replaced_at) VALUES ($1,$2,$3,$4,'like',$5,$6)`,
				uuid.NewString(), postID, userID, uuid.NewString(), at, replacedAt)
		}
	}
	return data
}

// newTestServer は旧実装を /old/feeds/:feed、新実装を /api/feeds/:feed に載せたテスト用サーバーを作る。
// セッションは X-Test-User ヘッダーのユーザーIDをそのまま使う。
func newTestServer(db *sqlx.DB) *echo.Echo {
	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	session := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Set("session", &Session{UserID: c.Request().Header.Get("X-Test-User")})
			return next(c)
		}
	}
	e.GET("/old/feeds/:feed", refGetFeedHandler(h), session)
	e.GET("/api/feeds/:feed", getFeedHandler(h), session)
	return e
}

func call(e *echo.Echo, path, userID string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Test-User", userID)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code, signaturePattern.ReplaceAllString(rec.Body.String(), "?SIGNED")
}

func TestLoadViewerContextEquivalence(t *testing.T) {
	db := openTestDB(t)
	data := seed(t, db, rand.New(rand.NewSource(1)))
	ctx := context.Background()
	for _, id := range append(append([]string{}, data.activeUserIDs...), data.deletedUserIDs...) {
		want, wantErr := refLoadViewerContext(ctx, db, id)
		got, gotErr := loadViewerContext(ctx, db, id)
		if wantErr != nil {
			// 退会済みユーザーは、どちらも sql.ErrNoRows になること
			if !errors.Is(wantErr, sql.ErrNoRows) || !errors.Is(gotErr, sql.ErrNoRows) {
				t.Fatalf("user %s: エラーが不一致 want=%v got=%v", id, wantErr, gotErr)
			}
			continue
		}
		if gotErr != nil {
			t.Fatalf("user %s: 予期しないエラー %v", id, gotErr)
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("user %s: viewerContext が不一致\nwant=%+v\n got=%+v", id, want, got)
		}
	}
}

func TestFeedEquivalence(t *testing.T) {
	db := openTestDB(t)
	rng := rand.New(rand.NewSource(2))
	data := seed(t, db, rng)
	e := newTestServer(db)
	feeds := []string{"public", "friends", "following", "friends-of-friends"}
	users := append(append([]string{}, data.activeUserIDs...), data.deletedUserIDs...)
	compared, nonEmpty, paged := 0, 0, 0

	compare := func(feed, userID string, query url.Values) (string, bool) {
		t.Helper()
		path := "/feeds/" + feed
		if len(query) > 0 {
			path += "?" + query.Encode()
		}
		wantCode, want := call(e, "/old"+path, userID)
		gotCode, got := call(e, "/api"+path, userID)
		compared++
		if wantCode != gotCode || want != got {
			t.Fatalf("不一致 user=%s path=%s\nwant(%d)=%s\n got(%d)=%s", userID, path, wantCode, want, gotCode, got)
		}
		return got, wantCode == http.StatusOK
	}

	for _, userID := range users {
		for _, feed := range feeds {
			// ページ送り（新実装の最後の要素をカーソルにして、新旧に同じカーソルを渡す）
			query := url.Values{}
			for page := 0; page < 10; page++ {
				body, ok := compare(feed, userID, query)
				if !ok {
					break
				}
				var res struct {
					Items []struct {
						ID       string    `json:"id"`
						PostedAt time.Time `json:"postedAt"`
						Score    *int64    `json:"score"`
					} `json:"items"`
					HasMore bool `json:"hasMore"`
				}
				if err := json.Unmarshal([]byte(body), &res); err != nil {
					t.Fatal(err)
				}
				if len(res.Items) > 0 {
					nonEmpty++
				}
				if !res.HasMore {
					break
				}
				paged++
				last := res.Items[len(res.Items)-1]
				query = url.Values{"beforePostedAt": {last.PostedAt.Format(time.RFC3339Nano)}, "beforeId": {last.ID}}
				if last.Score != nil {
					query.Set("beforeScore", fmt.Sprint(*last.Score))
				}
			}
			// 任意の境界カーソル（同時刻の投稿の途中から始まるケースを含む）
			at := data.postedAts[rng.Intn(len(data.postedAts))]
			cursor := url.Values{"beforePostedAt": {at.Format(time.RFC3339Nano)}, "beforeId": {uuid.NewString()}}
			if feed == "friends-of-friends" {
				cursor.Set("beforeScore", fmt.Sprint(rng.Intn(60)))
			}
			compare(feed, userID, cursor)
		}
	}
	// 不正なパラメーターも同じエラーになること
	compare("unknown", data.activeUserIDs[0], nil)
	compare("public", data.activeUserIDs[0], url.Values{"beforeId": {"x"}})
	compare("friends-of-friends", data.activeUserIDs[0], url.Values{"beforePostedAt": {time.Now().Format(time.RFC3339)}, "beforeId": {"x"}})

	t.Logf("比較 %d 回（空でない結果 %d 回、次ページあり %d 回）", compared, nonEmpty, paged)
	if nonEmpty == 0 || paged == 0 {
		t.Fatal("テストデータが薄すぎて検証になっていない")
	}
}
