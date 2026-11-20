package main

// Native SSH for the Servers tab and the remote_shell tool (v0.9.4).
// Muse99 keeps its own key in ~/.muse99/ssh, can log in with a
// password, and trusts a server's host key the first time it sees it
// (then refuses if it ever changes).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

var sshKeyMu sync.Mutex

func sshDir() string { return filepath.Join(configDir(), "ssh") }

// museKey returns Muse99's own SSH key, creating it on first use.
func museKey() (ssh.Signer, string, error) {
	sshKeyMu.Lock()
	defer sshKeyMu.Unlock()
	dir := sshDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	priv := filepath.Join(dir, "id_ed25519")
	if data, err := os.ReadFile(priv); err == nil {
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, "", err
		}
		return signer, authorizedLine(signer.PublicKey()), nil
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(key, "muse99")
	if err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(priv, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, "", err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	line := authorizedLine(sp)
	_ = os.WriteFile(priv+".pub", []byte(line+"\n"), 0o644)
	signer, err := ssh.NewSignerFromKey(key)
	return signer, line, err
}

func authorizedLine(k ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " muse99"
}

// keyInstallCommand is the one line the user pastes on a new server so
// Muse99's key can log in there.
func keyInstallCommand(pubLine string) string {
	return "mkdir -p ~/.ssh && chmod 700 ~/.ssh && grep -qxF '" + pubLine + "' ~/.ssh/authorized_keys 2>/dev/null || echo '" + pubLine + "' >> ~/.ssh/authorized_keys; chmod 600 ~/.ssh/authorized_keys && echo Muse99 key installed"
}

// hostKeyCheck trusts a host key the first time and rejects a change.
func hostKeyCheck() (ssh.HostKeyCallback, error) {
	if err := os.MkdirAll(sshDir(), 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(sshDir(), "known_hosts")
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
		f.Close()
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		cb, err := knownhosts.New(path)
		if err != nil {
			return err
		}
		err = cb(hostname, remote, key)
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) && len(ke.Want) == 0 {
			f, ferr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if ferr != nil {
				return ferr
			}
			defer f.Close()
			_, ferr = f.WriteString(knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key) + "\n")
			return ferr
		}
		if errors.As(err, &ke) {
			return fmt.Errorf("the server's host key CHANGED since last time (possible attack, or the server was reinstalled). If you reinstalled it, delete its line from %s", path)
		}
		return err
	}, nil
}

// runRemote runs one command on a saved server and returns its output.
func runRemote(srv Server, command string, timeout time.Duration) (string, error) {
	var auths []ssh.AuthMethod
	var signers []ssh.Signer
	if s, _, err := museKey(); err == nil {
		signers = append(signers, s)
	}
	if srv.KeyPath != "" {
		p := srv.KeyPath
		if strings.HasPrefix(p, "~/") {
			if h, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(h, p[2:])
			}
		}
		if data, err := os.ReadFile(p); err == nil {
			if s, err := ssh.ParsePrivateKey(data); err == nil {
				signers = append(signers, s)
			}
		}
	}
	if len(signers) > 0 {
		auths = append(auths, ssh.PublicKeys(signers...))
	}
	if srv.Password != "" {
		pw := srv.Password
		auths = append(auths, ssh.Password(pw), ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			out := make([]string, len(qs))
			for i := range qs {
				out[i] = pw
			}
			return out, nil
		}))
	}
	hk, err := hostKeyCheck()
	if err != nil {
		return "", err
	}
	port := srv.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(srv.Host, strconv.Itoa(port))
	conf := &ssh.ClientConfig{User: srv.User, Auth: auths, HostKeyCallback: hk, Timeout: 15 * time.Second}
	ctx, cancel := context.WithTimeout(ctl.context(), timeout)
	defer cancel()
	type res struct {
		out string
		err error
	}
	ch := make(chan res, 1)
	var client *ssh.Client
	var cmu sync.Mutex
	go func() {
		c, err := ssh.Dial("tcp", addr, conf)
		if err != nil {
			ch <- res{"", explainSSHError(err, srv)}
			return
		}
		cmu.Lock()
		client = c
		cmu.Unlock()
		defer c.Close()
		sess, err := c.NewSession()
		if err != nil {
			ch <- res{"", err}
			return
		}
		defer sess.Close()
		var buf, ebuf bytes.Buffer
		sess.Stdout = &buf
		sess.Stderr = &ebuf
		err = sess.Run(command)
		if ebuf.Len() > 0 {
			buf.WriteString(ebuf.String())
		}
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			ch <- res{buf.String() + fmt.Sprintf("\n(exit code %d)", ee.ExitStatus()), nil}
			return
		}
		ch <- res{buf.String(), err}
	}()
	select {
	case r := <-ch:
		return r.out, r.err
	case <-ctx.Done():
		cmu.Lock()
		if client != nil {
			client.Close()
		}
		cmu.Unlock()
		if ctx.Err() == context.Canceled {
			return "", fmt.Errorf("stopped by the user (Stop button); the SSH connection was closed")
		}
		return "", fmt.Errorf("timed out after %s", timeout)
	}
}

func explainSSHError(err error, srv Server) error {
	msg := err.Error()
	if strings.Contains(msg, "unable to authenticate") {
		if srv.Password == "" {
			return fmt.Errorf("login refused: Muse99's key is not on the server yet. Run the key command from the Servers tab on the server, or add the password")
		}
		return fmt.Errorf("login refused: wrong password, or this server does not allow password login")
	}
	if strings.Contains(msg, "connection refused") {
		return fmt.Errorf("connection refused on port %d (wrong port, or SSH is not running)", srv.Port)
	}
	if strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "no route") {
		return fmt.Errorf("cannot reach %s (wrong address, or a firewall blocks port %d)", srv.Host, srv.Port)
	}
	return err
}

// findServer looks a saved server up by name, or by user@host.
func findServer(name string) (Server, bool) {
	name = strings.TrimSpace(name)
	for _, s := range loadServers() {
		if !s.Enabled {
			continue
		}
		if s.Name == name || s.User+"@"+s.Host == name || s.Host == name {
			return s, true
		}
	}
	return Server{}, false
}

// serversPrompt tells the model which servers it may use.
func serversPrompt() string {
	var lines []string
	for _, s := range loadServers() {
		if s.Enabled {
			lines = append(lines, fmt.Sprintf("- %s (%s@%s:%d)", s.Name, s.User, s.Host, s.Port))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "Servers the user gave you (run commands there with the remote_shell tool, server = the name):\n" + strings.Join(lines, "\n") + "\n\n"
}
