package session

import (
	"fmt"
	"io"
	"net"
	"time"
)

// ReportReadyToHost tells the host the environment is usable and waits for it
// to finish printing, mirroring the pipe handshake used on native Linux. The
// connection stays open while the session runs, and hostGone is called when
// the host closes it: when range on the host ends.
func ReportReadyToHost(port int, hostGone func()) {
	if port == 0 {
		return
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		return
	}
	if _, err := conn.Write([]byte{'R'}); err != nil {
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.ReadFull(conn, make([]byte, 1))
	conn.SetReadDeadline(time.Time{})
	go func() {
		defer conn.Close()
		io.Copy(io.Discard, conn)
		hostGone()
	}()
}
