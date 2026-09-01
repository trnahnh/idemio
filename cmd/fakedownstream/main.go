package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
)

type switchableListener struct {
	mu      sync.Mutex
	addr    string
	handler http.Handler
	server  *http.Server
	serving bool
}

func newSwitchableListener(bind string, handler http.Handler) (*switchableListener, error) {
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("bind %s: %w", bind, err)
	}

	s := &switchableListener{addr: listener.Addr().String(), handler: handler}
	s.serve(listener)
	s.serving = true
	return s, nil
}

func (s *switchableListener) serve(listener net.Listener) {
	server := &http.Server{Handler: s.handler}
	s.server = server
	go server.Serve(listener)
}

func (s *switchableListener) down() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.serving {
		return nil
	}
	if err := s.server.Close(); err != nil {
		return fmt.Errorf("close data listener: %w", err)
	}
	s.serving = false
	return nil
}

func (s *switchableListener) up() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.serving {
		return nil
	}
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("rebind %s: %w", s.addr, err)
	}
	s.serve(listener)
	s.serving = true
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	dataAddr := flag.String("data-addr", "127.0.0.1:0", "address for the write path")
	controlAddr := flag.String("control-addr", "127.0.0.1:0", "address for scripting and the probe")
	ledgerPath := flag.String("ledger", "", "path to the append-only execution ledger")
	flag.Parse()

	if *ledgerPath == "" {
		return errors.New("-ledger is required")
	}

	records, err := openLedger(*ledgerPath)
	if err != nil {
		return err
	}
	defer records.close()

	downstream := &fake{ledger: records, scripts: newScripts()}

	data, err := newSwitchableListener(*dataAddr, downstream.dataMux())
	if err != nil {
		return err
	}

	control, err := net.Listen("tcp", *controlAddr)
	if err != nil {
		return fmt.Errorf("bind %s: %w", *controlAddr, err)
	}

	fmt.Printf("data=%s\ncontrol=%s\n", data.addr, control.Addr().String())
	os.Stdout.Sync()

	server := &http.Server{Handler: downstream.controlMux(data)}
	if err := server.Serve(control); err != nil {
		return fmt.Errorf("serve control: %w", err)
	}
	return nil
}
