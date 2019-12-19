package chshare

import (
	"sync/atomic"
	"context"
	"encoding/json"
	socks5 "github.com/armon/go-socks5"
	"golang.org/x/crypto/ssh"
	"net"
	"time"
)

// ProxySSHSession wraps a primary SSH connection with a single client proxy
type ProxySSHSession struct {
	*Logger

	// Server is the chisel proxy server on which this session is running
	server *Server

	// id is a unique id of this session, for logging purposes
	id int32

	// sshConn is the server-side ssh session connection
	sshConn *ssh.ServerConn

	// newSSHChannels is the chan on which connect requests from remote stub to local endpoint are eceived
	newSSHChannels <-chan ssh.NewChannel

	// sshRequests is the chan on which ssh requests are received (including initial config request)
	sshRequests <-chan *ssh.Request

	// done is closed at completion of Run
	done chan struct{}
}

// LastSSHSessionID is the last allocated ID for SSH sessions, for logging purposes
var LastSSHSessionID int32

// AllocSSHSessionID allocates a monotonically incresing session ID number (for debugging/logging only)
func AllocSSHSessionID() int32 {
	id := atomic.AddInt32(&LastSSHSessionID, 1)
	return id
}

// NewServerSSHSession creates a server-side proxy session object
func NewServerSSHSession(server *Server) (*ProxySSHSession, error) {
	id := AllocSSHSessionID()
	s := &ProxySSHSession{
		Logger: server.Logger.Fork("SSHSession#%d", id),
		server: server,
		id:     id,
		done:   make(chan struct{}),
	}
	return s, nil
}

// Implement LocalChannelEnv

// IsServer returns true if this is a proxy server; false if it is a cliet
func (s *ProxySSHSession) IsServer() bool {
	return true
}

// GetLoopServer returns the shared LoopServer if loop protocol is enabled; nil otherwise
func (s *ProxySSHSession) GetLoopServer() *LoopServer {
	return s.server.loopServer
}

// GetSocksServer returns the shared socks5 server if socks protocol is enabled;
// nil otherwise
func (s *ProxySSHSession) GetSocksServer() *socks5.Server {
	return s.server.socksServer
}

// GetSSHConn waits for and returns the main ssh.Conn that this proxy is using to
// communicate with the remote proxy. It is possible that goroutines servicing
// local stub sockets will ask for this before it is available (if for example
// a listener on the client accepts a connection before the server has ackknowledged
// configuration. An error response indicates that the SSH connection failed to initialize.
func (s *ProxySSHSession) GetSSHConn() (ssh.Conn, error) {
	return s.sshConn, nil
}

// receiveSSHRequest receives a single SSH request from the ssh.ServerConn. Can be
// canceled with the context
func (s *ProxySSHSession) receiveSSHRequest(ctx context.Context) (*ssh.Request, error) {
	select {
	case r := <-s.sshRequests:
		return r, nil
	case <-ctx.Done():
		return nil, s.DebugErrorf("SSH request not received: %s", ctx.Err())
	}
}

// sendSSHReply sends a reply to an SSH request received from ssh.ServerConn.
// If the context is cancelled before the response is sent, a goroutine will leak
// until the ssh.ServerConn is closed (which should come quickly due to err returned)
func (s *ProxySSHSession) sendSSHReply(ctx context.Context, r *ssh.Request, ok bool, payload []byte) error {
	// TODO: currently no way to cancel the send without closing the sshConn
	result := make(chan error)

	go func() {
		err := r.Reply(ok, payload)
		result <- err
		close(result)
	}()

	var err error

	select {
	case err = <-result:
	case <-ctx.Done():
		err = ctx.Err()
	}

	if err != nil {
		err = s.DebugErrorf("SSH repy send failed: %s", err)
	}

	return err
}

// sendSSHErrorReply sends an error reply to an SSH request received from ssh.ServerConn.
// If the context is cancelled before the response is sent, a goroutine will leak
// until the ssh.ServerConn is closed (which should come quickly due to err returned)
func (s *ProxySSHSession) sendSSHErrorReply(ctx context.Context, r *ssh.Request, err error) error {
	s.Debugf("Sending SSH error reply: %s", err)
	return s.sendSSHReply(ctx, r, false, []byte(err.Error()))
}

