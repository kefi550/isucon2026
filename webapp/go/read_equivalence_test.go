package isureal

// 読み取り系 API（投稿詳細・コメント一覧・RealMoji 一覧）の並列化の等価性テスト。
// ISUREAL_TEST_DSN を指定したときだけ実行する（指定先のスキーマは作り直される）。

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

func TestReadAPIsEquivalence(t *testing.T) {
	db := openTestDB(t)
	rng := rand.New(rand.NewSource(9))
	data := seed(t, db, rng)

	// ページ送りを確かめるため、有効な投稿の一部にコメントと RealMoji を60件ずつ追加する
	var busyPosts []string
	if err := db.Select(&busyPosts, `SELECT id FROM posts WHERE deleted_at IS NULL ORDER BY id LIMIT 4`); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)
	for _, postID := range busyPosts {
		for i := range 60 {
			at := base.Add(time.Duration(i%20) * time.Second) // 同時刻の行も作る
			if _, err := db.Exec(`INSERT INTO comments (id, post_id, author_id, body, created_at) VALUES ($1,$2,$3,'busy',$4)`, uuid.NewString(), postID, data.activeUserIDs[i%len(data.activeUserIDs)], at); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO realmojis (id, post_id, reactor_id, image_id, reaction_type, created_at, replaced_at) VALUES ($1,$2,$3,$4,'smile',$5,$5)`, uuid.NewString(), postID, data.activeUserIDs[i%len(data.activeUserIDs)], uuid.NewString(), at); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO realmojis (id, post_id, reactor_id, image_id, reaction_type, created_at)
			  SELECT $1::text,$2::text,$3::text,$4::text,'heart',$5::timestamptz WHERE NOT EXISTS (SELECT 1 FROM realmojis WHERE post_id=$2::text AND reactor_id=$3::text AND replaced_at IS NULL AND deleted_at IS NULL)`,
				uuid.NewString(), postID, data.activeUserIDs[(i*7)%len(data.activeUserIDs)], uuid.NewString(), at); err != nil {
				t.Fatal(err)
			}
		}
	}

	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	session := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Set("session", &Session{UserID: c.Request().Header.Get("X-Test-User")})
			return next(c)
		}
	}
	e.GET("/old/posts/:postId", refGetPostHandler(h), session)
	e.GET("/old/posts/:postId/comments", refGetCommentsHandler(h), session)
	e.GET("/old/posts/:postId/realmojis", refGetRealMojisHandler(h), session)
	e.GET("/api/posts/:postId", getPostHandler(h), session)
	e.GET("/api/posts/:postId/comments", getCommentsHandler(h), session)
	e.GET("/api/posts/:postId/realmojis", getRealMojisHandler(h), session)

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

	var postIDs []string
	if err := db.Select(&postIDs, `SELECT id FROM posts ORDER BY id`); err != nil {
		t.Fatal(err)
	}
	rng.Shuffle(len(postIDs), func(i, j int) { postIDs[i], postIDs[j] = postIDs[j], postIDs[i] })
	// 重くなりすぎないよう、閲覧者と投稿は一部に絞る（それでも公開範囲・友達・ブロック・削除・終了済みの組み合わせは十分に通る）
	targets := append(append([]string{}, busyPosts...), postIDs[:20]...)
	targets = append(targets, uuid.NewString()) // 存在しない投稿
	active := append([]string{}, data.activeUserIDs...)
	rng.Shuffle(len(active), func(i, j int) { active[i], active[j] = active[j], active[i] })
	viewers := append(active[:25], data.deletedUserIDs[:min(5, len(data.deletedUserIDs))]...)

	for _, viewer := range viewers {
		for _, postID := range targets {
			compare("post", "/posts/"+postID, viewer)

			// コメント一覧（作成順、後ろ向きのカーソル）
			query := url.Values{}
			for range 4 {
				body := compare("comments", "/posts/"+postID+"/comments?"+query.Encode(), viewer)
				var res struct {
					Items []struct {
						ID        string    `json:"id"`
						CreatedAt time.Time `json:"createdAt"`
					} `json:"items"`
					HasMore bool `json:"hasMore"`
				}
				if json.Unmarshal([]byte(body), &res) != nil || !res.HasMore {
					break
				}
				last := res.Items[len(res.Items)-1]
				query = url.Values{"afterCreatedAt": {last.CreatedAt.Format(time.RFC3339Nano)}, "afterId": {last.ID}}
			}

			// RealMoji 一覧（新しい順、前向きのカーソル）
			query = url.Values{}
			for range 4 {
				body := compare("realmojis", "/posts/"+postID+"/realmojis?"+query.Encode(), viewer)
				var res struct {
					Items []struct {
						ID        string    `json:"id"`
						CreatedAt time.Time `json:"createdAt"`
					} `json:"items"`
					HasMore bool `json:"hasMore"`
				}
				if json.Unmarshal([]byte(body), &res) != nil || !res.HasMore {
					break
				}
				last := res.Items[len(res.Items)-1]
				query = url.Values{"beforeCreatedAt": {last.CreatedAt.Format(time.RFC3339Nano)}, "beforeId": {last.ID}}
			}
		}
	}
	// 不正なページ指定
	compare("comments", "/posts/"+busyPosts[0]+"/comments?afterId=x", data.activeUserIDs[0])
	compare("realmojis", "/posts/"+busyPosts[0]+"/realmojis?beforeId=x", data.activeUserIDs[0])

	for label, n := range codes {
		t.Logf("%-28s %6d 回", label, n)
	}
	for _, label := range []string{"post OK", "post Not Found", "comments OK", "comments Forbidden", "realmojis OK", "realmojis Forbidden"} {
		if codes[label] == 0 {
			t.Errorf("%s のケースを1回も通っていない", label)
		}
	}
}
