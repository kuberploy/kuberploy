package rfc2136test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const (
	defaultListenAddress = "0.0.0.0:5353"
	defaultTTL           = 30
	maximumRecords       = 2048
)

type Config struct {
	ListenAddress string
	Zone          string
	Nameserver    string
	TSIGName      string
	TSIGSecret    string
	TTL           uint32
}

func ConfigFromEnvironment() (Config, error) {
	cfg := Config{
		ListenAddress: strings.TrimSpace(os.Getenv("KUBERPLOY_RFC2136_LISTEN_ADDRESS")),
		Zone:          strings.TrimSpace(os.Getenv("KUBERPLOY_RFC2136_ZONE")),
		Nameserver:    strings.TrimSpace(os.Getenv("KUBERPLOY_RFC2136_NAMESERVER")),
		TSIGName:      strings.TrimSpace(os.Getenv("KUBERPLOY_RFC2136_TSIG_NAME")),
		TSIGSecret:    strings.TrimSpace(os.Getenv("KUBERPLOY_RFC2136_TSIG_SECRET")),
		TTL:           defaultTTL,
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = defaultListenAddress
	}
	if raw := strings.TrimSpace(os.Getenv("KUBERPLOY_RFC2136_TTL_SECONDS")); raw != "" {
		value, err := strconv.ParseUint(raw, 10, 32)
		if err != nil || value < 1 || value > 300 {
			return Config{}, errors.New("KUBERPLOY_RFC2136_TTL_SECONDS must be an integer from 1 to 300")
		}
		cfg.TTL = uint32(value)
	}
	return normalizeConfig(cfg)
}

func normalizeConfig(cfg Config) (Config, error) {
	if _, _, err := net.SplitHostPort(cfg.ListenAddress); err != nil {
		return Config{}, fmt.Errorf("listen address: %w", err)
	}
	cfg.Zone = canonicalName(cfg.Zone)
	cfg.Nameserver = canonicalName(cfg.Nameserver)
	cfg.TSIGName = canonicalName(cfg.TSIGName)
	if cfg.Zone == "." || cfg.Nameserver == "." || cfg.TSIGName == "." {
		return Config{}, errors.New("zone, nameserver, and TSIG name are required DNS names")
	}
	if !dns.IsSubDomain(cfg.Zone, cfg.Nameserver) {
		return Config{}, errors.New("nameserver must be inside the authoritative zone")
	}
	secretBytes, err := base64.StdEncoding.DecodeString(cfg.TSIGSecret)
	if err != nil || len(secretBytes) < 16 || len(secretBytes) > 64 {
		return Config{}, errors.New("TSIG secret must be 16 to 64 bytes of canonical base64")
	}
	if base64.StdEncoding.EncodeToString(secretBytes) != cfg.TSIGSecret {
		return Config{}, errors.New("TSIG secret must use canonical padded base64")
	}
	if cfg.TTL < 1 || cfg.TTL > 300 {
		return Config{}, errors.New("TTL must be from 1 to 300 seconds")
	}
	return cfg, nil
}

func canonicalName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "."
	}
	return dns.Fqdn(value)
}

type recordKey struct {
	name     string
	typeCode uint16
}

type Authority struct {
	cfg     Config
	mu      sync.RWMutex
	records map[recordKey]map[string]dns.RR
	serial  uint32
}

// AcceptMessage is deliberately narrower than the library default (which does
// not admit RFC 2136 UPDATE) and rejects oversized packets before decoding.
func AcceptMessage(header dns.Header) dns.MsgAcceptAction {
	if header.Bits&(1<<15) != 0 || header.Qdcount != 1 {
		return dns.MsgReject
	}
	opcode := int(header.Bits>>11) & 0xf
	switch opcode {
	case dns.OpcodeQuery:
		if header.Ancount != 0 || header.Nscount != 0 || header.Arcount > 1 {
			return dns.MsgReject
		}
	case dns.OpcodeUpdate:
		if header.Ancount != 0 || header.Nscount > maximumRecords || header.Arcount != 1 {
			return dns.MsgReject
		}
	default:
		return dns.MsgRejectNotImplemented
	}
	return dns.MsgAccept
}

func NewAuthority(cfg Config) (*Authority, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Authority{
		cfg:     normalized,
		records: make(map[recordKey]map[string]dns.RR),
		serial:  uint32(time.Now().UTC().Unix()),
	}, nil
}

func (a *Authority) TSIGSecrets() map[string]string {
	return map[string]string{a.cfg.TSIGName: a.cfg.TSIGSecret}
}

func (a *Authority) ServeDNS(w dns.ResponseWriter, request *dns.Msg) {
	response := new(dns.Msg)
	response.SetReply(request)
	response.Authoritative = true
	if request.Opcode == dns.OpcodeUpdate {
		a.update(w, request, response)
	} else if request.Opcode == dns.OpcodeQuery {
		a.query(request, response)
	} else {
		response.Rcode = dns.RcodeNotImplemented
	}
	if requestTSIG := request.IsTsig(); requestTSIG != nil {
		response.SetTsig(requestTSIG.Hdr.Name, requestTSIG.Algorithm, requestTSIG.Fudge, time.Now().UTC().Unix())
	}
	_ = w.WriteMsg(response)
}

