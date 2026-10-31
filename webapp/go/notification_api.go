package isureal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

type getNotificationsResponse struct {
	ID              string    `json:"id" db:"id"`
	PostingWindowID string    `json:"postingWindowId" db:"posting_window_id"`
	Region          string    `json:"region" db:"region"`
	CreatedAt       time.Time `json:"createdAt" db:"created_at"`
}

func getNotificationsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		session := GetSession(c)
		var connectedAt time.Time
		if err := h.DB.GetContext(ctx, &connectedAt, `SELECT clock_timestamp()`); err != nil {
			return err
		}
		response := c.Response()
		response.Header().Set("Content-Type", "text/event-stream")
		response.WriteHeader(http.StatusOK)
		controller := http.NewResponseController(response)
		if err := controller.Flush(); err != nil {
			return nil
		}

		hub := h.notificationHub()
		sub := hub.subscribe(session.UserID, session.Region, connectedAt)
		defer hub.unsubscribe(sub)

		var delivery notificationDelivery
		select {
		case <-ctx.Done():
			select {
			case delivery = <-sub.ch:
			default:
				return nil
			}
		case delivery = <-sub.ch:
		}
		if !delivery.send {
			return nil
		}
		data, err := json.Marshal(delivery.body)
		if err != nil {
			c.Logger().Error("failed to encode notification", "error", err)
			return nil
		}
		if _, err := fmt.Fprintf(response, "data: %s\n\n", data); err != nil {
			return nil
		}
		_ = controller.Flush()
		return nil
	}
}
