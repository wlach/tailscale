// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipnserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go4.org/mem"
	"inet.af/netaddr"
	"inet.af/peercred"
	"tailscale.com/control/controlclient"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/localapi"
	"tailscale.com/log/filelogger"
	"tailscale.com/logtail/backoff"
	"tailscale.com/net/netstat"
	"tailscale.com/safesocket"
	"tailscale.com/smallzstd"
	"tailscale.com/types/logger"
	"tailscale.com/util/groupmember"
	"tailscale.com/util/pidowner"
	"tailscale.com/util/systemd"
	"tailscale.com/version"
	"tailscale.com/version/distro"
	"tailscale.com/wgengine"
)

// Options is the configuration of the Tailscale node agent.
type Options struct {
	// SocketPath, on unix systems, is the unix socket path to listen
	// on for frontend connections.
	SocketPath string

	// Port, on windows, is the localhost TCP port to listen on for
	// frontend connections.
	Port int

	// StatePath is the path to the stored agent state.
	StatePath string

	KubeSecret string

	// AutostartStateKey, if non-empty, immediately starts the agent
	// using the given StateKey. If empty, the agent stays idle and
	// waits for a frontend to start it.
	AutostartStateKey ipn.StateKey

	// SurviveDisconnects specifies how the server reacts to its
	// frontend disconnecting. If true, the server keeps running on
	// its existing state, and accepts new frontend connections. If
	// false, the server dumps its state and becomes idle.
	//
	// This is effectively whether the platform is in "server
	// mode" by default. On Linux, it's true; on Windows, it's
	// false. But on some platforms (currently only Windows), the
	// "server mode" can be overridden at runtime with a change in
	// Prefs.ForceDaemon/WantRunning.
	//
	// To support CLI connections (notably, "tailscale status"),
	// the actual definition of "disconnect" is when the
	// connection count transitions from 1 to 0.
	SurviveDisconnects bool

	// DebugMux, if non-nil, specifies an HTTP ServeMux in which
	// to register a debug handler.
	DebugMux *http.ServeMux
}

// server is an IPN backend and its set of 0 or more active connections
// talking to an IPN backend.
type server struct {
	b            *ipnlocal.LocalBackend
	logf         logger.Logf
	backendLogID string
	// resetOnZero is whether to call bs.Reset on transition from
	// 1->0 connections.  That is, this is whether the backend is
	// being run in "client mode" that requires an active GUI
	// connection (such as on Windows by default).  Even if this
	// is true, the ForceDaemon pref can override this.
	resetOnZero bool

	bsMu sync.Mutex // lock order: bsMu, then mu
	bs   *ipn.BackendServer

	mu             sync.Mutex
	serverModeUser *user.User                   // or nil if not in server mode
	lastUserID     string                       // tracks last userid; on change, Reset state for paranoia
	allClients     map[net.Conn]connIdentity    // HTTP or IPN
	clients        map[net.Conn]bool            // subset of allClients; only IPN protocol
	disconnectSub  map[chan<- struct{}]struct{} // keys are subscribers of disconnects
}

// connIdentity represents the owner of a localhost TCP or unix socket connection.
type connIdentity struct {
	Conn       net.Conn
	NotWindows bool // runtime.GOOS != "windows"

	// Fields used when NotWindows:
	IsUnixSock bool            // Conn is a *net.UnixConn
	Creds      *peercred.Creds // or nil

	// Used on Windows:
	// TODO(bradfitz): merge these into the peercreds package and
	// use that for all.
	Pid    int
	UserID string
	User   *user.User
}

