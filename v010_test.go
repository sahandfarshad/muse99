package main

import "testing"

func TestV010LocalHelpers(t *testing.T) {
	if got := normalizeLocalBase("127.0.0.1:5000"); got != "http://127.0.0.1:5000/v1" {
		t.Fatalf("normalize: %s", got)
	}
	if got := normalizeLocalBase("http://pc:8080/v1/"); got != "http://pc:8080/v1" {
		t.Fatalf("normalize keep path: %s", got)
	}
	if m := pickLocalModel([]localModel{{ID: "a"}, {ID: "OxCoder-9B"}, {ID: "b", Loaded: true}}); m != "b" {
		t.Fatalf("loaded first: %s", m)
	}
	if m := pickLocalModel([]localModel{{ID: "a"}, {ID: "OxCoder-9B"}}); m != "OxCoder-9B" {
		t.Fatalf("oxcoder next: %s", m)
	}
	if matchProvider("http://127.0.0.1:1234/v1", "") != "lmstudio" || matchProvider("http://localhost:8080/v1", "") != "llamacpp" {
		t.Fatal("matchProvider local")
	}
	if !isDesktopDefault(nil, "windows") || isDesktopDefault([]string{"--menu"}, "windows") || isDesktopDefault(nil, "linux") {
		t.Fatal("desktop default")
	}
	if _, ok := providerPreset("lmstudio"); !ok {
		t.Fatal("lmstudio preset")
	}
}
