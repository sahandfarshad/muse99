package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// v0.9.16: what the model says between tool steps ("let me check the
// DNS…") is shown in the chat while it works, not only after the chat
// is reopened. The texts of the turn now running are kept here; the
// page polls /api/chat/live while it waits for the final reply.
var live struct {
	mu    sync.Mutex
	chat  string
	texts []string
}

func liveBegin(chatID string) {
	live.mu.Lock()
	live.chat, live.texts = chatID, nil
	live.mu.Unlock()
}

func liveSay(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	live.mu.Lock()
	live.texts = append(live.texts, text)
	live.mu.Unlock()
}

func liveTexts() (string, []string) {
	live.mu.Lock()
	defer live.mu.Unlock()
	return live.chat, append([]string(nil), live.texts...)
}

// handleLive serves GET /api/chat/live?from=N: the texts from N on.
func (s *webServer) handleLive(w http.ResponseWriter, r *http.Request) {
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	chat, texts := liveTexts()
	if from < 0 || from > len(texts) {
		from = len(texts)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"chat_id": chat, "texts": texts[from:], "total": len(texts)})
}
