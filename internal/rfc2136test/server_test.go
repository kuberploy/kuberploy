package rfc2136test

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestAuthorityServesAuthenticatedRFC2136OverUDP(t *testing.T) {
	authority, err := NewAuthority(Config{ListenAddress: "127.0.0.1:5353", Zone: "qualification.test.", Nameserver: "ns.qualification.test.", TSIGName: "external-dns.qualification.test.", TSIGSecret: testSecret, TTL: 30})
	if err != nil {
		t.Fatal(err)
	}
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var received atomic.Value
	server := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		received.Store([4]int{len(request.Question), len(request.Answer), len(request.Ns), len(request.Extra)})
		authority.ServeDNS(w, request)
	}), TsigSecret: authority.TSIGSecrets(), MsgAcceptFunc: AcceptMessage}
	started := make(chan error, 1)
	go func() { started <- server.ActivateAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.ShutdownContext(ctx)
		<-started
	})

	client := &dns.Client{Net: "udp", Timeout: time.Second, TsigSecret: authority.TSIGSecrets()}
	update := new(dns.Msg).SetUpdate("qualification.test.")
	rr, _ := dns.NewRR("route.qualification.test. 60 IN A 192.0.2.20")
	update.Insert([]dns.RR{rr})
	update.SetTsig("external-dns.qualification.test.", dns.HmacSHA256, 300, time.Now().Unix())
	response, _, err := client.Exchange(update, packet.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if response.Rcode != dns.RcodeSuccess {
		t.Fatalf("update rcode = %s sections=%v", dns.RcodeToString[response.Rcode], received.Load())
	}
	if response.IsTsig() == nil || canonicalName(response.IsTsig().Algorithm) != canonicalName(dns.HmacSHA256) {
		t.Fatalf("update response was not signed with HMAC-SHA256: %#v", response.IsTsig())
	}
	query := new(dns.Msg)
	query.SetQuestion("route.qualification.test.", dns.TypeA)
	response, _, err = client.Exchange(query, packet.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Answer) != 1 || response.Answer[0].String() != "route.qualification.test.\t30\tIN\tA\t192.0.2.20" {
		t.Fatalf("query answer = %#v", response.Answer)
	}
}

const testSecret = "MDEyMzQ1Njc4OWFiY2RlZg=="

func TestAuthorityRequiresAuthenticatedInZoneUpdates(t *testing.T) {
	authority, err := NewAuthority(Config{
		ListenAddress: "127.0.0.1:5353",
		Zone:          "qualification.test.",
		Nameserver:    "ns.qualification.test.",
		TSIGName:      "external-dns.qualification.test.",
		TSIGSecret:    testSecret,
		TTL:           30,
	})
	if err != nil {
		t.Fatal(err)
	}

	update := new(dns.Msg).SetUpdate("qualification.test.")
	rr, err := dns.NewRR("app.qualification.test. 60 IN A 192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	update.Insert([]dns.RR{rr})
	update.SetTsig("external-dns.qualification.test.", dns.HmacSHA256, 300, 1)
	w := &recordingWriter{}
	authority.ServeDNS(w, update)
	if w.message == nil || w.message.Rcode != dns.RcodeSuccess {
		t.Fatalf("authenticated update rcode = %#v", w.message)
	}

	query := new(dns.Msg)
	query.SetQuestion("app.qualification.test.", dns.TypeA)
	w = &recordingWriter{}
	authority.ServeDNS(w, query)
	if w.message == nil || len(w.message.Answer) != 1 || w.message.Answer[0].String() != "app.qualification.test.\t30\tIN\tA\t192.0.2.10" {
		t.Fatalf("query answer = %#v", w.message)
	}

	unsigned := new(dns.Msg).SetUpdate("qualification.test.")
	unsigned.Insert([]dns.RR{rr})
	w = &recordingWriter{}
	authority.ServeDNS(w, unsigned)
	if w.message.Rcode != dns.RcodeNotAuth {
		t.Fatalf("unsigned update rcode = %s", dns.RcodeToString[w.message.Rcode])
	}

	badMAC := new(dns.Msg).SetUpdate("qualification.test.")
	badMAC.Insert([]dns.RR{rr})
	badMAC.SetTsig("external-dns.qualification.test.", dns.HmacSHA256, 300, 1)
	w = &recordingWriter{tsigStatus: errors.New("bad MAC")}
	authority.ServeDNS(w, badMAC)
	if w.message.Rcode != dns.RcodeNotAuth {
		t.Fatalf("bad-MAC update rcode = %s", dns.RcodeToString[w.message.Rcode])
	}

	outside := new(dns.Msg).SetUpdate("qualification.test.")
	outsideRR, _ := dns.NewRR("escape.example. 30 IN A 192.0.2.11")
	outside.Insert([]dns.RR{outsideRR})
	outside.SetTsig("external-dns.qualification.test.", dns.HmacSHA256, 300, 1)
	w = &recordingWriter{}
	authority.ServeDNS(w, outside)
	if w.message.Rcode != dns.RcodeNotZone {
		t.Fatalf("outside update rcode = %s", dns.RcodeToString[w.message.Rcode])
	}
}

func TestConfigRejectsWeakOrNonCanonicalSecrets(t *testing.T) {
	base := Config{ListenAddress: "127.0.0.1:5353", Zone: "qualification.test", Nameserver: "ns.qualification.test", TSIGName: "key.qualification.test", TTL: 30}
	for _, secret := range []string{"", "not-base64", base64.StdEncoding.EncodeToString([]byte("too-short")), base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef"))} {
		candidate := base
		candidate.TSIGSecret = secret
		if _, err := NewAuthority(candidate); err == nil {
			t.Fatalf("accepted invalid secret %q", secret)
		}
	}
	base.TSIGSecret = testSecret
	authority, err := NewAuthority(base)
	if err != nil {
		t.Fatal(err)
	}
	if authority.cfg.Zone != "qualification.test." || authority.cfg.Nameserver != "ns.qualification.test." {
		t.Fatalf("names were not canonicalized: %#v", authority.cfg)
	}
}

type recordingWriter struct {
	message    *dns.Msg
	tsigStatus error
}

func (w *recordingWriter) LocalAddr() net.Addr             { return &net.UDPAddr{} }
func (w *recordingWriter) RemoteAddr() net.Addr            { return &net.UDPAddr{} }
func (w *recordingWriter) WriteMsg(message *dns.Msg) error { w.message = message.Copy(); return nil }
func (w *recordingWriter) Write(value []byte) (int, error) { return len(value), nil }
func (w *recordingWriter) Close() error                    { return nil }
func (w *recordingWriter) TsigStatus() error               { return w.tsigStatus }
func (w *recordingWriter) TsigTimersOnly(bool)             {}
func (w *recordingWriter) Hijack()                         {}
