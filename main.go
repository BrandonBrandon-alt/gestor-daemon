package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	scp "github.com/bramvdbogaerde/go-scp"
	"golang.org/x/crypto/ssh"
)

// ─── Configuración ───────────────────────────────────────────────────────────

var (
	sshUser       = getEnvOrDefault("SSH_USER", "servidor1")
	sshKeyPath    = getEnvOrDefault("SSH_KEY_PATH", `C:\Users\sneid\.ssh\id_rsa`)
	vboxManage    = getEnvOrDefault("VBOX_MANAGE", `C:\Program Files\Oracle\VirtualBox\VBoxManage.exe`)
	bridgeAdapter = getEnvOrDefault("BRIDGE_ADAPTER", "Intel(R) Wi-Fi 6 AX201 160MHz")
)

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// ─── SSH Config centralizada ─────────────────────────────────────────────────

func buildSSHConfig() (*ssh.ClientConfig, error) {
	key, err := os.ReadFile(sshKeyPath)
	if err != nil {
		return nil, fmt.Errorf("no se pudo leer la llave SSH: %v", err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("llave SSH inválida: %v", err)
	}
	return &ssh.ClientConfig{
		User:            sshUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}, nil
}

// ─── SSH y SCP ───────────────────────────────────────────────────────────────

func sshClient(ip string) (*ssh.Client, error) {
	config, err := buildSSHConfig()
	if err != nil {
		return nil, err
	}
	return ssh.Dial("tcp", ip+":22", config)
}

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

// ─── VBoxManage ──────────────────────────────────────────────────────────────

func runVBox(args ...string) (string, error) {
	fmt.Printf("VBox: %s\n", strings.Join(args, " "))
	out, err := exec.Command(vboxManage, args...).CombinedOutput()
	if err != nil {
		fmt.Printf("Error: %v | %s\n", err, string(out))
	}
	return string(out), err
}

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

func runVBoxQuiet(args ...string) (string, error) {
	out, err := exec.Command(vboxManage, args...).CombinedOutput()
	return string(out), err
}

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

// ─── Estructuras JSON ────────────────────────────────────────────────────────

type Response struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type Disk struct {
	Name     string `json:"name"`
	Location string `json:"location"`
	UUID     string `json:"uuid"`
}

// ─── Handlers API ────────────────────────────────────────────────────────────

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

func handleCreateVM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VMName   string `json:"vmName"`
		DiskUUID string `json:"diskUUID"`
		Port     string `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "JSON inválido: "+err.Error())
		return
	}
	if body.VMName == "" || body.DiskUUID == "" {
		jsonError(w, "vmName y diskUUID son obligatorios")
		return
	}

	if _, err := runVBox("createvm", "--name", body.VMName, "--ostype", "Debian_64", "--register"); err != nil {
		jsonError(w, "Error creando VM: "+err.Error())
		return
	}

	runVBox("modifyvm", body.VMName, "--memory", "512", "--nic1", "bridged", "--bridgeadapter1", bridgeAdapter)
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

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, htmlContent)
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

	fmt.Println("Gestor de demonios corriendo en http://localhost:8090")
	go displayVMStatusTable()
	log.Fatal(http.ListenAndServe(":8090", nil))
}

// ─── HTML Frontend ────────────────────────────────────────────────────────────

const htmlContent = `<!DOCTYPE html>
<html lang="es">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Gestor de Demonios — systemd</title>
<style>
  @import url('https://fonts.googleapis.com/css2?family=Space+Mono:wght@400;700&family=Inter:wght@300;400;600&display=swap');
  :root {
    --bg: #0d0d0d; --surface: #161616; --border: #2a2a2a;
    --accent: #00ff88; --accent2: #ff4466; --accent3: #4488ff;
    --text: #e8e8e8; --muted: #666;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { background: var(--bg); color: var(--text); font-family: 'Inter', sans-serif; min-height: 100vh; padding: 2rem; }
  header { display: flex; align-items: center; gap: 1rem; margin-bottom: 2.5rem; border-bottom: 1px solid var(--border); padding-bottom: 1.5rem; }
  header h1 { font-family: 'Space Mono', monospace; font-size: 1.4rem; color: var(--accent); }
  header span { font-size: 0.75rem; color: var(--muted); font-family: 'Space Mono', monospace; }
  .dot { width: 10px; height: 10px; border-radius: 50%; background: var(--accent); box-shadow: 0 0 8px var(--accent); animation: pulse 2s infinite; }
  @keyframes pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.3; } }
  .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 1.5rem; margin-bottom: 1.5rem; }
  @media (max-width: 900px) { .grid { grid-template-columns: 1fr; } }
  .card { background: var(--surface); border: 1px solid var(--border); border-radius: 8px; padding: 1.5rem; }
  .card h2 { font-family: 'Space Mono', monospace; font-size: 0.75rem; color: var(--muted); text-transform: uppercase; letter-spacing: 2px; margin-bottom: 1.2rem; }
  .card h2 span { color: var(--accent); margin-right: 0.5rem; }
  table { width: 100%; border-collapse: collapse; font-size: 0.875rem; }
  th { text-align: left; padding: 0.5rem 0.75rem; color: var(--muted); font-weight: 400; font-size: 0.75rem; border-bottom: 1px solid var(--border); }
  td { padding: 0.6rem 0.75rem; border-bottom: 1px solid #1a1a1a; font-family: 'Space Mono', monospace; font-size: 0.8rem; }
  tr:hover td { background: #1c1c1c; }
  .btn { padding: 0.3rem 0.75rem; border: none; border-radius: 4px; font-family: 'Space Mono', monospace; font-size: 0.7rem; cursor: pointer; transition: opacity 0.15s; text-decoration: none; display: inline-block; line-height: 1.6; }
  .btn:hover { opacity: 0.8; }
  .btn-green { background: var(--accent); color: #000; }
  .btn-red   { background: var(--accent2); color: #fff; }
  .btn-blue  { background: var(--accent3); color: #fff; }
  .btn-gray  { background: #2a2a2a; color: var(--text); }
  .form-group { margin-bottom: 1rem; }
  label { display: block; font-size: 0.75rem; color: var(--muted); margin-bottom: 0.3rem; font-family: 'Space Mono', monospace; }
  input { width: 100%; background: #111; border: 1px solid var(--border); border-radius: 4px; padding: 0.5rem 0.75rem; color: var(--text); font-family: 'Space Mono', monospace; font-size: 0.8rem; outline: none; transition: border-color 0.15s; }
  input:focus { border-color: var(--accent); }
  .output { background: #0a0a0a; border: 1px solid var(--border); border-radius: 4px; padding: 1rem; font-family: 'Space Mono', monospace; font-size: 0.75rem; color: var(--accent); white-space: pre-wrap; max-height: 300px; overflow-y: auto; min-height: 60px; }
  .service-actions { display: flex; flex-wrap: wrap; gap: 0.5rem; margin-bottom: 1rem; }
  .full { grid-column: 1 / -1; }
  .ip-input { display: flex; gap: 0.5rem; margin-bottom: 1rem; }
  .ip-input input { flex: 1; }
  .vm-ip   { color: var(--accent); }
  .vm-port { color: var(--accent3); }
</style>
</head>
<body>

<header>
  <div class="dot"></div>
  <h1>gestor-daemon</h1>
  <span>// systemd · VirtualBox · SSH · Guest Additions</span>
</header>

<div class="grid">

  <div class="card full">
    <h2><span>//</span>Preparar Máquina Virtual (Plantilla)</h2>
    <form id="prepare-vm-form" onsubmit="prepararVM(event)">
      <div class="grid" style="grid-template-columns:1fr 1fr;margin-bottom:0">
        <div class="form-group">
          <label>Archivo ejecutable</label>
          <input type="file" id="exec-file" required>
        </div>
        <div class="form-group">
          <label>Número de puerto</label>
          <input type="number" id="vm-port" placeholder="ej. 8081" required>
        </div>
        <div class="form-group">
          <label>Archivos adicionales (.zip)</label>
          <input type="file" id="zip-file" accept=".zip">
        </div>
        <div class="form-group">
          <label>Máquina virtual (plantilla)</label>
          <input type="text" id="template-vm-name" placeholder="ej. plantillaServer" required>
        </div>
        <div class="form-group">
          <label>Nombre disco multiconexión</label>
          <input type="text" id="new-disk-name" placeholder="ej. disco-srvimg" required>
        </div>
      </div>
      <button type="submit" class="btn btn-green" id="btn-preparar" style="margin-top:1rem">
        Ejecutar Automatización
      </button>
      <div id="prepare-output" class="output" style="margin-top:1rem;display:none"></div>
    </form>
  </div>

  <div class="card">
    <h2><span>//</span>Discos Multiconexión</h2>
    <table>
      <thead><tr><th>Nombre</th><th>Ruta</th><th>Acciones</th></tr></thead>
      <tbody id="disks-table">
        <tr><td colspan="3" style="color:var(--muted)">Cargando...</td></tr>
      </tbody>
    </table>
  </div>

  <div class="card">
    <h2><span>//</span>Crear Máquina Virtual Hija</h2>
    <div class="form-group">
      <label>Nombre de la nueva VM</label>
      <input type="text" id="new-vm-name" placeholder="ej. Srv-img1">
    </div>
    <div class="form-group">
      <label>UUID del disco multiconexión</label>
      <input type="text" id="disk-uuid" placeholder="Se llena al hacer clic en Usar este disco">
    </div>
    <button class="btn btn-green" onclick="createVM()">+ Crear e iniciar VM</button>
    <div id="create-output" class="output" style="margin-top:1rem;display:none"></div>
  </div>

  <div class="card full">
    <h2><span>//</span>Máquinas Virtuales Hijas</h2>
    <table>
      <thead>
        <tr><th>Nombre</th><th>Dirección IP</th><th>Puerto</th><th>Acciones</th></tr>
      </thead>
      <tbody id="vms-hijas-table">
        <tr><td colspan="4" style="color:var(--muted)">Las VMs creadas aparecerán aquí</td></tr>
      </tbody>
    </table>
  </div>

  <div class="card full">
    <h2><span>//</span>Máquinas Virtuales Registradas (VirtualBox)</h2>
    <table>
      <thead><tr><th>Nombre</th><th>UUID</th><th>Acciones</th></tr></thead>
      <tbody id="vms-table">
        <tr><td colspan="3" style="color:var(--muted)">Cargando...</td></tr>
      </tbody>
    </table>
  </div>

  <div class="card full">
    <h2><span>//</span>Gestión del Servicio (systemctl)</h2>
    <div class="ip-input">
      <input type="text" id="service-ip" placeholder="IP de la VM  ej. 192.168.1.50">
      <input type="text" id="service-name" placeholder="Nombre del servicio" value="app-custom.service">
    </div>
    <div class="service-actions">
      <button class="btn btn-green" onclick="serviceAction('start')">▶ Iniciar</button>
      <button class="btn btn-red"   onclick="serviceAction('stop')">■ Detener</button>
      <button class="btn btn-blue"  onclick="serviceAction('restart')">↺ Reiniciar</button>
      <button class="btn btn-gray"  onclick="serviceAction('enable')">✓ Habilitar</button>
      <button class="btn btn-gray"  onclick="serviceAction('disable')">✗ Deshabilitar</button>
      <button class="btn btn-gray"  onclick="serviceAction('status')">? Estado</button>
      <button class="btn btn-gray"  onclick="serviceAction('logs')">≡ Logs</button>
    </div>
    <div class="output" id="service-output">// La salida aparecerá aquí...</div>
  </div>

  <div class="card">
    <h2><span>//</span>Gestor de Balanceadores (HAProxy)</h2>
    <div class="form-group">
      <label>Nombre del Balanceador</label>
      <input type="text" id="lb-name" placeholder="ej. LB-Principal">
    </div>
    <div class="form-group">
      <label>IP del Balanceador</label>
      <input type="text" id="lb-ip" placeholder="ej. 192.168.1.100">
    </div>
    <button class="btn btn-blue" onclick="addLB()">+ Registrar Balanceador</button>
    
    <table style="margin-top:1rem;">
      <thead><tr><th>NOMBRE</th><th>IP</th><th>ACCIONES</th></tr></thead>
      <tbody id="lb-table">
        <tr><td colspan="3" style="color:var(--muted)">Sin balanceadores</td></tr>
      </tbody>
    </table>
  </div>

  <div class="card">
    <h2><span>//</span>Reglas de Nodos (Backends)</h2>
    <div class="form-group">
      <label>1. Seleccionar Balanceador Destino</label>
      <select id="select-lb" onchange="renderAssignedNodes()" style="width:100%;padding:0.4rem;background:var(--bg);color:var(--text);border:1px solid var(--border)">
        <option value="">(Primero registra un LB)</option>
      </select>
    </div>
    <div class="form-group" style="display:flex;gap:0.5rem;align-items:flex-end;">
      <div style="flex:1">
        <label>2. Seleccionar Servidor Activo</label>
        <select id="select-node" style="width:100%;padding:0.4rem;background:var(--bg);color:var(--text);border:1px solid var(--border)">
           <option value="">(Buscando VMs activas...)</option>
        </select>
      </div>
      <button class="btn btn-green" onclick="assignNode()">+ Adjuntar</button>
    </div>

    <table style="margin-top:1rem;">
      <thead><tr><th>SERVIDOR (NODO)</th><th>IP:PUERTO</th><th>ACCIONES</th></tr></thead>
      <tbody id="nodes-table">
        <tr><td colspan="3" style="color:var(--muted)">Sin nodos asignados</td></tr>
      </tbody>
    </table>

    <button class="btn btn-accent" style="width:100%; margin-top:1rem; font-size:1rem; font-weight:bold; padding:0.8rem;" onclick="deployHAProxy()">🚀 Desplegar Configuración a HAProxy</button>
    <div id="haproxy-output" class="output" style="margin-top:1rem;display:none"></div>
  </div>

</div>

<script>
  let ipCheckInterval = null;
  let lbs = [];
  let asignaciones = {};
  window.activeHijas = [];

  function addLB() {
    const name = document.getElementById('lb-name').value.trim();
    const ip = document.getElementById('lb-ip').value.trim();
    if (!name || !ip) return alert("Completa Nombre e IP");
    lbs.push({ name, ip });
    if (!asignaciones[ip]) asignaciones[ip] = [];
    document.getElementById('lb-name').value = '';
    document.getElementById('lb-ip').value = '';
    renderLBs();
    renderLBSelects();
  }

  function deleteLB(ip) {
    lbs = lbs.filter(lb => lb.ip !== ip);
    delete asignaciones[ip];
    renderLBs();
    renderLBSelects();
    renderAssignedNodes();
  }

  function renderLBs() {
    const tbody = document.getElementById('lb-table');
    if (lbs.length === 0) {
      tbody.innerHTML = '<tr><td colspan="3" style="color:var(--muted)">Sin balanceadores</td></tr>';
      return;
    }
    tbody.innerHTML = lbs.map(lb => '<tr><td>' + lb.name + '</td><td>' + lb.ip + '</td><td><button class="btn btn-red" onclick="deleteLB(\'' + lb.ip + '\')">Borrar</button></td></tr>').join('');
  }

  function renderLBSelects() {
    const sel = document.getElementById('select-lb');
    if (lbs.length === 0) {
      sel.innerHTML = '<option value="">(Primero registra un LB)</option>';
      return;
    }
    sel.innerHTML = '<option value="">-- Seleccionar --</option>' + lbs.map(lb => '<option value="' + lb.ip + '">' + lb.name + ' (' + lb.ip + ')</option>').join('');
  }

  function assignNode() {
    const lbIp = document.getElementById('select-lb').value;
    const nodeName = document.getElementById('select-node').value;
    if (!lbIp) return alert("Selecciona un balanceador");
    if (!nodeName) return alert("No hay nodo válido para asignar");
    const nodeData = window.activeHijas.find(h => h.name === nodeName);
    if (!nodeData || !nodeData.ip) return alert("Este nodo aún no tiene IP asignada");
    if (!asignaciones[lbIp].find(n => n.name === nodeName)) {
      asignaciones[lbIp].push({ name: nodeData.name, ip: nodeData.ip, port: nodeData.port || "8081" });
    }
    renderAssignedNodes();
  }

  function removeNode(lbIp, nodeName) {
    asignaciones[lbIp] = asignaciones[lbIp].filter(n => n.name !== nodeName);
    renderAssignedNodes();
  }

  function renderAssignedNodes() {
    const lbIp = document.getElementById('select-lb').value;
    const tbody = document.getElementById('nodes-table');
    if (!lbIp || !asignaciones[lbIp] || asignaciones[lbIp].length === 0) {
      tbody.innerHTML = '<tr><td colspan="3" style="color:var(--muted)">Sin nodos asignados</td></tr>';
      return;
    }
    tbody.innerHTML = asignaciones[lbIp].map(n => '<tr><td>' + n.name + '</td><td>' + n.ip + ':' + n.port + '</td><td><button class="btn btn-red" onclick="removeNode(\'' + lbIp + '\', \'' + n.name + '\')">Quitar</button></td></tr>').join('');
  }

  async function deployHAProxy() {
    const lbIp = document.getElementById('select-lb').value;
    if (!lbIp) return alert("No has seleccionado un balanceador al cual desplegar");
    const out = document.getElementById('haproxy-output');
    out.style.display = 'block';
    out.textContent = 'Desplegando configuración por SSH y reiniciando el servicio...\n(Depende de la red, puede demorar unos segundos)';
    
    const payload = {
      lbIp: lbIp,
      servers: asignaciones[lbIp] || []
    };
    
    try {
      const res = await fetch('/api/haproxy/apply', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      const data = await res.json();
      out.textContent = data.message;
    } catch(e) {
      out.textContent = "Error: " + e.message;
    }
  }

  async function prepararVM(event) {
    event.preventDefault();
    const form = document.getElementById('prepare-vm-form');
    const btn  = document.getElementById('btn-preparar');
    const out  = document.getElementById('prepare-output');

    const formData = new FormData();
    formData.append('execFile',    document.getElementById('exec-file').files[0]);
    const zip = document.getElementById('zip-file').files[0];
    if (zip) formData.append('zipFile', zip);
    formData.append('port',        document.getElementById('vm-port').value);
    formData.append('templateVM',  document.getElementById('template-vm-name').value);
    formData.append('newDiskName', document.getElementById('new-disk-name').value);

    btn.disabled    = true;
    btn.textContent = 'Procesando... (puede tardar ~2 min)';
    out.style.display = 'block';
    out.textContent   = 'Encendiendo plantilla y detectando IP via Guest Additions...\n';

    try {
      const res  = await fetch('/api/vm/prepare', { method: 'POST', body: formData });
      const data = await res.json();
      out.textContent = data.message + (data.output ? '\n\nDetalles:\n' + data.output : '');
      if (res.ok) { form.reset(); loadDisks(); }
    } catch (err) {
      out.textContent = 'Error de conexión: ' + err.message;
    } finally {
      btn.disabled    = false;
      btn.textContent = 'Ejecutar Automatización';
    }
  }

  async function loadDisks() {
    const res   = await fetch('/api/disks');
    const disks = await res.json();
    const tbody = document.getElementById('disks-table');
    if (!disks || disks.length === 0) {
      tbody.innerHTML = '<tr><td colspan="3" style="color:var(--muted)">Sin discos multiconexión</td></tr>';
      return;
    }
    tbody.innerHTML = disks.map(d =>
      '<tr>' +
        '<td>' + d.name + '</td>' +
        '<td style="color:var(--muted);font-size:0.7rem">' + d.location + '</td>' +
        '<td>' +
          '<button class="btn btn-green" onclick="fillUUID(\'' + d.uuid + '\')">Usar este disco</button> ' +
          '<button class="btn btn-red"   onclick="deleteDisk(\'' + d.uuid + '\')">Eliminar</button>' +
        '</td>' +
      '</tr>'
    ).join('');
  }

  async function loadVMs() {
    const res   = await fetch('/api/vms');
    const vms   = await res.json();
    
    // Auto-actualizar IP si alguna máquina está corriendo pero no tiene IP
    const needsPolling = vms.some(v => v.state === 'corriendo' && (!v.ip || v.ip === ''));
    if (needsPolling && !ipCheckInterval) {
      ipCheckInterval = setInterval(loadVMs, 5000);
    } else if (!needsPolling && ipCheckInterval) {
      clearInterval(ipCheckInterval);
      ipCheckInterval = null;
    }

    const hijas = vms.filter(v => v.port && v.port !== '');
    const otras = vms.filter(v => !v.port || v.port === '');

    window.activeHijas = hijas;
    const sel = document.getElementById('select-node');
    if (hijas.length > 0) {
       sel.innerHTML = hijas.map(h => '<option value="' + h.name + '">' + h.name + ' (' + (h.ip?h.ip:'Sin IP') + ':' + h.port + ')</option>').join('');
    } else {
       sel.innerHTML = '<option value="">(Sin VMs disponibles)</option>';
    }

    const tbodyHijas = document.getElementById('vms-hijas-table');
    if (!hijas || hijas.length === 0) {
      tbodyHijas.innerHTML = '<tr><td colspan="4" style="color:var(--muted)">No hay máquinas virtuales hijas registradas</td></tr>';
    } else {
      tbodyHijas.innerHTML = hijas.map(v => {
        const url   = 'http://' + (v.ip || 'localhost') + ':' + v.port;
        const irBtn = (v.ip && v.state === 'corriendo')
          ? '<a class="btn btn-blue" href="' + url + '" target="_blank">↗ Ir</a>'
          : '<span class="btn btn-gray" style="opacity:0.4">↗ Ir</span>';
        
        const ipDisplay = v.ip ? v.ip : '<span style="opacity:0.5">N/A</span>';
        const portDisplay = v.port ? v.port : '<span style="opacity:0.5">N/A</span>';
        const stateColor = v.state === 'corriendo' ? 'var(--accent)' : 'var(--accent2)';
        const stateDisplay = '<br><small style="color:' + stateColor + '">(' + v.state + ')</small>';

        return '<tr>' +
          '<td>' + v.name + stateDisplay + '</td>' +
          '<td class="vm-ip">' + ipDisplay + '</td>' +
          '<td class="vm-port">' + portDisplay + '</td>' +
          '<td style="display:flex;gap:0.4rem;align-items:center;">' +
            '<button class="btn btn-green" onclick="startVM(\'' + v.name + '\')">▶ Iniciar</button>' +
            '<button class="btn btn-red"   onclick="stopVM(\'' + v.name + '\')">■ Apagar</button>' +
            '<button class="btn btn-red"   onclick="deleteVM(\'' + v.name + '\')" style="background:#552222">🗑 Borrar</button>' +
            '<button class="btn btn-gray"  onclick="seleccionarIP(\'' + v.name + '\', \'' + v.ip + '\')">🌐 Gestor</button>' +
            irBtn +
          '</td>' +
        '</tr>';
      }).join('');
    }

    const tbodyOtras = document.getElementById('vms-table');
    if (!otras || otras.length === 0) {
      tbodyOtras.innerHTML = '<tr><td colspan="3" style="color:var(--muted)">Sin otras VMs registradas</td></tr>';
    } else {
      tbodyOtras.innerHTML = otras.map(v => {
        const stateColor = v.state === 'corriendo' ? 'var(--accent)' : 'var(--muted)';
        const stateDisplay = '<br><small style="color:' + stateColor + '">(' + v.state + ')</small>';
        return '<tr>' +
          '<td>' + v.name + stateDisplay + '</td>' +
          '<td style="color:var(--muted);font-size:0.7rem">' + v.uuid + '</td>' +
          '<td style="display:flex;gap:0.4rem;align-items:center;">' +
            '<button class="btn btn-green" onclick="startVM(\'' + v.name + '\')">▶ Iniciar</button>' +
            '<button class="btn btn-red"   onclick="stopVM(\'' + v.name + '\')">■ Apagar</button>' +
            '<button class="btn btn-red"   onclick="deleteVM(\'' + v.name + '\')" style="background:#552222">🗑 Borrar</button>' +
            '<button class="btn btn-blue"  onclick="seleccionarIP(\'' + v.name + '\', \'' + v.ip + '\')">🌐 Gestor</button>' +
          '</td>' +
        '</tr>';
      }).join('');
    }
  }

  function fillUUID(uuid) { document.getElementById('disk-uuid').value = uuid; }

  function seleccionarIP(nombre, ip) {
    const input = document.getElementById('service-ip');
    input.value = ip || '';
    if (!ip) input.placeholder = 'IP de ' + nombre + ' — escríbela manualmente';
    input.focus();
    input.scrollIntoView({ behavior: 'smooth', block: 'center' });
  }

  async function createVM() {
    const vmName   = document.getElementById('new-vm-name').value.trim();
    const diskUUID = document.getElementById('disk-uuid').value.trim();
    const port     = "8081"; // Puerto estandarizado para VMs hijas
    const out      = document.getElementById('create-output');

    if (!vmName || !diskUUID) {
      alert('Completa el nombre de la VM y el UUID del disco');
      return;
    }

    out.style.display = 'block';
    out.textContent   = 'Creando VM y esperando IP via Guest Additions...\n(hasta 90 segundos)';

    const res  = await fetch('/api/vm/create', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ vmName, diskUUID, port })
    });
    const data = await res.json();
    out.textContent = data.message || JSON.stringify(data);

    if (res.ok) {
      loadVMs();
    }
  }

  async function stopVM(name) {
    const res  = await fetch('/api/vm/stop', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ vmName: name })
    });
    const data = await res.json();
    alert(data.message);
    loadVMs();
  }

  async function deleteVM(name) {
    if (!confirm('¿Estás SEGURO de que deseas eliminar permanentemente la máquina "' + name + '" y destruir todos sus archivos del disco duro?\n\nEsta acción es irreversible.')) {
      return;
    }
    const res  = await fetch('/api/vm/delete', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ vmName: name })
    });
    const data = await res.json();
    alert(data.message);
    if (res.ok) {
      loadVMs(); // Gracias a la configuración reactiva de loadVMs obtenemos el nuevo estado
    }
  }

  async function startVM(name) {
    const res  = await fetch('/api/vm/start', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ vmName: name })
    });
    const data = await res.json();
    alert(data.message);
    loadVMs();
  }

  async function deleteDisk(uuid) {
    if (!confirm('¿Eliminar este disco permanentemente?')) return;
    const res  = await fetch('/api/disk/delete', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ diskUUID: uuid })
    });
    const data = await res.json();
    alert(data.message);
    loadDisks();
  }

  async function serviceAction(action) {
    const ip      = document.getElementById('service-ip').value.trim();
    const service = document.getElementById('service-name').value.trim();
    const out     = document.getElementById('service-output');
    if (!ip) { out.textContent = '⚠ Ingresa la IP de la VM primero.'; return; }
    out.textContent = 'Ejecutando ' + action + ' en ' + ip + '...';
    const res  = await fetch('/api/service', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ip, action, service })
    });
    const data = await res.json();
    out.textContent = data.output || data.message || JSON.stringify(data);
  }

  loadDisks();
  loadVMs();
</script>
</body>
</html>`
