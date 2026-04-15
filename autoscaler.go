package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type AutoScalingConfig struct {
	Enabled        bool   `json:"enabled"`
	UpperThreshold int    `json:"upperThreshold"`
	UpperTime      int    `json:"upperTime"` // en segundos
	LowerThreshold int    `json:"lowerThreshold"`
	LowerTime      int    `json:"lowerTime"` // en segundos
	LBIp           string `json:"lbIp"`
	DiskUUID       string `json:"diskUuid"`
	NetworkAdapter string `json:"networkAdapter"`
	AppPort        string `json:"appPort"`
	MaxNodes       int    `json:"maxNodes"`
	MinNodes       int    `json:"minNodes"`
}

type AutoScalingState struct {
	CpuAvg         float64           `json:"cpuAvg"`
	ActiveNodes    int               `json:"activeNodes"`
	TimeAboveUpper int               `json:"timeAboveUpper"`
	TimeBelowLower int               `json:"timeBelowLower"`
	Status         string            `json:"status"`
	EventLogs      []string          `json:"eventLogs"`
	Config         AutoScalingConfig `json:"config"`
	IsScaling      bool              `json:"isScaling"`
}

var asMu sync.Mutex
var asState AutoScalingState
var asConfigFile = "autoscaling_config.json"

func init() {
	asState.Config = AutoScalingConfig{
		UpperThreshold: 80,
		UpperTime:      60,
		LowerThreshold: 20,
		LowerTime:      60,
		MaxNodes:       5,
		MinNodes:       1,
	}
	asState.EventLogs = make([]string, 0)
	
	data, err := os.ReadFile(asConfigFile)
	if err == nil {
		json.Unmarshal(data, &asState.Config)
	}
}

func logEvent(msg string) {
	asMu.Lock()
	defer asMu.Unlock()
	entry := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), msg)
	asState.EventLogs = append(asState.EventLogs, entry)
	if len(asState.EventLogs) > 20 { // Mantener solo 20 logs recientes
		asState.EventLogs = asState.EventLogs[1:]
	}
	fmt.Println("AutoScaler:", msg)
}

func getHAProxyStateUnsafe() (map[string]interface{}, map[string][]map[string]string) {
	stateFile := "haproxy_state.json"
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return nil, nil
	}

	var haState struct {
		LBS          []map[string]interface{}         `json:"lbs"`
		Asignaciones map[string][]map[string]string `json:"asignaciones"`
	}
	json.Unmarshal(data, &haState)
	return nil, haState.Asignaciones
}

func saveHAProxyStateUnsafe(asignaciones map[string][]map[string]string) {
	stateFile := "haproxy_state.json"
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return
	}
	var haState struct {
		LBS          []map[string]interface{}         `json:"lbs"`
		Asignaciones map[string][]map[string]string `json:"asignaciones"`
	}
	json.Unmarshal(data, &haState)
	haState.Asignaciones = asignaciones
	newData, _ := json.Marshal(haState)
	os.WriteFile(stateFile, newData, 0644)
}


