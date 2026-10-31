package isureal

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

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
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		// ブロック関係（どちら向きでも）にある相手は SQL の段階で除く（従来は Go 側で読み飛ばしていた。選ばれる人と順番は同じ）。
		// ブロック一覧を別に取る往復が要らなくなる。
		var users []User
		if err := h.DB.SelectContext(ctx, &users, `SELECT `+strings.Join(userColumns, ", ")+` FROM users u WHERE status='active' AND id<>$1 AND ($2='' OR (username,id)>($2,$3))
		  AND NOT EXISTS (SELECT 1 FROM block_relations b WHERE (b.blocker_id=$1 AND b.blocked_id=u.id) OR (b.blocker_id=u.id AND b.blocked_id=$1))
		  ORDER BY username, id`, viewer, input.BeforeUsername, input.BeforeID); err != nil {
			return err
		}
		query := normalizeSearchText(input.Q)
		items := make([]relationshipUserResponse, 0)
		for _, user := range users {
			if !strings.Contains(normalizeSearchText(user.Username), query) && !strings.Contains(normalizeSearchText(user.DisplayName), query) {
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