// getConnIdentity returns the localhost TCP connection's identity information
// (pid, userid, user). If it's not Windows (for now), it returns a nil error
// and a ConnIdentity with NotWindows set true. It's only an error if we expected
// to be able to map it and couldn't.
func (s *server) getConnIdentity(c net.Conn) (ci connIdentity, err error) {
	ci = connIdentity{Conn: c}
	if runtime.GOOS != "windows" { // for now; TODO: expand to other OSes
		ci.NotWindows = true
		_, ci.IsUnixSock = c.(*net.UnixConn)
		ci.Creds, _ = peercred.Get(c)
		return ci, nil
	}
	la, err := netaddr.ParseIPPort(c.LocalAddr().String())
	if err != nil {
		return ci, fmt.Errorf("parsing local address: %w", err)
	}
	ra, err := netaddr.ParseIPPort(c.RemoteAddr().String())
	if err != nil {
		return ci, fmt.Errorf("parsing local remote: %w", err)
	}
	if !la.IP().IsLoopback() || !ra.IP().IsLoopback() {
		return ci, errors.New("non-loopback connection")
	}
	tab, err := netstat.Get()
	if err != nil {
		return ci, fmt.Errorf("failed to get local connection table: %w", err)
	}
	pid := peerPid(tab.Entries, la, ra)
	if pid == 0 {
		return ci, errors.New("no local process found matching localhost connection")
	}
	ci.Pid = pid
	uid, err := pidowner.OwnerOfPID(pid)
	if err != nil {
		var hint string
		if runtime.GOOS == "windows" {
			hint = " (WSL?)"
		}
		return ci, fmt.Errorf("failed to map connection's pid to a user%s: %w", hint, err)
	}
	ci.UserID = uid
	u, err := s.lookupUserFromID(uid)
	if err != nil {
		return ci, fmt.Errorf("failed to look up user from userid: %w", err)
	}
	ci.User = u
	return ci, nil
}

func (s *server) lookupUserFromID(uid string) (*user.User, error) {
	u, err := user.LookupId(uid)
	if err != nil && runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(0x534)) {
		s.logf("[warning] issue 869: os/user.LookupId failed; ignoring")
		// Work around https://github.com/tailscale/tailscale/issues/869 for
		// now. We don't strictly need the username. It's just a nice-to-have.
		// So make up a *user.User if their machine is broken in this way.
		return &user.User{
			Uid:      uid,
			Username: "unknown-user-" + uid,
			Name:     "unknown user " + uid,
		}, nil
	}
	return u, err
}

// blockWhileInUse blocks while until either a Read from conn fails
// (i.e. it's closed) or until the server is able to accept ci as a
// user.
func (s *server) blockWhileInUse(conn io.Reader, ci connIdentity) {
	s.logf("blocking client while server in use; connIdentity=%v", ci)
	connDone := make(chan struct{})
	go func() {
		io.Copy(ioutil.Discard, conn)
		close(connDone)
	}()
	ch := make(chan struct{}, 1)
	s.registerDisconnectSub(ch, true)
	defer s.registerDisconnectSub(ch, false)
	for {
		select {
		case <-connDone:
			s.logf("blocked client Read completed; connIdentity=%v", ci)
			return
		case <-ch:
			s.mu.Lock()
			err := s.checkConnIdentityLocked(ci)
			s.mu.Unlock()
			if err == nil {
				s.logf("unblocking client, server is free; connIdentity=%v", ci)
				// Server is now available again for a new user.
				// TODO(bradfitz): keep this connection alive. But for
				// now just return and have our caller close the connection
				// (which unblocks the io.Copy goroutine we started above)
				// and then the client (e.g. Windows) will reconnect and
				// discover that it works.
				return
			}
		}
	}
}

// bufferHasHTTPRequest reports whether br looks like it has an HTTP
// request in it, without reading any bytes from it.
func bufferHasHTTPRequest(br *bufio.Reader) bool {
	peek, _ := br.Peek(br.Buffered())
	return mem.HasPrefix(mem.B(peek), mem.S("GET ")) ||
		mem.HasPrefix(mem.B(peek), mem.S("POST ")) ||
		mem.Contains(mem.B(peek), mem.S(" HTTP/"))
}

