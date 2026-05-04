// Package main provides DNS management capabilities for the infrastructure orchestrator.
// This file handles dynamic DNS registration/deregistration with BIND9 using nsupdate (RFC 2136).
package main

import (
"fmt"
"os/exec"
"strings"
)

// DNS server configuration constants.
// These values define the DNS infrastructure that the orchestrator manages.
const (
// dnsServerIP is the IP address of the BIND9 DNS server managing the domain
dnsServerIP = "192.168.10.10"
// dnsZone is the DNS zone managed by the infrastructure
dnsZone = "cloud.local"
)

// registerDNS adds a new A record to the DNS zone for a newly provisioned instance.
// This function uses nsupdate to dynamically update the BIND9 DNS server,
// establishing hostname to IP mappings for freshly created VMs.
// The zone must have allow-update configured to permit this orchestrator's IP address.
//
// hostname: the hostname to register (without zone suffix, e.g., "myapp")
// ip: the IPv4 address to associate with the hostname
// Returns an error if the DNS update fails.
func registerDNS(hostname, ip string) error {
commands := fmt.Sprintf(`server %s
zone %s
update add %s.%s. 60 A %s
show
send
`, dnsServerIP, dnsZone, hostname, dnsZone, ip)

return executeNsUpdate(commands)
}

// deregisterDNS removes an A record from the DNS zone when an instance is terminated.
// This function ensures DNS cleanup when VMs are destroyed, preventing stale records
// and allowing hostname reuse.
//
// hostname: the hostname to deregister (without zone suffix)
// Returns an error if the DNS update fails.
func deregisterDNS(hostname string) error {
commands := fmt.Sprintf(`server %s
zone %s
update delete %s.%s. A
show
send
`, dnsServerIP, dnsZone, hostname, dnsZone)

return executeNsUpdate(commands)
}

// executeNsUpdate sends RFC 2136 dynamic update commands to the BIND9 DNS server.
// It invokes the nsupdate utility with the provided update commands through stdin.
// This is the internal mechanism for all DNS record management operations.
//
// commands: the nsupdate command script containing zone update directives
// Returns an error if nsupdate execution fails or returns a non-zero exit code.
func executeNsUpdate(commands string) error {
cmd := exec.Command("nsupdate")
cmd.Stdin = strings.NewReader(commands)

out, err := cmd.CombinedOutput()
if err != nil {
return fmt.Errorf("error ejecutando nsupdate: %v, salida: %s", err, string(out))
}

return nil
}
