package main

import (
	"net"
	"os"
	"sync"
)

// OneConnection is the backing connection dressed up as net.Listener.
// Only the first call to Accept will deliver, all subsequent will block until it is closed.
func OneConnection(connection *os.File) (net.Listener, error) {
	pc, err := net.FileListener(connection)
	if err != nil {
		return nil, err
	}
	return &wrappedConnection{
		Listener: pc,
		file:     connection,
	}, nil
}

// wrappedConnection implements net.Listener, but for an accepted connection.
type wrappedConnection struct {
	// Both are backed by the same file descriptor.
	net.Listener
	file *os.File

	mu       sync.Mutex
	doneChan <-chan struct{}
	permErr  error
}

func (c *wrappedConnection) Accept() (net.Conn, error) {
	// The FileConn is gotten here for its error "fcntl: too many open files"
	// that can be used to back off.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.permErr != nil {
		return nil, c.permErr
	}
	if c.doneChan != nil {
		return c.tailWaitUntilFirstIsDone()
	}

	sharedBlockingChan := make(chan struct{})
	c.doneChan = sharedBlockingChan

	conn, err := net.FileConn(c.file)
	if err != nil {
		close(sharedBlockingChan)
		c.permErr = err
		return nil, err
	}

	return &cascadingCloser{conn, sharedBlockingChan}, err
}

func (c *wrappedConnection) tailWaitUntilFirstIsDone() (net.Conn, error) {
	<-c.doneChan
	return nil, os.ErrClosed
}

func (c *wrappedConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.permErr != nil {
		c.permErr = os.ErrClosed
	}
	return c.file.Close()
}

// cascadingCloser is used to unblock any receivers listening to the given channel.
type cascadingCloser struct {
	net.Conn

	closeChan chan<- struct{}
}

// Close implements the net.Conn interface.
func (c *cascadingCloser) Close() error {
	close(c.closeChan)
	return c.Conn.Close()
}
