package main

import (
	"path/filepath"
	"testing"
)

func TestChatTokens(t *testing.T) {
	d, err := OpenDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, _ := d.CreateChat("x")
	d.AddChatUsage(c.ID, usageDelta{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, Calls: 2})
	ch, err := d.AddChatUsage(c.ID, usageDelta{PromptTokens: 50, CompletionTokens: 5, TotalTokens: 55, Calls: 1})
	if err != nil || ch.Tokens.TotalTokens != 165 || ch.Tokens.Calls != 3 || ch.LastTokens == nil || ch.LastTokens.TotalTokens != 55 {
		t.Fatalf("got %+v %v", ch, err)
	}
	got, _ := d.GetChat(c.ID)
	if got.Tokens.TotalTokens != 165 {
		t.Fatalf("not stored: %+v", got.Tokens)
	}
	_ = d.ClearChat(c.ID)
	got, _ = d.GetChat(c.ID)
	if got.Tokens.TotalTokens != 0 || got.LastTokens != nil {
		t.Fatalf("clear kept tokens: %+v", got)
	}
}
