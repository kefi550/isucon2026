package isureal

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNotificationEventJSON(t *testing.T) {
	created := time.Date(2026, 10, 31, 2, 3, 4, 5000000, time.UTC)
	data, err := json.Marshal(getNotificationsResponse{
		ID:              "notif-1",
		PostingWindowID: "window-1",
		Region:          "europe",
		CreatedAt:       created,
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["id"] != "notif-1" || payload["postingWindowId"] != "window-1" || payload["region"] != "europe" {
		t.Fatalf("payload = %#v", payload)
	}
	if payload["createdAt"] != created.Format(time.RFC3339Nano) {
		t.Fatalf("createdAt = %#v", payload["createdAt"])
	}
}

func TestWindowMayNotify(t *testing.T) {
	connected := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	if !windowMayNotify(connected, connected.Add(time.Microsecond)) {
		t.Fatal("later window should be visible")
	}
	if windowMayNotify(connected, connected) {
		t.Fatal("window at the same instant must not be sent")
	}
	if windowMayNotify(connected, connected.Add(-time.Microsecond)) {
		t.Fatal("older window must not be sent")
	}
}

func TestDecideNotification(t *testing.T) {
	connected := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	later := connected.Add(time.Millisecond)
	user := &notificationUser{ID: "u", Region: "asia", Status: "active"}
	note := &notificationRow{ID: "n", PostingWindowID: "w", Region: "asia", CreatedAt: later}

	decision, body := decideNotification(connected, "asia", user, note)
	if decision != notificationSend || body.ID != "n" || body.PostingWindowID != "w" || body.Region != "asia" || !body.CreatedAt.Equal(later) {
		t.Fatalf("send = %v %#v", decision, body)
	}

	same := *note
	same.CreatedAt = connected
	if decision, _ = decideNotification(connected, "asia", user, &same); decision != notificationKeepWaiting {
		t.Fatalf("equal created_at = %v", decision)
	}

	earlier := *note
	earlier.CreatedAt = connected.Add(-time.Millisecond)
	if decision, _ = decideNotification(connected, "asia", user, &earlier); decision != notificationKeepWaiting {
		t.Fatalf("earlier created_at = %v", decision)
	}

	if decision, _ = decideNotification(connected, "asia", user, nil); decision != notificationKeepWaiting {
		t.Fatalf("missing notification = %v", decision)
	}

	inactive := *user
	inactive.Status = "deleted"
	if decision, _ = decideNotification(connected, "asia", &inactive, note); decision != notificationDisconnect {
		t.Fatalf("inactive = %v", decision)
	}
	if decision, _ = decideNotification(connected, "asia", &inactive, nil); decision != notificationDisconnect {
		t.Fatalf("inactive without notification = %v", decision)
	}
	if decision, _ = decideNotification(connected, "asia", nil, note); decision != notificationDisconnect {
		t.Fatalf("missing user = %v", decision)
	}

	moved := *user
	moved.Region = "europe"
	if decision, _ = decideNotification(connected, "asia", &moved, note); decision != notificationDisconnect {
		t.Fatalf("region change = %v", decision)
	}
}
