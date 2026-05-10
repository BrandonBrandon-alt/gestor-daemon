package dns

import (
	"fmt"
	"os/exec"
	"strings"
	"gestor-daemon/internal/config"
)

// UpdateLocalHosts adds or updates an entry in the host's /etc/hosts file.
// It uses sudo tee as configured by setup-hosts.sh to avoid password prompts.
func UpdateLocalHosts(hostname, ip string) error {
	zone := config.Global.DNSZone
	fqdn := fmt.Sprintf("%s.%s", hostname, zone)
	
	// First, remove existing entry to avoid duplicates
	_ = RemoveLocalHosts(hostname)
	
	entry := fmt.Sprintf("%s    %s", ip, fqdn)
	cmd := fmt.Sprintf("echo '%s' | sudo tee -a /etc/hosts", entry)
	
	out, err := exec.Command("bash", "-c", cmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("error updating /etc/hosts: %v | output: %s", err, string(out))
	}
	
	fmt.Printf("✅ /etc/hosts actualizado: %s -> %s\n", fqdn, ip)
	return nil
}

// RemoveLocalHosts removes any entry matching the hostname.cloud.local from /etc/hosts.
func RemoveLocalHosts(hostname string) error {
	zone := config.Global.DNSZone
	fqdn := fmt.Sprintf("%s.%s", hostname, zone)
	
	// Escape dots for sed
	escapedFQDN := strings.ReplaceAll(fqdn, ".", "\\.")
	
	// Use sed to remove lines containing the fqdn exactly
	// We use a pattern that matches the start of the line or space before the fqdn
	cmd := fmt.Sprintf("sudo sed -i '/[[:space:]]%s$/d' /etc/hosts", escapedFQDN)
	
	out, err := exec.Command("bash", "-c", cmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("error removing from /etc/hosts: %v | output: %s", err, string(out))
	}
	
	return nil
}
