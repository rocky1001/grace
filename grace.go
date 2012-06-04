// Package grace allows for gracefully waiting for a listener to
// finish serving it's active requests.
package grace

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

var (
	// This error is returned by Inherits() when we're not inheriting any fds.
	ErrNotInheriting = errors.New("no inherited listeners")

	// This error is returned by Listener.Accept() when Close is in progress.
	ErrAlreadyClosed = errors.New("already closed")
)

const (
	// Used to indicate a graceful restart in the new process.
	envCountKey = "GRACE"

	// The error returned by the standard library when the socket is closed.
	errClosed = "use of closed network connection"

	// Used for the counter chan.
	inc = true
	dec = false
)

// A Listener providing a graceful Close process and can be sent
// across processes using the underlying File descriptor.
type Listener interface {
	net.Listener

	// Will return the underlying file representing this Listener.
	File() (f *os.File, err error)
}

// A goroutine based counter that provides graceful Close for listeners.
type listener struct {
	Listener
	closed       bool      // Indicates we're already closed.
	closeRequest chan bool // Send a bool here to indicate we want to Close.
	allClosed    chan bool // Receive from here will indicate a clean Close.
	counter      chan bool // Use the inc/dec counters.
}

// Allows for us to notice when the connection is closed.
type conn struct {
	net.Conn
	counter chan bool
}

func (c conn) Close() error {
	c.counter <- dec
	return c.Conn.Close()
}

// Create a new listener.
func NewListener(l Listener) Listener {
	i := &listener{
		Listener:     l,
		closeRequest: make(chan bool),
		allClosed:    make(chan bool),
		counter:      make(chan bool),
	}
	go i.enabler()
	return i
}

func (l *listener) enabler() {
	var counter uint64
	var change bool
	for {
		select {
		case <-l.closeRequest:
			l.closed = true
		case change = <-l.counter:
			if change == inc {
				counter++
			} else {
				counter--
			}
		}
		if l.closed && counter == 0 {
			l.allClosed <- true
			break
		}
	}
}

func (l *listener) Close() error {
	if l.closed == true {
		return nil
	}
	l.closeRequest <- true
	<-l.allClosed
	return l.Listener.Close()
}

func (l *listener) Accept() (net.Conn, error) {
	if l.closed == true {
		return nil, ErrAlreadyClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		if strings.HasSuffix(err.Error(), errClosed) {
			return nil, ErrAlreadyClosed
		}
		return nil, err
	}
	l.counter <- inc
	return conn{
		Conn:    c,
		counter: l.counter,
	}, nil
}

// Wait for signals.
func Wait(listeners []Listener) (err error) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGUSR2)
	for {
		sig := <-ch
		switch sig {
		case syscall.SIGTERM:
			var wg sync.WaitGroup
			wg.Add(len(listeners))
			for _, l := range listeners {
				go func(l Listener) {
					cErr := l.Close()
					if cErr != nil {
						err = cErr
					}
					wg.Done()
				}(l)
			}
			wg.Wait()
			return
		case syscall.SIGUSR2:
			rErr := Restart(listeners)
			if rErr != nil {
				return rErr
			}
		}
	}
	panic("not reached")
}

// Try to inherit listeners from environment variables.
func Inherit() (listeners []Listener, err error) {
	countStr := os.Getenv(envCountKey)
	if countStr == "" {
		return nil, ErrNotInheriting
	}
	count, err := strconv.Atoi(countStr)
	if err != nil {
		return nil, err
	}
	// If we are inheriting, the listeners will begin at fd 3
	for i := 3; i < 3+count; i++ {
		file := os.NewFile(uintptr(i), "listener")
		tmp, err := net.FileListener(file)
		file.Close()
		if err != nil {
			return nil, err
		}
		l := tmp.(*net.TCPListener)
		listeners = append(listeners, NewListener(l))
	}
	return
}

// Start the Close process in the parent.
func CloseParent() error {
	return syscall.Kill(os.Getppid(), syscall.SIGTERM)
}

// Restart the process passing it the given listeners.
func Restart(listeners []Listener) (err error) {
	if len(listeners) == 0 {
		return errors.New("restart must be given listeners.")
	}
	files := make([]*os.File, len(listeners))
	for i, l := range listeners {
		files[i], err = l.File()
		if err != nil {
			return err
		}
		defer files[i].Close()
		syscall.CloseOnExec(int(files[i].Fd()))
	}
	argv0, err := exec.LookPath(os.Args[0])
	if err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	allFiles := append([]*os.File{os.Stdin, os.Stdout, os.Stderr}, files...)
	allFiles = append(allFiles, nil)
	_, err = os.StartProcess(argv0, os.Args, &os.ProcAttr{
		Dir:   wd,
		Env:   append(os.Environ(), fmt.Sprintf("%s=%d", envCountKey, len(files))),
		Files: allFiles,
	})
	return err
}
