package main

// v0.9.6: "Behind the scenes" events are saved per chat in the
// database (bucket "activity", key "<chat>/<id>"), so each chat keeps
// its own history across restarts. At most activityPerChat per chat.

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"go.etcd.io/bbolt"
)

var bucketActivity = []byte("activity")

const activityPerChat = 500

var (
	actChatMu sync.Mutex
	actChat   string
)

func setActivityChat(id string) {
	actChatMu.Lock()
	actChat = id
	actChatMu.Unlock()
}

func currentActivityChat() string {
	actChatMu.Lock()
	defer actChatMu.Unlock()
	return actChat
}

func activityKey(ev *ActivityEvent) string {
	chat := ev.Chat
	if chat == "" {
		chat = "_other"
		if strings.HasPrefix(ev.Source, "task") {
			chat = "_tasks"
		}
	}
	return fmt.Sprintf("%s/%020d", chat, ev.ID)
}

// saveActivityEvent writes ev (called with the activity lock held).
func saveActivityEvent(ev *ActivityEvent) {
	if appDB == nil || appDB.bolt == nil {
		return
	}
	if ev.key == "" {
		ev.key = activityKey(ev)
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	key := ev.key
	prefix := key[:strings.Index(key, "/")+1]
	_ = appDB.bolt.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketActivity)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(key), data); err != nil {
			return err
		}
		// Prune the oldest past the cap (keys sort by id).
		var keys [][]byte
		c := b.Cursor()
		for k, _ := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, _ = c.Next() {
			keys = append(keys, append([]byte{}, k...))
		}
		for i := 0; i < len(keys)-activityPerChat; i++ {
			_ = b.Delete(keys[i])
		}
		return nil
	})
}

// LoadActivity returns a chat's last n saved events, oldest first.
func (d *DB) LoadActivity(chat string, n int) []ActivityEvent {
	var out []ActivityEvent
	prefix := chat + "/"
	_ = d.bolt.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketActivity)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, v = c.Next() {
			var ev ActivityEvent
			if json.Unmarshal(v, &ev) == nil {
				if ev.Pending {
					// Left over from a run that stopped mid-step.
					ev.Pending = false
					ev.OK = false
				}
				out = append(out, ev)
			}
		}
		return nil
	})
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// DeleteActivity removes every saved event of a chat.
func (d *DB) DeleteActivity(chat string) error {
	prefix := chat + "/"
	return d.bolt.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketActivity)
		if b == nil {
			return nil
		}
		var keys [][]byte
		c := b.Cursor()
		for k, _ := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, _ = c.Next() {
			keys = append(keys, append([]byte{}, k...))
		}
		for _, k := range keys {
			_ = b.Delete(k)
		}
		return nil
	})
}
