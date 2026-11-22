package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestV0103LimitReport(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(b, &req)
		calls++
		w.Header().Set("Content-Type", "application/json")
		if req["tools"] == nil {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Wrote 5 files. Left: index.html."}}]}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_time","arguments":"{}"}}]}}]}`)
	}))
	defer srv.Close()
	setChatStepLimit(5)
	defer setChatStepLimit(0)
	c := NewClient(srv.URL, "k", "m")
	r := &toolRunner{workdir: t.TempDir()}
	var msgs []Message
	reply, used := runTurnDetailed(c, r, toolDefinitions(), &msgs, "build it")
	if len(used) != 5 || !strings.Contains(reply, "continue") || !strings.Contains(reply, "Left: index.html") {
		t.Fatalf("reply %q used %d calls %d", reply, len(used), calls)
	}
	if msgs[len(msgs)-1].Role != "assistant" {
		t.Fatal("report not kept in history")
	}
}
