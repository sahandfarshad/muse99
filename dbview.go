package main

// v0.9.6: the Database tab. Lists what is stored in muse99.db (one
// "bucket" per kind of thing), shows entries, and deletes the ones the
// user picks. Deleting a chat also deletes its messages and its
// Behind the scenes history.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.etcd.io/bbolt"
)

var dbBucketInfo = map[string]string{
	"chats":      "Your chats (title, dates)",
	"messages":   "Every message of every chat",
	"memories":   "Facts Muse99 remembers about you",
	"meta":       "Token usage totals and settings",
	"modelLists": "Saved model lists per provider",
	"activity":   "Behind the scenes history, per chat",
}

func dbPreview(v []byte, n int) string {
	s := string(v)
	if !utf8.ValidString(s) {
		return "(binary, " + strconv.Itoa(len(v)) + " bytes)"
	}
	var m map[string]any
	if json.Unmarshal(v, &m) == nil {
		for _, k := range []string{"title", "content", "text", "fact"} {
			if t, ok := m[k].(string); ok && strings.TrimSpace(t) != "" {
				s = t
				break
			}
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	return truncate(s, n)
}

// GET    /api/db                          -> buckets with counts and sizes
// GET    /api/db?bucket=B&prefix=P&q=Q    -> up to 300 entries (key, size, preview)
// GET    /api/db?bucket=B&key=K           -> one entry, full value
// DELETE /api/db?bucket=B&key=K           -> delete one entry
// DELETE /api/db?bucket=B&prefix=P        -> delete every entry under P (P required, except bucket activity)
func (s *webServer) handleDB(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
	}
	if s.db == nil || s.db.bolt == nil {
		fail(http.StatusServiceUnavailable, "no database open")
		return
	}
	q := r.URL.Query()
	bucket, key, prefix := q.Get("bucket"), q.Get("key"), q.Get("prefix")
	if bucket != "" {
		if _, ok := dbBucketInfo[bucket]; !ok {
			fail(http.StatusBadRequest, "unknown bucket")
			return
		}
	}
	if r.Method == http.MethodDelete {
		if bucket == "" || (key == "" && prefix == "" && bucket != "activity") {
			fail(http.StatusBadRequest, "need bucket and key (or prefix)")
			return
		}
		n := 0
		if bucket == "chats" && key != "" {
			if err := s.db.DeleteChat(key); err != nil {
				fail(http.StatusInternalServerError, err.Error())
				return
			}
			if s.currentChatID == key {
				s.messages = []Message{{Role: "system", Content: s.system}}
				s.currentChatID = ""
			}
			activity.clearChat(key)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "deleted": 1})
			return
		}
		err := s.db.bolt.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket([]byte(bucket))
			if b == nil {
				return nil
			}
			if key != "" {
				if b.Get([]byte(key)) != nil {
					n = 1
				}
				return b.Delete([]byte(key))
			}
			var keys [][]byte
			c := b.Cursor()
			for k, _ := c.Seek([]byte(prefix)); k != nil && bytes.HasPrefix(k, []byte(prefix)); k, _ = c.Next() {
				keys = append(keys, append([]byte{}, k...))
			}
			for _, k := range keys {
				_ = b.Delete(k)
			}
			n = len(keys)
			return nil
		})
		if err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}
		if bucket == "activity" {
			if prefix == "" {
				activity.clear()
			} else {
				activity.clearChat(strings.TrimSuffix(prefix, "/"))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "deleted": n})
		return
	}
	if bucket == "" {
		type info struct {
			Name  string `json:"name"`
			About string `json:"about"`
			Count int    `json:"count"`
			Bytes int    `json:"bytes"`
		}
		var out []info
		_ = s.db.bolt.View(func(tx *bbolt.Tx) error {
			for _, name := range []string{"chats", "messages", "memories", "activity", "meta", "modelLists"} {
				it := info{Name: name, About: dbBucketInfo[name]}
				if b := tx.Bucket([]byte(name)); b != nil {
					_ = b.ForEach(func(k, v []byte) error {
						it.Count++
						it.Bytes += len(k) + len(v)
						return nil
					})
				}
				out = append(out, it)
			}
			return nil
		})
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "path": s.db.path, "buckets": out})
		return
	}
	if key != "" {
		var val []byte
		_ = s.db.bolt.View(func(tx *bbolt.Tx) error {
			if b := tx.Bucket([]byte(bucket)); b != nil {
				val = append([]byte{}, b.Get([]byte(key))...)
			}
			return nil
		})
		text := string(val)
		var pretty bytes.Buffer
		if json.Indent(&pretty, val, "", "  ") == nil {
			text = pretty.String()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "key": key, "value": redactActivity(truncate(text, 50000))})
		return
	}
	needle := strings.ToLower(q.Get("q"))
	type entry struct {
		Key     string `json:"key"`
		Size    int    `json:"size"`
		Preview string `json:"preview"`
	}
	var out []entry
	total := 0
	_ = s.db.bolt.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Seek([]byte(prefix)); k != nil && bytes.HasPrefix(k, []byte(prefix)); k, v = c.Next() {
			if needle != "" && !strings.Contains(strings.ToLower(string(k)+" "+string(v)), needle) {
				continue
			}
			total++
			if len(out) < 300 {
				out = append(out, entry{Key: string(k), Size: len(v), Preview: redactActivity(dbPreview(v, 160))})
			}
		}
		return nil
	})
	if out == nil {
		out = []entry{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "entries": out, "total": total})
}
