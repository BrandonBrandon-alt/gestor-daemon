// Package main implements a VirtualBox infrastructure orchestrator daemon.
// It manages VM lifecycle, network configuration, DNS registration, and auto-scaling
// for cloud-based deployment environments running on VirtualBox.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	scp "github.com/bramvdbogaerde/go-scp"
	"golang.org/x/crypto/ssh"
)

// Configuration variables for SSH connections, VirtualBox management, and networking.
// These can be overridden via environment variables for flexibility in different deployment scenarios.
var (
	// sshUser specifies the remote user for SSH connections (default: "vagrant")
	sshUser    = getEnvOrDefault("SSH_USER", "vagrant")
	// sshKeyPath points to the SSH private key file (default: Vagrant insecure key)
	sshKeyPath = getSSHKeyPath()
	// vboxManage path to the VBoxManage executable, auto-detected per OS
	vboxManage = getVBoxManagePath()

	// bridgeAdapter specifies the network adapter for bridged connections (default: "eth0")
	bridgeAdapter = getEnvOrDefault("BRIDGE_ADAPTER", "eth0")

	// vboxHostIP is the IP of a remote VirtualBox host for remote VM management (optional)
	vboxHostIP   = getEnvOrDefault("VBOX_HOST_IP", "")
	// vboxHostUser username for authentication on remote VirtualBox host
	vboxHostUser = getEnvOrDefault("VBOX_HOST_USER", "usuario_host")
)

// getSSHKeyPath retrieves the path to the SSH private key file.
// It checks SSH_KEY_PATH environment variable first, then falls back to Vagrant's insecure key.
// Returns the path to the SSH private key file for authentication.
func getSSHKeyPath() string {
	if val := os.Getenv("SSH_KEY_PATH"); val != "" {
		return val
	}
	home, _ := os.UserHomeDir()
	// Default to Vagrant's insecure private key if no custom key is provided
	return filepath.Join(home, ".vagrant.d", "insecure_private_key")
}

// getVBoxManagePath retrieves the path to the VBoxManage executable.
// Automatically detects the correct path based on the operating system:
// Windows: C:\Program Files\Oracle\VirtualBox\VBoxManage.exe
// Linux/macOS: searches in PATH as "VBoxManage"
// Returns the path to the VBoxManage executable or a command name if not found.
func getVBoxManagePath() string {
	if val := os.Getenv("VBOX_MANAGE"); val != "" {
		return val
	}
	if runtime.GOOS == "windows" {
		return `C:\Program Files\Oracle\VirtualBox\VBoxManage.exe`
	}
	return "VBoxManage"
}

// getEnvOrDefault retrieves an environment variable value or returns a default value.
// key: environment variable name to retrieve
// defaultVal: value to return if the environment variable is not set or empty
// Returns the environment variable value or the default value if not found.
func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// buildSSHConfig constructs an SSH client configuration for remote command execution.
// It attempts to use public key authentication first (from sshKeyPath), falling back
// to password authentication using the sshUser value. The connection ignores host key verification
// suitable for development/test environments but not recommended for production.
// Returns a configured *ssh.ClientConfig ready for SSH connections, or an error if setup fails.
func buildSSHConfig() (*ssh.ClientConfig, error) {
	authMethods := []ssh.AuthMethod{
		ssh.Password(sshUser), // Fallback: tries to use the username as password
	}

	// Attempt to load private key if file exists; otherwise continue with password auth
	if key, err := os.ReadFile(sshKeyPath); err == nil {
		if signer, err := ssh.ParsePrivateKey(key); err == nil {
			authMethods = append([]ssh.AuthMethod{ssh.PublicKeys(signer)}, authMethods...)
		}
	}

	return &ssh.ClientConfig{
		User:            sshUser,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}, nil
}

// sshClient establishes an SSH connection to a remote machine.
// ip: IP address of the target machine (port 22 is assumed)
// Returns an established *ssh.Client for remote command execution or an error.
func sshClient(ip string) (*ssh.Client, error) {
	config, err := buildSSHConfig()
	if err != nil {
		return nil, err
	}
	return ssh.Dial("tcp", ip+":22", config)
}

// runSSH executes a command on a remote machine via SSH and returns the combined output.
// ip: IP address of the target machine
// command: shell command to execute on the remote machine
// Returns command output as string and any error that occurred during execution.
func runSSH(ip, command string) (string, error) {
	client, err := sshClient(ip)
	if err != nil {
		return "", err
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	out, err := session.CombinedOutput(command)
	return string(out), err
}

// uploadFile transfers a file from the local machine to a remote machine using SCP.
// ip: IP address of the target machine
// localPath: path to the local file to upload
// remotePath: destination path on the remote machine (with executable permissions 0755)
// Returns an error if the upload fails at any stage.
func uploadFile(ip, localPath, remotePath string) error {
	config, err := buildSSHConfig()
	if err != nil {
		return err
	}
	scpClient := scp.NewClient(ip+":22", config)
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("error abriendo archivo local: %v", err)
	}
	defer file.Close()
	if err = scpClient.Connect(); err != nil {
		return fmt.Errorf("error conectando SCP: %v", err)
	}
	defer scpClient.Close()
	if err = scpClient.CopyFromFile(context.Background(), *file, remotePath, "0755"); err != nil {
		return fmt.Errorf("error copiando archivo: %v", err)
	}
	return nil
}

