// Package main implements HTTPaaS (HTTP Application as a Service) provisioning.
// This file handles VM cloning, disk management, instance provisioning via multipart uploads,
// and Apache-based web application deployment.
package main

import (
"context"
"encoding/json"
"fmt"
"io"
"net/http"
"os"
"path/filepath"
"strings"
"sync"
"time"

scp "github.com/bramvdbogaerde/go-scp"
"golang.org/x/crypto/ssh"
)

// Instance represents a provisioned web application instance.
// Each instance corresponds to a cloned VM running Apache2 with deployed application files.
type Instance struct {
// Name is the unique identifier for the instance (used in VM naming as "web-{Name}")
Name      string `json:"name"`
// IP is the internal network address assigned to the instance within vboxnet0
IP        string `json:"ip"`
// URL is the fully qualified domain name-based access URL (http://{Name}.cloud.local)
URL       string `json:"url"`
// CreatedAt records the RFC3339 timestamp when the instance was provisioned
CreatedAt string `json:"createdAt"`
}

// Synchronization primitives for instance and provisioning state management.
// Prevents concurrent operations on the same shared resources.
var (
// instancesFile is the persistent storage location for instance metadata
instancesFile = "/home/vagrant/instances.json"
// instancesMu protects concurrent access to instances.json (read/write operations)
instancesMu   sync.Mutex
// provisionMu serializes provisioning operations to prevent IP allocation conflicts
// particularly during the temporary IP stage (.30) when configuring network
provisionMu   sync.Mutex
)

// getInstances loads all provisioned instances from persistent storage.
// Attempts to read from the Vagrant-specific path first, falls back to local path if running outside Vagrant.
// Thread-safe: acquires lock for the entire read operation.
// Returns a slice of Instance structs or empty slice if file doesn't exist.
func getInstances() ([]Instance, error) {
instancesMu.Lock()
defer instancesMu.Unlock()

data, err := os.ReadFile(instancesFile)
if err != nil {
if os.IsNotExist(err) {
// Fallback if not running in Vagrant environment
data, err = os.ReadFile("instances.json")
if err != nil {
return []Instance{}, nil
}
} else {
return nil, err
}
}
var insts []Instance
json.Unmarshal(data, &insts)
return insts, nil
}

// saveInstances persists the instance list to storage as JSON with indentation.
// Attempts primary path first, falls back to local path if primary fails.
// Thread-safe: acquires lock for the entire write operation.
// insts: slice of Instance structs to persist
// Returns an error if both write attempts fail.
func saveInstances(insts []Instance) error {
instancesMu.Lock()
defer instancesMu.Unlock()

data, _ := json.MarshalIndent(insts, "", "  ")
err := os.WriteFile(instancesFile, data, 0644)
if err != nil {
return os.WriteFile("instances.json", data, 0644)
}
return nil
}

// getVBoxDefaultFolder retrieves the default VirtualBox VM storage directory.
// Queries VBoxManage for the system's configured default machine folder.
// Falls back to platform-specific default locations if query fails.
// Returns the absolute path to the VirtualBox VMs directory.
func getVBoxDefaultFolder() string {
out, err := runVBoxQuiet("list", "systemproperties")
if err != nil {
home, _ := os.UserHomeDir()
return filepath.Join(home, "VirtualBox VMs")
}
lines := strings.Split(out, "\n")
for _, line := range lines {
if strings.Contains(line, "Default machine folder:") {
return strings.TrimSpace(strings.Split(line, ":")[1])
}
}
home, _ := os.UserHomeDir()
return filepath.Join(home, "VirtualBox VMs")
}

