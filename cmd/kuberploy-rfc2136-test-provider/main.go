package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kuberploy/kuberploy/internal/rfc2136test"
	"github.com/miekg/dns"
)

func main() {
	cfg, err := rfc2136test.ConfigFromEnvironment()
	if err != nil {
		slog.Error("RFC2136 qualification authority configuration rejected", "error", err)
		os.Exit(1)
	}
	authority, err := rfc2136test.NewAuthority(cfg)
	if err != nil {
		slog.Error("RFC2136 qualification authority configuration rejected", "error", err)
		os.Exit(1)
	}
	udp := &dns.Server{Addr: cfg.ListenAddress, Net: "udp", Handler: authority, TsigSecret: authority.TSIGSecrets(), MsgAcceptFunc: rfc2136test.AcceptMessage}
	tcp := &dns.Server{Addr: cfg.ListenAddress, Net: "tcp", Handler: authority, TsigSecret: authority.TSIGSecrets(), MsgAcceptFunc: rfc2136test.AcceptMessage}
	errors := make(chan error, 2)
	go func() { errors <- udp.ListenAndServe() }()
	go func() { errors <- tcp.ListenAndServe() }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errors:
		slog.Error("RFC2136 qualification authority stopped", "error", err)
		os.Exit(1)
	case <-signals:
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
	}
}
