package chserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	chshare "github.com/XevoInc/chisel/share"
	"golang.org/x/crypto/ssh"
)

// handleClientHandler is the main http websocket handler for the chisel server
func (s *Server) handleClientHandler(w http.ResponseWriter, r *http.Request) {
	//websockets upgrade AND has chisel prefix
	upgrade := strings.ToLower(r.Header.Get("Upgrade"))
	protocol := r.Header.Get("Sec-WebSocket-Protocol")
	if upgrade == "websocket" && strings.HasPrefix(protocol, "xevo-chisel-") {
		if protocol == chshare.ProtocolVersion {
			s.handleWebsocket(w, r)
			return
		}
		//print into server logs and silently fall-through
		s.Infof("ignored client connection using protocol '%s', expected '%s'",
			protocol, chshare.ProtocolVersion)
	}
	//proxy target was provided
	if s.reverseProxy != nil {
		s.reverseProxy.ServeHTTP(w, r)
		return
	}
	//no proxy defined, provide access to health/version checks
	switch r.URL.String() {
	case "/health":
		w.Write([]byte("OK\n"))
		return
	case "/version":
		w.Write([]byte(chshare.BuildVersion))
		return
	}
	//missing :O
	w.WriteHeader(404)
	w.Write([]byte("Not found"))
}

// handleWebsocket is responsible for handling the websocket connection
func (s *Server) handleWebsocket(w http.ResponseWriter, req *http.Request) {
	id := atomic.AddInt32(&s.sessCount, 1)
	clog := s.Fork("session#%d", id)
	clog.Debugf("Upgrading to websocket")
	wsConn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		clog.Debugf("Failed to upgrade (%s)", err)
		return
	}
	conn := chshare.NewWebSocketConn(wsConn)
	// perform SSH handshake on net.Conn
	clog.Debugf("Handshaking...")
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, s.sshConfig)
	if err != nil {
		s.Debugf("Failed to handshake (%s)", err)
		return
	}
	// pull the users from the session map
	var user *chshare.User
	if s.users.Len() > 0 {
		sid := string(sshConn.SessionID())
		user, _ = s.sessions.Get(sid)
		s.sessions.Del(sid)
	}
	//verify configuration
	clog.Debugf("Verifying configuration")
	//wait for request, with timeout
	var r *ssh.Request
	select {
	case r = <-reqs:
	case <-time.After(10 * time.Second):
		sshConn.Close()
		return
	}
	failed := func(err error) {
		clog.Debugf("Failed: %s", err)
		r.Reply(false, []byte(err.Error()))
	}
	if r.Type != "config" {
		failed(s.Errorf("expecting config request"))
		return
	}
	c, err := chshare.DecodeConfig(r.Payload)
	if err != nil {
		failed(s.Errorf("invalid config"))
		return
	}
	//print if client and server  versions dont match
	if c.Version != chshare.BuildVersion {
		v := c.Version
		if v == "" {
			v = "<unknown>"
		}
		clog.Infof("Client version (%s) differs from server version (%s)",
			v, chshare.BuildVersion)
	}
	//confirm reverse tunnels are allowed
	for _, chd := range c.ChannelDescriptors {
		if chd.Reverse && !s.reverseOk {
			clog.Debugf("Denied reverse port forwarding request, please enable --reverse")
			failed(s.Errorf("Reverse port forwaring not enabled on server"))
			return
		}
	}
	//if user is provided, ensure they have
	//access to the desired remotes
	if user != nil {
		for _, chd := range c.ChannelDescriptors {
			chdString := chd.String()
			if !user.HasAccess(chdString) {
				failed(s.Errorf("access to '%s' denied", chdString))
				return
			}
		}
	}
	//set up reverse port forwarding
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i, chd := range c.ChannelDescriptors {
		clog.Debugf("%s", chd.LongString())
		if chd.Reverse {
			proxy := chshare.NewTCPProxy(s.Logger, func() ssh.Conn { return sshConn }, i, chd)
			if err := proxy.Start(ctx); err != nil {
				failed(s.Errorf("%s", err))
				return
			}
		}
	}
	//success!
	r.Reply(true, nil)
	//prepare connection logger
	clog.Debugf("Open")
	go s.handleSSHRequests(clog, reqs)
	go s.handleSSHChannels(clog, chans)
	sshConn.Wait()
	clog.Debugf("Close")
}

