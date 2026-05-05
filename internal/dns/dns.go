// Package dns provides DNS management capabilities for the infrastructure orchestrator.
// This file handles dynamic DNS registration/deregistration with BIND9 natively using github.com/miekg/dns.
package dns

import (
	"fmt"
	"time"

	"github.com/miekg/dns"
	"gestor-daemon/internal/config"
)

// RegisterDNS adds a new A record to the DNS zone for a newly provisioned instance.
func RegisterDNS(hostname, ip string) error {
	server := config.Global.DNSServerIP + ":53"
	zone := config.Global.DNSZone + "."
	fqdn := hostname + "." + zone

	m := new(dns.Msg)
	m.SetUpdate(zone)

	rr, err := dns.NewRR(fmt.Sprintf("%s 60 IN A %s", fqdn, ip))
	if err != nil {
		return fmt.Errorf("error creating DNS RR: %v", err)
	}

	m.Insert([]dns.RR{rr})

	c := new(dns.Client)
	c.Timeout = 5 * time.Second

	reply, _, err := c.Exchange(m, server)
	if err != nil {
		return fmt.Errorf("error sending DNS update: %v", err)
	}
	if reply != nil && reply.Rcode != dns.RcodeSuccess {
		return fmt.Errorf("DNS server returned error code: %s", dns.RcodeToString[reply.Rcode])
	}

	return nil
}

// DeregisterDNS removes an A record from the DNS zone when an instance is terminated.
func DeregisterDNS(hostname string) error {
	server := config.Global.DNSServerIP + ":53"
	zone := config.Global.DNSZone + "."
	fqdn := hostname + "." + zone

	m := new(dns.Msg)
	m.SetUpdate(zone)

	rr, err := dns.NewRR(fmt.Sprintf("%s 0 IN ANY", fqdn))
	if err != nil {
		return fmt.Errorf("error creating DNS RR: %v", err)
	}

	m.Remove([]dns.RR{rr})

	c := new(dns.Client)
	c.Timeout = 5 * time.Second

	reply, _, err := c.Exchange(m, server)
	if err != nil {
		return fmt.Errorf("error sending DNS delete: %v", err)
	}
	if reply != nil && reply.Rcode != dns.RcodeSuccess {
		return fmt.Errorf("DNS server returned error code: %s", dns.RcodeToString[reply.Rcode])
	}

	return nil
}
