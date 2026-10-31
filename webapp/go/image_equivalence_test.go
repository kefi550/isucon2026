package isureal

// 画像取得のクエリ統合の等価性テスト。ISUREAL_TEST_DSN を指定したときだけ実行する（指定先のスキーマは作り直される）。

import (
	"encoding/hex"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

func TestGetImageEquivalence(t *testing.T) {
	db := openTestDB(t)
	rng := rand.New(rand.NewSource(3))
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("seed 失敗: %v\n%s", err, query)
		}
	}
	base := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	windowID := uuid.NewString()
	exec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline) VALUES ($1,'asia',$2,$3,$3)`, windowID, base, base)

	// ユーザー（一部は退会済み）
	type seededUser struct {
		User
		deleted bool
	}
	var users []seededUser
	for i := range 30 {
		key := make([]byte, 32)
		rng.Read(key)
		u := seededUser{User: User{ID: uuid.NewString(), SecretKeyHash: hex.EncodeToString(key)}, deleted: rng.Intn(6) == 0}
		if err := u.setImageKey(); err != nil {
			t.Fatal(err)
		}
		status := "active"
		if u.deleted {
			status = "deleted"
		}
		exec(`INSERT INTO users (id, username, secret_key_hash, display_name, region, status) VALUES ($1,$2,$3,'u','asia',$4)`, u.ID, fmt.Sprintf("img%02d", i), u.SecretKeyHash, status)
		users = append(users, u)
	}

	// 画像: プロフィール画像・投稿画像・RealMoji 画像・どこからも参照されない画像を、有効/削除済みの両方で作る
	type seededImage struct {
		id    string
		owner seededUser
	}
	var images []seededImage
	newImage := func(owner seededUser) string {
		id := uuid.NewString()
		data := []byte("jpeg-" + id)
		exec(`INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at) VALUES ($1,$2,$3,'image/jpeg',$4)`, id, owner.ID, data, base.Add(time.Duration(rng.Intn(1000))*time.Hour*-1))
		images = append(images, seededImage{id: id, owner: owner})
		return id
	}
	var postIDs []string
	for _, u := range users {
		if rng.Intn(2) == 0 {
			exec(`UPDATE users SET profile_image_id=$2 WHERE id=$1`, u.ID, newImage(u))
		}
		newImage(u) // 参照されない画像（差し替え前のプロフィール画像などに相当）
		for range 2 {
			postID := uuid.NewString()
			var deletedAt *time.Time
			if rng.Intn(3) == 0 {
				deletedAt = &base
			}
			exec(`INSERT INTO posts (id, author_id, posting_window_id, image_id, visibility, location_kind, posted_at, deleted_at) VALUES ($1,$2,$3,$4,'public','none',$5,$6)`,
				postID, u.ID, windowID, newImage(u), base, deletedAt)
			postIDs = append(postIDs, postID)
		}
	}
	for range 60 {
		reactor := users[rng.Intn(len(users))]
		var replacedAt, deletedAt *time.Time
		switch rng.Intn(4) {
		case 0:
			replacedAt = &base
		case 1:
			deletedAt = &base
		}
		postID := postIDs[rng.Intn(len(postIDs))]
		if replacedAt == nil && deletedAt == nil {
			var exists bool
			if err := db.Get(&exists, `SELECT EXISTS(SELECT 1 FROM realmojis WHERE post_id=$1 AND reactor_id=$2 AND replaced_at IS NULL AND deleted_at IS NULL)`, postID, reactor.ID); err != nil {
				t.Fatal(err)
			}
			if exists {
				replacedAt = &base
			}
		}
		exec(`INSERT INTO realmojis (id, post_id, reactor_id, image_id, reaction_type, created_at, replaced_at, deleted_at) VALUES ($1,$2,$3,$4,'like',$5,$6,$7)`,
			uuid.NewString(), postID, reactor.ID, newImage(reactor), base, replacedAt, deletedAt)
	}

	// 新しい実装は本番と同じく、計画を使い回す設定（force_generic_plan）の接続で画像を取る
	imageDSN, err := url.Parse(os.Getenv("ISUREAL_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	q := imageDSN.Query()
	q.Set("plan_cache_mode", "force_generic_plan")
	imageDSN.RawQuery = q.Encode()
	imageDB, err := sqlx.Open("pgx", imageDSN.String())
	if err != nil {
		t.Fatal(err)
	}
	defer imageDB.Close()
	var mode string
	if err := imageDB.Get(&mode, `SHOW plan_cache_mode`); err != nil || mode != "force_generic_plan" {
		t.Fatalf("画像用の接続で plan_cache_mode が設定されていない: %q %v", mode, err)
	}
	h := &HandlerContext{DB: db, ImageDB: imageDB, Config: &Config{Region: "asia"}}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	// abuse の記録（guard の BAN 判定に使われる）も比較するため、レスポンスヘッダーに書き出す
	abuse := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			err := next(c)
			if v, _ := c.Get("abuse").(string); v != "" && err != nil {
				c.Response().Header().Set("X-Test-Abuse", v)
			}
			return err
		}
	}
	e.GET("/old/images/:imageId", refGetImageHandler(h), abuse)
	e.GET("/api/images/:imageId", getImageHandler(h), abuse)

	callImage := func(path string) (int, string, string, string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code, rec.Header().Get("Content-Type") + "|" + rec.Header().Get("Cache-Control"), rec.Header().Get("X-Test-Abuse"), rec.Body.String()
	}
	compared, ok200, forbidden, abused := 0, 0, 0, 0
	compare := func(path string) {
		t.Helper()
		wc, wh, wa, wb := callImage("/old" + path)
		gc, gh, ga, gb := callImage("/api" + path)
		compared++
		if wc != gc || wh != gh || wa != ga || wb != gb {
			t.Fatalf("不一致 path=%s\nwant=%d %s abuse=%q body=%q\n got=%d %s abuse=%q body=%q", path, wc, wh, wa, wb, gc, gh, ga, gb)
		}
		switch {
		case gc == http.StatusOK:
			ok200++
		case ga != "":
			abused++
		default:
			forbidden++
		}
	}

	// 比較するパスは1周目で作り、2周目も同じものを使う（2周目は created_at のキャッシュが効く経路になる）
	clearImageCreatedAtCache()
	var paths []string
	for _, img := range images {
		signed := signedImageURL(img.owner.User, img.id)
		paths = append(paths, strings.TrimPrefix(signed, "/api")) // 正しい署名
		other := users[rng.Intn(len(users))]
		paths = append(paths, strings.TrimPrefix(signedImageURL(other.User, img.id), "/api")) // 他人のキーで署名（多くは不正署名）
		exp := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
		paths = append(paths, "/images/"+img.id+"?exp="+exp+"&sig="+strings.Repeat("A", 43)) // 長さは正しいが不正な署名
		paths = append(paths, "/images/"+img.id+"?exp=1&sig="+strings.Repeat("A", 43))       // 期限切れ
	}
	paths = append(paths,
		"/images/"+uuid.NewString()+"?exp=9999999999&sig="+strings.Repeat("A", 43), // 存在しない画像
		"/images/x",                // パラメーター不足
		"/images/x?exp=abc&sig=zz", // 形式不正
	)
	for _, path := range paths {
		compare(path)
	}
	t.Logf("1周目: 比較 %d 回（200: %d、不正署名: %d、その他 403 など: %d）", compared, ok200, abused, forbidden)
	if ok200 == 0 || abused == 0 || forbidden == 0 {
		t.Fatal("テストデータが偏っていて検証になっていない")
	}
	cached := len(imageCreatedAtCache)
	if cached == 0 {
		t.Fatal("1周目で created_at がキャッシュされていない")
	}

	// キャッシュが残ったまま DB の状態を変え、2周目も新旧が一致することを確かめる。
	// 特に、行が消えた画像に不正な署名でアクセスしても abuse を記録しない（署名検証の前に 403）ことを確かめる。
	exec(`DELETE FROM image_objects WHERE id IN (SELECT id FROM image_objects ORDER BY id LIMIT 40)`)
	exec(`UPDATE posts SET deleted_at = now() WHERE id IN (SELECT id FROM posts WHERE deleted_at IS NULL ORDER BY id LIMIT 10)`)
	exec(`UPDATE realmojis SET replaced_at = now() WHERE id IN (SELECT id FROM realmojis WHERE replaced_at IS NULL AND deleted_at IS NULL ORDER BY id LIMIT 10)`)
	exec(`UPDATE users SET status = 'deleted' WHERE id IN (SELECT id FROM users WHERE status = 'active' ORDER BY id LIMIT 3)`)
	exec(`UPDATE users SET profile_image_id = NULL WHERE id IN (SELECT id FROM users WHERE profile_image_id IS NOT NULL ORDER BY id LIMIT 3)`)
	compared, ok200, abused, forbidden = 0, 0, 0, 0
	for _, path := range paths {
		compare(path)
	}
	t.Logf("2周目（キャッシュ %d 件が残った状態）: 比較 %d 回（200: %d、不正署名: %d、その他 403 など: %d）", cached, compared, ok200, abused, forbidden)
	if ok200 == 0 || abused == 0 || forbidden == 0 {
		t.Fatal("2周目のテストデータが偏っていて検証になっていない")
	}
}