// getPlantillaDiskUUID retrieves the UUID of the base template disk for cloning.
// First attempts to extract from the template VM's storage configuration.
// Falls back to searching through the global HDD list if initial method fails.
// The template disk must be attached to "plantilla_http_base" VM.
// Returns the disk UUID string or an error if the disk cannot be located.
// getPlantillaDiskUUID retrieves the UUID of the base template disk for cloning.
// It parses showvminfo output to find the first attached storage medium (.vdi or .vmdk).
// Returns the disk UUID string or an error if the disk cannot be located.
func getPlantillaDiskUUID() (string, error) {
	// Primero obtenemos el disco actualmente asociado a la plantilla
	out, _ := runVBoxQuiet("showvminfo", "plantilla_http_base", "--machinereadable")
	var attachedUUID string
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.Contains(line, "-ImageUUID-0-0") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				attachedUUID = strings.Trim(parts[1], "\"\r\n ")
				break
			}
		}
	}

	if attachedUUID == "" {
		return "", fmt.Errorf("no se encontró disco asociado a plantilla_http_base")
	}

	// Rastreamos la jerarquía hacia arriba para encontrar el disco base
	// (esencial porque el modo multiattach utiliza discos de diferenciación)
	currentUUID := attachedUUID
	for {
		infoOut, _ := runVBoxQuiet("showmediuminfo", currentUUID)
		var parentUUID string
		isBase := false
		for _, line := range strings.Split(infoOut, "\n") {
			if strings.HasPrefix(line, "Parent UUID:") {
				parentUUID = strings.TrimSpace(strings.TrimPrefix(line, "Parent UUID:"))
				if parentUUID == "base" {
					isBase = true
				}
			}
		}
		if isBase {
			return currentUUID, nil // Encontramos el disco raíz
		}
		if parentUUID == "" {
			return currentUUID, nil // Fallback
		}
		currentUUID = parentUUID
	}
}

// getHostOnlyAdapter finds the host-only adapter for the 192.168.10.1 subnet.
func getHostOnlyAdapter() string {
	out, err := runVBoxQuiet("list", "hostonlyifs")
	if err != nil {
		return "vboxnet1"
	}
	lines := strings.Split(out, "\n")
	currentName := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "Name:") {
			currentName = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
		} else if strings.HasPrefix(line, "IPAddress:") && strings.Contains(line, "192.168.10.1") {
			return currentName
		}
	}
	return "vboxnet1"
}

// getNextFreeIP allocates the next available IP address from the instance pool.
// Scans the range 192.168.10.100-200 and returns the first unassigned address.
// Reads current instances to determine which IPs are in use.
// Returns a free IP string or an error if all IPs in range are exhausted.
func getNextFreeIP() (string, error) {
insts, _ := getInstances()
usedIPs := make(map[string]bool)
for _, i := range insts {
usedIPs[i.IP] = true
}
for i := 100; i <= 200; i++ {
ip := fmt.Sprintf("192.168.10.%d", i)
if !usedIPs[ip] {
return ip, nil
}
}
return "", fmt.Errorf("no hay IPs libres en el rango 100-200")
}

