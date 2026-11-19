package main

// Version is the release tag this binary was built from (e.g. v0.8.0).
// The release workflow sets it with
//
//	go build -ldflags "-X main.Version=v0.8.0"
//
// A local build without that flag reports "dev".
var Version = "dev"

// versionLabel is how the version is shown in the menu and web UI.
func versionLabel() string {
	if Version == "" {
		return "dev"
	}
	return Version
}
