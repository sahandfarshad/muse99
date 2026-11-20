package main

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestActivityPerChatSavedAndDBView(t *testing.T) {
	d, err := OpenDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	old := appDB
	appDB = d
	defer func() { appDB = old }()
	ch, _ := d.CreateChat("one")
	setActivityChat(ch.ID)
	id := activity.add("chat", "model", "Waiting…", "", true)
	activity.finish(id, true, "Model wrote the answer", "hello")
	setActivityChat("")
	activity.add("chat", "user", "elsewhere", "", false)

	evs := d.LoadActivity(ch.ID, 100)
	if len(evs) != 1 || evs[0].Title != "Model wrote the answer" || evs[0].Chat != ch.ID {
		t.Fatalf("saved events: %+v", evs)
	}
	s := &webServer{db: d}
	rec := httptest.NewRecorder()
	s.handleDB(rec, httptest.NewRequest("GET", "/api/db", nil))
	if !strings.Contains(rec.Body.String(), `"activity"`) {
		t.Fatal(rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleDB(rec, httptest.NewRequest("GET", "/api/db?bucket=activity&prefix="+ch.ID+"/", nil))
	var lst struct{ Entries []struct{ Key string } }
	_ = json.Unmarshal(rec.Body.Bytes(), &lst)
	if len(lst.Entries) != 1 {
		t.Fatal(rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleDB(rec, httptest.NewRequest("DELETE", "/api/db?bucket=chats&key="+ch.ID, nil))
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatal(rec.Body.String())
	}
	if n := len(d.LoadActivity(ch.ID, 100)); n != 0 {
		t.Fatalf("activity left after chat delete: %d", n)
	}
	rec = httptest.NewRecorder()
	s.handleDB(rec, httptest.NewRequest("DELETE", "/api/db?bucket=memories", nil))
	if rec.Code != 400 {
		t.Fatal("bucket-wide delete without prefix must be refused")
	}
}