// handleProvision processes HTTP POST requests for provisioning new instances.
// Expected multipart form parameters:
//   - "nombre": unique instance name (required)
//   - "zip": ZIP file containing application files to deploy (required)
//
// The provisioning flow:
// 1. Validates request and extracts multipart form data
// 2. Allocates next available IP address
// 3. Clones the base disk for the new instance
// 4. Creates VM with NAT + Host-Only networking
// 5. Boots VM and waits for SSH availability
// 6. Configures static IP on eth1 (vboxnet0)
// 7. Registers hostname in BIND9 DNS
// 8. Uploads and deploys application files
// 9. Persists instance metadata to JSON
//
// Concurrent calls are serialized via provisionMu to prevent IP conflicts.
func handleProvision(w http.ResponseWriter, r *http.Request) {
if r.Method != "POST" {
http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
return
}

provisionMu.Lock()
defer provisionMu.Unlock()

// 1. Parse multipart form
r.ParseMultipartForm(50 << 20)
nombre := r.FormValue("nombre")
if nombre == "" {
jsonError(w, "El nombre de la instancia es obligatorio")
return
}

zipFile, zipHeader, err := r.FormFile("zip")
if err != nil {
jsonError(w, "Se requiere un archivo .zip: "+err.Error())
return
}
defer zipFile.Close()

// Save ZIP to temporary location for later transfer
zipTempPath := "./temp_" + zipHeader.Filename
zf, err := os.Create(zipTempPath)
if err != nil {
jsonError(w, "Error creando archivo temporal: "+err.Error())
return
}
io.Copy(zf, zipFile)
zf.Close()
defer os.Remove(zipTempPath)

// === SSE Setup: stream progress to the browser ===
flusher, ok := w.(http.Flusher)
if !ok {
jsonError(w, "Streaming no soportado")
return
}
w.Header().Set("Content-Type", "text/event-stream")
w.Header().Set("Cache-Control", "no-cache")
w.Header().Set("Connection", "keep-alive")

sendProgress := func(pct int, msg string) {
	data := fmt.Sprintf(`{"progress":%d,"message":"%s"}`, pct, msg)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
	fmt.Printf("[Provision %s] %d%% — %s\n", nombre, pct, msg)
}

sendProgress(5, "Validación completada, asignando recursos...")

// 2. Allocate next available IP address
nuevaIP, err := getNextFreeIP()
if err != nil {
sendProgress(-1, "Error: "+err.Error())
return
}
sendProgress(10, fmt.Sprintf("IP asignada: %s", nuevaIP))

// 3. Locate template disk
plantillaDiskUUID, err := getPlantillaDiskUUID()
if err != nil {
sendProgress(-1, "Error localizando disco plantilla: "+err.Error())
return
}
sendProgress(15, "Disco plantilla localizado")

// 4. Preparar carpeta de la VM
vmName := "web-" + nombre
vboxFolder := getVBoxDefaultFolder()
vmFolder := filepath.Join(vboxFolder, vmName)
os.MkdirAll(vmFolder, 0755)

// 5. Calcular puerto SSH único
ipParts := strings.Split(nuevaIP, ".")
ipOffset := 0
fmt.Sscanf(ipParts[3], "%d", &ipOffset)
sshPort := fmt.Sprintf("%d", 2300+ipOffset)

sendProgress(20, "Creando máquina virtual...")

// 6. Crear VM con red dual
_, err = runVBox("createvm", "--name", vmName, "--ostype", "Debian_64", "--register")
if err != nil {
	sendProgress(-1, "Error creando VM: "+err.Error())
	return
}

	runVBox("modifyvm", vmName, "--memory", "512",
		"--ioapic", "on",
		"--nic1", "nat",
		"--natpf1", fmt.Sprintf("ssh,tcp,127.0.0.1,%s,,22", sshPort),
		"--nic2", "hostonly", "--hostonlyadapter2", getHostOnlyAdapter())

runVBox("storagectl", vmName, "--name", "SATA", "--add", "sata")
sendProgress(30, "Adjuntando disco base (multiattach)...")

_, err = runVBox("storageattach", vmName, "--storagectl", "SATA", "--port", "0", "--device", "0",
	"--type", "hdd", "--medium", plantillaDiskUUID, "--mtype", "multiattach")
if err != nil {
	sendProgress(-1, "Error al adjuntar disco: "+err.Error())
	return
}

sendProgress(35, "Iniciando VM en modo headless...")
runVBox("startvm", vmName, "--type", "headless")

// 7. Wait for boot with progress updates
sendProgress(40, "Esperando arranque del sistema (45s)...")
for i := 1; i <= 9; i++ {
	time.Sleep(5 * time.Second)
	pct := 40 + (i * 3) // 43, 46, 49, 52, 55, 58, 61, 64, 67
	sendProgress(pct, fmt.Sprintf("Arrancando sistema... %ds/45s", i*5))
}

// 8. SSH lambda for NAT port access
cloneSSH := func(cmd string) (string, error) {
config, _ := buildSSHConfig()
client, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%s", sshPort), config)
if err != nil {
return "", err
}
defer client.Close()
session, err := client.NewSession()
if err != nil {
return "", err
}
defer session.Close()
out, err := session.CombinedOutput(cmd)
return string(out), err
}

// 9. Configure static IP
sendProgress(70, fmt.Sprintf("Configurando IP estática %s...", nuevaIP))
ifaceContent := fmt.Sprintf("#VAGRANT-BEGIN\nauto eth1\niface eth1 inet static\n      address %s\n      netmask 255.255.255.0\n#VAGRANT-END\n", nuevaIP)
cloneSSH(fmt.Sprintf(`sudo bash -c "printf '%s' > /etc/network/interfaces.d/eth1-static"`, ifaceContent))
cloneSSH("sudo hostnamectl set-hostname " + nombre)
cloneSSH("sudo ip addr flush dev eth1 || true")
cloneSSH(fmt.Sprintf("sudo ip addr add %s/24 dev eth1 && sudo ip link set eth1 up", nuevaIP))

time.Sleep(2 * time.Second)

// 10. Register in BIND9 DNS
sendProgress(78, "Registrando en servidor DNS...")
err = registerDNS(nombre, nuevaIP)
if err != nil {
fmt.Println("Advertencia DNS:", err)
}

// 11. Upload ZIP via SCP through NAT port
sendProgress(82, "Subiendo archivos del sitio web...")
	remoteZipPath := "/home/" + sshUser + "/deploy.zip"
	scpConfig, _ := buildSSHConfig()
	scpNATClient := scp.NewClient(fmt.Sprintf("127.0.0.1:%s", sshPort), scpConfig)
	zipLocalFile, err := os.Open(zipTempPath)
	if err != nil {
		sendProgress(-1, "Error abriendo ZIP: "+err.Error())
		return
	}
	defer zipLocalFile.Close()
	if err = scpNATClient.Connect(); err != nil {
		sendProgress(-1, "Error conectando SCP: "+err.Error())
		return
	}
	defer scpNATClient.Close()
	if err = scpNATClient.CopyFromFile(context.Background(), *zipLocalFile, remoteZipPath, "0644"); err != nil {
		sendProgress(-1, "Error subiendo ZIP: "+err.Error())
		return
	}

// 12. Deploy: unzip and flatten
sendProgress(88, "Descomprimiendo y desplegando sitio...")
	deployCmd := fmt.Sprintf("sudo rm -rf /var/www/html/* && sudo unzip -o %s -d /var/www/html/", remoteZipPath)
	deployCmd += ` && sudo bash -c 'cd /var/www/html && COUNT=$(ls -1 | wc -l) && if [ "$COUNT" -eq 1 ]; then DIR=$(ls -1); if [ -d "$DIR" ]; then shopt -s dotglob; mv "$DIR"/* . 2>/dev/null; rmdir "$DIR" 2>/dev/null; fi; fi'`
	out, err := cloneSSH(deployCmd)
	if err != nil {
		fmt.Printf("Advertencia deploy: %v | %s\n", err, out)
	}

sendProgress(93, "Reiniciando Apache...")
	cloneSSH("sudo systemctl restart apache2")

// 13. Save instance metadata
sendProgress(97, "Guardando metadatos de instancia...")
insts, _ := getInstances()
newInstance := Instance{
Name:      nombre,
IP:        nuevaIP,
URL:       "http://" + nombre + ".cloud.local",
CreatedAt: time.Now().Format(time.RFC3339),
}
insts = append(insts, newInstance)
saveInstances(insts)

// Final event: send the result data
finalData := fmt.Sprintf(`{"progress":100,"message":"¡Sitio desplegado exitosamente!","done":true,"name":"%s","ip":"%s","url":"http://%s.cloud.local"}`, nombre, nuevaIP, nombre)
fmt.Fprintf(w, "data: %s\n\n", finalData)
flusher.Flush()
}

