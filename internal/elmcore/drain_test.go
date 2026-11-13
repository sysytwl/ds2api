package elmcore

import "testing"

// The broker publishes these on /user/exchange/amq.direct/drain; the web app
// reconnects and republishes the stored query exactly once.
func TestWantsRetry(t *testing.T) {
	cases := []struct {
		body   string
		chatID string
		want   bool
	}{
		{`{"type":"RETRY_QUERY","chatId":"chat-1"}`, "chat-1", true},
		{`{"type":"RETRY_QUERY","chatId":"chat-2"}`, "chat-1", false},
		{`{"type":"RETRY_QUERY","chatId":null}`, "chat-1", false},
		{`{"type":"RECONNECT","chatId":null}`, "chat-1", true},
		{`{"type":"RECONNECT","chatId":"chat-1"}`, "chat-1", false},
		{`{"reply":"hi","replyType":"USER"}`, "chat-1", false},
		{`not json`, "chat-1", false},
	}
	for _, tc := range cases {
		if got := wantsRetry(tc.body, tc.chatID); got != tc.want {
			t.Fatalf("wantsRetry(%q, %q) = %v, want %v", tc.body, tc.chatID, got, tc.want)
		}
	}
}

func TestIsDrainNotice(t *testing.T) {
	if !isDrainNotice(`{"type":"RECONNECT","chatId":null}`) {
		t.Fatal("drain notice not recognised")
	}
	if isDrainNotice("hello") {
		t.Fatal("plain text mistaken for a drain notice")
	}
}