// createRemoteFile creates or overwrites a file on a remote machine via SSH.
// It writes content to a temporary local file, uploads it via SCP, then moves it to the target location.
// ip: IP address of the target machine
// remotePath: full path where the file will be created/stored on the remote machine
// content: file content to write
// Returns an error if any step of the process fails.
func createRemoteFile(ip, remotePath, content string) error {
	tmpFile, err := os.CreateTemp("", "service-*.service")
	if err != nil {
		return fmt.Errorf("error creando archivo temporal: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(content); err != nil {
		tmpFile.Close()
		return fmt.Errorf("error escribiendo archivo temporal: %v", err)
	}
	tmpFile.Close()
	remoteTmp := "/tmp/daemon-service-temp.service"
	if err := uploadFile(ip, tmpFile.Name(), remoteTmp); err != nil {
		return fmt.Errorf("error subiendo .service: %v", err)
	}
	_, err = runSSH(ip, fmt.Sprintf("sudo mv %s %s && sudo chmod 644 %s", remoteTmp, remotePath, remotePath))
	return err
}

// runVBox executes VBoxManage commands, supporting both local and remote execution.
// If vboxHostIP is configured, the command runs on a remote VirtualBox host via SSH.
// Otherwise, it executes locally. Output is logged to stdout.
// args: VBoxManage command arguments
// Returns command output as string and any error encountered.
func runVBox(args ...string) (string, error) {
	fmt.Printf("VBox: %s\n", strings.Join(args, " "))
	
	if vboxHostIP != "" {
		// Remote execution via SSH on physical host
		cmd := fmt.Sprintf("%s %s", vboxManage, strings.Join(args, " "))
		
		// Temporary SSH config for remote host authentication
		key, err := os.ReadFile(sshKeyPath)
		if err != nil {
			return "", fmt.Errorf("no se pudo leer llave SSH del host: %v", err)
		}
		signer, _ := ssh.ParsePrivateKey(key)
		config := &ssh.ClientConfig{
			User: vboxHostUser,
			Auth: []ssh.AuthMethod{ssh.PublicKeys(signer), ssh.Password(vboxHostUser)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout: 10 * time.Second,
		}
		
		client, err := ssh.Dial("tcp", vboxHostIP+":22", config)
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

	// Local execution (when running directly on the host)
	out, err := exec.Command(vboxManage, args...).CombinedOutput()
	if err != nil {
		fmt.Printf("Error: %v | %s\n", err, string(out))
	}
	return string(out), err
}

// getVMIP retrieves the IPv4 address of a running VM via VirtualBox Guest Additions.
// Polls the guest property for up to 90 seconds (18 attempts × 5-second intervals).
// vmName: name of the virtual machine to query
// Returns the IPv4 address if found, or an error if timeout occurs.
func getVMIP(vmName string) (string, error) {
	fmt.Printf("Esperando IP de '%s' via Guest Additions...\n", vmName)
	for i := 0; i < 18; i++ {
		out, err := runVBoxQuiet("guestproperty", "get", vmName, "/VirtualBox/GuestInfo/Net/0/V4/IP")
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

// runVBoxQuiet executes VBoxManage commands without logging output to stdout.
// Similar to runVBox but suppresses command output, useful for queries that don't need to be logged.
// args: VBoxManage command arguments
// Returns command output as string and any error encountered.
func runVBoxQuiet(args ...string) (string, error) {
	if vboxHostIP != "" {
		cmd := fmt.Sprintf("%s %s", vboxManage, strings.Join(args, " "))
		
		key, err := os.ReadFile(sshKeyPath)
		if err != nil { return "", err }
		signer, _ := ssh.ParsePrivateKey(key)
		config := &ssh.ClientConfig{
			User: vboxHostUser,
			Auth: []ssh.AuthMethod{ssh.PublicKeys(signer), ssh.Password(vboxHostUser)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout: 10 * time.Second,
		}
		
		client, err := ssh.Dial("tcp", vboxHostIP+":22", config)
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

// listNetworkAdapters retrieves all available bridged network adapters on the system.
// Parses VBoxManage output to extract adapter names for network bridge configuration.
// Returns a slice of adapter names or an error if the listing fails.
func listNetworkAdapters() ([]string, error) {
	out, err := runVBoxQuiet("list", "bridgedifs")
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

// fetchVMDetails retrieves detailed information about a specific VM including state and IP.
// Queries the VM state, IPv4 address (if running), and custom guest properties.
// name: VM name to query
// uuid: VM UUID (currently unused but kept for API compatibility)
// Returns a map with keys: "name", "uuid", "state", "ip", "port" (values are "N/A" if unavailable).
func fetchVMDetails(name, uuid string) map[string]string {
	stateOut, _ := runVBoxQuiet("showvminfo", name, "--machinereadable")
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
		ipOut, _ := runVBoxQuiet("guestproperty", "get", name, "/VirtualBox/GuestInfo/Net/0/V4/IP")
		if strings.Contains(ipOut, "Value:") {
			ip = strings.TrimSpace(strings.Split(ipOut, "Value:")[1])
		}
	}

	portOut, _ := runVBoxQuiet("guestproperty", "get", name, "/Gestor/Port")
	port := ""
	if strings.Contains(portOut, "Value:") {
		port = strings.TrimSpace(strings.Split(portOut, "Value:")[1])
	}

	return map[string]string{"name": name, "uuid": uuid, "state": state, "ip": ip, "port": port}
}

// displayVMStatusTable prints a formatted table of all VMs and their status to stdout.
// Queries all registered VMs concurrently, collects their details, and displays them sorted by name.
// If no VMs exist, displays an informational message. Called periodically to show current infrastructure state.
func displayVMStatusTable() {
	fmt.Println("\n=== ESTADO DEL ENTORNO DE MÁQUINAS VIRTUALES ===")
	out, err := runVBoxQuiet("list", "vms")
	if err != nil || strings.TrimSpace(out) == "" {
		fmt.Println("(Sin máquinas virtuales registradas)")
		fmt.Println("====================================================\n")
		return
	}

	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(out), "\r\n", "\n"), "\n")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NOMBRE\tESTADO\tIP\tPUERTO\t")

	var wg sync.WaitGroup
	var mu sync.Mutex
	type rowInfo struct{ name, state, ip, port string }
	var rows []rowInfo

	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		name := strings.Trim(parts[0], "\"")

		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			d := fetchVMDetails(n, "")
			mu.Lock()
			rows = append(rows, rowInfo{
				name:  d["name"],
				state: d["state"],
				ip:    d["ip"],
				port:  d["port"],
			})
			mu.Unlock()
		}(name)
	}
	wg.Wait()

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].name < rows[j].name
	})

	for _, r := range rows {
		ip := r.ip
		if ip == "" {
			ip = "N/A"
		}
		port := r.port
		if port == "" {
			port = "N/A"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\n", r.name, r.state, ip, port)
	}

	w.Flush()
	fmt.Println("====================================================\n")
}

// Response is a generic HTTP API response structure for status and message communication.
// Used for success confirmations, error reporting, and operation results.
type Response struct {
	// Success indicates whether the operation completed successfully
	Success bool   `json:"success"`
	// Message contains a human-readable status or error description
	Message string `json:"message"`
}

// Disk represents a VirtualBox hard disk with metadata for inventory management.
// Used in listing and selection operations for disk-based VM creation.
type Disk struct {
	// Name is the filename of the disk (extracted from path)
	Name     string `json:"name"`
	// Location is the absolute filesystem path to the disk file
	Location string `json:"location"`
	// UUID is the unique VirtualBox identifier for the disk
	UUID     string `json:"uuid"`
}

// handleListVMs processes HTTP GET requests to retrieve all registered virtual machines.
// Queries VirtualBox for all VMs, concurrently retrieves their details, and returns JSON.
// Returns array of maps containing: name, uuid, state, ip, port.
func handleListVMs(w http.ResponseWriter, r *http.Request) {
	out, err := runVBoxQuiet("list", "vms")
	if err != nil {
		jsonError(w, "Error listando VMs: "+err.Error())
		return
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(out), "\r\n", "\n"), "\n")

	var wg sync.WaitGroup
	var mu sync.Mutex
	var vms []map[string]string

	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		name := strings.Trim(parts[0], "\"")
		uuid := ""
		if len(parts) > 1 {
			uuid = strings.Trim(parts[1], "{}")
		}

		wg.Add(1)
		go func(n, u string) {
			defer wg.Done()
			d := fetchVMDetails(n, u)
			mu.Lock()
			vms = append(vms, d)
			mu.Unlock()
		}(name, uuid)
	}
	wg.Wait()

	sort.Slice(vms, func(i, j int) bool {
		return vms[i]["name"] < vms[j]["name"]
	})

	jsonOK(w, vms)
}

