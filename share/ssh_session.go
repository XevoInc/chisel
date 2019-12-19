package chshare

import (
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/ssh"
	"sync/atomic"
)

// SSHSession wraps a primary SSH connection to the remote proxy
type SSHSession struct {
	*Logger

	// id is a unique id of this session, for logging purposes
	id int32

	// strname is a name of this session for logging purposes
	strname string

	localChannelEnv LocalChannelEnv

	// sshConn is the ssh session connection
	sshConn ssh.Conn

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

// InitSSHSession initializes a new SSHSession
func(s *SSHSession) InitSSHSession(logger *Logger, localChannelEnv LocalChannelEnv) {
	s.id = AllocSSHSessionID()
	s.localChannelEnv = localChannelEnv
	s.done = make(chan struct{})
	s.strname = fmt.Sprintf("SSHSession#%d", s.id)
	s.Logger = logger.Fork(s.strname)
}

func (s *SSHSession) String() string {
	return s.strname
}

// receiveSSHRequest receives a single SSH request from the ssh.ServerConn. Can be
// canceled with the context
func (s *SSHSession) receiveSSHRequest(ctx context.Context) (*ssh.Request, error) {
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
func (s *SSHSession) sendSSHReply(ctx context.Context, r *ssh.Request, ok bool, payload []byte) error {
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
func (s *SSHSession) sendSSHErrorReply(ctx context.Context, r *ssh.Request, err error) error {
	s.Debugf("Sending SSH error reply: %s", err)
	return s.sendSSHReply(ctx, r, false, []byte(err.Error()))
}

func (s *SSHSession) handleSSHRequests(ctx context.Context, sshRequests <-chan *ssh.Request) {
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
func (s *SSHSession) handleSSHNewChannel(ctx context.Context, ch ssh.NewChannel) error {
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
		return reject(ssh.UnknownChannelType, s.Errorf("Badly formatted NewChannel request"))
	}
	s.Debugf("SSH NewChannel request, endpoint ='%s'", epd.String())

	// TODO: ***MUST*** implement access control here

	ep, err := NewLocalSkeletonChannelEndpoint(s.Logger, s.localChannelEnv, epd)
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

func (s *SSHSession) handleSSHChannels(ctx context.Context, newChannels <-chan ssh.NewChannel) {
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
