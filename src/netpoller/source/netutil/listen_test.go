package netutil

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const defaultMaxOpenFiles = 256
const timeout = 5 * time.Second

func TestLimitListener(t *testing.T) {
	const max = 5
	attempts := (maxOpenFiles() - max) / 2
	if attempts > 256 { // maximum length of accept queue is 128 by default
		attempts = 256
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l = LimitListener(l, max)

	var open int32
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := atomic.AddInt32(&open, 1); n > max {
			t.Errorf("%d open connections, want <= %d", n, max)
		}
		defer atomic.AddInt32(&open, -1)
		time.Sleep(10 * time.Millisecond)
		fmt.Fprint(w, "some body")
	}))

	var wg sync.WaitGroup
	var failed int32
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := http.Client{Timeout: 3 * time.Second}
			r, err := c.Get("http://" + l.Addr().String())
			if err != nil {
				t.Log(err)
				atomic.AddInt32(&failed, 1)
				return
			}
			defer r.Body.Close()
			io.Copy(ioutil.Discard, r.Body)
		}()
	}
	wg.Wait()

	// We expect some Gets to fail as the kernel's accept queue is filled,
	// but most should succeed.
	if int(failed) >= attempts/2 {
		t.Errorf("%d requests failed within %d attempts", failed, attempts)
	}
}

type errorListener struct {
	net.Listener
}

func (errorListener) Accept() (net.Conn, error) {
	return nil, errFake
}

var errFake = errors.New("fake error from errorListener")

// This used to hang.
func TestLimitListenerError(t *testing.T) {
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		const n = 2
		ll := LimitListener(errorListener{}, n)
		for i := 0; i < n+1; i++ {
			_, err := ll.Accept()
			if err != errFake {
				errCh <- fmt.Errorf("Accept error = %v; want errFake", err)
				return
			}
		}
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("server: %v", err)
		}
	case <-time.After(timeout):
		t.Fatal("timeout. deadlock?")
	}
}

func TestLimitListenerClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ln = LimitListener(ln, 1)

	errCh := make(chan error)
	go func() {
		defer close(errCh)
		c, err := net.DialTimeout("tcp", ln.Addr().String(), timeout)
		if err != nil {
			errCh <- err
			return
		}
		c.Close()
	}()

	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	err = <-errCh
	if err != nil {
		t.Fatalf("DialTimeout: %v", err)
	}

	acceptDone := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
			t.Errorf("Unexpected successful Accept()")
		}
		close(acceptDone)
	}()

	// Wait a tiny bit to ensure the Accept() is blocking.
	time.Sleep(10 * time.Millisecond)
	ln.Close()

	select {
	case <-acceptDone:
	case <-time.After(timeout):
		t.Fatalf("Accept() still blocking")
	}
}