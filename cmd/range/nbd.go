package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/andreygrehov/range/internal/bytesize"
	"github.com/andreygrehov/range/internal/nbd"
)

func commandNBD(args []string) error {
	if len(args) == 0 {
		return errors.New("nbd: expected \"serve\" or \"attach\"")
	}
	switch args[0] {
	case "serve":
		return commandNBDServe(args[1:])
	case "attach":
		return commandNBDAttach(args[1:])
	default:
		return fmt.Errorf("nbd: unknown subcommand %q", args[0])
	}
}

func commandNBDServe(args []string) error {
	var addr string
	var readOnly bool
	r, _, _, err := openFromArgs("nbd serve", args, func(fs *flag.FlagSet) {
		fs.StringVar(&addr, "addr", "127.0.0.1:10809", "listen address")
		fs.BoolVar(&readOnly, "read-only", true, "export read-only (writes are not supported)")
	})
	if err != nil {
		return err
	}
	defer r.Close()
	if !readOnly {
		return errors.New("nbd serve: writable exports are not supported; drop --read-only=false")
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	go r.PublishStats(5 * time.Second)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	log.Printf("serving %s (%s) as NBD on %s", r.Ident.URI, bytesize.Format(r.Size()), addr)
	log.Printf("connect with: nbd-client %s -N export /dev/nbd0", strings.Replace(addr, ":", " ", 1))
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				printStats(r.Snapshot())
				return nil
			}
			return err
		}
		go func() {
			defer conn.Close()
			if err := nbd.ServerHandshake(conn, r.Size()); err != nil {
				log.Printf("handshake: %v", err)
				return
			}
			if err := nbd.Serve(conn, r, r.Size()); err != nil {
				log.Printf("session: %v", err)
			}
		}()
	}
}

func commandNBDAttach(args []string) error {
	var readOnly bool
	r, _, rest, err := openFromArgs("nbd attach", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&readOnly, "read-only", true, "export read-only (writes are not supported)")
	})
	if err != nil {
		return err
	}
	defer r.Close()
	if !readOnly {
		return errors.New("nbd attach: writable exports are not supported; drop --read-only=false")
	}
	if len(rest) == 0 {
		return errors.New("nbd attach: missing <device>, e.g. /dev/nbd0")
	}
	device := rest[0]
	go r.PublishStats(5 * time.Second)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("attaching %s (%s) to %s", r.Ident.URI, bytesize.Format(r.Size()), device)
	if err := nbd.Attach(ctx, device, r, r.Size()); err != nil {
		return err
	}
	printStats(r.Snapshot())
	return nil
}
