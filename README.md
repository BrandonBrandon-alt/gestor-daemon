# Gestor de Demonios Infraestructura - Orquestador de VMs con Go y VirtualBox

Orquestador avanzado de máquinas virtuales diseñado para despliegue automático de entornos web aislados. Utiliza Go para gestionar el ciclo de vida completo de máquinas virtuales, configurar redes estáticas, automatizar registros DNS dinámicos y proporcionar autoscaling inteligente.

## Tabla de Contenidos

- [Características](#características)
- [Arquitectura](#arquitectura)
- [Requisitos Previos](#requisitos-previos)
- [Instalación](#instalación)
- [Configuración](#configuración)
- [Uso](#uso)
- [Endpoints API](#endpoints-api)
- [Consideraciones Importantes](#consideraciones-importantes)
- [Solución de Problemas](#solución-de-problemas)
- [Estructura del Código](#estructura-del-código)

## Características

- **Clonación de Discos Independientes**: Crea copias rápidas e independientes del disco de la plantilla base usando VirtualBox
- **Gestión Automática de Red**: Configura direcciones IP estáticas y redes aisladas automáticamente
- **Registro DNS Dinámico**: Integración con BIND9 para registro automático de dominios `*.cloud.local`
- **Aprovisionamiento Automático**: Carga archivos ZIP y configura aplicaciones vía SSH
- **Interfaz Web Moderna**: Panel de control dark mode con monitoreo de instancias en tiempo real
- **Autoscaling Inteligente**: Escalado automático basado en umbrales de CPU y tiempo
- **Detección Multiplataforma**: Soporte automático para Windows y Linux
- **Ejecución Remota**: Capacidad de ejecutar contra host VirtualBox remoto vía SSH

## Arquitectura

### Componentes de Infraestructura

```
┌─────────────────────────────────────────┐
│         HOST FÍSICO / VAGRANT           │
├─────────────────────────────────────────┤
│                                         │
│  ┌──────────────────┐                  │
│  │  NS (Servidor    │  IP: 192.168.10.10
│  │  DNS - BIND9)    │  Puerto: 53
│  │  • Gestiona      │                  │
│  │    cloud.local   │                  │
│  └──────────────────┘                  │
│                                         │
│  ┌──────────────────┐                  │
│  │  PLANTILLA       │  IP: 192.168.10.30
│  │  (Debian + APx)  │  Disco Base VDI  │
│  │  • Base clones   │                  │
│  │  • Apache2       │                  │
│  └──────────────────┘                  │
│                                         │
│  ┌──────────────────┐                  │
│  │  GESTION         │  IP: 192.168.10.50
│  │  (Go Daemon)     │  Puerto: 8090    │
│  │  • Main.go       │  Ejecuta aquí →  │
│  │  • API REST      │                  │
│  │  • UI Web        │                  │
│  └──────────────────┘                  │
│                                         │
│  ┌──────────────────┐                  │
│  │  CLONES          │  IPs: 192.168.10.100-200
│  │  (Dinámicos)     │  Creadas on-demand
│  │  • web-{nombre}  │  SSH: 2300+IP_offset
│  │  • Aislados      │                  │
│  └──────────────────┘                  │
│                                         │
└─────────────────────────────────────────┘
         Red Host-Only: vboxnet0
        (192.168.10.0/24)
```

### Flujo de Provisión

```
1. Usuario solicita crear instancia
   ↓
2. Asignar IP libre (192.168.10.100-200)
   ↓
3. Clonar disco de plantilla
   ↓
4. Crear VM en VirtualBox
   ↓
5. Configurar NIC1 (NAT) + NIC2 (Host-Only)
   ↓
6. Iniciar VM
   ↓
7. Esperar IP vía Guest Additions (hasta 90s)
   ↓
8. Conectar vía SSH
   ↓
9. Configurar red estática
   ↓
10. Registrar DNS dinámico (BIND9)
   ↓
11. Subir archivo ZIP y descomprimir
   ↓
12. Crear servicio systemd (opcional)
   ↓
13. Instancia lista ✓
```

## Requisitos Previos

### Software Requerido

| Componente | Versión | Notas |
|-----------|---------|-------|
| **VirtualBox** | 7.x | Obligatorio. Guest Additions en plantilla |
| **Vagrant** | 2.x+ | Para provisionar infraestructura |
| **Go** | 1.22+ | Para compilar/ejecutar el daemon |
| **nsupdate** | (BIND9) | Herramienta para actualizar DNS dinámico |
| **SSH Client** | Cualquiera | Para conectar a instancias |

### Hardware Recomendado

- **CPU**: 4+ núcleos (para múltiples VMs simultáneamente)
- **RAM**: 8GB mínimo (16GB+ si planeas muchas instancias)
- **Almacenamiento**: 50GB+ disponible (cada clone ~1-2GB)

### Red

- **Red Host-Only "vboxnet0"**: `192.168.10.0/24` con gateway `192.168.10.1`
- **Rango de IPs de clones**: `192.168.10.100` a `192.168.10.200` (101 instancias máximo)
- **IPs reservadas**: 
  - `192.168.10.10` - Servidor DNS (BIND9)
  - `192.168.10.30` - Plantilla base
  - `192.168.10.50` - Servidor de gestión (Go daemon)

### Dependencias de Go

```
github.com/bramvdbogaerde/go-scp v1.6.0  (SCP para transferencia de archivos)
golang.org/x/crypto                      (SSH client)
golang.org/x/sys                         (Utilidades del sistema)
```

## Instalación

### Paso 1: Clonar el Repositorio

```bash
git clone https://github.com/BrandonBrandon-alt/gestor-daemon.git
cd gestor-daemon
```

### Paso 2: Provisionar la Infraestructura Base

```bash
# Esto crea 3 VMs: ns (DNS), plantilla (base), gestion (Go)
vagrant up

# Verificar que las VMs están corriendo
vagrant status
```

**Esperar 3-5 minutos** hasta que Vagrant complete la provisionamiento de todas las VMs.

Verificar conectividad:
```bash
# Probar DNS
nslookup ns.cloud.local 192.168.10.10
nslookup plantilla.cloud.local 192.168.10.10

# Probar SSH a VM de gestión
ssh -i ~/.vagrant.d/insecure_private_key vagrant@192.168.10.50
```

### Paso 3: Configurar Resolución de Dominio Local (IMPORTANTE)

El daemon ahora soporta dos modos de resolución automática:

#### Opción A: Usando /etc/hosts Dinámico (⭐ Recomendado - Sin dependencias externas)

El daemon puede actualizar automáticamente tu `/etc/hosts` local. Esto funciona incluso cuando cambias de red sin necesidad de un servidor DNS externo.

```bash
# Ejecutar el script de setup (one-time)
sudo ./setup-hosts.sh

# Eso es todo. El daemon ahora actualizará /etc/hosts automáticamente.
```

Cómo funciona:
- Al desplegar un sitio → se agrega `192.168.x.x    nombre.cloud.local` a `/etc/hosts`
- Al cambiar de red → la resolución sigue funcionando localmente
- Al eliminar un sitio → se remueve la línea de `/etc/hosts`

#### Opción B: Usando Servidor DNS Externo (BIND9)

#### Para Linux (systemd-resolved)

```bash
# Crear directorio de configuración
sudo mkdir -p /etc/systemd/resolved.conf.d

# Configurar cloud.local para usar el DNS interno
sudo tee /etc/systemd/resolved.conf.d/cloud-local.conf > /dev/null <<EOF
[Resolve]
DNS=192.168.10.10
Domains=~cloud.local
EOF

# Reiniciar systemd-resolved
sudo systemctl restart systemd-resolved

# Verificar
resolvectl query ns.cloud.local
```

#### Para Linux (sin systemd)

```bash
# Agregar al /etc/resolv.conf (temporal)
sudo sh -c 'echo "nameserver 192.168.10.10" >> /etc/resolv.conf'

# O si uses dnsmasq/NetworkManager:
sudo tee /etc/NetworkManager/dnsmasq.d/cloud-local.conf > /dev/null <<EOF
server=/cloud.local/192.168.10.10
EOF
sudo systemctl restart NetworkManager
```

**Nota**: La Opción A (/etc/hosts dinámico) no requiere esta configuración y funciona sin cambios de DNS.

#### Para Windows

1. Abrir **Centro de redes y recursos compartidos**
2. Seleccionar **"VirtualBox Host-Only Network"** → Propiedades
3. Seleccionar **IPv4** → Propiedades
4. Usar DNS personalizado: `192.168.10.10`
5. Aceptar y reiniciar

**Nota**: Si usas Opción A (/etc/hosts dinámico), edita `C:\Windows\System32\drivers\etc\hosts` manualmente O el daemon lo puede hacer si ejecutas con permisos de administrador.

#### Para macOS

```bash
sudo mkdir -p /etc/resolver
sudo tee /etc/resolver/cloud.local > /dev/null <<EOF
nameserver 192.168.10.10
EOF

# Vaciar caché DNS
sudo dscacheutil -flushcache
```

### Paso 4: Descargar Dependencias de Go

```bash
# Dentro de la VM de gestión O en tu máquina (si ejecutas localmente)
go mod download
go mod verify
```

### Paso 5: Ejecutar el Daemon

#### Opción A: Dentro de la VM de gestión (Recomendado)

```bash
# SSH a la VM de gestión
ssh -i ~/.vagrant.d/insecure_private_key vagrant@192.168.10.50

# Dentro de la VM, navegar a la carpeta compartida (si existe)
# O copiar el proyecto allá via SCP

# Ejecutar el daemon
go run .

# O compilar y ejecutar:
go build -o gestor-daemon
./gestor-daemon
```

#### Opción B: Desde el Host (Local)

```bash
# En tu máquina host (Linux/macOS)
go run .

# El daemon buscará VBoxManage localmente
# Si está en Windows, se detectará automáticamente
```

## Configuración

### Variables de Entorno

```bash
# SSH y llaves
export SSH_USER="vagrant"                          # Usuario SSH (default: vagrant)
export SSH_KEY_PATH="/home/user/.ssh/id_rsa"       # Ruta a llave privada
export VBOX_MANAGE="/usr/bin/VBoxManage"           # Ruta a VBoxManage

# VirtualBox remoto (ejecutar contra host remoto)
export VBOX_HOST_IP="192.168.1.100"                # IP del host con VirtualBox
export VBOX_HOST_USER="usuario_vbox"               # Usuario en host remoto

# Red
export BRIDGE_ADAPTER="eth0"                       # Adaptador para bridge (default: eth0)
```

### Configuración de Autoscaling

Editar `autoscaling_config.json`:

```json
{
  "enabled": true,
  "upperThreshold": 80,       # CPU % para escalar UP
  "upperTime": 60,            # Segundos sobre threshold para disparar
  "lowerThreshold": 20,       # CPU % para escalar DOWN
  "lowerTime": 60,            # Segundos bajo threshold para disparar
  "maxNodes": 5,              # Máximo de instancias
  "minNodes": 1,              # Mínimo de instancias
  "lbIp": "192.168.10.100",   # IP del Load Balancer
  "diskUuid": "...",          # UUID del disco a clonar
  "networkAdapter": "vboxnet0",
  "appPort": "8080"
}
```

## Uso

### Interfaz Web

Acceder a `http://localhost:8090` (o `http://gestion.cloud.local:8090` desde host configurado)

**Funcionalidades:**
- Listar todas las instancias con estado
- Crear nuevas instancias (cargar ZIP)
- Eliminar instancias
- Monitoreo de autoscaling
- Logs de eventos

### CLI / Línea de Comandos

Al ejecutar `go run .`, el daemon muestra tabla de estado:

```
=== ESTADO DEL ENTORNO DE MÁQUINAS VIRTUALES ===
NOMBRE                    ESTADO      IP              PUERTO
plantilla_http_base       corriendo   192.168.10.30   N/A
web-miapp                 corriendo   192.168.10.100  2300
web-otraapp               corriendo   192.168.10.105  2305
web-demo                  detenida    N/A             N/A
====================================================
```

### Crear una Instancia (Via UI o curl)

```bash
# Crear archivo ZIP con aplicación
mkdir -p app && echo "<h1>Mi App</h1>" > app/index.html
zip -r app.zip app/

# Subir via curl
curl -X POST http://localhost:8090/api/provision \
  -F "nombre=miapp" \
  -F "zip=@app.zip"

# Respuesta
{
  "success": true,
  "message": "Instancia creada",
  "instance": {
    "name": "web-miapp",
    "ip": "192.168.10.100",
    "url": "http://web-miapp.cloud.local"
  }
}
```

### Acceder a la Instancia

```bash
# Via HTTP
curl http://web-miapp.cloud.local

# Via SSH (usando puerto asignado)
ssh -i ~/.vagrant.d/insecure_private_key -p 2300 vagrant@127.0.0.1

# Directo por IP (desde red Host-Only)
ssh -i ~/.vagrant.d/insecure_private_key vagrant@192.168.10.100
```

### Eliminar una Instancia

```bash
# Via API
curl -X DELETE http://localhost:8090/api/instances/web-miapp

# Respuesta
{
  "success": true,
  "message": "Instancia eliminada"
}
```

## Endpoints API

### Gestión de Instancias

| Método | Endpoint | Descripción |
|--------|----------|-------------|
| **GET** | `/api/instances` | Listar todas las instancias |
| **POST** | `/api/provision` | Crear nueva instancia (multipart: nombre, zip) |
| **DELETE** | `/api/instances/{nombre}` | Eliminar instancia |
| **GET** | `/api/instances/{nombre}/logs` | Obtener logs de instancia |
| **POST** | `/api/instances/{nombre}/start` | Iniciar instancia detenida |
| **POST** | `/api/instances/{nombre}/stop` | Detener instancia |

### Gestión de Discos

| Método | Endpoint | Descripción |
|--------|----------|-------------|
| **GET** | `/api/disks` | Listar discos VirtualBox |
| **POST** | `/api/disks/clone` | Clonar disco |

### Autoscaling

| Método | Endpoint | Descripción |
|--------|----------|-------------|
| **GET** | `/api/autoscaling/config` | Obtener config de autoscaling |
| **POST** | `/api/autoscaling/config` | Actualizar config |
| **GET** | `/api/autoscaling/state` | Estado actual del autoscaling |

### Infraestructura

| Método | Endpoint | Descripción |
|--------|----------|-------------|
| **GET** | `/api/vms` | Listar todas las VMs |
| **GET** | `/api/network/adapters` | Listar adaptadores de red |
| **GET** | `/api/status` | Estado general del sistema |

### Web

| Ruta | Descripción |
|------|-------------|
| `/` | Interfaz web principal (HTML) |
| `/api/*` | Endpoints REST |

## Consideraciones Importantes

### 1. **Rango de IPs**
- Solo 101 instancias máximo (192.168.10.100-200)
- Planeación de capacidad importante si esperas muchas instancias

### 2. **Autenticación**
- **NO hay autenticación por defecto** en el API
- ⚠️ **En producción**: Agregar JWT, OAuth o limitación por IP
- El SSH usa llaves inseguras de Vagrant en desarrollo

### 3. **SSH y Conectividad**
- Primer inicio de VM: hasta 90 segundos para obtener IP
- IP temporal (192.168.10.30) durante provisionamiento
- Requiere sincronización para evitar conflictos
- Los puertos SSH reenvían via NAT desde el host

### 4. **Almacenamiento de Discos**
- Cada clon es una copia **independiente** completa (~1-2GB por instancia)
- No es deduplicación - consume espacio total
- Verificar espacio disponible: `vagrant ssh gestion -- df -h`

### 5. **DNS Dinámico (BIND9)**
- Requiere que "allow-update" esté configurado en la zona
- Usa protocolo nsupdate (RFC 2136)
- El servidor BIND9 debe estar corriendo antes de crear instancias
- Puede haber delay de 1-2 segundos en resolución inicial

### 6. **Red Host-Only**
- Debe existir "vboxnet0" previamente
- Si no existe: `VBoxManage hostonlyif create` (Linux/Mac)
- En Windows: crear via interfaz VirtualBox

### 7. **Concurrencia**
- Lock mutex en la IP temporal `.30` para evitar conflictos
- Máximo 1 provision concurrente por instancia
- Múltiples provision simultáneas de diferentes instancias: OK

### 8. **Seguridad (Desarrollo vs Producción)**
- Desarrollo: SSH con llaves inseguras, sin validación de host
- Producción: 
  - Implementar autenticación en API
  - Usar llaves SSH propias y seguras
  - Configurar firewall
  - Validar certificados SSH
  - Limitar acceso por VLAN/VPN

## Solución de Problemas

### "Error conectando a VBoxManage"

```bash
# Verificar VBoxManage
which VBoxManage
VBoxManage --version

# En Windows, verificar ruta
"C:\Program Files\Oracle\VirtualBox\VBoxManage.exe" --version

# Si no existe, instalar VirtualBox
```

### "Time out esperando IP"

```bash
# Verificar Guest Additions en plantilla
vagrant ssh plantilla
sudo VBoxControl --version

# Si falta: instalar Guest Additions
sudo apt-get install build-essential
# Montar CD de Guest Additions desde VirtualBox UI
```

### "Server Not Found" al cambiar de red

**Este es el problema principal que hemos resuelto con `/etc/hosts` dinámico:**

```bash
# Si usas Opción A (/etc/hosts dinámico):
# ✅ Ya está resuelto automáticamente - el daemon actualiza /etc/hosts
# y funciona sin depender de configuración de red externa

# Verificar que sudoers está configurado correctamente:
sudo -l
# Debe mostrar: NOPASSWD: /usr/bin/tee /etc/hosts

# Limpiar caché DNS del navegador:
# Chrome/Edge: Ctrl+Shift+Delete → Cookies y datos de sitios → Limpiar ahora
# Firefox: about:preferences → Privacy → Limpiar datos

# Si sigue sin funcionar, reinicia el navegador completamente
```

### "DNS no resuelve cloud.local"

**Si usas Opción B (servidor DNS externo):**

```bash
# Verificar servidor DNS
nslookup ns.cloud.local 192.168.10.10

# Verificar BIND9 en VM
vagrant ssh ns
sudo systemctl status bind9
sudo journalctl -u bind9 -n 50

# Verificar configuración del host
resolvectl status
```

### "No se puede conectar por SSH a instancia"

```bash
# Verificar IP asignada
vagrant ssh gestion
curl -s http://localhost:8090/api/instances | jq

# Verificar si VM está corriendo
VBoxManage list runningvms

# Probar SSH directamente
ssh -v -i ~/.vagrant.d/insecure_private_key vagrant@192.168.10.100
```

### "Disco de plantilla no encontrado"

```bash
# Listar discos
VBoxManage list hdds

# Buscar plantilla
VBoxManage list hdds | grep -i plantilla

# Reconstruir VM de plantilla
vagrant destroy plantilla
vagrant up plantilla
```

### "Error: nsupdate: command not found"

```bash
# Instalar BIND9 utilities
sudo apt-get install bind9-utils

# Verificar
nsupdate -v
```

## Estructura del Código

### `main.go` (≈800 líneas)
- **Configuración global**: Variables de SSH, VBoxManage, red
- **SSH & SCP**: Funciones para ejecutar comandos remotos y transferir archivos
- **HTTP Handlers**: Endpoints API (GET /api/instances, POST /api/provision, etc.)
- **Infraestructura de VMs**: Listar, crear, eliminar VMs
- **Punto de entrada**: `func main()` que inicia servidor HTTP en puerto 8090

**Funciones clave:**
```go
buildSSHConfig()          // Configura cliente SSH con llave y fallback password
runSSH(ip, cmd)           // Ejecuta comando en VM remota
uploadFile(ip, local, remote)  // Sube archivo vía SCP
handleProvision(w, r)     // POST /api/provision - crear instancia
```

### `httpaas.go` (≈600 líneas)
- **HTTPaaS**: HTTP Application as a Service
- **Provisión de Instancias**: Lógica completa de crear clones y configurarlos
- **Gestión de Instancias**: CRUD de instancias almacenadas en JSON
- **Configuración de Red**: Inyectar IP estática via Netplan/Debian
- **Manejo de Archivos**: Descomprimir ZIP, crear servicios systemd

**Funciones clave:**
```go
handleProvision(w, r)     // POST - crear instancia (clona disco, configura red, sube ZIP)
getNextFreeIP()           // Busca IP libre en rango 192.168.10.100-200
getPlantillaDiskUUID()    // Obtiene UUID del disco base
getInstances()            // Lee instances.json
saveInstances(insts)      // Escribe instances.json (sincronizado con mutex)
```

### `dns.go` (≈50 líneas)
- **Integración BIND9**: Actualiza DNS dinámico
- **nsupdate RFC 2136**: Protocolo estándar para actualizaciones DNS
- **Registro y Desregistro**: Agregar/quitar registros A en zona cloud.local

**Funciones clave:**
```go
registerDNS(hostname, ip)   // Agrega registro A
deregisterDNS(hostname)     // Elimina registro A
executeNsUpdate(commands)   // Ejecuta nsupdate con comandos
```

### `autoscaler.go` (≈500 líneas)
- **Autoscaling**: Escalado automático de instancias
- **Monitoreo de CPU**: Recopila estado de HAProxy (carga) y CPU
- **Políticas**: Escalar UP si CPU > umbral por N segundos, DOWN si CPU < umbral
- **Event Logging**: Registro de eventos de escalado

**Funciones clave:**
```go
AutoScalingConfig           // Estructura de configuración
logEvent(msg)               // Registra evento de escalado
getHAProxyStateUnsafe()     // Lee estado de load balancer
monitorAndScale()           // Goroutine que chequea y escala
```

### `index.html` (≈600 líneas CSS+JS+HTML)
- **Interfaz Dark Mode**: UI moderna con gradientes y blur
- **Dashboard**: Monitoreo en tiempo real de instancias
- **Formulario de Provisión**: Cargar ZIP y crear instancias
- **Logs de Autoscaling**: Visualizar eventos de escalado

**Secciones:**
```
<head>         // Estilos CSS (dark theme, variables CSS)
<body>
  #sidebar    // Navegación lateral
  #content    // Área principal
    #instances-list    // Tabla de instancias
    #provision-form    // Formulario crear instancia
    #autoscaling-logs  // Logs de eventos
```

### `Vagrantfile` (≈170 líneas)
- **VM 1 - ns**: Servidor BIND9 (DNS) - 512MB RAM
- **VM 2 - plantilla**: Debian + Apache2 (base para clones) - 512MB RAM
- **VM 3 - gestion**: Go daemon + herramientas - 1GB RAM
- **Provisioning**: Scripts de instalación de paquetes y configuración

**Configuración de Red:**
```ruby
# Todas conectan a vboxnet0 (192.168.10.0/24)
ns.vm.network "private_network", ip: "192.168.10.10"
plantilla.vm.network "private_network", ip: "192.168.10.30"
gestion.vm.network "private_network", ip: "192.168.10.50"
```

### `instances.json` (Generado en runtime)
```json
[
  {
    "name": "web-miapp",
    "ip": "192.168.10.100",
    "url": "http://web-miapp.cloud.local",
    "createdAt": "2025-05-03T19:30:00Z"
  },
  ...
]
```

### `autoscaling_config.json` (Configuración)
```json
{
  "enabled": true,
  "upperThreshold": 80,
  "upperTime": 60,
  "lowerThreshold": 20,
  "lowerTime": 60,
  "maxNodes": 5,
  "minNodes": 1,
  "lbIp": "192.168.10.100",
  "diskUuid": "...",
  "networkAdapter": "vboxnet0",
  "appPort": "8080"
}
```

---

## Soporte

Para issues, preguntas o contribuciones: [GitHub Issues](https://github.com/BrandonBrandon-alt/gestor-daemon/issues)
