// Copyright 2016 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// TODO: High-level file comment.
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"sync"

	"github.com/google/shlex"
	"github.com/Matir/sshdog/pty"
	"golang.org/x/crypto/ssh"
)

// Handling for a single incoming connection
type ServerConn struct {
	*Server
	*ssh.ServerConn
	pty        *pty.Pty
	reqs       <-chan *ssh.Request
	chans      <-chan ssh.NewChannel
	environ    []string
	exitStatus uint32
}

func NewServerConn(conn net.Conn, s *Server) (*ServerConn, error) {
	sConn, chans, reqs, err := ssh.NewServerConn(conn, &s.ServerConfig)
	if err != nil {
		return nil, err
	}
	return &ServerConn{
		Server:     s,
		ServerConn: sConn,
		reqs:       reqs,
		chans:      chans,
		environ:    os.Environ(),
	}, nil
}

// Remote forwarding (RFC 4254 §7): the payload of a tcpip-forward or
// cancel-tcpip-forward global request.
type tcpipForwardRequest struct {
	BindAddr string
	BindPort uint32
}

// Payload of a forwarded-tcpip channel opened back to the client when a
// connection arrives on a remotely forwarded port.
type forwardedTCPIPPayload struct {
	Addr       string
	Port       uint32
	OriginAddr string
	OriginPort uint32
}

func (conn *ServerConn) ServiceGlobalRequests() {
	listeners := make(map[string]net.Listener)
	defer func() {
		for key, ln := range listeners {
			dbg.Debug("Closing forwarding listener: %s", key)
			ln.Close()
		}
	}()
	for r := range conn.reqs {
		dbg.Debug("Received request %s plus %d bytes.", r.Type, len(r.Payload))
		switch r.Type {
		case "tcpip-forward":
			conn.handleTCPIPForward(r, listeners)
		case "cancel-tcpip-forward":
			var fwd tcpipForwardRequest
			if err := ssh.Unmarshal(r.Payload, &fwd); err != nil {
				dbg.Debug("Error unmarshaling cancel-tcpip-forward: %v", err)
				if r.WantReply {
					r.Reply(false, []byte{})
				}
				continue
			}
			key := fmt.Sprintf("%s:%d", fwd.BindAddr, fwd.BindPort)
			ln, ok := listeners[key]
			if ok {
				delete(listeners, key)
				ln.Close()
				dbg.Debug("Canceled forwarding: %s", key)
			}
			if r.WantReply {
				r.Reply(ok, []byte{})
			}
		default:
			if r.WantReply {
				r.Reply(true, []byte{})
			}
		}
	}
}

func (conn *ServerConn) handleTCPIPForward(r *ssh.Request, listeners map[string]net.Listener) {
	var fwd tcpipForwardRequest
	if err := ssh.Unmarshal(r.Payload, &fwd); err != nil {
		dbg.Debug("Error unmarshaling tcpip-forward: %v", err)
		if r.WantReply {
			r.Reply(false, []byte{})
		}
		return
	}
	key := fmt.Sprintf("%s:%d", fwd.BindAddr, fwd.BindPort)
	if _, exists := listeners[key]; exists {
		dbg.Debug("Forwarding already exists: %s", key)
		if r.WantReply {
			r.Reply(false, []byte{})
		}
		return
	}
	ln, err := net.Listen("tcp", key)
	if err != nil {
		dbg.Debug("Unable to setup forwarding: %v", err)
		if r.WantReply {
			r.Reply(false, []byte{})
		}
		return
	}
	// The actual bound port matters when the client requested port 0.
	actualPort := uint32(ln.Addr().(*net.TCPAddr).Port)
	if fwd.BindPort == 0 {
		key = fmt.Sprintf("%s:%d", fwd.BindAddr, actualPort)
	}
	listeners[key] = ln
	dbg.Debug("Forwarding request: listening on %s for %v", ln.Addr(), fwd)
	if r.WantReply {
		if fwd.BindPort == 0 {
			payload := ssh.Marshal(struct{ Port uint32 }{actualPort})
			r.Reply(true, payload)
		} else {
			r.Reply(true, []byte{})
		}
	}
	go conn.serveForwardListener(ln, fwd.BindAddr, actualPort)
}

