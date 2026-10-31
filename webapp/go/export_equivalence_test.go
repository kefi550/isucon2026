package isureal

// エクスポートのパーティション絞り込みの等価性テスト。ISUREAL_TEST_DSN を指定したときだけ実行する（指定先のスキーマは作り直される）。

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// unzipForCompare は ZIP の各ファイルを名前→内容で返す（posts.json の署名は時刻依存なので伏せる）
func unzipForCompare(t *testing.T, body []byte) map[string]string {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("ZIP として読めない: %v (%q)", err, string(body[:min(len(body), 200)]))
	}
	files := map[string]string{}
	for _, f := range reader.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		if f.Name == "posts.json" {
			content = signaturePattern.ReplaceAllString(content, "?SIGNED")
		}
		files[fmt.Sprintf("%s(method=%d)", f.Name, f.Method)] = content
	}
	return files
}

func TestExportEquivalence(t *testing.T) {
	db := openTestDB(t)
	rng := rand.New(rand.NewSource(7))
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("seed 失敗: %v\n%s", err, query)
		}
	}
	// 投稿期間: 2024-09 から 2026-10 まで月ごとに1つ（最後だけ開いている）
	start := time.Date(2024, 9, 15, 0, 0, 0, 0, time.UTC)
	var windows []string
	var windowAt []time.Time
	for m := 0; m < 26; m++ {
		at := start.AddDate(0, m, 0)
		id := uuid.NewString()
		var endedAt *time.Time
		if m < 25 {
			e := at.Add(24 * time.Hour)
			endedAt = &e
		}
		exec(`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline, ended_at) VALUES ($1,'asia',$2,$3,$4,$5)`, id, at, at, at.Add(2*time.Minute), endedAt)
		windows = append(windows, id)
		windowAt = append(windowAt, at)
	}
	var userIDs []string
	for i := range 12 {
		id := uuid.NewString()
		key := make([]byte, 32)
		rng.Read(key)
		var profile *string
		if i%2 == 0 {
			s := uuid.NewString()
			profile = &s
		}
		exec(`INSERT INTO users (id, username, secret_key_hash, display_name, bio, profile_image_id, region) VALUES ($1,$2,$3,$4,'bio',$5,'asia')`, id, fmt.Sprintf("exp%02d", i), hex.EncodeToString(key), fmt.Sprintf("Exporter %d", i), profile)
		userIDs = append(userIDs, id)
	}
	// 投稿と画像（画像の created_at は投稿時刻と同じ。本番と同じ規則）。一部は削除済み、同時刻の投稿も作る
	for _, userID := range userIDs {
		for range rng.Intn(30) {
			w := rng.Intn(len(windows))
			postedAt := windowAt[w].Add(time.Duration(rng.Intn(5)) * time.Minute).Add(time.Duration(rng.Intn(1000)) * time.Microsecond)
			imageID, postID := uuid.NewString(), uuid.NewString()
			data := make([]byte, 100+rng.Intn(5000))
			rng.Read(data)
			exec(`INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at) VALUES ($1,$2,$3,'image/jpeg',$4)`, imageID, userID, data, postedAt)
			var deletedAt *time.Time
			if rng.Intn(6) == 0 {
				deletedAt = &postedAt
			}
			exec(`INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility, location_kind, latitude, longitude, posted_at, deleted_at) VALUES ($1,$2,$3,$4,$5,'public','exact',35.1,139.2,$6,$7)`,
				postID, userID, windows[w], imageID, fmt.Sprintf("caption %d", rng.Intn(100)), postedAt, deletedAt)
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
	e.GET("/old/me/export", refExportPostsHandler(h), session)
	e.GET("/api/me/export", exportPostsHandler(h), session)
	call := func(path, userID string) (int, http.Header, []byte) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Test-User", userID)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code, rec.Header(), rec.Body.Bytes()
	}

	totalImages := 0
	for _, userID := range userIDs {
		wc, wh, wb := call("/old/me/export", userID)
		gc, gh, gb := call("/api/me/export", userID)
		if wc != gc || wh.Get("Content-Type") != gh.Get("Content-Type") || wh.Get("Content-Disposition") != gh.Get("Content-Disposition") || wh.Get("Cache-Control") != gh.Get("Cache-Control") {
			t.Fatalf("user %s: ステータスかヘッダーが不一致 want=%d %v got=%d %v", userID, wc, wh, gc, gh)
		}
		want, got := unzipForCompare(t, wb), unzipForCompare(t, gb)
		if len(want) != len(got) {
			t.Fatalf("user %s: ZIP のファイル数が不一致 want=%d got=%d", userID, len(want), len(got))
		}
		names := make([]string, 0, len(want))
		for name := range want {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if want[name] != got[name] {
				t.Fatalf("user %s: %s の内容が不一致", userID, name)
			}
		}
		totalImages += len(want) - 1
	}
	t.Logf("ユーザー %d 人分のエクスポートが一致（画像 %d 枚）", len(userIDs), totalImages)
	if totalImages == 0 {
		t.Fatal("テストデータが空で検証になっていない")
	}
}
