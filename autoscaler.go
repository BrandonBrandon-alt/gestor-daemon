// Package main implements auto-scaling orchestration for dynamic infrastructure.
// This file manages automatic VM creation and destruction based on CPU load thresholds
// and integrates with HAProxy load balancer for backend server management.
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

// AutoScalingConfig defines the policy parameters for automatic scaling decisions.
// These settings control when and how the infrastructure scales up or down.
type AutoScalingConfig struct {
// Enabled toggles auto-scaling on/off without losing configuration
Enabled        bool   `json:"enabled"`
// UpperThreshold is the CPU percentage that triggers scale-out (scale up)
UpperThreshold int    `json:"upperThreshold"`
// UpperTime is duration in seconds CPU must stay above threshold before scaling out
UpperTime      int    `json:"upperTime"`
// LowerThreshold is the CPU percentage that triggers scale-in (scale down)
LowerThreshold int    `json:"lowerThreshold"`
// LowerTime is duration in seconds CPU must stay below threshold before scaling in
LowerTime      int    `json:"lowerTime"`
// LBIp is the IP address of the HAProxy load balancer managing traffic distribution
LBIp           string `json:"lbIp"`
// DiskUUID is the UUID of the template disk to clone for new instances
DiskUUID       string `json:"diskUuid"`
// NetworkAdapter specifies the bridge adapter for new VM network connectivity
NetworkAdapter string `json:"networkAdapter"`
// AppPort is the port number where application instances listen for requests
AppPort        string `json:"appPort"`
// MaxNodes is the maximum number of instances allowed (scaling out upper limit)
MaxNodes       int    `json:"maxNodes"`
// MinNodes is the minimum number of instances that must remain active
MinNodes       int    `json:"minNodes"`
}

// AutoScalingState represents the current operational state of auto-scaling.
// Updated continuously by the monitoring goroutine, accessed by API handlers.
type AutoScalingState struct {
// CpuAvg is the averaged CPU percentage across all monitored nodes
CpuAvg         float64           `json:"cpuAvg"`
// ActiveNodes is the current count of running application instances
ActiveNodes    int               `json:"activeNodes"`
// TimeAboveUpper tracks seconds CPU has exceeded the upper threshold
TimeAboveUpper int               `json:"timeAboveUpper"`
// TimeBelowLower tracks seconds CPU has been below the lower threshold
TimeBelowLower int               `json:"timeBelowLower"`
// Status is a human-readable string describing current state
Status         string            `json:"status"`
// EventLogs contains timestamped messages of scaling operations (last 20 entries)
EventLogs      []string          `json:"eventLogs"`
// Config is the current auto-scaling configuration being used
Config         AutoScalingConfig `json:"config"`
// IsScaling indicates whether a scaling operation is currently in progress
IsScaling      bool              `json:"isScaling"`
}

// Synchronization and state storage for auto-scaling subsystem.
var (
// asMu protects concurrent access to asState fields
asMu sync.Mutex
// asState holds the current auto-scaling state and configuration
asState AutoScalingState
// asConfigFile is the path to the persistent auto-scaling configuration
asConfigFile = "autoscaling_config.json"
)

// init initializes the auto-scaling subsystem with default configuration.
// Loads saved configuration from asConfigFile if it exists.
// Called once at program startup.
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

// logEvent adds a timestamped message to the event log.
// Maintains a sliding window of the last 20 events.
// Thread-safe: acquires lock for the entire operation.
// msg: the event message to log
func logEvent(msg string) {
asMu.Lock()
defer asMu.Unlock()
entry := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), msg)
asState.EventLogs = append(asState.EventLogs, entry)
if len(asState.EventLogs) > 20 { // Keep only 20 recent logs
asState.EventLogs = asState.EventLogs[1:]
}
}

// getHAProxyStateUnsafe reads HAProxy state from persistent JSON file.
// This function is "Unsafe" because it doesn't use synchronization (caller's responsibility).
// Returns the load balancer configuration and node assignments per LB.
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

// saveHAProxyStateUnsafe persists HAProxy state to file.
// Updates the node assignments while preserving other state fields.
// Called after scaling operations to maintain load balancer configuration.
// asignaciones: map of LB IP to list of backend node definitions
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

