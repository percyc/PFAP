package p2p

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover"
)

func TestHandshakeChecksDoNotWaitForStopLock(t *testing.T) {
	key := newkey()
	self := discover.PubkeyID(&key.PublicKey)
	srv := &Server{Config: Config{PrivateKey: key, MaxPeers: 10}, ourHandshake: &protoHandshake{ID: self}}
	for _, id := range []discover.NodeID{randomID(), self} {
		srv.lock.Lock()
		done := make(chan error, 1)
		go func(id discover.NodeID) { done <- srv.encHandshakeChecks(nil, 0, &conn{id: id}) }(id)
		select {
		case err := <-done:
			srv.lock.Unlock()
			if id == self && err != DiscSelf {
				t.Fatalf("self connection accepted: %v", err)
			}
			if id != self && err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			srv.lock.Unlock()
			<-done
			t.Fatal("handshake check waits for the lock held by Stop while Stop waits for the network loop")
		}
	}
}

// Model an Accept that has obtained a socket just as Close begins. Its
// handshake cannot acquire srv.lock until Stop returns, occupying the only
// handshake slot. The listener must exit on quit without waiting for that slot.
type shutdownRaceListener struct {
	accepted chan struct{}
	closed   chan struct{}
	once     sync.Once
	conn     net.Conn
}

func (l *shutdownRaceListener) Accept() (net.Conn, error) {
	select {
	case <-l.accepted:
		return nil, errors.New("listener closed")
	default:
	}
	close(l.accepted)
	<-l.closed
	return l.conn, nil
}
func (l *shutdownRaceListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }
func (l *shutdownRaceListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 30303}
}

func TestStopCancelsListenerWaitingForHandshakeSlot(t *testing.T) {
	oldSelf := IsSelfNode
	label := "shutdown-test"
	IsSelfNode = &label // avoid the legacy external-address lookup in this unit test
	defer func() { IsSelfNode = oldSelf }()
	fd, remote := net.Pipe()
	defer fd.Close()
	defer remote.Close()
	l := &shutdownRaceListener{accepted: make(chan struct{}), closed: make(chan struct{}), conn: fd}
	srv := &Server{Config: Config{PrivateKey: newkey(), MaxPendingPeers: 1}, running: true, listener: l, quit: make(chan struct{}), log: log.New(), newTransport: newRLPX}
	srv.loopWG.Add(1)
	go srv.listenLoop()
	select {
	case <-l.accepted:
	case <-time.After(time.Second):
		t.Fatal("listener did not reach Accept")
	}
	done := make(chan struct{})
	go func() { srv.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop waits forever for a handshake slot held by SetupConn")
	}
	remote.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := remote.Read(make([]byte, 1)); err == nil {
		t.Fatal("pending connection was not closed")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("pending connection remained open after stop")
	}
}
