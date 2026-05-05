// Package virtualbox provides utilities for managing VirtualBox VMs.
package virtualbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gestor-daemon/internal/config"
	"golang.org/x/crypto/ssh"
)

var vboxManage = getVBoxManagePath()

func getSSHKeyPath() string {
	if val := os.Getenv("SSH_KEY_PATH"); val != "" {
		return val
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".vagrant.d", "insecure_private_key")
}

// getVBoxManagePath retrieves the path to the VBoxManage executable.
func getVBoxManagePath() string {
	if val := os.Getenv("VBOX_MANAGE"); val != "" {
		return val
	}
	if runtime.GOOS == "windows" {
		return `C:\Program Files\Oracle\VirtualBox\VBoxManage.exe`
	}
	return "VBoxManage"
}

// RunVBox executes VBoxManage commands, supporting both local and remote execution.
func RunVBox(args ...string) (string, error) {
	fmt.Printf("VBox: %s\n", strings.Join(args, " "))
	
	if config.Global.VBoxHostIP != "" {
		cmd := fmt.Sprintf("%s %s", vboxManage, strings.Join(args, " "))
		
		key, err := os.ReadFile(getSSHKeyPath())
		if err != nil {
			return "", fmt.Errorf("no se pudo leer llave SSH del host: %v", err)
		}
		signer, _ := ssh.ParsePrivateKey(key)
		cfg := &ssh.ClientConfig{
			User: config.Global.VBoxHostUser,
			Auth: []ssh.AuthMethod{ssh.PublicKeys(signer), ssh.Password(config.Global.VBoxHostUser)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout: 10 * time.Second,
		}
		
		client, err := ssh.Dial("tcp", config.Global.VBoxHostIP+":22", cfg)
		if err != nil {
			return "", fmt.Errorf("error conectando al host: %v", err)
		}
		defer client.Close()
		
		session, err := client.NewSession()
		if err != nil {
			return "", err
		}
		defer session.Close()
		
		out, err := session.CombinedOutput(cmd)
		if err != nil {
			fmt.Printf("Error: %v | %s\n", err, string(out))
		}
		return string(out), err
	}

	out, err := exec.Command(vboxManage, args...).CombinedOutput()
	if err != nil {
		fmt.Printf("Error: %v | %s\n", err, string(out))
	}
	return string(out), err
}

// RunVBoxQuiet executes VBoxManage commands without logging output to stdout.
func RunVBoxQuiet(args ...string) (string, error) {
	if config.Global.VBoxHostIP != "" {
		cmd := fmt.Sprintf("%s %s", vboxManage, strings.Join(args, " "))
		
		key, err := os.ReadFile(getSSHKeyPath())
		if err != nil { return "", err }
		signer, _ := ssh.ParsePrivateKey(key)
		cfg := &ssh.ClientConfig{
			User: config.Global.VBoxHostUser,
			Auth: []ssh.AuthMethod{ssh.PublicKeys(signer), ssh.Password(config.Global.VBoxHostUser)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout: 10 * time.Second,
		}
		
		client, err := ssh.Dial("tcp", config.Global.VBoxHostIP+":22", cfg)
		if err != nil { return "", err }
		defer client.Close()
		
		session, err := client.NewSession()
		if err != nil { return "", err }
		defer session.Close()
		
		out, err := session.CombinedOutput(cmd)
		return string(out), err
	}

	out, err := exec.Command(vboxManage, args...).CombinedOutput()
	return string(out), err
}

// GetVMIP retrieves the IPv4 address of a running VM via VirtualBox Guest Additions.
func GetVMIP(vmName string) (string, error) {
	fmt.Printf("Esperando IP de '%s' via Guest Additions...\n", vmName)
	for i := 0; i < 18; i++ {
		out, err := RunVBoxQuiet("guestproperty", "get", vmName, "/VirtualBox/GuestInfo/Net/0/V4/IP")
		if err == nil && strings.Contains(out, "Value:") {
			parts := strings.Split(out, "Value:")
			if len(parts) > 1 {
				ip := strings.TrimSpace(parts[1])
				if ip != "" {
					fmt.Printf("IP detectada: %s\n", ip)
					return ip, nil
				}
			}
		}
		fmt.Printf("  Intento %d/18 — esperando 5s...\n", i+1)
		time.Sleep(5 * time.Second)
	}
	return "", fmt.Errorf("tiempo agotado esperando IP de '%s'", vmName)
}

// ListNetworkAdapters retrieves all available bridged network adapters on the system.
func ListNetworkAdapters() ([]string, error) {
	out, err := RunVBoxQuiet("list", "bridgedifs")
	if err != nil {
		return nil, err
	}
	var adapters []string
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "Name:") {
			name := strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			if name != "" {
				adapters = append(adapters, name)
			}
		}
	}
	return adapters, nil
}

// FetchVMDetails retrieves detailed information about a specific VM.
func FetchVMDetails(name, uuid string) map[string]string {
	stateOut, _ := RunVBoxQuiet("showvminfo", name, "--machinereadable")
	state := "desconocido"
	for _, l := range strings.Split(strings.ReplaceAll(stateOut, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(l, "VMState=") {
			state = strings.TrimSpace(strings.Trim(strings.TrimPrefix(l, "VMState="), "\""))
			switch state {
			case "running":
				state = "corriendo"
			case "poweroff":
				state = "detenida"
			case "aborted":
				state = "error (abortada)"
			case "saved":
				state = "guardada"
			case "paused":
				state = "pausada"
			}
			break
		}
	}

	ip := ""
	if state == "corriendo" {
		ipOut, _ := RunVBoxQuiet("guestproperty", "get", name, "/VirtualBox/GuestInfo/Net/0/V4/IP")
		if strings.Contains(ipOut, "Value:") {
			ip = strings.TrimSpace(strings.Split(ipOut, "Value:")[1])
		}
	}

	portOut, _ := RunVBoxQuiet("guestproperty", "get", name, "/Gestor/Port")
	port := ""
	if strings.Contains(portOut, "Value:") {
		port = strings.TrimSpace(strings.Split(portOut, "Value:")[1])
	}

	return map[string]string{
		"name":  name,
		"uuid":  uuid,
		"state": state,
		"ip":    ip,
		"port":  port,
	}
}