func (conn *ServerConn) serveForwardListener(ln net.Listener, bindAddr string, bindPort uint32) {
	for {
		c, err := ln.Accept()
		if err != nil {
			dbg.Debug("Forwarding listener closed: %v", err)
			return
		}
		go conn.handleForwardedConn(c, bindAddr, bindPort)
	}
}

func (conn *ServerConn) handleForwardedConn(c net.Conn, bindAddr string, bindPort uint32) {
	defer c.Close()
	originAddr, originPort := "", uint32(0)
	if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		originAddr = ta.IP.String()
		originPort = uint32(ta.Port)
	}
	connectedAddr := bindAddr
	if connectedAddr == "" {
		if ta, ok := c.LocalAddr().(*net.TCPAddr); ok {
			connectedAddr = ta.IP.String()
		}
	}
	payload := ssh.Marshal(forwardedTCPIPPayload{
		Addr:       connectedAddr,
		Port:       bindPort,
		OriginAddr: originAddr,
		OriginPort: originPort,
	})
	ch, reqs, err := conn.OpenChannel("forwarded-tcpip", payload)
	if err != nil {
		dbg.Debug("Unable to open forwarded-tcpip channel: %v", err)
		return
	}
	defer ch.Close()
	go ssh.DiscardRequests(reqs)
	go io.Copy(ch, c)
	io.Copy(c, ch)
}

// Handle a single established connection
func (conn *ServerConn) HandleConn() {
	defer func() {
		dbg.Debug("Closing connection to: %s", conn.RemoteAddr())
		conn.Close()
	}()

	go conn.ServiceGlobalRequests()
	wg := &sync.WaitGroup{}

	for newChan := range conn.chans {
		dbg.Debug("Incoming channel request: %s", newChan.ChannelType())
		switch newChan.ChannelType() {
		case "session":
			wg.Add(1)
			go conn.HandleSessionChannel(wg, newChan)
		case "direct-tcpip":
			wg.Add(1)
			go conn.HandleTCPIPChannel(wg, newChan)
		default:
			dbg.Debug("Unable to handle channel request, rejecting.")
			newChan.Reject(ssh.Prohibited, "Prohibited")
		}
	}

	wg.Wait()
}

type PTYRequest struct {
	Term     string
	Width    uint32
	Height   uint32
	WidthPx  uint32
	HeightPx uint32
	Modes    string
}

type EnvRequest struct {
	Name  string
	Value string
}

type ExecRequest struct {
	Cmd string
}

func defaultShell() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{
			"C:\\windows\\system32\\cmd.exe",
			"/Q",
		}
	default:
		return []string{"/bin/sh"}
	}
}

func commandWithShell(command string) []string {
	switch runtime.GOOS {
	case "windows":
		return []string{
			"C:\\windows\\system32\\cmd.exe",
			"/C",
			command,
		}
	default:
		return []string{
			"/bin/sh",
			"-c",
			command,
		}
	}
}