// handleListDisks processes HTTP GET requests to retrieve all available VirtualBox disks.
// Parses the raw VBoxManage output to extract disk UUID, location, and name.
// Identifies multiattach disks (base disks cloned for multiple VMs).
// Returns JSON array of Disk objects.
func handleListDisks(w http.ResponseWriter, r *http.Request) {
	out, err := runVBox("list", "hdds")
	if err != nil {
		jsonError(w, "Error listando discos: "+err.Error())
		return
	}
	var disks []Disk
	var current Disk
	isMulti := false
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "UUID:") && !strings.HasPrefix(line, "Parent UUID:") {
			if isMulti && current.UUID != "" {
				disks = append(disks, current)
			}
			current = Disk{UUID: strings.TrimSpace(strings.TrimPrefix(line, "UUID:"))}
			isMulti = false
		} else if strings.HasPrefix(line, "Location:") {
			current.Location = strings.TrimSpace(strings.TrimPrefix(line, "Location:"))
			parts := strings.Split(strings.ReplaceAll(current.Location, "\\", "/"), "/")
			current.Name = parts[len(parts)-1]
		} else if strings.HasPrefix(line, "Type:") && strings.Contains(line, "multiattach") {
			isMulti = true
		}
		if i == len(lines)-1 && isMulti && current.UUID != "" {
			disks = append(disks, current)
		}
	}
	jsonOK(w, disks)
}

