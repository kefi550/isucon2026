package isureal

import (
	"bytes"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type replaceRealMojiRequest struct {
	PostID       string                `param:"postId" json:"-"`
	Image        *multipart.FileHeader `form:"image"`
	ReactionType string                `form:"reactionType"`
}

type replaceRealMojiResponse struct {
	ID           string                   `json:"id"`
	Reactor      relationshipUserResponse `json:"reactor"`
	ReactionType string                   `json:"reactionType"`
	ImageURL     string                   `json:"imageUrl"`
	CreatedAt    time.Time                `json:"createdAt"`
}

func newRealMojiResponse(reaction RealMoji, user User) replaceRealMojiResponse {
	return replaceRealMojiResponse{ID: reaction.ID, Reactor: newRelationshipUserResponse(user), ReactionType: reaction.ReactionType, ImageURL: signedImageURL(user, reaction.ImageID), CreatedAt: reaction.CreatedAt}
}

func replaceRealMojiHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input replaceRealMojiRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Image == nil {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		switch input.ReactionType {
		case "like", "smile", "surprise", "heart", "laugh", "instant":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		file, err := input.Image.Open()
		if err != nil {
			return err
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			return err
		}
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		if img.Bounds().Dx() > 512 || img.Bounds().Dy() > 512 {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}

		ctx := c.Request().Context()
		// 投稿の確認から画像の保存、権限の確認、置き換えまでを、DB 関数の1往復で行う（004-functions.sql）
		var reaction RealMojiWithReactor
		err = h.DB.GetContext(ctx, &reaction, `
   SELECT id, post_id, reactor_id, image_id, reaction_type, created_at, replaced_at, deleted_at,
          user_id AS "reactor.id", username AS "reactor.username", secret_key_hash AS "reactor.secret_key_hash",
          display_name AS "reactor.display_name", bio AS "reactor.bio", profile_image_id AS "reactor.profile_image_id",
          region AS "reactor.region", account_type AS "reactor.account_type", status AS "reactor.status"
   FROM isureal_replace_realmoji($1, $2, $3, $4, $5, $6)
  `, uuid.NewString(), uuid.NewString(), input.PostID, GetSession(c).UserID, data, input.ReactionType)
		if err != nil {
			return dbFunctionError(err)
		}
		if err := reaction.Reactor.setImageKey(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, newRealMojiResponse(reaction.RealMoji, reaction.Reactor))
	}
}

type getRealMojisRequest struct {
	PostID          string     `param:"postId"`
	BeforeCreatedAt *time.Time `query:"beforeCreatedAt"`
	BeforeID        string     `query:"beforeId"`
}

type getRealMojisResponse struct {
	Items   []replaceRealMojiResponse `json:"items"`
	HasMore bool                      `json:"hasMore"`
}

func getRealMojisHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getRealMojisRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.BeforeCreatedAt == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		// 権限の判定と RealMoji の取得を並列に投げる。権限がなければ RealMoji は捨てる。
		// エラーは従来の逐次処理と同じ順番（権限の判定 → 403 → RealMoji の取得）で判定する。
		var (
			allowed      bool
			allowedErr   error
			reactions    []RealMojiWithReactor
			reactionsErr error
			wg           sync.WaitGroup
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			allowed, allowedErr = canViewPost(ctx, h.DB, GetSession(c).UserID, input.PostID)
		}()
		go func() {
			defer wg.Done()
			reactions, reactionsErr = selectCurrentRealMojis(ctx, h.DB, `AND r.post_id=$1 AND ($2::timestamptz IS NULL OR (r.created_at,r.id)<($2,$3)) ORDER BY r.created_at DESC,r.id DESC LIMIT 51`, input.PostID, input.BeforeCreatedAt, input.BeforeID)
		}()
		wg.Wait()
		if allowedErr != nil {
			return allowedErr
		}
		if !allowed {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		if reactionsErr != nil {
			return reactionsErr
		}
		items := make([]replaceRealMojiResponse, 0, len(reactions))
		for _, reaction := range reactions {
			items = append(items, newRealMojiResponse(reaction.RealMoji, reaction.Reactor))
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, getRealMojisResponse{Items: items, HasMore: hasMore})
	}
}

type deleteRealMojiRequest struct {
	RealMojiID string `param:"realMojiId" json:"-"`
}

func deleteRealMojiHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteRealMojiRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if _, err := h.DB.ExecContext(c.Request().Context(), `SELECT isureal_delete_realmoji($1, $2)`, input.RealMojiID, GetSession(c).UserID); err != nil {
			return dbFunctionError(err)
		}
		return c.NoContent(http.StatusNoContent)
	}
}
