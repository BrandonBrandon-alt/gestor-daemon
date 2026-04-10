# Gestor de Demonios — Orquestador de Infraestructura con VirtualBox y HAProxy

Un panel de control integral desarrollado en **Go** y **Vanilla HTML/JS** para la gestión automatizada de máquinas virtuales, aprovisionamiento de software y orquestación de balanceo de carga en entornos de red local.

Este proyecto permite transformar una máquina base en una plantilla lista para escalado horizontal, inyectar servicios `systemd` y orquestar el tráfico mediante **HAProxy**, todo desde una interfaz web moderna y fluida.

---

## 🚀 Características Principales

### 1. Gestión de Ciclo de Vida de VMs
- **CRUD Completo:** Listado, creación, inicio (headless), apagado y eliminación total de instancias de VirtualBox.
- **Detección Automática de IP:** Utiliza *Guest Additions* para obtener la IP real de las máquinas en tiempo real sin intervención manual.

### 2. Pipeline de Automatización (Aprovisionamiento)
- **Preparación de Plantillas:** Sube binarios ejecutables y archivos adicionales (.zip) a una máquina base.
- **Configuración Nativa Linux:** Genera e instala automáticamente archivos de servicio `.service` para `systemd`.
- **Discos Inmutables (Multiattach):** Convierte discos duros virtuales a modo `multiattach`, permitiendo que múltiples instancias compartan el mismo disco base con capas de escritura independientes.

### 3. Orquestación de Red y Balanceo
- **Gestión de HAProxy:** Registro y monitoreo de balanceadores.
- **Reglas Dinámicas:** Interfaz para añadir/quitar nodos del pool de balanceo en caliente.
- **Hot-Reload:** Genera configuraciones `haproxy.cfg` y las despliega vía SSH reiniciando el servicio de forma transparente.
- **Estadísticas en Tiempo Real:** Visualización del estado de salud de los backends usando sockets de administración (`socat`).

### 4. Terminal de Operaciones Remotas
- Control directo de servicios vía SSH (`start`, `stop`, `restart`, `enable`, `status`).
- Visor de logs en tiempo real mediante `journalctl`.

---

## 🛠️ Stack Tecnológico

- **Backend:** [Go](https://go.dev/) (Golang)
    - Concurrencia nativa para escaneo de red y comandos VBox.
    - Comunicación vía SSH y SCP (protocolos nativos).
    - Servidor API embebido.
- **Frontend:** HTML5, CSS3 (Glassmorphism design), JavaScript moderno (ES6+).
    - Iconografía: Google Material Symbols.
    - Tipografía: Inter & Space Mono.
- **Infraestructura:** 
    - [VirtualBox](https://www.virtualbox.org/) (VBoxManage CLI).
    - [HAProxy](http://www.haproxy.org/) para balanceo de carga L7.
    - [systemd](https://systemd.io/) para gestión de demonios en Linux.

---

## 📋 Requisitos Previos

1. **VirtualBox** instalado y la ruta de `VBoxManage.exe` configurada.
2. **Llave SSH** configurada para acceso sin contraseña a las máquinas virtuales.
3. **Guest Additions** instalado en las máquinas base (necesario para la telemetría de red).
4. **Socat** instalado en los balanceadores (para el monitoreo de HAProxy).

---

## ⚙️ Configuración

El gestor puede configurarse mediante variables de entorno o editando las constantes en `main.go`:

| Variable | Descripción | Valor por Defecto |
| --- | --- | --- |
| `SSH_USER` | Usuario para conexiones remotas | `servidor1` |
| `SSH_KEY_PATH` | Ruta a la llave privada RSA/Ed25519 | `~/.ssh/id_rsa` |
| `VBOX_MANAGE` | Ruta al ejecutable de VirtualBox | `C:\...\VBoxManage.exe` |
| `BRIDGE_ADAPTER` | Adaptador de red para el modo puente | `Intel(R) Wi-Fi...` |

---

## 🏃 Modo de Uso

1. **Clonar y Ejecutar:**
   ```bash
   go run main.go
   ```
2. **Acceder a la Interfaz:**
   Abre [http://localhost:8090](http://localhost:8090) en tu navegador.

3. **Flujo de Trabajo Recomendado:**
   - **Paso 1:** Prepara una máquina base en VirtualBox.
   - **Paso 2:** En la pestaña "Catálogo", usa el "Generador de Plantilla" para inyectar tu app y crear un disco multiconexión.
   - **Paso 3:** En "Aprovisionamiento", levanta tantos nodos como necesites y un balanceador HAProxy.
   - **Paso 4:** En "Operaciones", asocia los nodos al balanceador y pulsa "Forzar Hot-Reload".

---

## 📂 Estructura del Proyecto

```text
├── main.go            # Lógica del servidor, API y orquestación SSH/VBox
├── index.html         # Frontend integrado (Single Page Application)
├── go.mod             # Dependencias del proyecto
├── haproxy_state.json # Persistencia del estado de la red (LBs y nodos)
└── README.md          # Documentación técnica
```

---

## 🛡️ Notas de Seguridad
- El proyecto utiliza `InsecureIgnoreHostKey()` para facilitar la gestión en redes locales dinámicas. En entornos productivos, se recomienda implementar verificación de huellas digitales SSH.
- Se recomienda el uso de redes *Bridged* para que las VMs tengan visibilidad completa en la LAN.

---
*Desarrollado con ❤️ para la simplificación de servidores y automatización de despliegues.*
