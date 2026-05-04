package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Instance struct {
	Name      string `json:"name"`
	IP        string `json:"ip"`
	URL       string `json:"url"`
	CreatedAt string `json:"createdAt"`
}

var (
	instancesFile = "/home/vagrant/instances.json"
	instancesMu   sync.Mutex
	provisionMu   sync.Mutex // Evita concurrencia en la IP temporal .30
)

func getInstances() ([]Instance, error) {
	instancesMu.Lock()
	defer instancesMu.Unlock()

	data, err := os.ReadFile(instancesFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Fallback si no estamos en Vagrant
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

func getPlantillaDiskUUID() (string, error) {
	out, _ := runVBoxQuiet("showvminfo", "plantilla_http_base", "--machinereadable")
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "\"SATA-0-0\"=") {
			return strings.Trim(strings.Split(line, "=")[1], "\""), nil
		}
	}
	// Fallback: buscar en la lista general de HDDs
	out, _ = runVBoxQuiet("list", "hdds")
	hdds := strings.Split(out, "UUID:")
	for _, hdd := range hdds {
		if strings.Contains(hdd, "plantilla_http_base") {
			lines := strings.Split(hdd, "\n")
			return strings.TrimSpace(lines[0]), nil
		}
	}
	return "", fmt.Errorf("no se pudo encontrar el disco de la plantilla")
}

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

// ─── API Handlers ────────────────────────────────────────────────────────────

func handleProvision(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	provisionMu.Lock()
	defer provisionMu.Unlock()

	// 1. Leer parámetros
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

	// Guardar zip temporal
	zipTempPath := "./temp_" + zipHeader.Filename
	zf, err := os.Create(zipTempPath)
	if err != nil {
		jsonError(w, "Error creando archivo temporal: "+err.Error())
		return
	}
	io.Copy(zf, zipFile)
	zf.Close()
	defer os.Remove(zipTempPath)

	// 2. Elegir IP libre
	nuevaIP, err := getNextFreeIP()
	if err != nil {
		jsonError(w, err.Error())
		return
	}

	// 3. Buscar el disco de la plantilla dinámicamente
	plantillaDiskUUID, err := getPlantillaDiskUUID()
	if err != nil {
		jsonError(w, err.Error())
		return
	}

	// 4. Clonar el disco de la plantilla para este clon
	vmName := "web-" + nombre
	vboxFolder := getVBoxDefaultFolder()
	vmFolder := filepath.Join(vboxFolder, vmName)
	cloneDiskPath := filepath.Join(vmFolder, vmName+".vdi")

	// Crear directorio para la VM
	os.MkdirAll(vmFolder, 0755)

	fmt.Printf("Clonando disco para %s...\n", vmName)
	_, err = runVBox("clonemedium", plantillaDiskUUID, cloneDiskPath, "--format", "VDI")
	if err != nil {
		jsonError(w, "Error clonando disco: "+err.Error())
		return
	}

	// 5. Calcular puerto SSH libre (base 2300 + offset según IP)
	ipParts := strings.Split(nuevaIP, ".")
	ipOffset := 0
	fmt.Sscanf(ipParts[3], "%d", &ipOffset)
	sshPort := fmt.Sprintf("%d", 2300+ipOffset)

	// 6. Crear VM
	_, err = runVBox("createvm", "--name", vmName, "--ostype", "Debian_64", "--register")
	if err != nil {
		jsonError(w, "Error creando VM: "+err.Error())
		return
	}

	// NIC1: NAT con reenvío SSH propio (para no depender de la IP en hostonly al inicio)
	// NIC2: Host-only (para que la VM sea alcanzable después con su IP fija)
	runVBox("modifyvm", vmName, "--memory", "512",
		"--nic1", "nat",
		"--natpf1", fmt.Sprintf("ssh,tcp,127.0.0.1,%s,,22", sshPort),
		"--nic2", "hostonly", "--hostonlyadapter2", "vboxnet0")
	runVBox("storagectl", vmName, "--name", "SATA", "--add", "sata")
	runVBox("storageattach", vmName, "--storagectl", "SATA", "--port", "0", "--device", "0",
		"--type", "hdd", "--medium", cloneDiskPath)

	// 7. Arrancar el clon
	runVBox("startvm", vmName, "--type", "headless")

	fmt.Printf("Esperando que %s bootee (45s)...\n", vmName)
	time.Sleep(45 * time.Second)

	// 8. SSH via el puerto NAT exclusivo del clon (127.0.0.1:sshPort)
	//    El clon tiene su propio disco → no interfiere con la plantilla
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

	// 9. Configurar IP estática en eth1 (vboxnet0) del clon
	fmt.Printf("Configurando IP %s en %s...\n", nuevaIP, vmName)
	ifaceContent := fmt.Sprintf("#VAGRANT-BEGIN\nauto eth1\niface eth1 inet static\n      address %s\n      netmask 255.255.255.0\n#VAGRANT-END\n", nuevaIP)
	cloneSSH(fmt.Sprintf(`sudo bash -c "printf '%s' > /etc/network/interfaces.d/eth1-static"`, ifaceContent))
	cloneSSH("sudo hostnamectl set-hostname " + nombre)
	cloneSSH("sudo ip addr flush dev eth1 || true")
	cloneSSH(fmt.Sprintf("sudo ip addr add %s/24 dev eth1 && sudo ip link set eth1 up", nuevaIP))

	time.Sleep(2 * time.Second)

	// 10. Registrar en DNS
	err = registerDNS(nombre, nuevaIP)
	if err != nil {
		fmt.Println("Advertencia DNS:", err)
	}

	// 11. Subir .zip vía SCP usando la nueva IP en vboxnet0
	remoteZipPath := "/home/" + sshUser + "/" + zipHeader.Filename
	uploadFile(nuevaIP, zipTempPath, remoteZipPath)
	runSSH(nuevaIP, fmt.Sprintf("sudo rm -rf /var/www/html/* && sudo unzip -o %s -d /var/www/html/", remoteZipPath))
	runSSH(nuevaIP, "sudo systemctl restart apache2")

	// 12. Guardar en JSON
	insts, _ := getInstances()
	newInstance := Instance{
		Name:      nombre,
		IP:        nuevaIP,
		URL:       "http://" + nombre + ".cloud.local",
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	insts = append(insts, newInstance)
	saveInstances(insts)

	jsonOK(w, newInstance)
}


func handleInstances(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		insts, _ := getInstances()
		jsonOK(w, insts)
	}
}

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
	runVBox("controlvm", vmName, "poweroff")
	time.Sleep(3 * time.Second)
	runVBox("unregistervm", vmName, "--delete-all")

	deregisterDNS(nombre)
	saveInstances(newInsts)

	jsonOK(w, Response{Success: true, Message: "Instancia eliminada"})
}
