package config

import "os"

// Config holds the centralized configuration for the orchestrator daemon.
type Config struct {
	DNSServerIP   string
	DNSZone       string
	SSHUser       string
	VBoxHostIP    string
	VBoxHostUser  string
	BridgeAdapter string
	BaseIPPrefix  string
}

// Global provides application-wide access to configuration settings.
var Global Config

// Load initializes Global by reading from environment variables
// and falling back to default values that align with the Vagrantfile.
func Load() {
	Global = Config{
		DNSServerIP:   GetEnvOrDefault("DNS_SERVER_IP", "192.168.10.10"),
		DNSZone:       GetEnvOrDefault("DNS_ZONE", "cloud.local"),
		SSHUser:       GetEnvOrDefault("SSH_USER", "vagrant"),
		VBoxHostIP:    GetEnvOrDefault("VBOX_HOST_IP", ""),
		VBoxHostUser:  GetEnvOrDefault("VBOX_HOST_USER", "usuario_host"),
		BridgeAdapter: GetEnvOrDefault("BRIDGE_ADAPTER", "eth0"),
		BaseIPPrefix:  GetEnvOrDefault("BASE_IP_PREFIX", "192.168.10"),
	}
}

// GetEnvOrDefault retrieves an environment variable value or returns a default value.
func GetEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
