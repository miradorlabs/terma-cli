package sensor

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"
)

// ErrNoEnforcement is a backend's answer to a policy it can observe but not enforce.
var ErrNoEnforcement = errors.New("this platform's sensor cannot block; policy rules are reported only")

// Rule denies a watched session one destination or one part of the file system. Exactly
// one of Host, IP and Path is set. A Host matches its subdomains too; a Path is a file or a
// directory and everything under it.
type Rule struct {
	ID   int    `json:"id"`
	Host string `json:"host,omitempty"`
	IP   string `json:"ip,omitempty"`
	Path string `json:"path,omitempty"`
}

// Policy is what watched sessions may not reach. Without Enforce the backend only reports
// what it would have denied (audit mode).
type Policy struct {
	Enforce bool   `json:"enforce"`
	Rules   []Rule `json:"rules"`
}

// Resolved is a policy as the kernel matches it: addresses and paths, each to its rule,
// and the host rules as written, for a backend that sees names (a macOS content filter).
type Resolved struct {
	Enforce bool
	IPs     map[netip.Addr]int
	Paths   map[string]int
	Hosts   map[string]int
}

// resolve turns p into addresses: the names the sensor has seen answered, and a lookup of
// each host now. A host's address the sensor first learns from a DNS answer is denied from
// then on, which can be just after the first connection to it has started.
func resolve(ctx context.Context, p Policy, seen map[netip.Addr]string) Resolved {
	r := Resolved{Enforce: p.Enforce, IPs: map[netip.Addr]int{}, Paths: map[string]int{}, Hosts: map[string]int{}}
	for _, rule := range p.Rules {
		switch {
		case rule.IP != "":
			if pfx, err := netip.ParsePrefix(rule.IP); err == nil && pfx.IsSingleIP() {
				r.IPs[pfx.Addr()] = rule.ID
			} else if a, err := netip.ParseAddr(rule.IP); err == nil {
				r.IPs[a] = rule.ID
			}
		case rule.Host != "":
			r.Hosts[rule.Host] = rule.ID
			for a, host := range seen {
				if hostMatches(rule.Host, host) {
					r.IPs[a] = rule.ID
				}
			}
			lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			addrs, _ := net.DefaultResolver.LookupNetIP(lctx, "ip", rule.Host)
			cancel()
			for _, a := range addrs {
				r.IPs[a.Unmap()] = rule.ID
			}
		case rule.Path != "":
			r.Paths[rule.Path] = rule.ID
		}
	}
	return r
}

// ruleFor is the host rule host falls under, if any.
func ruleFor(p Policy, host string) (int, bool) {
	for _, rule := range p.Rules {
		if rule.Host != "" && hostMatches(rule.Host, host) {
			return rule.ID, true
		}
	}
	return 0, false
}

func hostMatches(rule, host string) bool {
	rule, host = strings.ToLower(strings.TrimSuffix(rule, ".")), strings.ToLower(strings.TrimSuffix(host, "."))
	return host == rule || strings.HasSuffix(host, "."+rule)
}
