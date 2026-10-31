package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"
)

type getImageRequest struct {
	ImageID string `param:"imageId" json:"-"`
	Exp     string `query:"exp" json:"-"`
	Sig     string `query:"sig" json:"-"`
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
		// 署名キーの取得と、削除状態（まだ参照されているか）の確認を1往復で行う。
		// 削除状態は毎回確認する（即時反映も5秒以内の許容範囲に含まれる）。
		// 画像データは参照中のときだけ返し、無効な画像のデータを転送しないようにする。
		var object struct {
			SecretKeyHash string `db:"secret_key_hash"`
			Available     bool   `db:"available"`
			Data          []byte `db:"data"`
			MediaType     string `db:"media_type"`
		}
		err = h.DB.GetContext(ctx, &object, `
   SELECT u.secret_key_hash, a.available, CASE WHEN a.available THEN i.data END AS data, i.media_type
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
   WHERE i.id = $1 AND u.status = 'active'
  `, input.ImageID)
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
		return c.Blob(http.StatusOK, object.MediaType, object.Data)
	}
}
