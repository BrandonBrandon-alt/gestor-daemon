// Package main implements a VirtualBox infrastructure orchestrator daemon.
// It manages VM lifecycle, network configuration, DNS registration, and auto-scaling
// for cloud-based deployment environments running on VirtualBox.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"gestor-daemon/internal/api"
	"gestor-daemon/internal/autoscaler"
	"gestor-daemon/internal/config"
	"gestor-daemon/internal/httpaas"
	"gestor-daemon/internal/sshutil"
	"gestor-daemon/internal/virtualbox"
	"gestor-daemon/web"
)

// displayVMStatusTable prints a formatted table of all VMs and their status to stdout.
// Queries all registered VMs concurrently, collects their details, and displays them sorted by name.
// If no VMs exist, displays an informational message. Called periodically to show current infrastructure state.
func displayVMStatusTable() {
	fmt.Println("\n=== ESTADO DEL ENTORNO DE MÁQUINAS VIRTUALES ===")
	out, err := virtualbox.RunVBoxQuiet("list", "vms")
	if err != nil || strings.TrimSpace(out) == "" {
		fmt.Println("(Sin máquinas virtuales registradas)")
		fmt.Println("====================================================")
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
			d := virtualbox.FetchVMDetails(n, "")
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
	fmt.Println("====================================================")
}

// Disk represents a VirtualBox hard disk with metadata for inventory management.
// Used in listing and selection operations for disk-based VM creation.
type Disk struct {
	// Name is the filename of the disk (extracted from path)
	Name string `json:"name"`
	// Location is the absolute filesystem path to the disk file
	Location string `json:"location"`
	// UUID is the unique VirtualBox identifier for the disk
	UUID string `json:"uuid"`
}

// handleListVMs processes HTTP GET requests to retrieve all registered virtual machines.
// Queries VirtualBox for all VMs, concurrently retrieves their details, and returns JSON.
// Returns array of maps containing: name, uuid, state, ip, port.
func handleListVMs(w http.ResponseWriter, r *http.Request) {
	out, err := virtualbox.RunVBoxQuiet("list", "vms")
	if err != nil {
		api.JSONError(w, "Error listando VMs: "+err.Error())
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
			d := virtualbox.FetchVMDetails(n, u)
			mu.Lock()
			vms = append(vms, d)
			mu.Unlock()
		}(name, uuid)
	}
	wg.Wait()

	sort.Slice(vms, func(i, j int) bool {
		return vms[i]["name"] < vms[j]["name"]
	})

	api.JSONOK(w, vms)
}