// handleInstances processes HTTP GET requests to list all provisioned instances.
// Returns JSON array of Instance structs with their metadata.
func handleInstances(w http.ResponseWriter, r *http.Request) {
if r.Method == "GET" {
insts, _ := getInstances()
jsonOK(w, insts)
}
}

// handleDeleteInstance processes HTTP DELETE requests to remove an instance.
// Extracts instance name from URL path (last segment: /api/instances/{name})
// The deletion flow:
// 1. Locates instance in metadata by name
// 2. Powers off the VM
// 3. Unregisters and deletes the VM from VirtualBox
// 4. Deregisters hostname from BIND9 DNS
// 5. Updates instance metadata file
//
// Returns error if instance not found or VirtualBox operations fail.
func handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
if r.Method != "DELETE" {
http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
return
}

parts := strings.Split(r.URL.Path, "/")
nombre := parts[len(parts)-1]

insts, _ := getInstances()
var newInsts []Instance
found := false
for _, i := range insts {
if i.Name == nombre {
found = true
continue
}
newInsts = append(newInsts, i)
}

if !found {
jsonError(w, "Instancia no encontrada")
return
}

	vmName := "web-" + nombre
	// Usar runVBoxQuiet para no llenar el log si la máquina no existe
	runVBoxQuiet("controlvm", vmName, "poweroff")
	time.Sleep(2 * time.Second)
	runVBoxQuiet("unregistervm", vmName, "--delete-all")

	deregisterDNS(nombre)
	saveInstances(newInsts)

jsonOK(w, Response{Success: true, Message: "Instancia eliminada"})
}
