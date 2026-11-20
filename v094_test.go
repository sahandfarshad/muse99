package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startTestSSH runs a tiny SSH server that answers "exec" requests by
// echoing the command back.
func startTestSSH(t *testing.T, password string, allowKey func(ssh.PublicKey) bool) (string, int) {
	_, hk, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(hk)
	conf := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if password != "" && string(p) == password {
				return nil, nil
			}
			return nil, errBad
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if allowKey != nil && allowKey(k) {
				return nil, nil
			}
			return nil, errBad
		},
	}
	conf.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, conf)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					ch, creqs, _ := nch.Accept()
					go func() {
						for req := range creqs {
							if req.Type == "exec" {
								n := binary.BigEndian.Uint32(req.Payload[:4])
								cmd := string(req.Payload[4 : 4+n])
								_ = req.Reply(true, nil)
								_, _ = ch.Write([]byte("ran: " + cmd))
								_ = ch.CloseWrite()
								_, _ = ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
								ch.Close()
								return
							}
							_ = req.Reply(false, nil)
						}
					}()
				}
			}()
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return host, p
}

type sshErr string

func (e sshErr) Error() string { return string(e) }

var errBad = sshErr("denied")

func TestRemotePasswordAndKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, pub, err := museKey()
	if err != nil || !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("key %q %v", pub, err)
	}
	if !strings.Contains(keyInstallCommand(pub), "authorized_keys") {
		t.Fatal("bad install command")
	}
	host, port := startTestSSH(t, "pw1", nil)
	srv := Server{Name: "box", Host: host, Port: port, User: "root", Password: "pw1", Enabled: true}
	out, err := runRemote(srv, "echo hi", 10*time.Second)
	if err != nil || !strings.Contains(out, "ran: echo hi") {
		t.Fatalf("password login: %q %v", out, err)
	}
	srv.Password = "wrong"
	if _, err := runRemote(srv, "x", 10*time.Second); err == nil || !strings.Contains(err.Error(), "wrong password") {
		t.Fatalf("want wrong password, got %v", err)
	}
	// Key login with Muse99's own key.
	signer, _, _ := museKey()
	host2, port2 := startTestSSH(t, "", func(k ssh.PublicKey) bool {
		return string(k.Marshal()) == string(signer.PublicKey().Marshal())
	})
	srv2 := Server{Name: "k", Host: host2, Port: port2, User: "u", Enabled: true}
	if out, err := runRemote(srv2, "ls", 10*time.Second); err != nil || !strings.Contains(out, "ran: ls") {
		t.Fatalf("key login: %q %v", out, err)
	}
	// Tool resolves saved servers only; prompt lists them; password not exposed.
	_ = saveServers([]Server{srv2})
	r := &toolRunner{workdir: t.TempDir()}
	if out := r.run("remote_shell", `{"server":"k","command":"pwd"}`); !strings.Contains(out, "ran: pwd") {
		t.Fatalf("tool: %s", out)
	}
	if out := r.run("remote_shell", `{"server":"evil@1.2.3.4","command":"pwd"}`); !strings.Contains(out, "unknown server") {
		t.Fatalf("unsaved target allowed: %s", out)
	}
	if !strings.Contains(systemPrompt("S", "", "", "m"), "- k (u@") {
		t.Fatal("servers not in prompt")
	}
	_ = saveServers([]Server{{Name: "p", Host: "h", User: "u", Password: "secret", Enabled: true}})
	for _, s := range publicServers() {
		if s.Password != "" || !s.HasPassword {
			t.Fatalf("leak: %+v", s)
		}
	}
}
