package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// ─── DNS Dinámico con BIND9 (nsupdate) ───────────────────────────────────────

const dnsServerIP = "192.168.10.10"
const dnsZone = "cloud.local"

// registerDNS añade un nuevo registro A en la zona DNS.
// Requiere que la zona tenga allow-update configurado para la IP de esta máquina.
func registerDNS(hostname, ip string) error {
	commands := fmt.Sprintf(`server %s
zone %s
update add %s.%s. 60 A %s
show
send
`, dnsServerIP, dnsZone, hostname, dnsZone, ip)

	return executeNsUpdate(commands)
}

// deregisterDNS elimina un registro A existente en la zona DNS.
func deregisterDNS(hostname string) error {
	commands := fmt.Sprintf(`server %s
zone %s
update delete %s.%s. A
show
send
`, dnsServerIP, dnsZone, hostname, dnsZone)

	return executeNsUpdate(commands)
}

func executeNsUpdate(commands string) error {
	cmd := exec.Command("nsupdate")
	cmd.Stdin = strings.NewReader(commands)
	
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("error ejecutando nsupdate: %v, salida: %s", err, string(out))
	}
	
	return nil
}
