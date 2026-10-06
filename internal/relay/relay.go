// Package relay carries a TCP connection on to another connection: a port
// published on this machine, relayed into an environment.
package relay

import (
	"io"
	"log"
	"net"
	"sync"
)

// Listen accepts connections on addr and relays each to what dial returns,
// until stop is called.
func Listen(addr string, dial func() (io.ReadWriteCloser, error)) (stop func() error, err error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				inside, err := dial()
				if err != nil {
					log.Printf("range: %s: %v", addr, err)
					conn.Close()
					return
				}
				Copy(conn, inside)
			}()
		}
	}()
	return func() error { listener.Close(); <-done; return nil }, nil
}

// Copy copies between a and b, both ways, until either side is done, and
// closes both.
func Copy(a, b io.ReadWriteCloser) {
	var once sync.Once
	closeBoth := func() { a.Close(); b.Close() }
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(a, b); once.Do(closeBoth) }()
	go func() { defer wg.Done(); io.Copy(b, a); once.Do(closeBoth) }()
	wg.Wait()
}
