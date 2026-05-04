# Gestor de Demonios — Orquestador de Infraestructura con VirtualBox y DNS Dinámico

Un panel de control integral desarrollado en **Go** y **Vanilla HTML/JS** para la gestión automatizada de máquinas virtuales, aprovisionamiento de software y orquestación de servicios en entornos de red local bajo el dominio `cloud.local`.

---

##  Arquitectura del Sistema

El proyecto utiliza un entorno híbrido de red para permitir la comunicación entre el Host y las VMs:
- **Red NAT (eth0):** Acceso a Internet para descarga de paquetes.
- **Red Host-Only (eth1/vboxnet0):** Red privada `192.168.10.0/24` para comunicación interna y servicios web.
- **DNS Dinámico:** Servidor BIND9 integrado que registra automáticamente las nuevas instancias.

---

##  Requisitos Previos

1. **VirtualBox & Vagrant** instalados.
2. **Go 1.22+** instalado en el host.
3. **Red Host-Only:** Asegúrate de tener una interfaz `vboxnet0` configurada con la IP `192.168.10.1`.
4. **Configuración de Resolución DNS (Host):**
   Para que tu navegador resuelva los dominios `.cloud.local`, configura `systemd-resolved` en tu Linux:
   ```bash
   sudo mkdir -p /etc/systemd/resolved.conf.d
   printf '[Resolve]\nDNS=192.168.10.10\nDomains=~cloud.local\n' | sudo tee /etc/systemd/resolved.conf.d/cloud-local.conf
   sudo systemctl restart systemd-resolved
   ```

---

##  Instalación y Configuración

### 1. Levantar la Infraestructura Base
El proyecto incluye un `Vagrantfile` que levanta el servidor DNS y la plantilla base:
```bash
vagrant up
```

### 2. Configurar Acceso SSH
El gestor utiliza la llave insegura de Vagrant por defecto para configurar los clones:
- **Usuario:** `vagrant`
- **Llave:** `~/.vagrant.d/insecure_private_key`

### 3. Ejecutar el Gestor
```bash
go run .
```
Accede a la interfaz en [http://localhost:8090](http://localhost:8090).

---

##  Estructura del Proyecto

- `main.go`: Servidor central y API de gestión de VirtualBox.
- `httpaas.go`: Pipeline de aprovisionamiento (Clonación de discos, inyección de red e IP).
- `dns.go`: Integración con BIND9 mediante `nsupdate`.
- `autoscaler.go`: Lógica experimental de auto-escalado basado en carga.
- `index.html`: Interfaz web moderna (Glassmorphism).
- `Vagrantfile`: Definición de la infraestructura base (DNS, Plantilla).

---

##  Notas Importantes
- **Clonación de Discos:** El sistema utiliza `clonemedium` para crear discos independientes por cada instancia. Asegúrate de que la máquina `plantilla_http_base` esté apagada antes de crear nuevos clones.
- **Dominios .local:** El uso del sufijo `~` en la configuración de `resolved` es crucial para evitar conflictos con mDNS/Avahi.

---
*Desarrollado para la automatización de despliegues locales y orquestación de microservicios.*