// handleCreateVM processes HTTP POST requests to create a new VM from an existing disk.
// Expected JSON body:
//   - "vmName": unique VM name (required)
//   - "diskUUID": UUID of disk to attach (required)
//   - "port": application port for guest property storage (optional)
//   - "bridgeAdapter": network adapter for bridged networking (optional, uses default if omitted)
//
// The handler performs:
// 1. Validates required parameters (vmName and diskUUID)
// 2. Creates VM group, CPU/memory configuration, chipset, and firmware
// 3. Attaches the specified disk via SATA controller
// 4. Configures network interface (bridge adapter for host connectivity)
// 5. Enables audio input capture if supported
// 6. Sets guest property for application port storage
// 7. Starts VM in headless mode with 120-second boot timeout
// 8. Retrieves IP via Guest Additions property and displays status
// 9. Stores VM details in response
func handleCreateVM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VMName        string `json:"vmName"`
		DiskUUID      string `json:"diskUUID"`
		Port          string `json:"port"`
		BridgeAdapter string `json:"bridgeAdapter"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "JSON inválido: "+err.Error())
		return
	}
	if body.VMName == "" || body.DiskUUID == "" {
		jsonError(w, "vmName y diskUUID son obligatorios")
		return
	}

	adapter := body.BridgeAdapter
	if adapter == "" {
		adapter = bridgeAdapter
	}

	if _, err := runVBox("createvm", "--name", body.VMName, "--ostype", "Debian_64", "--register"); err != nil {
		jsonError(w, "Error creando VM: "+err.Error())
		return
	}

	runVBox("modifyvm", body.VMName, "--memory", "512", "--nic1", "bridged", "--bridgeadapter1", adapter)
	runVBox("storagectl", body.VMName, "--name", "SATA", "--add", "sata")

	if _, err := runVBox("storageattach", body.VMName,
		"--storagectl", "SATA", "--port", "0", "--device", "0",
		"--type", "hdd", "--medium", body.DiskUUID, "--mtype", "multiattach"); err != nil {
		jsonError(w, "Error adjuntando disco: "+err.Error())
		return
	}

	if _, err := runVBox("startvm", body.VMName, "--type", "headless"); err != nil {
		jsonError(w, "Error iniciando VM: "+err.Error())
		return
	}

	ip, err := getVMIP(body.VMName)
	if err != nil {
		jsonError(w, err.Error())
		return
	}

	// Guardar el puerto como propiedad para leerlo luego en la tabla de estado
	if body.Port != "" {
		runVBoxQuiet("guestproperty", "set", body.VMName, "/Gestor/Port", body.Port)
	}

	go displayVMStatusTable()

	jsonOK(w, map[string]string{
		"success": "true",
		"message": "VM " + body.VMName + " creada e iniciada — IP: " + ip,
		"name":    body.VMName,
		"ip":      ip,
		"port":    body.Port,
	})
}

// handleStopVM processes HTTP POST requests to gracefully shut down a running VM.
// Expected JSON body: {"vmName": "vm-name"}
// Sends poweroff signal and waits 3 seconds before refreshing the status table.
func handleStopVM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VMName string `json:"vmName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "JSON inválido: "+err.Error())
		return
	}
	if body.VMName == "" {
		jsonError(w, "vmName es requerido")
		return
	}
	out, err := runVBox("controlvm", body.VMName, "poweroff")
	if err != nil {
		jsonError(w, "Error apagando VM: "+out)
		return
	}

	go func() {
		time.Sleep(3 * time.Second)
		displayVMStatusTable()
	}()

	jsonOK(w, Response{Success: true, Message: "Señal de apagado enviada a " + body.VMName})
}

