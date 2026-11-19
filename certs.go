package main

// TLS helpers.
//
// Two problems with serving Let's Encrypt certificates straight from
// /etc/letsencrypt/live/<domain>/:
//   1. privkey.pem there is root-only (0600 root), and Muse99 runs as
//      a normal user, so ServeTLS failed with "permission denied".
//   2. certbot renews every ~60 days, and the old code loaded the
//      certificate once at start — a long-running web UI kept serving
//      the expired one until someone restarted it.
//
// Fix: after certbot, the SSL menu copies the pair into
// ~/.muse99/certs/ owned by this user (0600 key), and installs a
// certbot deploy hook that repeats the copy after every renewal. The
// web server loads certificates through certReloader, which re-reads
// the files when they change, so renewals apply without a restart.

import (
	"crypto/tls"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type certReloader struct {
	certFile, keyFile string
	mu                sync.Mutex
	cert              *tls.Certificate
	modTime           time.Time
	checked           time.Time
}

func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *certReloader) load() error {
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return err
	}
	st, _ := os.Stat(r.certFile)
	r.cert = &c
	if st != nil {
		r.modTime = st.ModTime()
	}
	return nil
}

// GetCertificate checks the file at most once a minute and reloads it
// when it changed. A failed reload keeps serving the last good cert.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) > time.Minute {
		r.checked = time.Now()
		if st, err := os.Stat(r.certFile); err == nil && st.ModTime().After(r.modTime) {
			if err := r.load(); err != nil {
				fmt.Println("TLS reload failed, keeping the old certificate:", err)
			}
		}
	}
	return r.cert, nil
}

// pathInWorkdir resolves p (relative to workdir, or absolute) and
// reports whether it stays inside workdir after cleaning and
// following symlinks. Used by the web Files endpoints.
func pathInWorkdir(workdir, p string) (string, bool) {
	root, err := filepath.Abs(workdir)
	if err != nil {
		return "", false
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	full := p
	if !filepath.IsAbs(full) {
		full = filepath.Join(root, full)
	}
	full = filepath.Clean(full)
	if r, err := filepath.EvalSymlinks(full); err == nil {
		full = r
	}
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return full, true
}

const deployHookPath = "/etc/letsencrypt/renewal-hooks/deploy/muse99.sh"

// installUserCerts copies the Let's Encrypt pair for domain into
// ~/.muse99/certs (owned by the current user) and installs a certbot
// deploy hook so every renewal copies the new pair there too.
func installUserCerts(domain string) (certFile, keyFile string, err error) {
	u, err := user.Current()
	if err != nil {
		return "", "", err
	}
	dir := certDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	src := filepath.Join("/etc/letsencrypt/live", domain)
	certFile, keyFile = filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	hook := fmt.Sprintf(`#!/bin/sh
# Installed by Muse99: copy renewed certificates for %[1]s to %[2]s
set -e
[ "${RENEWED_LINEAGE:-%[3]s}" = "%[3]s" ] || exit 0
install -o %[4]s -m 0644 %[3]s/fullchain.pem %[2]s/fullchain.pem
install -o %[4]s -m 0600 %[3]s/privkey.pem %[2]s/privkey.pem
`, domain, dir, src, u.Username)
	script := fmt.Sprintf(`set -e
install -o %[1]s -m 0644 %[2]s/fullchain.pem %[3]s
install -o %[1]s -m 0600 %[2]s/privkey.pem %[4]s
mkdir -p %[5]s
cat > %[6]s <<'MUSE99HOOK'
%[7]sMUSE99HOOK
chmod 0755 %[6]s
`, u.Username, src, certFile, keyFile, filepath.Dir(deployHookPath), deployHookPath, hook)
	cmd := exec.Command("sudo", "sh", "-c", script)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("copying certificates failed: %w", err)
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return "", "", fmt.Errorf("copied certificate does not load: %w", err)
	}
	return certFile, keyFile, nil
}