func StartAutoscaler() {
	ticker := time.NewTicker(5 * time.Second)
	for range ticker.C {
		asMu.Lock()
		cfg := asState.Config
		enabled := cfg.Enabled
		isScaling := asState.IsScaling
		asMu.Unlock()

		if !enabled || cfg.LBIp == "" {
			asMu.Lock()
			asState.CpuAvg = 0
			asState.Status = "Inactivo / Configuración Incompleta"
			asState.TimeAboveUpper = 0
			asState.TimeBelowLower = 0
			asMu.Unlock()
			continue
		}

		_, asign := getHAProxyStateUnsafe()
		nodes := asign[cfg.LBIp]

		if len(nodes) == 0 {
			asMu.Lock()
			asState.CpuAvg = 0
			asState.ActiveNodes = 0
			asState.Status = "Sin nodos para monitorear en LB"
			asMu.Unlock()
			continue
		}

		var totalCpu float64
		var wg sync.WaitGroup
		var mu sync.Mutex

		for _, n := range nodes {
			ip := n["ip"]
			name := n["name"]
			if ip == "" {
				continue
			}
			wg.Add(1)
			go func(nodeIP, nodeName string) {
				defer wg.Done()
				// Usamos vmstat 1 2: toma dos muestras en 1 seg. El último valor de la columna 15 es 'idle'.
				// CPU = 100 - idle.
				cmd := `vmstat 1 2 | tail -1 | awk '{print 100 - $15}'`
				out, err := runSSH(nodeIP, cmd)
				if err == nil {
					val, errParse := strconv.ParseFloat(strings.TrimSpace(out), 64)
					if errParse == nil {
						fmt.Printf("[AutoScaler] Nodo %s (%s) -> CPU: %.1f%%\n", nodeName, nodeIP, val)
						mu.Lock()
						totalCpu += val
						mu.Unlock()
					} else {
						fmt.Printf("[AutoScaler] Error parseando CPU de %s: %v (Out: %s)\n", nodeIP, errParse, out)
					}
				} else {
					fmt.Printf("[AutoScaler] Error SSH en nodo %s: %v\n", nodeIP, err)
				}
			}(ip, name)
		}
		wg.Wait()

		avgCpu := 0.0
		if len(nodes) > 0 {
			avgCpu = totalCpu / float64(len(nodes))
		}

		asMu.Lock()
		asState.CpuAvg = avgCpu
		asState.ActiveNodes = len(nodes)

		if avgCpu >= float64(cfg.UpperThreshold) {
			asState.TimeAboveUpper += 5
			asState.TimeBelowLower = 0
			asState.Status = fmt.Sprintf("CPU Crítica: %d s / %d s", asState.TimeAboveUpper, cfg.UpperTime)
		} else if avgCpu <= float64(cfg.LowerThreshold) {
			asState.TimeBelowLower += 5
			asState.TimeAboveUpper = 0
			asState.Status = fmt.Sprintf("CPU Baja: %d s / %d s", asState.TimeBelowLower, cfg.LowerTime)
		} else {
			asState.TimeAboveUpper = 0
			asState.TimeBelowLower = 0
			asState.Status = "Consumo Estable"
		}

        if isScaling {
            asState.Status = "Operación de escalado en progreso..."
        }

		triggerScaleOut := asState.TimeAboveUpper >= cfg.UpperTime
		triggerScaleIn := asState.TimeBelowLower >= cfg.LowerTime
		asMu.Unlock()

		if triggerScaleOut && !isScaling {
			if len(nodes) >= cfg.MaxNodes {
				logEvent("Alerta: Máximo de nodos alcanzado, no se escala hacia arriba.")
				asMu.Lock()
				asState.TimeAboveUpper = 0
				asMu.Unlock()
			} else {
				logEvent(fmt.Sprintf("Iniciando Scale OUT (Hacia Arriba). CPU prom: %.2f%%", avgCpu))
				
				asMu.Lock()
				asState.IsScaling = true
				asState.TimeAboveUpper = 0 // Reset
				asMu.Unlock()
				
				go func() {
				    performScaleOut(cfg)
				    asMu.Lock()
    				asState.IsScaling = false
    				asMu.Unlock()
				}()
			}
		}

		if triggerScaleIn && !isScaling {
			if len(nodes) <= cfg.MinNodes {
				// Silencioso. No queremos saturar logs.
			} else {
				logEvent(fmt.Sprintf("Iniciando Scale IN (Hacia Abajo). CPU prom: %.2f%%", avgCpu))
				
				asMu.Lock()
				asState.IsScaling = true
				asState.TimeBelowLower = 0 // Reset
				asMu.Unlock()
				
				go func() {
				    performScaleIn(cfg, nodes)
				    asMu.Lock()
    				asState.IsScaling = false
    				asMu.Unlock()
				}()
			}
		}
	}
}

func performScaleOut(cfg AutoScalingConfig) {
	if cfg.DiskUUID == "" || cfg.NetworkAdapter == "" || cfg.AppPort == "" {
		logEvent("Error: Falta configuración (UUID, Red o Puerto). Abortando.")
		return
	}

	nodeName := fmt.Sprintf("Node-Auto-%d", time.Now().Unix())
	logEvent("Instanciando VM automáticamente: " + nodeName)

	_, err := runVBoxQuiet("createvm", "--name", nodeName, "--ostype", "Debian_64", "--register")
	if err != nil {
		logEvent("Fallo creación: " + err.Error())
		return
	}

	runVBoxQuiet("modifyvm", nodeName, "--memory", "512", "--nic1", "bridged", "--bridgeadapter1", cfg.NetworkAdapter)
	runVBoxQuiet("storagectl", nodeName, "--name", "SATA", "--add", "sata")
	runVBoxQuiet("storageattach", nodeName, "--storagectl", "SATA", "--port", "0", "--device", "0", "--type", "hdd", "--medium", cfg.DiskUUID, "--mtype", "multiattach")
	runVBoxQuiet("startvm", nodeName, "--type", "headless")
	
	// Set port internally for dashboard
	runVBoxQuiet("guestproperty", "set", nodeName, "/Gestor/Port", cfg.AppPort)

	ip, err := getVMIP(nodeName)
	if err != nil {
		logEvent("No obtuvo IP (Timeout). Abortando incorporación a HAProxy.")
		return
	}
	
	logEvent(fmt.Sprintf("VM %s levantada con IP: %s. Anexando al Balanceador...", nodeName, ip))

	// Anexar a HAProxy
	_, asign := getHAProxyStateUnsafe()
	if asign[cfg.LBIp] == nil {
		asign[cfg.LBIp] = []map[string]string{}
	}

	newNode := map[string]string{
		"name":       nodeName,
		"ip":         ip,
		"port":       cfg.AppPort,
		"lastStatus": "UP",
	}
	asign[cfg.LBIp] = append(asign[cfg.LBIp], newNode)
	saveHAProxyStateUnsafe(asign)

	applyHAProxyLocal(cfg.LBIp, asign[cfg.LBIp])

	logEvent("Scale OUT finalizado con éxito. Nuevo nodo en servicio.")
	go displayVMStatusTable() // Refrescar tabla en consola
}

