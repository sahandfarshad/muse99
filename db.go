package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

// Lightweight embedded database (v3).
//
// One file: ~/.muse99/data/muse99.db, using bbolt — a pure-Go
// key/value store, no CGO, no server, no daemon. This is the
// deliberate choice for a 1 GB server:
//
//   - modernc.org/sqlite was tried first (the task's preference), but
//     its current release (v1.62) needs the Go 1.26 toolchain, while
//     this project and its GitHub Actions CI pin Go 1.23. Pulling a
//     newer toolchain into every CI/release build just for the DB
//     was not worth it, and the driver also adds ~10 MB to the binary.
//   - bbolt v1.3.10 builds on Go 1.23, adds ~1 MB to the binary, and
//     uses a few MB of RAM. The DB file is mmap'd, so RSS roughly
//     follows the file size — a few MB for a personal agent.
//
// Buckets (like tables): chats, messages, memories, meta.
// Values are JSON, keys are plain strings, so the file stays
// human-inspectable with any bbolt dump tool.
//
// What lives here: chats + their messages (side chats, persistent
// across restarts), memories (the advanced memory model), and the
// token-usage totals (migrated from usage.json on first run).
// Tasks, servers and goals stay in their JSON files in data/ —
// they are small and were already working.

var (
	bucketChats    = []byte("chats")
	bucketMessages = []byte("messages")
	bucketMemories = []byte("memories")
	bucketMeta     = []byte("meta")
)

type Chat struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	MsgCount       int       `json:"msg_count"`
	LastSummaryMsg int       `json:"last_summary_msg"`
}

