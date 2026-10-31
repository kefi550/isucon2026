package isureal

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/labstack/echo/v5"
)

type exportPostResponse struct {
	createPostResponse
	ImagePath string `json:"imagePath"`
}

// 初期データの画像は、isu1 の画像置き場（ローカルのファイル）にある（image_api.go）。
// エクスポートは1回に百枚以上読むので、DB（TOAST）からではなくそこから並列に読む。読めなかった画像は DB から読む。
var exportImageDir = envOrDefault("ISUREAL_IMAGE_DIR", "/var/lib/isureal-images")

const exportImageFetchConcurrency = 16

// imageStoreAvailable は、このサーバーに画像置き場があるか（isu1 だけにある。isu2 / isu3 は DB から返す）。
// 起動時に1度だけ確認する。
var imageStoreAvailable = func() bool {
	st, err := os.Stat(exportImageDir)
	return err == nil && st.IsDir()
}()

func fetchStoredImage(imageID string) ([]byte, bool) {
	if !imageIDPattern.MatchString(imageID) {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(exportImageDir, imageID[:2], imageID))
	return data, err == nil
}

func exportPostsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		author, err := getUser(ctx, h.DB, userID)
		if err != nil {
			return err
		}
		var posts []struct {
			Post
			ImageData []byte `db:"image_data"`
			Initial   bool   `db:"initial"`
		}
		if err := h.DB.SelectContext(ctx, &posts, `
   SELECT `+postColumns+`,
     CASE WHEN $2::boolean AND i.created_at <= (SELECT b.created_at FROM initial_data_boundary b) THEN NULL ELSE i.data END AS image_data,
     ($2::boolean AND i.created_at <= (SELECT b.created_at FROM initial_data_boundary b)) AS initial
   FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   -- 投稿画像の created_at は投稿の posted_at と同じ値（投稿作成時に同じ時刻を入れている。初期データも全件一致を確認済み）。
   -- パーティションキーの created_at を結合条件に入れて、36 個すべてのパーティションを探さないようにする。
   JOIN image_objects i ON i.id = p.image_id AND i.created_at = p.posted_at
   WHERE p.author_id = $1 AND p.deleted_at IS NULL
   ORDER BY p.posted_at, p.id
  `, userID, imageStoreAvailable); err != nil {
			return err
		}

		// 初期データの画像を画像置き場から並列に取る。取れなかったものは DB から読む。
		var wg sync.WaitGroup
		sem := make(chan struct{}, exportImageFetchConcurrency)
		for i := range posts {
			if !posts[i].Initial {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				if data, ok := fetchStoredImage(posts[i].ImageID); ok {
					posts[i].ImageData = data
				}
			}(i)
		}
		wg.Wait()
		for i := range posts {
			if posts[i].Initial && posts[i].ImageData == nil {
				if err := h.DB.GetContext(ctx, &posts[i].ImageData, `SELECT data FROM image_objects WHERE id = $1 AND created_at = $2`, posts[i].ImageID, posts[i].PostedAt); err != nil {
					return err
				}
			}
		}

		var body bytes.Buffer
		writer := zip.NewWriter(&body)
		defer writer.Close()
		items := make([]exportPostResponse, 0, len(posts))
		for _, post := range posts {
			imagePath := fmt.Sprintf("images/%06d.jpg", len(items)+1)
			image, err := writer.CreateHeader(&zip.FileHeader{Name: imagePath, Method: zip.Store})
			if err != nil {
				return err
			}
			if _, err := image.Write(post.ImageData); err != nil {
				return err
			}
			items = append(items, exportPostResponse{createPostResponse: newPostResponse(post.Post, author), ImagePath: imagePath})
		}
		manifest, err := writer.Create("posts.json")
		if err != nil {
			return err
		}
		if err := json.NewEncoder(manifest).Encode(items); err != nil {
			return err
		}
		if err := writer.Close(); err != nil {
			return err
		}
		c.Response().Header().Set(echo.HeaderContentDisposition, `attachment; filename="isureal-posts.zip"`)
		c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
		return c.Blob(http.StatusOK, "application/zip", body.Bytes())
	}
}