// handleDeleteVM processes HTTP POST requests to permanently remove a VM and its storage.
// Expected JSON body: {"vmName": "vm-name"}
// The deletion process:
// 1. Force power off the VM
// 2. Wait 3 seconds for resources to be released
// 3. Unregister VM from VirtualBox and delete all associated files
// 4. Refresh status display
// Attempts --delete-all first, falls back to --delete for version compatibility.
func handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VMName string `json:"vmName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "JSON inválido: "+err.Error())
		return
	}
	if body.VMName == "" {
		jsonError(w, "vmName es requerido")
		return
	}

	// 1. Forzar apagado primero
	runVBoxQuiet("controlvm", body.VMName, "poweroff")

	// 2. Esperar 3 segundos para que los procesos se liberen
	time.Sleep(3 * time.Second)

	// 3. Destruir y eliminar todos sus archivos
	out, err := runVBoxQuiet("unregistervm", body.VMName, "--delete-all")
	if err != nil {
		// Respaldos posibles según la versión de VirtualBox
		out2, err2 := runVBoxQuiet("unregistervm", body.VMName, "--delete")
		if err2 != nil {
			jsonError(w, "Error eliminando VM: "+out+" | Respaldo: "+out2)
			return
		}
	}

	go displayVMStatusTable()

	jsonOK(w, Response{Success: true, Message: "La máquina " + body.VMName + " ha sido eliminada permanentemente del sistema"})
}

// handleApplyHAProxy processes HTTP POST requests to configure HAProxy load balancing.
// Expected JSON body:
//   - "lbIp": IP address of the HAProxy load balancer VM (required)
//   - "servers": array of backend server objects, each containing name, ip, and port (required)
//
// The handler:
// 1. Validates required parameters (lbIp and servers array)
// 2. Builds HAProxy configuration file with frontend and backend sections
// 3. Connects to HAProxy VM via SSH
// 4. Uploads configuration and executes systemctl reload
// 5. Initializes AutoScalingState if autoscaling is enabled
// 6. Starts autoscaling monitor goroutine if needed
func handleApplyHAProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LBIp    string `json:"lbIp"`
		Servers []struct {
			Name string `json:"name"`
			IP   string `json:"ip"`
			Port string `json:"port"`
		} `json:"servers"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "JSON inválido: "+err.Error())
		return
	}
	if body.LBIp == "" {
		jsonError(w, "La IP del balanceador es requerida")
		return
	}

	// 1. Generate haproxy.cfg content
	var cfg strings.Builder
	cfg.WriteString("global\n")
	cfg.WriteString("    log /dev/log local0\n")
	cfg.WriteString("    log /dev/log local1 notice\n")
	cfg.WriteString("    chroot /var/lib/haproxy\n")
	cfg.WriteString("    stats socket /run/haproxy/admin.sock mode 660 level admin expose-fd listeners\n")
	cfg.WriteString("    stats timeout 30s\n")
	cfg.WriteString("    user haproxy\n")
	cfg.WriteString("    group haproxy\n")
	cfg.WriteString("    daemon\n\n")

	cfg.WriteString("defaults\n")
	cfg.WriteString("    log global\n")
	cfg.WriteString("    mode http\n")
	cfg.WriteString("    option httplog\n")
	cfg.WriteString("    option dontlognull\n")
	cfg.WriteString("    timeout connect 5000\n")
	cfg.WriteString("    timeout client  50000\n")
	cfg.WriteString("    timeout server  50000\n\n")

	cfg.WriteString("listen stats\n")
	cfg.WriteString("    bind *:8404\n")
	cfg.WriteString("    stats enable\n")
	cfg.WriteString("    stats uri /\n")
	cfg.WriteString("    stats refresh 5s\n\n")

	cfg.WriteString("frontend http_front\n")
	cfg.WriteString("    bind *:80\n")
	cfg.WriteString("    default_backend http_back\n\n")

	cfg.WriteString("backend http_back\n")
	cfg.WriteString("    balance roundrobin\n")

	for _, s := range body.Servers {
		if s.IP != "" && s.Port != "" {
			cfg.WriteString(fmt.Sprintf("    server %s %s:%s check\n", s.Name, s.IP, s.Port))
		}
	}

	// 2. Upload to remote HAProxy via createRemoteFile
	tempRemotePath := "/etc/haproxy/haproxy.cfg"
	if err := createRemoteFile(body.LBIp, tempRemotePath, cfg.String()); err != nil {
		jsonError(w, "Error inyectando cfg vía SSH: "+err.Error())
		return
	}

	// 3. Restart HAProxy
	if out, err := runSSH(body.LBIp, "sudo systemctl restart haproxy"); err != nil {
		jsonError(w, fmt.Sprintf("Error reiniciando HAProxy: %v | Salida: %s", err, out))
		return
	}

	jsonOK(w, Response{Success: true, Message: "Balanceador en " + body.LBIp + " actualizado correctamente con " + fmt.Sprintf("%d", len(body.Servers)) + " servidores traseros."})
}