// handleListDisks processes HTTP GET requests to retrieve all available VirtualBox disks.
// Parses the raw VBoxManage output to extract disk UUID, location, and name.
// Identifies multiattach disks (base disks cloned for multiple VMs).
// Returns JSON array of Disk objects.
func handleListDisks(w http.ResponseWriter, r *http.Request) {
	out, err := virtualbox.RunVBox("list", "hdds")
	if err != nil {
		api.JSONError(w, "Error listando discos: "+err.Error())
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
	api.JSONOK(w, disks)
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
		api.JSONError(w, "JSON inválido: "+err.Error())
		return
	}
	if body.VMName == "" || body.DiskUUID == "" {
		api.JSONError(w, "vmName y diskUUID son obligatorios")
		return
	}

	adapter := body.BridgeAdapter
	if adapter == "" {
		adapter = config.Global.BridgeAdapter
	}

	if _, err := virtualbox.RunVBox("createvm", "--name", body.VMName, "--ostype", "Debian_64", "--register"); err != nil {
		api.JSONError(w, "Error creando VM: "+err.Error())
		return
	}

	virtualbox.RunVBox("modifyvm", body.VMName, "--memory", "512", "--ioapic", "on", "--nic1", "bridged", "--bridgeadapter1", adapter)
	virtualbox.RunVBox("storagectl", body.VMName, "--name", "SATA", "--add", "sata")

	if _, err := virtualbox.RunVBox("storageattach", body.VMName,
		"--storagectl", "SATA", "--port", "0", "--device", "0",
		"--type", "hdd", "--medium", body.DiskUUID, "--mtype", "multiattach"); err != nil {
		api.JSONError(w, "Error adjuntando disco: "+err.Error())
		return
	}

	if _, err := virtualbox.RunVBox("startvm", body.VMName, "--type", "headless"); err != nil {
		api.JSONError(w, "Error iniciando VM: "+err.Error())
		return
	}

	ip, err := virtualbox.GetVMIP(body.VMName)
	if err != nil {
		api.JSONError(w, err.Error())
		return
	}

	// Guardar el puerto como propiedad para leerlo luego en la tabla de estado
	if body.Port != "" {
		virtualbox.RunVBoxQuiet("guestproperty", "set", body.VMName, "/Gestor/Port", body.Port)
	}

	go displayVMStatusTable()

	api.JSONOK(w, map[string]string{
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
		api.JSONError(w, "JSON inválido: "+err.Error())
		return
	}
	if body.VMName == "" {
		api.JSONError(w, "vmName es requerido")
		return
	}
	out, err := virtualbox.RunVBox("controlvm", body.VMName, "poweroff")
	if err != nil {
		api.JSONError(w, "Error apagando VM: "+out)
		return
	}

	go func() {
		time.Sleep(3 * time.Second)
		displayVMStatusTable()
	}()

	api.JSONOK(w, api.Response{Success: true, Message: "Señal de apagado enviada a " + body.VMName})
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
		api.JSONError(w, "JSON inválido: "+err.Error())
		return
	}
	if body.VMName == "" {
		api.JSONError(w, "vmName es requerido")
		return
	}

	// 1. Forzar apagado primero
	virtualbox.RunVBoxQuiet("controlvm", body.VMName, "poweroff")

	// 2. Esperar 3 segundos para que los procesos se liberen
	time.Sleep(3 * time.Second)

	// 3. Destruir y eliminar todos sus archivos
	out, err := virtualbox.RunVBoxQuiet("unregistervm", body.VMName, "--delete-all")
	if err != nil {
		// Respaldos posibles según la versión de VirtualBox
		out2, err2 := virtualbox.RunVBoxQuiet("unregistervm", body.VMName, "--delete")
		if err2 != nil {
			api.JSONError(w, "Error eliminando VM: "+out+" | Respaldo: "+out2)
			return
		}
	}

	go displayVMStatusTable()

	api.JSONOK(w, api.Response{Success: true, Message: "La máquina " + body.VMName + " ha sido eliminada permanentemente del sistema"})
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
		api.JSONError(w, "JSON inválido: "+err.Error())
		return
	}
	if body.LBIp == "" {
		api.JSONError(w, "La IP del balanceador es requerida")
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

	// 2. Upload to remote HAProxy via sshutil.CreateRemoteFile
	tempRemotePath := "/etc/haproxy/haproxy.cfg"
	if err := sshutil.CreateRemoteFile(body.LBIp, tempRemotePath, cfg.String()); err != nil {
		api.JSONError(w, "Error inyectando cfg vía SSH: "+err.Error())
		return
	}

	// 3. Restart HAProxy
	if out, err := sshutil.RunSSH(body.LBIp, "sudo systemctl restart haproxy"); err != nil {
		api.JSONError(w, fmt.Sprintf("Error reiniciando HAProxy: %v | Salida: %s", err, out))
		return
	}

	api.JSONOK(w, api.Response{Success: true, Message: "Balanceador en " + body.LBIp + " actualizado correctamente con " + fmt.Sprintf("%d", len(body.Servers)) + " servidores traseros."})
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
		api.JSONError(w, "IP del balanceador requerida")
		return
	}

	// Consultar estadísticas vía socket de HAProxy
	// El comando 'show stat' devuelve un CSV
	cmd := "echo \"show stat\" | sudo socat stdio /run/haproxy/admin.sock"
	out, err := sshutil.RunSSH(lbIp, cmd)
	if err != nil {
		api.JSONError(w, "Error obteniendo estadísticas (verifica que socat esté instalado): "+err.Error())
		return
	}

	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	if len(lines) < 2 {
		api.JSONError(w, "Respuesta de estadísticas vacía o inválida")
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

	api.JSONOK(w, stats)
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
			api.JSONError(w, "Error leyendo JSON")
			return
		}
		if err := os.WriteFile(stateFile, body, 0644); err != nil {
			api.JSONError(w, "Error guardando estado: "+err.Error())
			return
		}
		api.JSONOK(w, api.Response{Success: true, Message: "Estado HAProxy guardado"})
		return
	}

	w.WriteHeader(http.StatusMethodNotAllowed)
}

