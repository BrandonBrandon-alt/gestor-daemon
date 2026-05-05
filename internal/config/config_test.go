package config

import (
	"os"
	"testing"
)

func TestLoadConfigDefault(t *testing.T) {
	os.Unsetenv("DNS_SERVER_IP")
	os.Unsetenv("DNS_ZONE")
	os.Unsetenv("SSH_USER")
	os.Unsetenv("BASE_IP_PREFIX")

	Load()

	if Global.DNSServerIP != "192.168.10.10" {
		t.Errorf("Expected default DNSServerIP to be 192.168.10.10, got %s", Global.DNSServerIP)
	}
	if Global.DNSZone != "cloud.local" {
		t.Errorf("Expected default DNSZone to be cloud.local, got %s", Global.DNSZone)
	}
	if Global.SSHUser != "vagrant" {
		t.Errorf("Expected default SSHUser to be vagrant, got %s", Global.SSHUser)
	}
	if Global.BaseIPPrefix != "192.168.10" {
		t.Errorf("Expected default BaseIPPrefix to be 192.168.10, got %s", Global.BaseIPPrefix)
	}
}

func TestLoadConfigWithEnv(t *testing.T) {
	os.Setenv("DNS_SERVER_IP", "10.0.0.1")
	os.Setenv("DNS_ZONE", "test.local")
	os.Setenv("SSH_USER", "admin")
	os.Setenv("BASE_IP_PREFIX", "10.0.0")

	Load()

	if Global.DNSServerIP != "10.0.0.1" {
		t.Errorf("Expected DNSServerIP to be 10.0.0.1, got %s", Global.DNSServerIP)
	}
	if Global.DNSZone != "test.local" {
		t.Errorf("Expected DNSZone to be test.local, got %s", Global.DNSZone)
	}
	if Global.SSHUser != "admin" {
		t.Errorf("Expected SSHUser to be admin, got %s", Global.SSHUser)
	}
	if Global.BaseIPPrefix != "10.0.0" {
		t.Errorf("Expected BaseIPPrefix to be 10.0.0, got %s", Global.BaseIPPrefix)
	}

	os.Unsetenv("DNS_SERVER_IP")
	os.Unsetenv("DNS_ZONE")
	os.Unsetenv("SSH_USER")
	os.Unsetenv("BASE_IP_PREFIX")
}
