package httpaas

import (
	"os"
	"testing"
	"gestor-daemon/internal/config"
)

// Helper function to test hosts file manipulation safely
func TestUpdateHostsLogic(t *testing.T) {
	// Setup a temporary hosts file
	tmpFile, err := os.CreateTemp("", "hosts_test")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	initialContent := "127.0.0.1 localhost\n192.168.10.10 ns.cloud.local\n"
	tmpFile.WriteString(initialContent)
	tmpFile.Close()

	// Mock GlobalConfig for test
	config.Global = config.Config{
		DNSZone: "cloud.local",
	}

	// Wait, we can't easily override hostsFile inside updateHosts without changing the function signature or adding a global var.
	// Let's refactor the file reading logic to be testable or we can test getNextFreeIP logic if we mock instancesFile.

	// Let's test getInstances and saveInstances with a temp instances file
	instancesFile = tmpFile.Name()
	
	// Write empty json
	os.WriteFile(instancesFile, []byte("[]"), 0644)

	// Add an instance
	insts := []Instance{
		{Name: "test1", IP: "192.168.10.100", URL: "http://test1.cloud.local"},
	}
	err = saveInstances(insts)
	if err != nil {
		t.Fatalf("saveInstances failed: %v", err)
	}

	// Read back
	loaded, err := getInstances()
	if err != nil {
		t.Fatalf("getInstances failed: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Name != "test1" {
		t.Errorf("Expected 1 instance named test1, got %v", loaded)
	}

	// Set base IP
	config.Global.BaseIPPrefix = "192.168.10"
	nextIP, err := getNextFreeIP()
	if err != nil {
		t.Fatalf("getNextFreeIP failed: %v", err)
	}
	if nextIP != "192.168.10.101" { // .100 is used
		t.Errorf("Expected 192.168.10.101, got %s", nextIP)
	}
}
