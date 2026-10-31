package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type getImageRequest struct {
	ImageID string `param:"imageId" json:"-"`
	Exp     string `query:"exp" json:"-"`
	Sig     string `query:"sig" json:"-"`
}

// 初期データの画像（initial_data_boundary 以前に作られたもの）は、asia2 の画像置き場にファイルとして置いてある。
// 各サーバーの nginx が /api/images/ に X-Image-Accel: 1 を付けて転送したときだけ、初期データの画像は
// データを DB から読まず、X-Accel-Redirect で nginx に配信を任せる（置き場にないときは nginx が DB 経由の内部エンドポイントに回す）。
// ヘッダーがなければ（nginx の設定がない・クライアントが直接アクセスした場合など）、従来どおりアプリが DB から返す。
const imageStoreHeader = "X-Image-Accel"

// imageStorePathPrefix は nginx の internal ロケーション。asia2 の画像置き場へ proxy する。
const imageStorePathPrefix = "/_images/"

// imageIDPattern は X-Accel-Redirect のパスに埋め込める画像 ID（uuid.NewString() の形式）。
// これ以外の ID は、パスを組み立てずにアプリが直接返す。
var imageIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// imageLookupTemplate は画像の所有者の署名キー・参照状態・データを1本で取るクエリ。%s はデータの取り方。
// 呼び出し側で末尾に条件を足せるよう、WHERE 句で終わらせている。
const imageLookupTemplate = `
   SELECT u.secret_key_hash, a.available, %s AS data, i.media_type, i.created_at,
          (i.created_at <= (SELECT b.created_at FROM initial_data_boundary b)) AS initial
   FROM image_objects i JOIN users u ON u.id = i.owner_user_id
   CROSS JOIN LATERAL (
     SELECT COALESCE(u.profile_image_id = i.id, false)
       OR EXISTS (
         SELECT 1 FROM posts p
         WHERE p.image_id = i.id AND p.author_id = u.id AND p.deleted_at IS NULL
       )
       OR EXISTS (
         SELECT 1 FROM realmojis r
         JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
         JOIN users author ON author.id = p.author_id AND author.status = 'active'
         WHERE r.image_id = i.id AND r.reactor_id = u.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
       ) AS available
     OFFSET 0 -- 展開されると available を参照するたびに EXISTS が再評価されるため、1回だけ評価させる
   ) a
   WHERE i.id = $1 AND u.status = 'active'`

var (
	// imageLookupQuery は参照中のときだけデータを読む（従来どおり）。
	imageLookupQuery = fmt.Sprintf(imageLookupTemplate, `CASE WHEN a.available THEN i.data END`)
	// imageLookupQueryForStore は、初期データの画像のデータは読まない（画像置き場から返すため）。
	imageLookupQueryForStore = fmt.Sprintf(imageLookupTemplate,
		`CASE WHEN a.available AND NOT (i.created_at <= (SELECT b.created_at FROM initial_data_boundary b)) THEN i.data END`)
)

// 画像 ID → created_at のキャッシュ（このプロセスのメモリ内。サーバー間では共有しない）。
// created_at は作成後に変わらず ID も使い回されないので、値が古くなることはない。
// 行の有無はキャッシュに頼らず毎回 DB で確認する。
var (
	imageCreatedAtMu    sync.RWMutex
	imageCreatedAtCache = map[string]time.Time{}
)

func loadImageCreatedAt(imageID string) (time.Time, bool) {
	imageCreatedAtMu.RLock()
	createdAt, ok := imageCreatedAtCache[imageID]
	imageCreatedAtMu.RUnlock()
	return createdAt, ok
}

func storeImageCreatedAt(imageID string, createdAt time.Time) {
	imageCreatedAtMu.Lock()
	imageCreatedAtCache[imageID] = createdAt
	imageCreatedAtMu.Unlock()
}

// clearImageCreatedAtCache は初期化時に呼び、初期化で消える画像のぶんのメモリを解放する
func clearImageCreatedAtCache() {
	imageCreatedAtMu.Lock()
	imageCreatedAtCache = map[string]time.Time{}
	imageCreatedAtMu.Unlock()
}

func getImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-store")
		var input getImageRequest
		if err := c.Bind(&input); err != nil {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		query, err := url.ParseQuery(c.Request().URL.RawQuery)
		if err != nil || len(query["exp"]) != 1 || len(query["sig"]) != 1 {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		exp, err := strconv.ParseInt(input.Exp, 10, 64)
		if err != nil || strconv.FormatInt(exp, 10) != input.Exp || exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		sig, err := base64.RawURLEncoding.Strict().DecodeString(input.Sig)
		if err != nil || len(sig) != sha256.Size || base64.RawURLEncoding.EncodeToString(sig) != input.Sig {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}

		ctx := c.Request().Context()
		// 画像置き場に任せてよい経路か（nginx 経由で、ID が X-Accel-Redirect のパスに使える形式）
		forStore := imageStoreAvailable && c.Request().Header.Get(imageStoreHeader) == "1" && imageIDPattern.MatchString(input.ImageID)
		lookup := imageLookupQuery
		if forStore {
			lookup = imageLookupQueryForStore
		}
		// 署名キーの取得と、削除状態（まだ参照されているか）の確認を1往復で行う。
		// 削除状態は毎回確認する（即時反映も5秒以内の許容範囲に含まれる）。
		// 画像データは参照中のときだけ返し、無効な画像のデータを転送しないようにする。
		var object struct {
			SecretKeyHash string    `db:"secret_key_hash"`
			Available     bool      `db:"available"`
			Data          []byte    `db:"data"`
			MediaType     string    `db:"media_type"`
			CreatedAt     time.Time `db:"created_at"`
			Initial       bool      `db:"initial"`
		}
		// created_at（パーティションキー）が分かっていれば、1つのパーティションだけを引く。
		// 分からなければ全パーティションから引き、次回のために created_at を覚えておく。
		// どちらの経路でも画像の行の有無は毎回 DB で確認するので、行が消えた画像は従来どおり署名検証の前に 403 になる。
		if createdAt, ok := loadImageCreatedAt(input.ImageID); ok {
			err = h.imageDB().GetContext(ctx, &object, lookup+` AND i.created_at = $2`, input.ImageID, createdAt)
		} else {
			err = h.imageDB().GetContext(ctx, &object, lookup, input.ImageID)
			if err == nil {
				storeImageCreatedAt(input.ImageID, object.CreatedAt)
			}
		}
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if err != nil {
			return err
		}
		key, err := imageSigningKey(object.SecretKeyHash)
		if err != nil {
			return err
		}
		if !hmac.Equal(sig, imageSignature(key, input.ImageID, exp)) {
			c.Set("abuse", "invalid_signature")
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}

		// 署名は正しいが、削除済みなどで参照されていない画像
		if !object.Available {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if forStore && object.Initial {
			// 検証はここまでで済んでいる。画像のデータは、各サーバーの nginx が画像置き場（asia2）から返す。
			c.Response().Header().Set(echo.HeaderContentType, object.MediaType)
			c.Response().Header().Set("X-Accel-Redirect", imageStorePathPrefix+input.ImageID)
			return c.NoContent(http.StatusOK)
		}
		return c.Blob(http.StatusOK, object.MediaType, object.Data)
	}
}

// isLoopbackRequest は、同じサーバー上の nginx からのリクエストかを返す。
// X-Client-IP は偽装できるため使わず、接続元のアドレスで判定する。
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// getImageDataHandler は、画像置き場にファイルがなかったときに nginx が回してくる内部エンドポイント（DB から返す）。
// 署名や削除状態の検証は、X-Accel-Redirect を返す前に getImageHandler が済ませているので、ここでは行わない。
// そのため外部から直接呼べないよう、同じサーバーの loopback からの接続だけを受け付ける。
func getImageDataHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		imageID := c.Param("imageId")
		if !isLoopbackRequest(c.Request()) || !imageIDPattern.MatchString(imageID) {
			return NewAPIError(http.StatusNotFound, "not_found")
		}
		var object struct {
			Data      []byte `db:"data"`
			MediaType string `db:"media_type"`
		}
		var err error
		ctx := c.Request().Context()
		// created_at が分かっていれば、1つのパーティションだけを引く（検証のときに覚えたもの）
		if createdAt, ok := loadImageCreatedAt(imageID); ok {
			err = h.imageDB().GetContext(ctx, &object, `SELECT data, media_type FROM image_objects WHERE id = $1 AND created_at = $2`, imageID, createdAt)
		} else {
			err = h.imageDB().GetContext(ctx, &object, `SELECT data, media_type FROM image_objects WHERE id = $1`, imageID)
		}
		if errors.Is(err, sql.ErrNoRows) {
			// 検証の後に画像が消えた場合。5秒の猶予の範囲内で、画像を返さない。
			return NewAPIError(http.StatusNotFound, "not_found")
		}
		if err != nil {
			return err
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.Blob(http.StatusOK, object.MediaType, object.Data)
	}
}

// imageDB は画像取得に使う接続プールを返す（専用のプールが無ければ通常のプール）
func (h *HandlerContext) imageDB() *sqlx.DB {
	if h.ImageDB != nil {
		return h.ImageDB
	}
	return h.DB
}