func (conn *ServerConn) HandleSessionChannel(wg *sync.WaitGroup, newChan ssh.NewChannel) {
	// TODO: refactor this, too long
	defer wg.Done()
	ch, reqs, err := newChan.Accept()
	if err != nil {
		dbg.Debug("Unable to accept newChan: %v", err)
		return
	}
	defer func() {
		b := ssh.Marshal(struct{ ExitStatus uint32 }{conn.exitStatus})
		ch.SendRequest("exit-status", false, b)
		dbg.Debug("Closing session channel.")
		ch.Close()
	}()

	var success bool
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			ptyreq := &PTYRequest{}
			success = true
			if err := ssh.Unmarshal(req.Payload, ptyreq); err != nil {
				dbg.Debug("Error unmarshaling pty-req: %v", err)
				success = false
			}
			conn.pty, err = pty.OpenPty()
			if conn.pty != nil {
				conn.pty.Resize(uint16(ptyreq.Height), uint16(ptyreq.Width), uint16(ptyreq.WidthPx), uint16(ptyreq.HeightPx))
				os.Setenv("TERM", ptyreq.Term)
				// TODO: set pty modes
			}
			if err != nil {
				dbg.Debug("Failed allocating pty: %v", err)
				success = false
			}
			if req.WantReply {
				req.Reply(success, []byte{})
			}
		case "env":
			envreq := &EnvRequest{}
			if err := ssh.Unmarshal(req.Payload, envreq); err != nil {
				dbg.Debug("Error unmarshaling env: %v", err)
				success = false
			} else {
				dbg.Debug("env: %s=%s", envreq.Name, envreq.Value)
				conn.environ = append(conn.environ, fmt.Sprintf("%s=%s", envreq.Name, envreq.Value))
				success = true
			}
			if req.WantReply {
				req.Reply(success, []byte{})
			}
		case "shell":
			// TODO: get the user's shell
			conn.ExecuteForChannel(defaultShell(), ch)
			if req.WantReply {
				req.Reply(true, []byte{})
			}
			return
		case "exec":
			execReq := &ExecRequest{}
			if err := ssh.Unmarshal(req.Payload, execReq); err != nil {
				dbg.Debug("Error unmarshaling exec: %v", err)
				success = false
			} else {
				if cmd, err := shlex.Split(execReq.Cmd); err == nil {
					dbg.Debug("Command: %v", cmd)
					if req.WantReply {
						req.Reply(true, []byte{})
					}
					if cmd[0] == "scp" {
						if err := conn.SCPHandler(cmd, ch); err != nil {
							dbg.Debug("scp failure: %v", err)
							conn.exitStatus = 1
						}
					} else {
						conn.exitStatus = conn.ExecuteForChannel(commandWithShell(execReq.Cmd), ch)
					}
				} else {
					dbg.Debug("Error splitting cmd: %v", err)
					if req.WantReply {
						req.Reply(false, []byte{})
					}
				}
			}
			return
		default:
			dbg.Debug("Unknown session request: %s", req.Type)
			if req.WantReply {
				req.Reply(false, []byte{})
			}
		}
	}
}

// Execute a process for the channel.
func (conn *ServerConn) ExecuteForChannel(shellCmd []string, ch ssh.Channel) uint32 {
	dbg.Debug("Executing %v", shellCmd)
	var exerr *exec.ExitError
	proc := exec.Command(shellCmd[0], shellCmd[1:]...)
	proc.Env = conn.environ
	if userInfo, err := user.Current(); err == nil {
		proc.Dir = userInfo.HomeDir
	}
	if conn.pty == nil {
		stdin, _ := proc.StdinPipe()
		go func() {
			defer stdin.Close()
			io.Copy(stdin, ch)
		}()
		proc.Stdout = ch
		proc.Stderr = ch.Stderr()
	} else {
		conn.pty.AttachPty(proc)
	}
	if err := proc.Start(); err != nil {
		dbg.Debug("Failed to start process: %v", err)
		return 1
	}
	if conn.pty != nil {
		conn.pty.AttachIO(ch, ch)
	}
	err := proc.Wait()
	if conn.pty != nil {
		conn.pty.Close()
	}
	if errors.As(err, &exerr) {
		dbg.Debug("Finished execution with error: %v", err)
		return uint32(exerr.ExitCode())
	} else {
		dbg.Debug("Finished execution.")
		return 0
	}
}

// Message for port forwarding
type tcpipMessage struct {
	Host       string
	Port       uint32
	SourceIP   string
	SourcePort uint32
}

func (conn *ServerConn) HandleTCPIPChannel(wg *sync.WaitGroup, newChan ssh.NewChannel) {
	defer wg.Done()
	var msg tcpipMessage
	if err := ssh.Unmarshal(newChan.ExtraData(), &msg); err != nil {
		dbg.Debug("Unable to setup forwarding: %v", err)
		newChan.Reject(ssh.ResourceShortage, "Error parsing message.")
		return
	}
	dbg.Debug("Forwarding request: %v", msg)

	outbound, err := net.Dial("tcp", fmt.Sprintf("%s:%d", msg.Host, msg.Port))
	if err != nil {
		dbg.Debug("Unable to dial forward: %v", err)
		newChan.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	defer outbound.Close()

	ch, reqs, err := newChan.Accept()
	if err != nil {
		dbg.Debug("Unable to accept chan: %v", err)
		return
	}
	defer ch.Close()

	go func() {
		for req := range reqs {
			switch req.Type {
			default:
				dbg.Debug("Unknown direct-tcpip request: %s", req.Type)
				if req.WantReply {
					req.Reply(false, []byte{})
				}
			}
		}
	}()
	go io.Copy(ch, outbound)
	io.Copy(outbound, ch)

	dbg.Debug("Closing forwarding request: %v", msg)
}