// runWithSSHConn runs a proxy session from a client from start to end, given
// an incoming ssh.ServerConn. On exit, the incoming ssh.ServerConn still
// needs to be closed.
func (s *ProxySSHSession) runWithSSHConn(
	ctx context.Context,
	sshConn *ssh.ServerConn,
	newSSHChannels <-chan ssh.NewChannel,
	sshRequests <-chan *ssh.Request,
) error {
	subCtx, subCtxCancel := context.WithCancel(ctx)
	defer subCtxCancel()

	s.sshConn = sshConn
	s.newSSHChannels = newSSHChannels
	s.sshRequests = sshRequests

	// pull the users from the session map
	var user *User
	if s.server.users.Len() > 0 {
		sid := string(sshConn.SessionID())
		user, _ = s.server.sessions.Get(sid)
		s.server.sessions.Del(sid)
	}

	//verify configuration
	s.Debugf("Receiving configuration")
	// wait for configuration request, with timeout
	cfgCtx, cfgCtxCancel := context.WithTimeout(subCtx, 10*time.Second)
	r, err := s.receiveSSHRequest(cfgCtx)
	cfgCtxCancel()
	if err != nil {
		return s.DebugErrorf("receiveSSHRequest failed: %s", err)
	}

	s.Debugf("Received SSH Req")

	// convenience function to send an error reply and return
	// the original error. Ignores failures sending the reply
	// since we will be bailing out anyway
	failed := func(err error) error {
		s.sendSSHErrorReply(subCtx, r, err)
		return err
	}

	if r.Type != "config" {
		return failed(s.DebugErrorf("Expecting \"config\" request, got \"%s\"", r.Type))
	}

	c := &SessionConfigRequest{}
	err = c.Unmarshal(r.Payload)
	if err != nil {
		return failed(s.DebugErrorf("Invalid session config request encoding: %s", err))
	}

	//print if client and server  versions dont match
	if c.Version != BuildVersion {
		v := c.Version
		if v == "" {
			v = "<unknown>"
		}
		s.Infof("WARNING: Chisel Client version (%s) differs from server version (%s)", v, BuildVersion)
	}

	//confirm reverse tunnels are allowed
	for _, chd := range c.ChannelDescriptors {
		if chd.Reverse && !s.server.reverseOk {
			return failed(s.DebugErrorf("Reverse port forwarding not enabled on server"))
		}
	}
	//if user is provided, ensure they have
	//access to the desired remotes
	if user != nil {
		for _, chd := range c.ChannelDescriptors {
			chdString := chd.String()
			if !user.HasAccess(chdString) {
				return failed(s.DebugErrorf("Access to \"%s\" denied", chdString))
			}
		}
	}

	//set up reverse port forwarding
	for i, chd := range c.ChannelDescriptors {
		if chd.Reverse {
			s.Debugf("Reverse-mode route[%d] %s; starting stub listener", i, chd.String())
			proxy := NewTCPProxy(s.Logger, func() ssh.Conn { return sshConn }, i, chd)
			if err := proxy.Start(subCtx); err != nil {
				return failed(s.DebugErrorf("Unable to start stub listener %s: %s", chd.String(), err))
			}
		} else {
			s.Debugf("Forward-mode route[%d] %s; connections will be created on demand", i, chd.String())
		}
	}

	//success!
	err = s.sendSSHReply(subCtx, r, true, nil)
	if err != nil {
		return s.DebugErrorf("Failed to send SSH config success response: %s", err)
	}

	go s.handleSSHRequests(subCtx, sshRequests)
	go s.handleSSHChannels(subCtx, newSSHChannels)

	s.Debugf("SSH session up and running")

	return sshConn.Wait()
}

// Run runs an SSH session to completion from an incoming
// just-connected client socket (which has already been wrapped on a websocket)
// The incoming conn is not
func (s *ProxySSHSession) Run(ctx context.Context, conn net.Conn) error {
	s.Debugf("SSH Handshaking...")
	sshConn, newSSHChannels, sshRequests, err := ssh.NewServerConn(conn, s.server.sshConfig)
	if err != nil {
		s.Debugf("Failed to handshake (%s)", err)
		close(s.done)
		return err
	}

	err = s.runWithSSHConn(ctx, sshConn, newSSHChannels, sshRequests)
	if err != nil {
		s.Debugf("SSH session failed: %s", err)
	}

	s.Debugf("Closing SSH connection")
	sshConn.Close()
	close(s.done)
	return err
}