func handleStartVM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VMName string `json:"vmName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.JSONError(w, "JSON inválido: "+err.Error())
		return
	}
	out, err := virtualbox.RunVBox("startvm", body.VMName, "--type", "headless")
	if err != nil {
		api.JSONError(w, "Error iniciando VM: "+out)
		return
	}

	go displayVMStatusTable()

	api.JSONOK(w, api.Response{Success: true, Message: "VM " + body.VMName + " iniciada"})
}

func handleService(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IP      string `json:"ip"`
		Action  string `json:"action"`
		Service string `json:"service"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.JSONError(w, "JSON inválido: "+err.Error())
		return
	}
	if body.IP == "" {
		api.JSONError(w, "El campo ip es requerido")
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
		api.JSONError(w, "Acción inválida: "+body.Action)
		return
	}
	out, err := sshutil.RunSSH(body.IP, cmd)
	if err != nil {
		api.JSONOK(w, map[string]string{
			"output": fmt.Sprintf("Error SSH con %s: %v\n%s", body.IP, err, out),
		})
		return
	}
	api.JSONOK(w, map[string]string{"output": out})
}

func handlePrepareVM(w http.ResponseWriter, r *http.Request) {
	r.ParseMultipartForm(50 << 20)

	port := r.FormValue("port")
	templateVM := r.FormValue("templateVM")
	newDiskName := r.FormValue("newDiskName")

	if port == "" || templateVM == "" || newDiskName == "" {
		api.JSONError(w, "Faltan campos: port, templateVM, newDiskName")
		return
	}

	execFile, header, err := r.FormFile("execFile")
	if err != nil {
		api.JSONError(w, "Error leyendo ejecutable: "+err.Error())
		return
	}
	defer execFile.Close()

	tempExecPath := "./temp_" + header.Filename
	tempFile, err := os.Create(tempExecPath)
	if err != nil {
		api.JSONError(w, "Error creando archivo temporal: "+err.Error())
		return
	}
	if _, err := io.Copy(tempFile, execFile); err != nil {
		tempFile.Close()
		os.Remove(tempExecPath)
		api.JSONError(w, "Error guardando ejecutable: "+err.Error())
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
			api.JSONError(w, "Error creando zip temporal: "+err.Error())
			return
		}
		if _, err := io.Copy(zf, zipFile); err != nil {
			zf.Close()
			os.Remove(zipTempPath)
			api.JSONError(w, "Error guardando zip: "+err.Error())
			return
		}
		zf.Close()
		defer os.Remove(zipTempPath)
	}

	logOutput := fmt.Sprintf("Iniciando automatización para '%s'...\n", templateVM)

	// PASO 1: Encender plantilla en headless
	logOutput += "1. Encendiendo plantilla en modo headless...\n"
	virtualbox.RunVBox("startvm", templateVM, "--type", "headless")

	// PASO 2: Detectar IP via Guest Additions
	logOutput += "2. Detectando IP via Guest Additions...\n"
	templateIP, err := virtualbox.GetVMIP(templateVM)
	if err != nil {
		virtualbox.RunVBox("controlvm", templateVM, "poweroff")
		api.JSONError(w, err.Error())
		return
	}
	logOutput += fmt.Sprintf("   IP detectada: %s\n", templateIP)

	// PASO CRÍTICO: Pausa de 45s para que SSH esté 100% levantado
	logOutput += "   Esperando 45s a que SSH esté disponible...\n"
	time.Sleep(45 * time.Second)

	// PASO 3: Subir ejecutable
	logOutput += "3. Subiendo ejecutable por SCP...\n"
	remoteExecPath := "/home/" + config.Global.SSHUser + "/" + header.Filename
	if err = sshutil.UploadFile(templateIP, tempExecPath, remoteExecPath); err != nil {
		api.JSONError(w, "Error subiendo ejecutable: "+err.Error())
		return
	}
	logOutput += "   Ejecutable subido correctamente.\n"

	// PASO 4: Subir y descomprimir .zip
	if zipTempPath != "" {
		logOutput += "4. Subiendo archivos adicionales (.zip)...\n"
		remoteZipPath := "/home/" + config.Global.SSHUser + "/" + zipFileName
		if err = sshutil.UploadFile(templateIP, zipTempPath, remoteZipPath); err != nil {
			api.JSONError(w, "Error subiendo zip: "+err.Error())
			return
		}
		unzipOut, unzipErr := sshutil.RunSSH(templateIP, fmt.Sprintf("cd /home/%s && unzip -o %s", config.Global.SSHUser, zipFileName))
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
WantedBy=multi-user.target`, config.Global.SSHUser, config.Global.SSHUser, remoteExecPath, port)

	if err = sshutil.CreateRemoteFile(templateIP, "/etc/systemd/system/"+serviceName, serviceContent); err != nil {
		api.JSONError(w, "Error creando .service: "+err.Error())
		return
	}
	logOutput += "   Archivo .service creado en /etc/systemd/system/.\n"

	// PASO 6: Habilitar servicio y sincronizar disco
	logOutput += "6. Habilitando servicio y guardando en disco...\n"
	if out, err := sshutil.RunSSH(templateIP, "sudo systemctl daemon-reload"); err != nil {
		logOutput += fmt.Sprintf("   Advertencia daemon-reload: %v | %s\n", err, out)
	}
	if out, err := sshutil.RunSSH(templateIP, "sudo systemctl enable "+serviceName); err != nil {
		logOutput += fmt.Sprintf("   Advertencia enable: %v | %s\n", err, out)
	} else {
		logOutput += "   Servicio habilitado para inicio automático.\n" + out
	}

	// EL COMANDO MÁGICO: Forzar a Linux a escribir la caché en el disco duro
	logOutput += "   Sincronizando disco (sync)...\n"
	sshutil.RunSSH(templateIP, "sync")
	time.Sleep(2 * time.Second) // Damos 2 segundos para que el disco respire

	// PASO 7: Apagar limpiamente con poweroff
	logOutput += "7. Apagando plantilla (Forzado)...\n"
	virtualbox.RunVBox("controlvm", templateVM, "poweroff")
	time.Sleep(8 * time.Second)

	// PASO 8: Detectar ruta del disco y convertirlo a multiconexión
	logOutput += "8. Preparando disco multiconexión...\n"
	vminfo, _ := virtualbox.RunVBox("showvminfo", templateVM, "--machinereadable")

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
		virtualbox.RunVBox("storageattach", templateVM, "--storagectl", storageName, "--port", storagePort, "--device", device, "--type", "hdd", "--medium", "none")

		// 2. Convertir a multiconexión
		_, err := virtualbox.RunVBox("modifymedium", "disk", diskPath, "--type", "multiattach")
		if err != nil {
			logOutput += "   Nota al convertir disco: " + err.Error() + "\n"
		} else {
			logOutput += "   ¡Disco convertido a multiconexión exitosamente!\n"
		}
	} else {
		logOutput += "   ⚠ No se detectó un disco duro válido (.vdi o .vmdk) en la máquina.\n"
	}

	api.JSONOK(w, map[string]string{
		"message": "¡Automatización completada con éxito!",
		"output":  logOutput,
	})
}

func handleDeleteDisk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DiskUUID string `json:"diskUUID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		api.JSONError(w, "JSON inválido: "+err.Error())
		return
	}
	if body.DiskUUID == "" {
		api.JSONError(w, "diskUUID es requerido")
		return
	}
	out, err := virtualbox.RunVBox("closemedium", "disk", body.DiskUUID, "--delete")
	if err != nil {
		api.JSONError(w, "Error eliminando disco: "+out)
		return
	}
	api.JSONOK(w, api.Response{Success: true, Message: "Disco eliminado correctamente"})
}

func handleListNetworkAdapters(w http.ResponseWriter, r *http.Request) {
	adapters, err := virtualbox.ListNetworkAdapters()
	if err != nil {
		api.JSONError(w, "Error listando adaptadores: "+err.Error())
		return
	}
	api.JSONOK(w, adapters)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(web.IndexHTML)
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	config.Load()
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
	http.HandleFunc("/api/autoscaling/config", autoscaler.HandleAutoScalingConfig)
	http.HandleFunc("/api/autoscaling/status", autoscaler.HandleAutoScalingStatus)

	// Nuevos Endpoints HTTPaaS
	http.HandleFunc("/api/provision", httpaas.HandleProvision)
	http.HandleFunc("/api/instances", httpaas.HandleInstances)
	http.HandleFunc("/api/instances/", httpaas.HandleDeleteInstance)

	fmt.Println("Gestor de demonios corriendo en http://localhost:8090")
	go displayVMStatusTable()
	go autoscaler.StartAutoscaler()
	log.Fatal(http.ListenAndServe(":8090", nil))
}

// ─── Fin ──────────────────────────────────────────────────────────────────────