func (s *server) serveConn(ctx context.Context, c net.Conn, logf logger.Logf) {
	// First see if it's an HTTP request.
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(time.Second))
	br.Peek(4)
	c.SetReadDeadline(time.Time{})
	isHTTPReq := bufferHasHTTPRequest(br)

	ci, err := s.addConn(c, isHTTPReq)
	if err != nil {
		if isHTTPReq {
			fmt.Fprintf(c, "HTTP/1.0 500 Nope\r\nContent-Type: text/plain\r\nX-Content-Type-Options: nosniff\r\n\r\n%s\n", err.Error())
			c.Close()
			return
		}
		defer c.Close()
		bs := ipn.NewBackendServer(logf, nil, jsonNotifier(c, s.logf))
		_, occupied := err.(inUseOtherUserError)
		if occupied {
			bs.SendInUseOtherUserErrorMessage(err.Error())
			s.blockWhileInUse(c, ci)
		} else {
			bs.SendErrorMessage(err.Error())
			time.Sleep(time.Second)
		}
		return
	}

	// Tell the LocalBackend about the identity we're now running as.
	s.b.SetCurrentUserID(ci.UserID)

	if isHTTPReq {
		httpServer := &http.Server{
			// Localhost connections are cheap; so only do
			// keep-alives for a short period of time, as these
			// active connections lock the server into only serving
			// that user. If the user has this page open, we don't
			// want another switching user to be locked out for
			// minutes. 5 seconds is enough to let browser hit
			// favicon.ico and such.
			IdleTimeout: 5 * time.Second,
			ErrorLog:    logger.StdLogger(logf),
			Handler:     s.localhostHandler(ci),
		}
		httpServer.Serve(&oneConnListener{&protoSwitchConn{s: s, br: br, Conn: c}})
		return
	}

	defer s.removeAndCloseConn(c)
	logf("[v1] incoming control connection")

	if isReadonlyConn(ci, s.b.OperatorUserID(), logf) {
		ctx = ipn.ReadonlyContextOf(ctx)
	}

	for ctx.Err() == nil {
		msg, err := ipn.ReadMsg(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				logf("[v1] ReadMsg: %v", err)
			} else if ctx.Err() == nil {
				logf("ReadMsg: %v", err)
			}
			return
		}
		s.bsMu.Lock()
		if err := s.bs.GotCommandMsg(ctx, msg); err != nil {
			logf("GotCommandMsg: %v", err)
		}
		gotQuit := s.bs.GotQuit
		s.bsMu.Unlock()
		if gotQuit {
			return
		}
	}
}

func isReadonlyConn(ci connIdentity, operatorUID string, logf logger.Logf) bool {
	if runtime.GOOS == "windows" {
		// Windows doesn't need/use this mechanism, at least yet. It
		// has a different last-user-wins auth model.
		return false
	}
	const ro = true
	const rw = false
	if !safesocket.PlatformUsesPeerCreds() {
		return rw
	}
	creds := ci.Creds
	if creds == nil {
		logf("connection from unknown peer; read-only")
		return ro
	}
	uid, ok := creds.UserID()
	if !ok {
		logf("connection from peer with unknown userid; read-only")
		return ro
	}
	if uid == "0" {
		logf("connection from userid %v; root has access", uid)
		return rw
	}
	if selfUID := os.Getuid(); selfUID != 0 && uid == strconv.Itoa(selfUID) {
		logf("connection from userid %v; connection from non-root user matching daemon has access", uid)
		return rw
	}
	if operatorUID != "" && uid == operatorUID {
		logf("connection from userid %v; is configured operator", uid)
		return rw
	}
	if yes, err := isLocalAdmin(uid); err != nil {
		logf("connection from userid %v; read-only; %v", uid, err)
		return ro
	} else if yes {
		logf("connection from userid %v; is local admin, has access", uid)
		return rw
	}
	logf("connection from userid %v; read-only", uid)
	return ro
}

func isLocalAdmin(uid string) (bool, error) {
	u, err := user.LookupId(uid)
	if err != nil {
		return false, err
	}
	var adminGroup string
	switch {
	case runtime.GOOS == "darwin":
		adminGroup = "admin"
	case distro.Get() == distro.QNAP:
		adminGroup = "administrators"
	default:
		return false, fmt.Errorf("no system admin group found")
	}
	return groupmember.IsMemberOfGroup(adminGroup, u.Username)
}

// inUseOtherUserError is the error type for when the server is in use
// by a different local user.
type inUseOtherUserError struct{ error }

func (e inUseOtherUserError) Unwrap() error { return e.error }