func (a *Authority) update(w dns.ResponseWriter, request, response *dns.Msg) {
	tsig := request.IsTsig()
	if tsig == nil || canonicalName(tsig.Hdr.Name) != a.cfg.TSIGName || canonicalName(tsig.Algorithm) != canonicalName(dns.HmacSHA256) || w.TsigStatus() != nil {
		response.Rcode = dns.RcodeNotAuth
		return
	}
	if len(request.Question) != 1 || canonicalName(request.Question[0].Name) != a.cfg.Zone || request.Question[0].Qtype != dns.TypeSOA {
		response.Rcode = dns.RcodeNotZone
		return
	}
	if len(request.Answer) != 0 {
		response.Rcode = dns.RcodeNotImplemented
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, rr := range request.Ns {
		name := canonicalName(rr.Header().Name)
		if !dns.IsSubDomain(a.cfg.Zone, name) || name == a.cfg.Zone {
			response.Rcode = dns.RcodeNotZone
			return
		}
		if !supportedType(rr.Header().Rrtype) {
			response.Rcode = dns.RcodeRefused
			return
		}
	}
	working := cloneRecords(a.records)
	for _, rr := range request.Ns {
		key := recordKey{canonicalName(rr.Header().Name), rr.Header().Rrtype}
		switch rr.Header().Class {
		case dns.ClassINET:
			if countRecords(working) >= maximumRecords {
				response.Rcode = dns.RcodeRefused
				return
			}
			rr = dns.Copy(rr)
			rr.Header().Name = key.name
			rr.Header().Ttl = a.cfg.TTL
			if working[key] == nil {
				working[key] = make(map[string]dns.RR)
			}
			working[key][rr.String()] = rr
		case dns.ClassNONE:
			if set := working[key]; set != nil {
				delete(set, canonicalRRString(rr, a.cfg.TTL))
				if len(set) == 0 {
					delete(working, key)
				}
			}
		case dns.ClassANY:
			if rr.Header().Rrtype == dns.TypeANY {
				for existing := range working {
					if existing.name == key.name {
						delete(working, existing)
					}
				}
			} else {
				delete(working, key)
			}
		default:
			response.Rcode = dns.RcodeFormatError
			return
		}
	}
	a.records = working
	a.serial++
	response.Rcode = dns.RcodeSuccess
}

func (a *Authority) query(request, response *dns.Msg) {
	if len(request.Question) != 1 {
		response.Rcode = dns.RcodeFormatError
		return
	}
	question := request.Question[0]
	name := canonicalName(question.Name)
	if !dns.IsSubDomain(a.cfg.Zone, name) {
		response.Rcode = dns.RcodeRefused
		return
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if name == a.cfg.Zone {
		switch question.Qtype {
		case dns.TypeSOA, dns.TypeANY:
			response.Answer = append(response.Answer, a.soa())
		case dns.TypeNS:
			response.Answer = append(response.Answer, &dns.NS{Hdr: dns.RR_Header{Name: a.cfg.Zone, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: a.cfg.TTL}, Ns: a.cfg.Nameserver})
		}
		return
	}
	found := false
	for key, set := range a.records {
		if key.name != name || (question.Qtype != dns.TypeANY && key.typeCode != question.Qtype) {
			continue
		}
		found = true
		values := make([]string, 0, len(set))
		for text := range set {
			values = append(values, text)
		}
		sort.Strings(values)
		for _, text := range values {
			response.Answer = append(response.Answer, dns.Copy(set[text]))
		}
	}
	if !found {
		response.Rcode = dns.RcodeNameError
		response.Ns = append(response.Ns, a.soa())
	}
}

func (a *Authority) soa() dns.RR {
	return &dns.SOA{Hdr: dns.RR_Header{Name: a.cfg.Zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: a.cfg.TTL}, Ns: a.cfg.Nameserver, Mbox: "hostmaster." + a.cfg.Zone, Serial: a.serial, Refresh: 60, Retry: 30, Expire: 300, Minttl: a.cfg.TTL}
}

func supportedType(value uint16) bool {
	switch value {
	case dns.TypeA, dns.TypeAAAA, dns.TypeCNAME, dns.TypeTXT, dns.TypeANY:
		return true
	default:
		return false
	}
}

func cloneRecords(source map[recordKey]map[string]dns.RR) map[recordKey]map[string]dns.RR {
	result := make(map[recordKey]map[string]dns.RR, len(source))
	for key, set := range source {
		result[key] = make(map[string]dns.RR, len(set))
		for text, rr := range set {
			result[key][text] = dns.Copy(rr)
		}
	}
	return result
}

func countRecords(records map[recordKey]map[string]dns.RR) int {
	total := 0
	for _, set := range records {
		total += len(set)
	}
	return total
}

func canonicalRRString(rr dns.RR, ttl uint32) string {
	copy := dns.Copy(rr)
	copy.Header().Name = canonicalName(copy.Header().Name)
	copy.Header().Class = dns.ClassINET
	copy.Header().Ttl = ttl
	return copy.String()
}
