package isureal

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type searchUsersRequest struct {
	Q              string `query:"q"`
	BeforeUsername string `query:"beforeUsername"`
	BeforeID       string `query:"beforeId"`
}

type searchUsersResponse struct {
	Items   []relationshipUserResponse `json:"items"`
	HasMore bool                       `json:"hasMore"`
}

func normalizeSearchText(value string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(value)))
}

func userMatchesSearch(user User, query string) bool {
	return strings.Contains(normalizeSearchText(user.Username), query) || strings.Contains(normalizeSearchText(user.DisplayName), query)
}

// backfillUserSearchText は、正規化済みの検索用の列を、username と display_name から計算し直して埋める。
// 初期化では reset.sql が display_name を初期値に戻すので、init.sh のあとに呼ぶ。値が変わる行だけ更新する。
func backfillUserSearchText(ctx context.Context, db *sqlx.DB) error {
	var rows []struct {
		ID                string  `db:"id"`
		Username          string  `db:"username"`
		DisplayName       string  `db:"display_name"`
		SearchUsername    *string `db:"search_username"`
		SearchDisplayName *string `db:"search_display_name"`
	}
	if err := db.SelectContext(ctx, &rows, `SELECT id, username, display_name, search_username, search_display_name FROM users`); err != nil {
		return err
	}
	var ids, usernames, displayNames []string
	for _, row := range rows {
		username, displayName := normalizeSearchText(row.Username), normalizeSearchText(row.DisplayName)
		if row.SearchUsername != nil && *row.SearchUsername == username && row.SearchDisplayName != nil && *row.SearchDisplayName == displayName {
			continue
		}
		ids = append(ids, row.ID)
		usernames = append(usernames, username)
		displayNames = append(displayNames, displayName)
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, `
   UPDATE users u SET search_username = v.search_username, search_display_name = v.search_display_name
   FROM unnest($1::text[], $2::text[], $3::text[]) AS v(id, search_username, search_display_name)
   WHERE u.id = v.id
  `, ids, usernames, displayNames)
	return err
}

func searchUsersHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input searchUsersRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if !validUserText(input.Q, 1, 100) {
			return NewAPIError(http.StatusBadRequest, "invalid_relationship")
		}
		if (input.BeforeUsername == "") != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		query := normalizeSearchText(input.Q)
		items := make([]relationshipUserResponse, 0)
		// PostgreSQL の text は NUL を含められないので、NUL を含む検索語に一致するユーザーはいない。
		if strings.ContainsRune(query, 0) {
			return c.JSON(http.StatusOK, searchUsersResponse{Items: items})
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		// ブロック関係（どちら向きでも）にある相手は SQL の段階で除く（従来は Go 側で読み飛ばしていた。選ばれる人と順番は同じ）。
		// ブロック一覧を別に取る往復が要らなくなる。
		// 検索語の照合も、正規化済みの列（search_username / search_display_name）で SQL の段階で行い、51 件だけ取る。
		// UTF-8 では、バイト列の部分一致と文字列の部分一致が同じなので、strpos は strings.Contains と同じ結果になる。
		// 正規化済みの列が NULL の行は候補に残し、下の Go 側の照合で判定する。
		// そうした行が外れて 51 件に満たなくなったときは、LIMIT なしで取り直す。
		var users []User
		for _, limit := range []any{51, nil} {
			users = users[:0]
			if err := h.DB.SelectContext(ctx, &users, `SELECT `+strings.Join(userColumns, ", ")+` FROM users u WHERE status='active' AND id<>$1 AND ($2='' OR (username,id)>($2,$3))
		  AND (search_username IS NULL OR search_display_name IS NULL OR strpos(search_username, $4::text) > 0 OR strpos(search_display_name, $4::text) > 0)
		  AND NOT EXISTS (SELECT 1 FROM block_relations b WHERE (b.blocker_id=$1 AND b.blocked_id=u.id) OR (b.blocker_id=u.id AND b.blocked_id=$1))
		  ORDER BY username, id LIMIT $5`, viewer, input.BeforeUsername, input.BeforeID, query, limit); err != nil {
				return err
			}
			matched := 0
			for _, user := range users {
				if userMatchesSearch(user, query) {
					matched++
				}
			}
			if matched == len(users) || len(users) < 51 {
				break
			}
		}
		for _, user := range users {
			if !userMatchesSearch(user, query) {
				continue
			}
			if err := user.setImageKey(); err != nil {
				return err
			}
			if user.AccountType != "certified_public" {
				user.Bio = ""
			}
			items = append(items, newRelationshipUserResponse(user))
			if len(items) == 51 {
				break
			}
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, searchUsersResponse{Items: items, HasMore: hasMore})
	}
}

type getUserRequest struct {
	UserID string `param:"userId"`
}

type getUserResponse relationshipUserResponse

func getUserHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getUserRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		// 対象ユーザー・ブロック関係・友達関係を1往復で取る。判定の順番は従来どおり（見つからない → ブロック → 友達）。
		var row struct {
			User
			Blocked bool `db:"blocked"`
			Friend  bool `db:"friend"`
		}
		err := h.DB.GetContext(ctx, &row, `
   SELECT `+viewerUserColumns+`,
     EXISTS(SELECT 1 FROM block_relations b WHERE (b.blocker_id=$2 AND b.blocked_id=u.id) OR (b.blocker_id=u.id AND b.blocked_id=$2)) AS blocked,
     EXISTS(SELECT 1 FROM friendships f WHERE (f.user_low_id=$2 AND f.user_high_id=u.id) OR (f.user_low_id=u.id AND f.user_high_id=$2)) AS friend
   FROM users u WHERE u.id = $1 AND u.status = 'active'
  `, input.UserID, viewer)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err != nil {
			return err
		}
		user := row.User
		if err := user.setImageKey(); err != nil {
			return err
		}
		if row.Blocked {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if user.AccountType != "certified_public" && !row.Friend {
			user.Bio = ""
		}
		return c.JSON(http.StatusOK, getUserResponse(newRelationshipUserResponse(user)))
	}
}

func newRelationshipUserResponse(user User) relationshipUserResponse {
	return relationshipUserResponse{ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, ProfileImageURL: newGetMeResponse(user).ProfileImageURL, Bio: user.Bio, AccountType: user.AccountType}
}