// checkConnIdentityLocked checks whether the provided identity is
// allowed to connect to the server.
//
// The returned error, when non-nil, will be of type inUseOtherUserError.
//
// s.mu must be held.
func (s *server) checkConnIdentityLocked(ci connIdentity) error {
	// If clients are already connected, verify they're the same user.
	// This mostly matters on Windows at the moment.
	if len(s.allClients) > 0 {
		var active connIdentity
		for _, active = range s.allClients {
			break
		}
		if ci.UserID != active.UserID {
			return inUseOtherUserError{fmt.Errorf("Tailscale already in use by %s, pid %d", active.User.Username, active.Pid)}
		}
	}
	if su := s.serverModeUser; su != nil && ci.UserID != su.Uid {
		return inUseOtherUserError{fmt.Errorf("Tailscale already in use by %s", su.Username)}
	}
	return nil
}

// localAPIPermissions returns the permissions for the given identity accessing
// the Tailscale local daemon API.
//
// s.mu must not be held.
func (s *server) localAPIPermissions(ci connIdentity) (read, write bool) {
	if runtime.GOOS == "windows" {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.checkConnIdentityLocked(ci) == nil {
			return true, true
		}
		return false, false
	}
	if ci.IsUnixSock {
		return true, !isReadonlyConn(ci, s.b.OperatorUserID(), logger.Discard)
	}
	return false, false
}

// registerDisconnectSub adds ch as a subscribe to connection disconnect
// events. If add is false, the subscriber is removed.
func (s *server) registerDisconnectSub(ch chan<- struct{}, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		if s.disconnectSub == nil {
			s.disconnectSub = make(map[chan<- struct{}]struct{})
		}
		s.disconnectSub[ch] = struct{}{}
	} else {
		delete(s.disconnectSub, ch)
	}

}