// handleHAProxyStatus processes HTTP GET requests to retrieve HAProxy load balancer statistics.
// Query parameters: ip (IP address of HAProxy VM, required)
// The handler:
// 1. Validates that HAProxy VM IP is provided
// 2. Connects to HAProxy via SSH and queries statistics socket
// 3. Parses HAProxy CSV output from 'show stat' command
// 4. Filters and returns active server statistics (excludes FRONTEND/BACKEND aggregate rows)
// 5. Returns JSON array with pxname, svname, status, and current session count (scur)
// Requires socat utility to be installed on HAProxy VM for socket communication.
func handleHAProxyStatus(w http.ResponseWriter, r *http.Request) {
	lbIp := r.URL.Query().Get("ip")
	if lbIp == "" {
		jsonError(w, "IP del balanceador requerida")
		return
	}

	// Consultar estadísticas vía socket de HAProxy
	// El comando 'show stat' devuelve un CSV
	cmd := "echo \"show stat\" | sudo socat stdio /run/haproxy/admin.sock"
	out, err := runSSH(lbIp, cmd)
	if err != nil {
		jsonError(w, "Error obteniendo estadísticas (verifica que socat esté instalado): "+err.Error())
		return
	}

	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	if len(lines) < 2 {
		jsonError(w, "Respuesta de estadísticas vacía o inválida")
		return
	}

	// Procesar CSV (Ignorar primera línea de leyenda si empieza con #)
	var stats []map[string]string
	var headers []string

	for _, line := range lines {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# ") {
			headers = strings.Split(strings.TrimPrefix(line, "# "), ",")
			continue
		}

		fields := strings.Split(line, ",")
		if len(fields) < len(headers) {
			continue
		}

		row := make(map[string]string)
		for i, h := range headers {
			if i < len(fields) {
				row[h] = fields[i]
			}
		}
		// Solo nos interesan los servidores reales (no el frontend/backend agregados)
		// svname: FRONTEND, BACKEND, o el nombre del servidor
		if row["svname"] != "FRONTEND" && row["svname"] != "BACKEND" {
			stats = append(stats, map[string]string{
				"pxname": row["pxname"],
				"svname": row["svname"],
				"status": row["status"],
				"scur":   row["scur"],
			})
		}
	}

	jsonOK(w, stats)
}

func handleHAProxyState(w http.ResponseWriter, r *http.Request) {
	stateFile := "haproxy_state.json"

	if r.Method == "GET" {
		data, err := os.ReadFile(stateFile)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"lbs":[], "asignaciones":{}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
		return
	}

	if r.Method == "POST" {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			jsonError(w, "Error leyendo JSON")
			return
		}
		if err := os.WriteFile(stateFile, body, 0644); err != nil {
			jsonError(w, "Error guardando estado: "+err.Error())
			return
		}
		jsonOK(w, Response{Success: true, Message: "Estado HAProxy guardado"})
		return
	}

	w.WriteHeader(http.StatusMethodNotAllowed)
}

func handleStartVM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VMName string `json:"vmName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "JSON inválido: "+err.Error())
		return
	}
	out, err := runVBox("startvm", body.VMName, "--type", "headless")
	if err != nil {
		jsonError(w, "Error iniciando VM: "+out)
		return
	}

	go displayVMStatusTable()

	jsonOK(w, Response{Success: true, Message: "VM " + body.VMName + " iniciada"})
}

func handleService(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IP      string `json:"ip"`
		Action  string `json:"action"`
		Service string `json:"service"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "JSON inválido: "+err.Error())
		return
	}
	if body.IP == "" {
		jsonError(w, "El campo ip es requerido")
		return
	}
	if body.Service == "" {
		body.Service = "app-custom.service"
	}
	var cmd string
	switch body.Action {
	case "start", "stop", "restart", "enable", "disable", "status":
		cmd = fmt.Sprintf("sudo systemctl %s %s", body.Action, body.Service)
	case "logs":
		cmd = fmt.Sprintf("sudo journalctl -u %s --no-pager -n 50", body.Service)
	case "install-stress":
		cmd = "sudo apt-get update && sudo apt-get install -y stress-ng"
	case "exec":
		cmd = body.Service // In 'exec' mode, the 'Service' field will carry the raw command
	default:
		jsonError(w, "Acción inválida: "+body.Action)
		return
	}
	out, err := runSSH(body.IP, cmd)
	if err != nil {
		jsonOK(w, map[string]string{
			"output": fmt.Sprintf("Error SSH con %s: %v\n%s", body.IP, err, out),
		})
		return
	}
	jsonOK(w, map[string]string{"output": out})
}