type StoredMessage struct {
	Seq       int        `json:"seq"`
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// Memory is one durable fact about the user.
type Memory struct {
	ID           string    `json:"id"`
	Text         string    `json:"text"`
	Kind         string    `json:"kind"` // preference | fact | decision | project | person
	SourceChatID string    `json:"source_chat_id,omitempty"`
	Confidence   float64   `json:"confidence"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type DB struct {
	bolt *bbolt.DB
	path string
	mu   sync.Mutex
}

// appDB is the process-wide database, set by main(). It is nil in
// unit tests that do not open one, and every helper below tolerates
// a nil DB by falling back to the old file-based behaviour.
var appDB *DB

func dbPath() string { return filepath.Join(dataDir(), "muse99.db") }

func openAppDB() (*DB, error) {
	d, err := OpenDB(dbPath())
	if err != nil {
		return nil, err
	}
	appDB = d
	// First run: migrate the old usage.json totals, and import any
	// facts already in MEMORY.md (only if the DB has no memories yet).
	if u := loadUsageFromJSONFile(); u.Calls > 0 {
		if existing, ok := d.loadUsageDB(); !ok || existing.Calls == 0 {
			_ = d.saveUsageDB(u)
		}
	}
	if mems, _ := d.ListMemories(); len(mems) == 0 {
		if _, err := os.Stat(defaultMemoryPath(mustCwd())); err == nil {
			_, _ = d.ImportMemoriesFromFile(defaultMemoryPath(mustCwd()))
		}
	}
	return d, nil
}

func OpenDB(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	bolt, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	d := &DB{bolt: bolt, path: path}
	err = bolt.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{bucketChats, bucketMessages, bucketMemories, bucketMeta, bucketModelLists} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = bolt.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) Close() error {
	if d == nil || d.bolt == nil {
		return nil
	}
	return d.bolt.Close()
}

func (d *DB) Path() string { return d.path }

// ---------- chats ----------

func newChatID() string { return fmt.Sprintf("chat-%d", time.Now().UnixNano()) }

func (d *DB) CreateChat(title string) (Chat, error) {
	if strings.TrimSpace(title) == "" {
		title = "New chat"
	}
	now := time.Now()
	c := Chat{ID: newChatID(), Title: title, CreatedAt: now, UpdatedAt: now}
	err := d.bolt.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketChats)
		data, _ := json.Marshal(c)
		return b.Put([]byte(c.ID), data)
	})
	return c, err
}

func (d *DB) ListChats() ([]Chat, error) {
	var out []Chat
	err := d.bolt.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketChats).ForEach(func(k, v []byte) error {
			var c Chat
			if err := json.Unmarshal(v, &c); err == nil {
				out = append(out, c)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, err
}

func (d *DB) GetChat(id string) (Chat, error) {
	var c Chat
	err := d.bolt.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketChats).Get([]byte(id))
		if v == nil {
			return fmt.Errorf("chat not found: %s", id)
		}
		return json.Unmarshal(v, &c)
	})
	return c, err
}

// DefaultChat returns the most recently used chat, creating one if needed.
func (d *DB) DefaultChat() (Chat, error) {
	chats, err := d.ListChats()
	if err != nil {
		return Chat{}, err
	}
	if len(chats) > 0 {
		return chats[0], nil
	}
	return d.CreateChat("Chat 1")
}

func (d *DB) RenameChat(id, title string) error {
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("title is empty")
	}
	return d.bolt.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketChats)
		v := b.Get([]byte(id))
		if v == nil {
			return fmt.Errorf("chat not found: %s", id)
		}
		var c Chat
		if err := json.Unmarshal(v, &c); err != nil {
			return err
		}
		c.Title = title
		c.UpdatedAt = time.Now()
		data, _ := json.Marshal(c)
		return b.Put([]byte(id), data)
	})
}

func (d *DB) DeleteChat(id string) error {
	return d.bolt.Update(func(tx *bbolt.Tx) error {
		if err := tx.Bucket(bucketChats).Delete([]byte(id)); err != nil {
			return err
		}
		// Delete all messages for this chat (keys are "chatID/00000001").
		mb := tx.Bucket(bucketMessages)
		prefix := []byte(id + "/")
		c := mb.Cursor()
		var keys [][]byte
		for k, _ := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, _ = c.Next() {
			keys = append(keys, append([]byte{}, k...))
		}
		for _, k := range keys {
			_ = mb.Delete(k)
		}
		return nil
	})
}

// AddMessage appends one message to a chat. The first user message
// becomes the chat title (first 40 chars) when the title is still
// the default. Tool messages are stored too, so a turn replays
// exactly on reload.
func (d *DB) AddMessage(chatID string, m Message) error {
	if strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0 {
		return nil
	}
	return d.bolt.Update(func(tx *bbolt.Tx) error {
		cb := tx.Bucket(bucketChats)
		v := cb.Get([]byte(chatID))
		if v == nil {
			return fmt.Errorf("chat not found: %s", chatID)
		}
		var c Chat
		if err := json.Unmarshal(v, &c); err != nil {
			return err
		}
		c.MsgCount++
		c.UpdatedAt = time.Now()
		if m.Role == "user" && (c.Title == "New chat" || c.Title == "Chat 1") && c.MsgCount <= 2 {
			t := strings.TrimSpace(m.Content)
			if len(t) > 40 {
				t = t[:40] + "…"
			}
			if t != "" {
				c.Title = t
			}
		}
		data, _ := json.Marshal(c)
		if err := cb.Put([]byte(chatID), data); err != nil {
			return err
		}
		sm := StoredMessage{Seq: c.MsgCount, Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls, CreatedAt: time.Now()}
		mdata, _ := json.Marshal(sm)
		key := fmt.Sprintf("%s/%08d", chatID, sm.Seq)
		return tx.Bucket(bucketMessages).Put([]byte(key), mdata)
	})
}

// LoadMessages returns the last `limit` messages of a chat, in order.
// limit <= 0 means 100. System messages are never stored, so callers
// prepend their own system prompt.
func (d *DB) LoadMessages(chatID string, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 100
	}
	var all []StoredMessage
	err := d.bolt.View(func(tx *bbolt.Tx) error {
		mb := tx.Bucket(bucketMessages)
		prefix := []byte(chatID + "/")
		c := mb.Cursor()
		for k, v := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, v = c.Next() {
			var sm StoredMessage
			if err := json.Unmarshal(v, &sm); err == nil {
				all = append(all, sm)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	out := make([]Message, 0, len(all))
	for _, sm := range all {
		out = append(out, Message{Role: sm.Role, Content: sm.Content, ToolCalls: sm.ToolCalls})
	}
	return out, nil
}

func (d *DB) ClearChat(id string) error {
	return d.bolt.Update(func(tx *bbolt.Tx) error {
		mb := tx.Bucket(bucketMessages)
		prefix := []byte(id + "/")
		c := mb.Cursor()
		var keys [][]byte
		for k, _ := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, _ = c.Next() {
			keys = append(keys, append([]byte{}, k...))
		}
		for _, k := range keys {
			_ = mb.Delete(k)
		}
		cb := tx.Bucket(bucketChats)
		v := cb.Get([]byte(id))
		if v == nil {
			return nil
		}
		var ch Chat
		if err := json.Unmarshal(v, &ch); err != nil {
			return err
		}
		ch.MsgCount = 0
		ch.LastSummaryMsg = 0
		ch.UpdatedAt = time.Now()
		data, _ := json.Marshal(ch)
		return cb.Put([]byte(id), data)
	})
}

// SearchChats returns chats whose title or any message contains q.
func (d *DB) SearchChats(q string) ([]Chat, error) {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return d.ListChats()
	}
	chats, err := d.ListChats()
	if err != nil {
		return nil, err
	}
	var out []Chat
	for _, c := range chats {
		if strings.Contains(strings.ToLower(c.Title), q) {
			out = append(out, c)
			continue
		}
		msgs, _ := d.LoadMessages(c.ID, 1000)
		for _, m := range msgs {
			if strings.Contains(strings.ToLower(m.Content), q) {
				out = append(out, c)
				break
			}
		}
	}
	return out, nil
}

// ---------- memories ----------

var memoryStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"you": true, "your": true, "are": true, "have": true, "has": true, "was": true,
	"what": true, "when": true, "where": true, "which": true, "about": true,
	"from": true, "into": true, "will": true, "would": true, "should": true,
	"can": true, "could": true, "not": true, "but": true, "all": true, "any": true,
	"is": true, "to": true, "of": true, "in": true, "on": true, "at": true,
	"by": true, "it": true, "as": true, "be": true, "we": true, "us": true,
	"do": true, "so": true, "no": true, "or": true, "if": true, "an": true,
	"my": true, "me": true, "he": true, "she": true, "his": true, "her": true,
	"a": true, "i": true,
}

// stemWord is a tiny stemmer so "servers" matches "server" and
// "prefers" matches "prefer". It only strips a trailing "s".
func stemWord(w string) string {
	if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
		return w[:len(w)-1]
	}
	return w
}

func tokenize(s string) []string {
	s = strings.ToLower(s)
	var out []string
	for _, w := range strings.FieldsFunc(s, func(r rune) bool {
		return r < 'a' || r > 'z'
	}) {
		if len(w) >= 2 && !memoryStopwords[w] {
			out = append(out, stemWord(w))
		}
	}
	return out
}

func normalizeText(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == ' ' {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// similarMemory reports whether two facts are basically the same one
// (exact match, one containing the other, or high token overlap).
func similarMemory(a, b string) bool {
	na, nb := normalizeText(a), normalizeText(b)
	if na == nb || na == "" || nb == "" {
		return na == nb
	}
	if len(na) > 20 && strings.Contains(na, nb) {
		return true
	}
	if len(nb) > 20 && strings.Contains(nb, na) {
		return true
	}
	ta, tb := tokenize(na), tokenize(nb)
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	setA := map[string]bool{}
	for _, w := range ta {
		setA[w] = true
	}
	inter := 0
	for _, w := range tb {
		if setA[w] {
			inter++
		}
	}
	union := len(setA)
	for _, w := range tb {
		if !setA[w] {
			union++
		}
	}
	if union == 0 {
		return false
	}
	return float64(inter)/float64(union) >= 0.55
}

func classifyMemory(text string) string {
	l := strings.ToLower(text)
	switch {
	case strings.Contains(l, "prefer") || strings.Contains(l, "like") || strings.Contains(l, "love") ||
		strings.Contains(l, "hate") || strings.Contains(l, "always") || strings.Contains(l, "never") ||
		strings.Contains(l, "wants") || strings.Contains(l, "favourite") || strings.Contains(l, "favorite"):
		return "preference"
	case strings.Contains(l, "decided") || strings.Contains(l, "decision") || strings.Contains(l, "agreed") ||
		strings.Contains(l, "will use") || strings.Contains(l, "chosen") || strings.Contains(l, "chose"):
		return "decision"
	case strings.Contains(l, "project") || strings.Contains(l, "building") || strings.Contains(l, "app") ||
		strings.Contains(l, "repo") || strings.Contains(l, "github"):
		return "project"
	case strings.Contains(l, "name is") || strings.Contains(l, "called") || strings.Contains(l, "brother") ||
		strings.Contains(l, "sister") || strings.Contains(l, "wife") || strings.Contains(l, "husband") ||
		strings.Contains(l, "friend") || strings.Contains(l, "colleague"):
		return "person"
	default:
		return "fact"
	}
}

// AddMemory saves a fact, unless it is a near-duplicate of one already
// stored — then it returns the existing memory and false.
// On a successful save it re-exports MEMORY.md from the DB.
func (d *DB) AddMemory(text, kind, sourceChatID string) (Memory, bool, error) {
	return d.addMemory(text, kind, sourceChatID, true)
}

// addMemory is AddMemory with a switch: imports use export=false so
// importing a hand-written MEMORY.md never rewrites that file.
func (d *DB) addMemory(text, kind, sourceChatID string, export bool) (Memory, bool, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Memory{}, false, fmt.Errorf("empty memory")
	}
	if kind == "" {
		kind = classifyMemory(text)
	}
	existing, _ := d.ListMemories()
	for _, m := range existing {
		if similarMemory(m.Text, text) {
			return m, false, nil
		}
	}
	now := time.Now()
	mem := Memory{
		ID:           fmt.Sprintf("mem-%d", now.UnixNano()),
		Text:         text,
		Kind:         kind,
		SourceChatID: sourceChatID,
		Confidence:   0.8,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	err := d.bolt.Update(func(tx *bbolt.Tx) error {
		data, _ := json.Marshal(mem)
		return tx.Bucket(bucketMemories).Put([]byte(mem.ID), data)
	})
	if err != nil {
		return Memory{}, false, err
	}
	if export {
		_ = d.ExportMemories()
	}
	return mem, true, nil
}

func (d *DB) ListMemories() ([]Memory, error) {
	var out []Memory
	err := d.bolt.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMemories).ForEach(func(k, v []byte) error {
			var m Memory
			if err := json.Unmarshal(v, &m); err == nil {
				out = append(out, m)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, err
}

func (d *DB) UpdateMemory(id, text, kind string) error {
	return d.bolt.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMemories)
		v := b.Get([]byte(id))
		if v == nil {
			return fmt.Errorf("memory not found: %s", id)
		}
		var m Memory
		if err := json.Unmarshal(v, &m); err != nil {
			return err
		}
		if strings.TrimSpace(text) != "" {
			m.Text = strings.TrimSpace(text)
		}
		if kind != "" {
			m.Kind = kind
		}
		m.UpdatedAt = time.Now()
		data, _ := json.Marshal(m)
		return b.Put([]byte(id), data)
	})
}

func (d *DB) DeleteMemory(id string) error {
	return d.bolt.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMemories).Delete([]byte(id))
	})
}

// RelevantMemories scores memories against a user message by keyword
// overlap, then recency and confidence, and returns the top `limit`
// whose combined text stays under maxChars. With an empty query it
// just returns the most recent ones (used for the startup prompt).
func (d *DB) RelevantMemories(query string, limit, maxChars int) []Memory {
	if limit <= 0 {
		limit = 10
	}
	if maxChars <= 0 {
		maxChars = 4000
	}
	all, err := d.ListMemories()
	if err != nil {
		return nil
	}
	qwords := tokenize(query)
	type scored struct {
		m     Memory
		score float64
	}
	var scoredList []scored
	now := time.Now()
	for _, m := range all {
		score := 0.0
		if len(qwords) > 0 {
			textWords := map[string]bool{}
			for _, w := range tokenize(m.Text) {
				textWords[w] = true
			}
			for _, w := range qwords {
				if textWords[w] {
					score += 2
				}
			}
			// A memory that shares no keyword with the question is
			// only kept when the query is empty (startup prompt).
			if score == 0 {
				continue
			}
		}
		days := now.Sub(m.UpdatedAt).Hours() / 24
		recency := 1.0 / (1.0 + days/30.0)
		score += recency + m.Confidence*0.5
		scoredList = append(scoredList, scored{m, score})
	}
	sort.Slice(scoredList, func(i, j int) bool { return scoredList[i].score > scoredList[j].score })
	var out []Memory
	chars := 0
	for _, s := range scoredList {
		if len(out) >= limit {
			break
		}
		if chars+len(s.m.Text) > maxChars && len(out) > 0 {
			continue
		}
		chars += len(s.m.Text)
		out = append(out, s.m)
	}
	return out
}

// ExportMemories writes the current memories to MEMORY.md (grouped by
// kind) so the file stays in sync with the DB. The file is the
// human-readable export; the DB is the source of truth.
func (d *DB) ExportMemories() error {
	return d.ExportMemoriesTo(defaultMemoryPath(mustCwd()))
}

func (d *DB) ExportMemoriesTo(path string) error {
	mems, err := d.ListMemories()
	if err != nil {
		return err
	}
	if path == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("# Memory\n\n")
	b.WriteString("<!-- Exported from the Muse99 database (muse99.db). Edit facts in the web Memory tab, or edit this file — it is re-imported when the database is empty. -->\n\n")
	groups := map[string][]Memory{}
	order := []string{"preference", "fact", "decision", "project", "person"}
	titles := map[string]string{
		"preference": "Preferences",
		"fact":       "Facts",
		"decision":   "Decisions",
		"project":    "Projects",
		"person":     "People",
	}
	for _, m := range mems {
		groups[m.Kind] = append(groups[m.Kind], m)
	}
	for _, kind := range order {
		if len(groups[kind]) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s\n", titles[kind])
		for _, m := range groups[kind] {
			fmt.Fprintf(&b, "- [%s] %s — %s\n", kind, m.CreatedAt.Format("2006-01-02"), m.Text)
		}
		b.WriteString("\n")
	}
	// Any kind not in the fixed order still gets exported.
	for kind, list := range groups {
		known := false
		for _, k := range order {
			if k == kind {
				known = true
			}
		}
		if known {
			continue
		}
		fmt.Fprintf(&b, "## %s\n", strings.Title(kind))
		for _, m := range list {
			fmt.Fprintf(&b, "- [%s] %s — %s\n", kind, m.CreatedAt.Format("2006-01-02"), m.Text)
		}
		b.WriteString("\n")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// ImportMemoriesFromFile reads "- text" / "- [kind] date — text" lines
// from a MEMORY.md file into the DB (deduped). Returns facts imported.
func (d *DB) ImportMemoriesFromFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	imported := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		line = strings.TrimSpace(line[2:])
		// Strip a leading "[kind] " and a "YYYY-MM-DD — " date prefix.
		kind := ""
		if strings.HasPrefix(line, "[") {
			if i := strings.Index(line, "]"); i > 0 {
				kind = line[1:i]
				line = strings.TrimSpace(line[i+1:])
			}
		}
		if len(line) > 12 && line[4] == '-' && line[7] == '-' {
			if i := strings.Index(line, "—"); i > 0 {
				line = strings.TrimSpace(line[i+len("—"):])
			} else if i := strings.Index(line, "- "); i > 0 {
				line = strings.TrimSpace(line[i+2:])
			}
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if _, added, err := d.addMemory(line, kind, "", false); err == nil && added {
			imported++
		}
	}
	return imported, nil
}

// ---------- usage in the DB ----------

func (d *DB) saveUsageDB(u UsageTotals) error {
	data, _ := json.Marshal(u)
	return d.bolt.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Put([]byte("usage"), data)
	})
}

func (d *DB) loadUsageDB() (UsageTotals, bool) {
	var u UsageTotals
	found := false
	_ = d.bolt.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketMeta).Get([]byte("usage"))
		if v == nil {
			return nil
		}
		if err := json.Unmarshal(v, &u); err == nil && u.Calls > 0 {
			found = true
		}
		return nil
	})
	return u, found
}

// ---------- auto-summary ----------

// summariseChat asks the model to extract durable facts from a chat's
// recent messages, dedupes them, and saves the new ones. It only
// summarises messages since the chat's last summary, and it is
// deliberately NOT run on every turn (see maybeAutoSummarise).
// Returns how many new facts were saved.
func summariseChat(client *Client, d *DB, chatID string) (int, error) {
	if d == nil || client == nil {
		return 0, fmt.Errorf("no database or client")
	}
	chat, err := d.GetChat(chatID)
	if err != nil {
		return 0, err
	}
	msgs, err := d.LoadMessages(chatID, 1000)
	if err != nil {
		return 0, err
	}
	if len(msgs) == 0 {
		return 0, nil
	}
	// Messages since the last summary (or the last 20, whichever is smaller).
	var recent []Message
	for i, m := range msgs {
		if i+1 > chat.LastSummaryMsg {
			recent = append(recent, m)
		}
	}
	if len(recent) > 20 {
		recent = recent[len(recent)-20:]
	}
	if len(recent) == 0 {
		return 0, nil
	}
	var b strings.Builder
	b.WriteString("Extract durable facts about the user from this chat. ")
	b.WriteString("Only things worth remembering long-term: preferences, decisions, projects, names, constraints, goals. ")
	b.WriteString("Do NOT save small talk, one-off questions, or anything already obvious. ")
	b.WriteString("Reply with one fact per line, each starting with \"- \", and nothing else. If there is nothing durable, reply with nothing.\n\nChat:\n")
	for _, m := range recent {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, truncate(m.Content, 500))
	}
	resp, err := client.Chat([]Message{
		{Role: "system", Content: "You extract durable facts. Reply only with lines starting with \"- \"."},
		{Role: "user", Content: b.String()},
	}, nil)
	if err != nil {
		return 0, err
	}
	saved := 0
	for _, line := range strings.Split(resp.Content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		text := strings.TrimSpace(line[2:])
		if text == "" {
			continue
		}
		if _, added, err := d.AddMemory(text, "", chatID); err == nil && added {
			saved++
		}
	}
	// Mark summarised up to the current message count, even if
	// nothing new was found, so we do not re-scan the same messages.
	_ = d.bolt.Update(func(tx *bbolt.Tx) error {
		cb := tx.Bucket(bucketChats)
		v := cb.Get([]byte(chatID))
		if v == nil {
			return nil
		}
		var c Chat
		if err := json.Unmarshal(v, &c); err != nil {
			return err
		}
		c.LastSummaryMsg = c.MsgCount
		data, _ := json.Marshal(c)
		return cb.Put([]byte(chatID), data)
	})
	return saved, nil
}

// maybeAutoSummarise runs summariseChat only when a chat has 10 or
// more new messages since its last summary. That keeps the extra
// token cost tiny (one small call per 10 messages) instead of
// doubling the cost of every turn.
func maybeAutoSummarise(client *Client, d *DB, chatID string) int {
	if d == nil || client == nil || chatID == "" {
		return 0
	}
	chat, err := d.GetChat(chatID)
	if err != nil {
		return 0
	}
	if chat.MsgCount-chat.LastSummaryMsg < 10 {
		return 0
	}
	n, err := summariseChat(client, d, chatID)
	if err != nil {
		return 0
	}
	return n
}

// formatMemories renders memories for a system prompt.
func formatMemories(mems []Memory) string {
	if len(mems) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("What you remember about the user:\n")
	for _, m := range mems {
		fmt.Fprintf(&b, "- [%s] %s\n", m.Kind, m.Text)
	}
	return b.String()
}

// memoryContextFor returns the memory text for a system prompt:
// scored memories for this user message when a DB is open, else the
// old MEMORY.md file. An empty query returns the most recent memories.
func memoryContextFor(query, memoryPath string) string {
	if appDB != nil {
		mems := appDB.RelevantMemories(query, 10, 4000)
		if len(mems) > 0 {
			return formatMemories(mems)
		}
		// DB is open but has no memories yet: fall through to the file.
	}
	return loadMemory(memoryPath)
}