// StartAutoscaler is the main auto-scaling monitoring and control loop.
// Runs continuously on a 5-second polling interval, monitoring CPU and triggering scaling.
// Should be started as a goroutine from main().
// The loop:
// 1. Checks if auto-scaling is enabled and configuration is complete
// 2. Retrieves current node assignments from HAProxy state
// 3. Collects CPU metrics from all nodes concurrently via SSH
// 4. Calculates average CPU across nodes
// 5. Tracks duration above/below thresholds
// 6. Triggers scale-out or scale-in if conditions are met and max/min limits allow
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

// Collect CPU from all nodes in parallel
for _, n := range nodes {
ip := n["ip"]
name := n["name"]
if ip == "" {
continue
}
wg.Add(1)
go func(nodeIP, nodeName string) {
defer wg.Done()
// vmstat 1 2: two samples at 1-second interval; idle% is column 15
// CPU = 100 - idle
cmd := `vmstat 1 2 | tail -1 | awk '{print 100 - $15}'`
out, err := runSSH(nodeIP, cmd)
if err == nil {
val, errParse := strconv.ParseFloat(strings.TrimSpace(out), 64)
if errParse == nil {
mu.Lock()
totalCpu += val
mu.Unlock()
}
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

// Track threshold crossing duration
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

// Execute scale-out if triggered and not already scaling
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
asState.TimeAboveUpper = 0 // Reset counter
asMu.Unlock()

go func() {
performScaleOut(cfg)
asMu.Lock()
asState.IsScaling = false
asMu.Unlock()
}()
}
}

// Execute scale-in if triggered and not already scaling
if triggerScaleIn && !isScaling {
if len(nodes) <= cfg.MinNodes {
// Silent: avoid log saturation
} else {
logEvent(fmt.Sprintf("Iniciando Scale IN (Hacia Abajo). CPU prom: %.2f%%", avgCpu))

asMu.Lock()
asState.IsScaling = true
asState.TimeBelowLower = 0 // Reset counter
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

// performScaleOut creates a new instance and registers it with the load balancer.
// Validates configuration completeness, creates VM, waits for IP, and updates HAProxy.
// cfg: the auto-scaling configuration defining disk and network parameters
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

// Store port for dashboard display
runVBoxQuiet("guestproperty", "set", nodeName, "/Gestor/Port", cfg.AppPort)

ip, err := getVMIP(nodeName)
if err != nil {
logEvent("No obtuvo IP (Timeout). Abortando incorporación a HAProxy.")
return
}

logEvent(fmt.Sprintf("VM %s levantada con IP: %s. Anexando al Balanceador...", nodeName, ip))

// Add to HAProxy backend
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
go displayVMStatusTable()
}

// performScaleIn removes the most recently auto-created instance and updates the load balancer.
// Prefers terminating "Node-Auto-*" instances to preserve manually created instances.
// cfg: the auto-scaling configuration
// nodes: current list of active nodes
func performScaleIn(cfg AutoScalingConfig, nodes []map[string]string) {
// Find the last auto-created node (preference for removal)
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

// Remove from load balancer first (drain connections)
newNodes := append(nodes[:targetIdx], nodes[targetIdx+1:]...)

_, asign := getHAProxyStateUnsafe()
asign[cfg.LBIp] = newNodes
saveHAProxyStateUnsafe(asign)

applyHAProxyLocal(cfg.LBIp, newNodes)
logEvent("El nodo " + nodeName + " ha sido desvinculado de HAProxy.")

// Then destroy the VM
runVBoxQuiet("controlvm", nodeName, "poweroff")
time.Sleep(3 * time.Second)
runVBoxQuiet("unregistervm", nodeName, "--delete-all")

logEvent("Scale IN finalizado con éxito. Nodo destruido.")
go displayVMStatusTable()
}

// applyHAProxyLocal generates and deploys HAProxy configuration to a load balancer instance.
// Creates the complete haproxy.cfg with frontend, backend, and server definitions.
// lbIp: IP of the HAProxy load balancer
// nodes: list of backend server nodes to include in the configuration
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

// handleAutoScalingConfig processes GET and POST HTTP requests for auto-scaling configuration.
// GET returns current configuration, POST updates and persists new configuration.
// On enable/disable transitions, logs the state change.
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

// handleAutoScalingStatus processes HTTP GET requests to retrieve current auto-scaling state.
// Returns JSON representation of the AutoScalingState including current metrics and logs.
func handleAutoScalingStatus(w http.ResponseWriter, r *http.Request) {
asMu.Lock()
defer asMu.Unlock()
w.Header().Set("Content-Type", "application/json")
json.NewEncoder(w).Encode(asState)
}