func performScaleIn(cfg AutoScalingConfig, nodes []map[string]string) {
	// Buscar un nodo para apagar. Obligatoriamente uno que empiece con "Node-Auto-"
	var targetIdx = -1
	for i := len(nodes) - 1; i >= 0; i-- {
		if strings.HasPrefix(nodes[i]["name"], "Node-Auto-") {
			targetIdx = i
			break
		}
	}

	if targetIdx == -1 {
		logEvent("Abortado: No hay nodos auto-creados (Node-Auto-) para destruir.")
		return
	}

	targetNode := nodes[targetIdx]
	nodeName := targetNode["name"]
	logEvent("Iniciando destrucción segura de VM: " + nodeName)

	// Quitar del balanceador
	newNodes := append(nodes[:targetIdx], nodes[targetIdx+1:]...)

	_, asign := getHAProxyStateUnsafe()
	asign[cfg.LBIp] = newNodes
	saveHAProxyStateUnsafe(asign)
	
	applyHAProxyLocal(cfg.LBIp, newNodes)
	logEvent("El nodo " + nodeName + " ha sido desvinculado de HAProxy.")

    // Proceder a destruir
	runVBoxQuiet("controlvm", nodeName, "poweroff")
	time.Sleep(3 * time.Second)
	runVBoxQuiet("unregistervm", nodeName, "--delete-all")
	
	logEvent("Scale IN finalizado con éxito. Nodo destruido.")
	go displayVMStatusTable()
}

func applyHAProxyLocal(lbIp string, nodes []map[string]string) {
	var cfg strings.Builder
	cfg.WriteString("global\n    log /dev/log local0\n    log /dev/log local1 notice\n    chroot /var/lib/haproxy\n    stats socket /run/haproxy/admin.sock mode 660 level admin expose-fd listeners\n    stats timeout 30s\n    user haproxy\n    group haproxy\n    daemon\n\n")
	cfg.WriteString("defaults\n    log global\n    mode http\n    option httplog\n    option dontlognull\n    timeout connect 5000\n    timeout client  50000\n    timeout server  50000\n\n")
	cfg.WriteString("listen stats\n    bind *:8404\n    stats enable\n    stats uri /\n    stats refresh 5s\n\n")
	cfg.WriteString("frontend http_front\n    bind *:80\n    default_backend http_back\n\n")
	cfg.WriteString("backend http_back\n    balance roundrobin\n")

	for _, s := range nodes {
		if s["ip"] != "" && s["port"] != "" {
			cfg.WriteString(fmt.Sprintf("    server %s %s:%s check\n", s["name"], s["ip"], s["port"]))
		}
	}

	tempRemotePath := "/etc/haproxy/haproxy.cfg"
	createRemoteFile(lbIp, tempRemotePath, cfg.String())
	runSSH(lbIp, "sudo systemctl restart haproxy")
}

// ─── HTTP API Handlers ─────────────────────────────────────────────────────────

func handleAutoScalingConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		asMu.Lock()
		cfg := asState.Config
		asMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfg)
		return
	}
	if r.Method == "POST" {
		var newCfg AutoScalingConfig
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		asMu.Lock()
		wasEnabled := asState.Config.Enabled
		asState.Config = newCfg
		asState.TimeAboveUpper = 0
		asState.TimeBelowLower = 0
		asMu.Unlock()

		data, _ := json.MarshalIndent(newCfg, "", "  ")
		os.WriteFile(asConfigFile, data, 0644)
		
		if !wasEnabled && newCfg.Enabled {
		    logEvent("Elasticidad Automática ACTIVADA.")
		} else if wasEnabled && !newCfg.Enabled {
		    logEvent("Elasticidad Automática DESACTIVADA.")
		} else {
		    logEvent("Configuración de Elasticidad modificada.")
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Response{Success: true, Message: "Configuración de AutoScaling guardada"})
	}
}

func handleAutoScalingStatus(w http.ResponseWriter, r *http.Request) {
	asMu.Lock()
	defer asMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(asState)
}