func (s *ProxySSHSession) handleSSHRequests(ctx context.Context, sshRequests <-chan *ssh.Request) {
	for {
		select {
		case req := <-sshRequests:
			if req == nil {
				s.Debugf("End of incoming SSH request stream")
				return
			}
			switch req.Type {
			case "ping":
				err := s.sendSSHReply(ctx, req, true, nil)
				if err != nil {
					s.Debugf("SSH ping reply send failed, ignoring: %s", err)
				}
			default:
				err := s.DebugErrorf("Unknown SSH request type: %s", req.Type)
				err = s.sendSSHErrorReply(ctx, req, err)
				if err != nil {
					s.Debugf("SSH send reply for unknown request type failed, ignoring: %s", err)
				}
			}
		case <-ctx.Done():
			s.Debugf("SSH request stream processing aborted: %s", ctx.Err())
			return
		}
	}
}

// handleSSHNewChannel handles an incoming ssh.NewCHannel request from beginning to end
// It is intended to run in its own goroutine, so as to not block other
// SSH activity
func (s *ProxySSHSession) handleSSHNewChannel(ctx context.Context, ch ssh.NewChannel) error {
	reject := func(reason ssh.RejectionReason, err error) error {
		s.Debugf("Sending SSH NewChannel rejection (reason=%v): %s", reason, err)
		// TODO allow cancellation with ctx
		rejectErr := ch.Reject(reason, err.Error())
		if rejectErr != nil {
			s.Debugf("Unable to send SSH NewChannel reject response, ignoring: %s", rejectErr)
		}
		return err
	}
	epdJSON := ch.ExtraData()
	epd := &ChannelEndpointDescriptor{}
	err := json.Unmarshal(epdJSON, epd)
	if err != nil {
		return reject(ssh.UnknownChannelType, s.server.Errorf("Badly formatted NewChannel request"))
	}
	s.Debugf("SSH NewChannel request, endpoint ='%s'", epd.String())
	ep, err := NewLocalSkeletonChannelEndpoint(s.Logger, s, epd)
	if err != nil {
		s.Debugf("Failed to create skeleton endpoint for SSH NewChannel: %s", err)
		return reject(ssh.Prohibited, err)
	}

	// TODO: The actual local connect request should succeed before we accept the remote request.
	//       Need to refactor code here
	// TODO: Allow cancellation with ctx
	sshChannel, sshRequests, err := ch.Accept()
	if err != nil {
		s.Debugf("Failed to accept SSH NewChannel: %s", err)
		ep.Close()
		return err
	}

	// This will shut down when sshChannel is closed
	go ssh.DiscardRequests(sshRequests)

	// wrap the ssh.Channel to look like a ChannelConn
	sshConn, err := NewSSHConn(s.Logger, sshChannel)
	if err != nil {
		s.Debugf("Failed wrap SSH NewChannel: %s", err)
		sshChannel.Close()
		ep.Close()
		return err
	}

	// sshChannel is now wrapped by sshConn, and will be closed when sshConn is closed

	var extraData []byte
	numSent, numReceived, err := ep.DialAndServe(ctx, sshConn, extraData)

	// sshConn and sshChannel have now been closed

	if err != nil {
		s.Debugf("NewChannel session ended with error after %d bytes (caller->called), %d bytes (called->caller): %s", numSent, numReceived, err)
	} else {
		s.Debugf("NewChannel session ended normally after %d bytes (caller->called), %d bytes (called->caller)", numSent, numReceived)
	}

	return err
}

func (s *ProxySSHSession) handleSSHChannels(ctx context.Context, newChannels <-chan ssh.NewChannel) {
	for {
		select {
		case ch := <-newChannels:
			if ch == nil {
				s.Debugf("End of incoming SSH NewChannels stream")
				return
			}
			go s.handleSSHNewChannel(ctx, ch)
		case <-ctx.Done():
			s.Debugf("SSH NewChannels stream processing aborted: %s", ctx.Err())
			return
		}
	}
}