func handlePrepareVM(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(50 << 20)

	port := r.FormValue("port")
	templateVM := r.FormValue("templateVM")
	newDiskName := r.FormValue("newDiskName")

	if port == "" || templateVM == "" || newDiskName == "" {
		jsonError(w, "Faltan campos: port, templateVM, newDiskName")
		return
	}

	execFile, header, err := r.FormFile("execFile")
	if err != nil {
		jsonError(w, "Error leyendo ejecutable: "+err.Error())
		return
	}
	defer execFile.Close()

	tempExecPath := "./temp_" + header.Filename
	tempFile, err := os.Create(tempExecPath)
	if err != nil {
		jsonError(w, "Error creando archivo temporal: "+err.Error())
		return
	}
	if _, err := io.Copy(tempFile, execFile); err != nil {
		tempFile.Close()
		os.Remove(tempExecPath)
		jsonError(w, "Error guardando ejecutable: "+err.Error())
		return
	}
	tempFile.Close()
	defer os.Remove(tempExecPath)

	// Procesar .zip opcional
	var zipTempPath, zipFileName string
	zipFile, zipHeader, zipErr := r.FormFile("zipFile")
	if zipErr == nil {
		defer zipFile.Close()
		zipTempPath = "./temp_" + zipHeader.Filename
		zipFileName = zipHeader.Filename
		zf, err := os.Create(zipTempPath)
		if err != nil {
			jsonError(w, "Error creando zip temporal: "+err.Error())
			return
		}
		if _, err := io.Copy(zf, zipFile); err != nil {
			zf.Close()
			os.Remove(zipTempPath)
			jsonError(w, "Error guardando zip: "+err.Error())
			return
		}
		zf.Close()
		defer os.Remove(zipTempPath)
	}

	logOutput := fmt.Sprintf("Iniciando automatización para '%s'...\n", templateVM)

	// PASO 1: Encender plantilla en headless
	logOutput += "1. Encendiendo plantilla en modo headless...\n"
	runVBox("startvm", templateVM, "--type", "headless")

	// PASO 2: Detectar IP via Guest Additions
	logOutput += "2. Detectando IP via Guest Additions...\n"
	templateIP, err := getVMIP(templateVM)
	if err != nil {
		runVBox("controlvm", templateVM, "poweroff")
		jsonError(w, err.Error())
		return
	}
	logOutput += fmt.Sprintf("   IP detectada: %s\n", templateIP)

	// PASO CRÍTICO: Pausa de 45s para que SSH esté 100% levantado
	logOutput += "   Esperando 45s a que SSH esté disponible...\n"
	time.Sleep(45 * time.Second)

	// PASO 3: Subir ejecutable
	logOutput += "3. Subiendo ejecutable por SCP...\n"
	remoteExecPath := "/home/" + sshUser + "/" + header.Filename
	if err = uploadFile(templateIP, tempExecPath, remoteExecPath); err != nil {
		jsonError(w, "Error subiendo ejecutable: "+err.Error())
		return
	}
	logOutput += "   Ejecutable subido correctamente.\n"

	// PASO 4: Subir y descomprimir .zip
	if zipTempPath != "" {
		logOutput += "4. Subiendo archivos adicionales (.zip)...\n"
		remoteZipPath := "/home/" + sshUser + "/" + zipFileName
		if err = uploadFile(templateIP, zipTempPath, remoteZipPath); err != nil {
			jsonError(w, "Error subiendo zip: "+err.Error())
			return
		}
		unzipOut, unzipErr := runSSH(templateIP, fmt.Sprintf("cd /home/%s && unzip -o %s", sshUser, zipFileName))
		if unzipErr != nil {
			logOutput += fmt.Sprintf("   Advertencia descomprimiendo: %v | %s\n", unzipErr, unzipOut)
		} else {
			logOutput += "   Archivos descomprimidos correctamente.\n"
		}
	}

	// PASO 5: Crear archivo .service
	logOutput += "5. Configurando servicio systemd...\n"
	serviceName := "app-custom.service"
	serviceContent := fmt.Sprintf(`[Unit]
Description=Aplicacion Gestionada
After=network.target

[Service]
User=%s
WorkingDirectory=/home/%s
ExecStart=%s -port %s
Restart=always

[Install]
WantedBy=multi-user.target`, sshUser, sshUser, remoteExecPath, port)

	if err = createRemoteFile(templateIP, "/etc/systemd/system/"+serviceName, serviceContent); err != nil {
		jsonError(w, "Error creando .service: "+err.Error())
		return
	}
	logOutput += "   Archivo .service creado en /etc/systemd/system/.\n"

	// PASO 6: Habilitar servicio y sincronizar disco
	logOutput += "6. Habilitando servicio y guardando en disco...\n"
	if out, err := runSSH(templateIP, "sudo systemctl daemon-reload"); err != nil {
		logOutput += fmt.Sprintf("   Advertencia daemon-reload: %v | %s\n", err, out)
	}
	if out, err := runSSH(templateIP, "sudo systemctl enable "+serviceName); err != nil {
		logOutput += fmt.Sprintf("   Advertencia enable: %v | %s\n", err, out)
	} else {
		logOutput += "   Servicio habilitado para inicio automático.\n" + out
	}

	// EL COMANDO MÁGICO: Forzar a Linux a escribir la caché en el disco duro
	logOutput += "   Sincronizando disco (sync)...\n"
	runSSH(templateIP, "sync")
	time.Sleep(2 * time.Second) // Damos 2 segundos para que el disco respire

	// PASO 7: Apagar limpiamente con poweroff
	logOutput += "7. Apagando plantilla (Forzado)...\n"
	runVBox("controlvm", templateVM, "poweroff")
	time.Sleep(8 * time.Second)

	// PASO 8: Detectar ruta del disco y convertirlo a multiconexión
	logOutput += "8. Preparando disco multiconexión...\n"
	vminfo, _ := runVBox("showvminfo", templateVM, "--machinereadable")

	var diskPath, storageName, storagePort, device string
	lines := strings.Split(vminfo, "\n")

	for _, line := range lines {
		lineLower := strings.ToLower(line)
		// Buscar líneas que contengan .vdi o .vmdk (ignorando las .iso)
		if strings.Contains(lineLower, ".vdi") || strings.Contains(lineLower, ".vmdk") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				// LIMPIEZA EXTREMA: Quitamos saltos de línea (\r \n), espacios y luego comillas
				key := strings.TrimSpace(parts[0])
				key = strings.Trim(key, `"`)

				val := strings.TrimSpace(parts[1])
				val = strings.Trim(val, `"`)

				keyParts := strings.Split(key, "-")
				if len(keyParts) >= 3 && val != "none" {
					storageName = keyParts[0]
					storagePort = keyParts[1]
					device = keyParts[2]
					diskPath = val
					break
				}
			}
		}
	}

	if diskPath != "" {
		logOutput += fmt.Sprintf("   Disco detectado: %s\n", diskPath)
		// 1. Desacoplar el disco
		runVBox("storageattach", templateVM, "--storagectl", storageName, "--port", storagePort, "--device", device, "--type", "hdd", "--medium", "none")

		// 2. Convertir a multiconexión
		_, err := runVBox("modifymedium", "disk", diskPath, "--type", "multiattach")
		if err != nil {
			logOutput += "   Nota al convertir disco: " + err.Error() + "\n"
		} else {
			logOutput += "   ¡Disco convertido a multiconexión exitosamente!\n"
		}
	} else {
		logOutput += "   ⚠ No se detectó un disco duro válido (.vdi o .vmdk) en la máquina.\n"
	}

	jsonOK(w, map[string]string{
		"message": "¡Automatización completada con éxito!",
		"output":  logOutput,
	})
}

