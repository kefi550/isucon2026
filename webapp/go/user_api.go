package isureal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v5"
)

type registerRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Region      string `json:"region"`
}

type registerResponse struct {
	SecretKey string              `json:"secretKey"`
	User      sessionUserResponse `json:"user"`
}

type loginRequest struct {
	Username  string `json:"username"`
	SecretKey string `json:"secretKey"`
}

type loginResponse struct {
	Token string              `json:"token"`
	User  sessionUserResponse `json:"user"`
}

type sessionUserResponse struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Bio         string `json:"bio"`
	Region      string `json:"region"`
	AccountType string `json:"accountType"`
	Status      string `json:"status"`
}

type getMeResponse struct {
	sessionUserResponse
	ProfileImageURL *string `json:"profileImageUrl"`
}

type updateMeRequest struct {
	DisplayName *string `json:"displayName"`
	Bio         *string `json:"bio"`
	Region      *string `json:"region"`
}

type updateMeResponse getMeResponse

func newSessionUserResponse(user User) sessionUserResponse {
	return sessionUserResponse{
		ID: user.ID, Username: user.Username, DisplayName: user.DisplayName,
		Bio: user.Bio, Region: user.Region, AccountType: user.AccountType, Status: user.Status,
	}
}

func newGetMeResponse(user User) getMeResponse {
	response := getMeResponse{sessionUserResponse: newSessionUserResponse(user)}
	if user.ProfileImageID != nil {
		url := signedImageURLFor("profile", user, *user.ProfileImageID)
		response.ProfileImageURL = &url
	}
	return response
}

func registerHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input registerRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		c.Set("username", input.Username)
		if !usernamePattern.MatchString(input.Username) || !validUserText(input.DisplayName, 1, 100) || !validRegion(input.Region) {
			return NewAPIError(http.StatusBadRequest, "invalid_registration")
		}
		secretKey := uuid.NewString()
		hash := sha256.Sum256([]byte(secretKey))
		ctx := c.Request().Context()
		// 窓の開始が通知対象を確定するまで、登録のコミットを待たせる。ユーザー名の重複は UNIQUE が見る。
		// 「membership のロック → INSERT → COMMIT」を1文（autocommit）で実行する。別々に送ると、遅延の大きいリージョンでは
		// 往復の数だけロックを握り続け、同時に来た登録や窓の開始がその間待たされる（ロックはコミット時に解放される）。
		var user User
		if err := h.DB.GetContext(ctx, &user, `
   WITH membership AS (SELECT pg_advisory_xact_lock(hashtext('user_membership'), 1))
   INSERT INTO users (id, username, secret_key_hash, display_name, region, search_username, search_display_name)
   SELECT $1::text, $2::text, $3::text, $4::text, $5::text, $6::text, $7::text FROM membership
   RETURNING `+strings.Join(userColumns, ", "),
			uuid.NewString(), input.Username, hex.EncodeToString(hash[:]), input.DisplayName, input.Region,
			normalizeSearchText(input.Username), normalizeSearchText(input.DisplayName)); err != nil {
			if isUsernameConflict(err) {
				return NewAPIError(http.StatusConflict, "username_taken")
			}
			return err
		}
		if err := user.setImageKey(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, registerResponse{SecretKey: secretKey, User: newSessionUserResponse(user)})
	}
}

func loginHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input loginRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		c.Set("username", input.Username)
		if !usernamePattern.MatchString(input.Username) {
			return NewAPIError(http.StatusUnauthorized, "invalid_credentials")
		}
		hash := sha256.Sum256([]byte(input.SecretKey))
		token := uuid.NewString()
		tokenHash := sha256.Sum256([]byte(token))
		// 資格情報の確認からセッションの差し替えまでを、DB 関数の1往復で行う（004-functions.sql）
		var user User
		err := h.DB.GetContext(c.Request().Context(), &user, `
   SELECT id, username, display_name, bio, region, account_type, status
   FROM isureal_login($1, $2, $3, $4)
  `, input.Username, hex.EncodeToString(hash[:]), uuid.NewString(), hex.EncodeToString(tokenHash[:]))
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusUnauthorized, "invalid_credentials")
		}
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, loginResponse{Token: token, User: newSessionUserResponse(user)})
	}
}

func logoutHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		if err := InvalidateLoginSession(h.DB, session.UserID, session.TokenHash); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

func getMeHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		user, err := getUser(c.Request().Context(), h.DB, session.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, newGetMeResponse(user))
	}
}

func updateMeHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		var input updateMeRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.DisplayName == nil && input.Bio == nil && input.Region == nil) ||
			(input.DisplayName != nil && !validUserText(*input.DisplayName, 1, 100)) ||
			(input.Bio != nil && !validUserText(*input.Bio, 0, 500)) ||
			(input.Region != nil && !validRegion(*input.Region)) {
			return NewAPIError(http.StatusBadRequest, "invalid_profile")
		}
		ctx := c.Request().Context()
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if input.Region != nil {
			if err := lockUserMembership(ctx, tx); err != nil {
				return err
			}
		}
		if input.DisplayName != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET display_name = $2, search_display_name = $3, updated_at = clock_timestamp() WHERE id = $1`, session.UserID, *input.DisplayName, normalizeSearchText(*input.DisplayName)); err != nil {
				return err
			}
		}
		if input.Bio != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET bio = $2, updated_at = clock_timestamp() WHERE id = $1`, session.UserID, *input.Bio); err != nil {
				return err
			}
		}
		if input.Region != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET region = $2, updated_at = clock_timestamp() WHERE id = $1`, session.UserID, *input.Region); err != nil {
				return err
			}
		}
		user, err := getUser(ctx, tx, session.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, updateMeResponse(newGetMeResponse(user)))
	}
}

func isUsernameConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		(pgErr.ConstraintName == "users_username" || pgErr.ConstraintName == "users_username_key")
}