func (s *Server) handleSSHRequests(clientLog *chshare.Logger, reqs <-chan *ssh.Request) {
	for r := range reqs {
		switch r.Type {
		case "ping":
			r.Reply(true, nil)
		default:
			clientLog.Debugf("Unknown request: %s", r.Type)
			r.Reply(false, []byte(fmt.Sprintf("Unknown request type: %s", r.Type)))
		}
	}
}

func (s *Server) handleSSHChannels(clientLog *chshare.Logger, chans <-chan ssh.NewChannel) {
	for ch := range chans {
		epdJSON := ch.ExtraData()
		var epd chshare.ChannelEndpointDescriptor
		err := json.Unmarshal(epdJSON, &epd)
		if err != nil {
			clientLog.Debugf("Error: Remote channel connect request: bad JSON parameter string: '%s'", epdJSON)
			ch.Reject(ssh.UnknownChannelType, "Bad JSON ExtraData")
			continue
		}
		clientLog.Debugf("Remote channel connect request, endpoint ='%s'", epd.LongString())
		if epd.Role != chshare.ChannelEndpointRoleSkeleton {
			clientLog.Debugf("Error: Remote channel connect request: Role must be skeleton: '%s'", epd.LongString())
			ch.Reject(ssh.Prohibited, "Role must be skeleton")
			continue
		}
		if epd.Type == chshare.ChannelEndpointTypeStdio {
			clientLog.Debugf("Error: Remote channel connect request: Server-side skeleton STDIO not supported: '%s'", epd.LongString())
			ch.Reject(ssh.Prohibited, "Server-side STDIO not supported")
			continue
		}
		if epd.Type == chshare.ChannelEndpointTypeLoop {
			clientLog.Debugf("Error: Remote channel connect request: Loop channels not yet not supported: '%s'", epd.LongString())
			ch.Reject(ssh.Prohibited, "Loop channels not yet supported")
			continue
		}
		if epd.Type == chshare.ChannelEndpointTypeUnix {
			clientLog.Debugf("Error: Remote channel connect request: Unix domain sockets not yet not supported: '%s'", epd.LongString())
			ch.Reject(ssh.Prohibited, "Unix domain sockets not yet supported")
			continue
		}
		socks := epd.Type == chshare.ChannelEndpointTypeSocks
		//dont accept socks when --socks5 isn't enabled
		if socks && s.socksServer == nil {
			clientLog.Debugf("Denied socks request, please enable --socks5")
			ch.Reject(ssh.Prohibited, "SOCKS5 is not enabled on the server")
			continue
		}

		// TODO: The actual local connect request should succeed before we accept the remote request.
		//       Need to refactor code here
		stream, reqs, err := ch.Accept()
		if err != nil {
			clientLog.Debugf("Failed to accept stream: %s", err)
			continue
		}
		go ssh.DiscardRequests(reqs)
		//handle stream type
		connID := s.connStats.New()
		if socks {
			go s.handleSocksStream(clientLog.Fork("socksconn#%d", connID), stream)
		} else {
			go chshare.HandleTCPStream(clientLog.Fork("conn#%d", connID), &s.connStats, stream, epd.Path)
		}
	}
}

func (s *Server) handleSocksStream(l *chshare.Logger, src io.ReadWriteCloser) {
	conn := chshare.NewRWCConn(src)
	s.connStats.Open()
	l.Debugf("%s Opening", s.connStats)
	err := s.socksServer.ServeConn(conn)
	s.connStats.Close()
	if err != nil && !strings.HasSuffix(err.Error(), "EOF") {
		l.Debugf("%s: Closed (error: %s)", s.connStats, err)
	} else {
		l.Debugf("%s: Closed", s.connStats)
	}
}