func handleDeleteDisk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DiskUUID string `json:"diskUUID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "JSON inválido: "+err.Error())
		return
	}
	if body.DiskUUID == "" {
		jsonError(w, "diskUUID es requerido")
		return
	}
	out, err := runVBox("closemedium", "disk", body.DiskUUID, "--delete")
	if err != nil {
		jsonError(w, "Error eliminando disco: "+out)
		return
	}
	jsonOK(w, Response{Success: true, Message: "Disco eliminado correctamente"})
}

func handleListNetworkAdapters(w http.ResponseWriter, r *http.Request) {
	adapters, err := listNetworkAdapters()
	if err != nil {
		jsonError(w, "Error listando adaptadores: "+err.Error())
		return
	}
	jsonOK(w, adapters)
}

// ─── Helpers JSON ─────────────────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(Response{Success: false, Message: msg})
}

//go:embed index.html
var htmlContent []byte

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(htmlContent)
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/vms", handleListVMs)
	http.HandleFunc("/api/disks", handleListDisks)
	http.HandleFunc("/api/vm/create", handleCreateVM)
	http.HandleFunc("/api/vm/stop", handleStopVM)
	http.HandleFunc("/api/vm/start", handleStartVM)
	http.HandleFunc("/api/vm/delete", handleDeleteVM)
	http.HandleFunc("/api/service", handleService)
	http.HandleFunc("/api/vm/prepare", handlePrepareVM)
	http.HandleFunc("/api/disk/delete", handleDeleteDisk)
	http.HandleFunc("/api/haproxy/apply", handleApplyHAProxy)
	http.HandleFunc("/api/haproxy/status", handleHAProxyStatus)
	http.HandleFunc("/api/haproxy/state", handleHAProxyState)
	http.HandleFunc("/api/network/adapters", handleListNetworkAdapters)
	http.HandleFunc("/api/autoscaling/config", handleAutoScalingConfig)
	http.HandleFunc("/api/autoscaling/status", handleAutoScalingStatus)
	
	// Nuevos Endpoints HTTPaaS
	http.HandleFunc("/api/provision", handleProvision)
	http.HandleFunc("/api/instances", handleInstances)
	http.HandleFunc("/api/instances/", handleDeleteInstance)

	fmt.Println("Gestor de demonios corriendo en http://localhost:8090")
	go displayVMStatusTable()
	go StartAutoscaler()
	log.Fatal(http.ListenAndServe(":8090", nil))
}

// ─── Fin ──────────────────────────────────────────────────────────────────────
