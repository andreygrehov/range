package session

import (
	"fmt"
	"io"
	"net"
	"time"
)

// ReportReadyToHost tells the host the environment is usable and waits for it
// to finish printing, mirroring the pipe handshake used on native Linux.
func ReportReadyToHost(port int) {
	if port == 0 {
		return
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{'R'}); err != nil {
		return
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.ReadFull(conn, make([]byte, 1))
}
