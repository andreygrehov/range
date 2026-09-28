package nbd

import (
	"net"
	"testing"
)

func TestNBDClientRejectsForeignServer(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	go server.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	if _, _, err := clientHandshake(client); err == nil {
		t.Fatal("a non-NBD greeting was accepted")
	}
}