// addConn adds c to the server's list of clients.
//
// If the returned error is of type inUseOtherUserError then the
// returned connIdentity is also valid.
func (s *server) addConn(c net.Conn, isHTTP bool) (ci connIdentity, err error) {
	ci, err = s.getConnIdentity(c)
	if err != nil {
		return
	}

	// If the connected user changes, reset the backend server state to make
	// sure node keys don't leak between users.
	var doReset bool
	defer func() {
		if doReset {
			s.logf("identity changed; resetting server")
			s.b.ResetForClientDisconnect()
		}
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.clients == nil {
		s.clients = map[net.Conn]bool{}
	}
	if s.allClients == nil {
		s.allClients = map[net.Conn]connIdentity{}
	}

	if err := s.checkConnIdentityLocked(ci); err != nil {
		return ci, err
	}

	if !isHTTP {
		s.clients[c] = true
	}
	s.allClients[c] = ci

	if s.lastUserID != ci.UserID {
		if s.lastUserID != "" {
			doReset = true
		}
		s.lastUserID = ci.UserID
	}
	return ci, nil
}

func (s *server) removeAndCloseConn(c net.Conn) {
	s.mu.Lock()
	delete(s.clients, c)
	delete(s.allClients, c)
	remain := len(s.allClients)
	for sub := range s.disconnectSub {
		select {
		case sub <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()

	if remain == 0 && s.resetOnZero {
		if s.b.InServerMode() {
			s.logf("client disconnected; staying alive in server mode")
		} else {
			s.logf("client disconnected; stopping server")
			s.b.ResetForClientDisconnect()
		}
	}
	c.Close()
}

func (s *server) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.clients {
		safesocket.ConnCloseRead(c)
		safesocket.ConnCloseWrite(c)
	}
	s.clients = nil
}

// setServerModeUserLocked is called when we're in server mode but our s.serverModeUser is nil.
//
// s.mu must be held
func (s *server) setServerModeUserLocked() {
	var ci connIdentity
	var ok bool
	for _, ci = range s.allClients {
		ok = true
		break
	}
	if !ok {
		s.logf("ipnserver: [unexpected] now in server mode, but no connected client")
		return
	}
	if ci.NotWindows {
		return
	}
	if ci.User != nil {
		s.logf("ipnserver: now in server mode; user=%v", ci.User.Username)
		s.serverModeUser = ci.User
	} else {
		s.logf("ipnserver: [unexpected] now in server mode, but nil User")
	}
}

var jsonEscapedZero = []byte(`\u0000`)

func (s *server) writeToClients(n ipn.Notify) {
	inServerMode := s.b.InServerMode()

	s.mu.Lock()
	defer s.mu.Unlock()

	if inServerMode {
		if s.serverModeUser == nil {
			s.setServerModeUserLocked()
		}
	} else {
		if s.serverModeUser != nil {
			s.logf("ipnserver: no longer in server mode")
			s.serverModeUser = nil
		}
	}

	if len(s.clients) == 0 {
		// Common case (at least on busy servers): nobody
		// connected (no GUI, etc), so return before
		// serializing JSON.
		return
	}

	if b, ok := marshalNotify(n, s.logf); ok {
		for c := range s.clients {
			ipn.WriteMsg(c, b)
		}
	}
}

// Run runs a Tailscale backend service.
// The getEngine func is called repeatedly, once per connection, until it returns an engine successfully.
func Run(ctx context.Context, logf logger.Logf, logid string, getEngine func() (wgengine.Engine, error), opts Options) error {
	getEngine = getEngineUntilItWorksWrapper(getEngine)
	runDone := make(chan struct{})
	defer close(runDone)

	listen, _, err := safesocket.Listen(opts.SocketPath, uint16(opts.Port))
	if err != nil {
		return fmt.Errorf("safesocket.Listen: %v", err)
	}

	server := &server{
		backendLogID: logid,
		logf:         logf,
		resetOnZero:  !opts.SurviveDisconnects,
	}

	// When the context is closed or when we return, whichever is first, close our listner
	// and all open connections.
	go func() {
		select {
		case <-ctx.Done():
		case <-runDone:
		}
		server.stopAll()
		listen.Close()
	}()
	logf("Listening on %v", listen.Addr())

	var store ipn.StateStore
	if opts.KubeSecret != "" {
		store, err = ipn.NewKubeStore(opts.KubeSecret)
		if err != nil {
			return fmt.Errorf("ipn.NewKubeStore(%q): %v", opts.StatePath, err)
		}
	} else if opts.StatePath != "" {
		store, err = ipn.NewFileStore(opts.StatePath)
		if err != nil {
			return fmt.Errorf("ipn.NewFileStore(%q): %v", opts.StatePath, err)
		}
		if opts.AutostartStateKey == "" {
			autoStartKey, err := store.ReadState(ipn.ServerModeStartKey)
			if err != nil && err != ipn.ErrStateNotExist {
				return fmt.Errorf("calling ReadState on %s: %w", opts.StatePath, err)
			}
			key := string(autoStartKey)
			if strings.HasPrefix(key, "user-") {
				uid := strings.TrimPrefix(key, "user-")
				u, err := server.lookupUserFromID(uid)
				if err != nil {
					logf("ipnserver: found server mode auto-start key %q; failed to load user: %v", key, err)
				} else {
					logf("ipnserver: found server mode auto-start key %q (user %s)", key, u.Username)
					server.serverModeUser = u
				}
				opts.AutostartStateKey = ipn.StateKey(key)
			}
		}
	} else {
		store = &ipn.MemoryStore{}
	}

	bo := backoff.NewBackoff("ipnserver", logf, 30*time.Second)
	var unservedConn net.Conn // if non-nil, accepted, but hasn't served yet

	eng, err := getEngine()
	if err != nil {
		logf("ipnserver: initial getEngine call: %v", err)
		for i := 1; ctx.Err() == nil; i++ {
			c, err := listen.Accept()
			if err != nil {
				logf("%d: Accept: %v", i, err)
				bo.BackOff(ctx, err)
				continue
			}
			logf("ipnserver: try%d: trying getEngine again...", i)
			eng, err = getEngine()
			if err == nil {
				logf("%d: GetEngine worked; exiting failure loop", i)
				unservedConn = c
				break
			}
			logf("ipnserver%d: getEngine failed again: %v", i, err)
			errMsg := err.Error()
			go func() {
				defer c.Close()
				bs := ipn.NewBackendServer(logf, nil, jsonNotifier(c, logf))
				bs.SendErrorMessage(errMsg)
				time.Sleep(time.Second)
			}()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	b, err := ipnlocal.NewLocalBackend(logf, logid, store, eng)
	if err != nil {
		return fmt.Errorf("NewLocalBackend: %v", err)
	}
	defer b.Shutdown()
	b.SetDecompressor(func() (controlclient.Decompressor, error) {
		return smallzstd.NewDecoder(nil)
	})

	if opts.DebugMux != nil {
		opts.DebugMux.HandleFunc("/debug/ipn", func(w http.ResponseWriter, r *http.Request) {
			serveHTMLStatus(w, b)
		})
	}

	server.b = b
	server.bs = ipn.NewBackendServer(logf, b, server.writeToClients)

	if opts.AutostartStateKey != "" {
		server.bs.GotCommand(context.TODO(), &ipn.Command{
			Version: version.Long,
			Start: &ipn.StartArgs{
				Opts: ipn.Options{StateKey: opts.AutostartStateKey},
			},
		})
	}

	systemd.Ready()
	for i := 1; ctx.Err() == nil; i++ {
		var c net.Conn
		var err error
		if unservedConn != nil {
			c = unservedConn
			unservedConn = nil
		} else {
			c, err = listen.Accept()
		}
		if err != nil {
			if ctx.Err() == nil {
				logf("ipnserver: Accept: %v", err)
				bo.BackOff(ctx, err)
			}
			continue
		}
		go server.serveConn(ctx, c, logger.WithPrefix(logf, fmt.Sprintf("ipnserver: conn%d: ", i)))
	}
	return ctx.Err()
}

// BabysitProc runs the current executable as a child process with the
// provided args, capturing its output, writing it to files, and
// restarting the process on any crashes.
//
// It's only currently (2020-10-29) used on Windows.
func BabysitProc(ctx context.Context, args []string, logf logger.Logf) {

	executable, err := os.Executable()
	if err != nil {
		panic("cannot determine executable: " + err.Error())
	}

	if runtime.GOOS == "windows" {
		if len(args) != 2 && args[0] != "/subproc" {
			panic(fmt.Sprintf("unexpected arguments %q", args))
		}
		logID := args[1]
		logf = filelogger.New("tailscale-service", logID, logf)
	}

	var proc struct {
		mu sync.Mutex
		p  *os.Process
	}

	done := make(chan struct{})
	go func() {
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
		var sig os.Signal
		select {
		case sig = <-interrupt:
			logf("BabysitProc: got signal: %v", sig)
			close(done)
		case <-ctx.Done():
			logf("BabysitProc: context done")
			sig = os.Kill
			close(done)
		}

		proc.mu.Lock()
		proc.p.Signal(sig)
		proc.mu.Unlock()
	}()

	bo := backoff.NewBackoff("BabysitProc", logf, 30*time.Second)

	for {
		startTime := time.Now()
		log.Printf("exec: %#v %v", executable, args)
		cmd := exec.Command(executable, args...)

		// Create a pipe object to use as the subproc's stdin.
		// When the writer goes away, the reader gets EOF.
		// A subproc can watch its stdin and exit when it gets EOF;
		// this is a very reliable way to have a subproc die when
		// its parent (us) disappears.
		// We never need to actually write to wStdin.
		rStdin, wStdin, err := os.Pipe()
		if err != nil {
			log.Printf("os.Pipe 1: %v", err)
			return
		}

		// Create a pipe object to use as the subproc's stdout/stderr.
		// We'll read from this pipe and send it to logf, line by line.
		// We can't use os.exec's io.Writer for this because it
		// doesn't care about lines, and thus ends up merging multiple
		// log lines into one or splitting one line into multiple
		// logf() calls. bufio is more appropriate.
		rStdout, wStdout, err := os.Pipe()
		if err != nil {
			log.Printf("os.Pipe 2: %v", err)
		}
		go func(r *os.File) {
			defer r.Close()
			rb := bufio.NewReader(r)
			for {
				s, err := rb.ReadString('\n')
				if s != "" {
					logf("%s", s)
				}
				if err != nil {
					break
				}
			}
		}(rStdout)

		cmd.Stdin = rStdin
		cmd.Stdout = wStdout
		cmd.Stderr = wStdout
		err = cmd.Start()

		// Now that the subproc is started, get rid of our copy of the
		// pipe reader. Bad things happen on Windows if more than one
		// process owns the read side of a pipe.
		rStdin.Close()
		wStdout.Close()

		if err != nil {
			log.Printf("starting subprocess failed: %v", err)
		} else {
			proc.mu.Lock()
			proc.p = cmd.Process
			proc.mu.Unlock()

			err = cmd.Wait()
			log.Printf("subprocess exited: %v", err)
		}

		// If the process finishes, clean up the write side of the
		// pipe. We'll make a new one when we restart the subproc.
		wStdin.Close()

		if os.Getenv("TS_DEBUG_RESTART_CRASHED") == "0" {
			log.Fatalf("Process ended.")
		}

		if time.Since(startTime) < 60*time.Second {
			bo.BackOff(ctx, fmt.Errorf("subproc early exit: %v", err))
		} else {
			// Reset the timeout, since the process ran for a while.
			bo.BackOff(ctx, nil)
		}

		select {
		case <-done:
			return
		default:
		}
	}
}

// FixedEngine returns a func that returns eng and a nil error.
func FixedEngine(eng wgengine.Engine) func() (wgengine.Engine, error) {
	return func() (wgengine.Engine, error) { return eng, nil }
}

// getEngineUntilItWorksWrapper returns a getEngine wrapper that does
// not call getEngine concurrently and stops calling getEngine once
// it's returned a working engine.
func getEngineUntilItWorksWrapper(getEngine func() (wgengine.Engine, error)) func() (wgengine.Engine, error) {
	var mu sync.Mutex
	var engGood wgengine.Engine
	return func() (wgengine.Engine, error) {
		mu.Lock()
		defer mu.Unlock()
		if engGood != nil {
			return engGood, nil
		}
		e, err := getEngine()
		if err != nil {
			return nil, err
		}
		engGood = e
		return e, nil
	}
}

type dummyAddr string
type oneConnListener struct {
	conn net.Conn
}

func (l *oneConnListener) Accept() (c net.Conn, err error) {
	c = l.conn
	if c == nil {
		err = io.EOF
		return
	}
	err = nil
	l.conn = nil
	return
}

func (l *oneConnListener) Close() error { return nil }

func (l *oneConnListener) Addr() net.Addr { return dummyAddr("unused-address") }

func (a dummyAddr) Network() string { return string(a) }
func (a dummyAddr) String() string  { return string(a) }

// protoSwitchConn is a net.Conn that's we want to speak HTTP to but
// it's already had a few bytes read from it to determine that it's
// HTTP. So we Read from its bufio.Reader. On Close, we we tell the
// server it's closed, so the server can account the who's connected.
type protoSwitchConn struct {
	s *server
	net.Conn
	br        *bufio.Reader
	closeOnce sync.Once
}

func (psc *protoSwitchConn) Read(p []byte) (int, error) { return psc.br.Read(p) }
func (psc *protoSwitchConn) Close() error {
	psc.closeOnce.Do(func() { psc.s.removeAndCloseConn(psc.Conn) })
	return nil
}

func (s *server) localhostHandler(ci connIdentity) http.Handler {
	lah := localapi.NewHandler(s.b, s.logf, s.backendLogID)
	lah.PermitRead, lah.PermitWrite = s.localAPIPermissions(ci)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/localapi/") {
			lah.ServeHTTP(w, r)
			return
		}
		if ci.NotWindows {
			io.WriteString(w, "<html><title>Tailscale</title><body><h1>Tailscale</h1>This is the local Tailscale daemon.")
			return
		}
		serveHTMLStatus(w, s.b)
	})
}

func serveHTMLStatus(w http.ResponseWriter, b *ipnlocal.LocalBackend) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	st := b.Status()
	// TODO(bradfitz): add LogID and opts to st?
	st.WriteHTML(w)
}

func peerPid(entries []netstat.Entry, la, ra netaddr.IPPort) int {
	for _, e := range entries {
		if e.Local == ra && e.Remote == la {
			return e.Pid
		}
	}
	return 0
}

// jsonNotifier returns a notify-writer func that writes ipn.Notify
// messages to w.
func jsonNotifier(w io.Writer, logf logger.Logf) func(ipn.Notify) {
	return func(n ipn.Notify) {
		if b, ok := marshalNotify(n, logf); ok {
			ipn.WriteMsg(w, b)
		}
	}
}

func marshalNotify(n ipn.Notify, logf logger.Logf) (b []byte, ok bool) {
	b, err := json.Marshal(n)
	if err != nil {
		logf("ipnserver: [unexpected] error serializing JSON: %v", err)
		return nil, false
	}
	if bytes.Contains(b, jsonEscapedZero) {
		logf("[unexpected] zero byte in BackendServer.send notify message: %q", b)
	}
	return b, true
}
