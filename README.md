# Gestor de Demonios — Orquestador de Infraestructura con VirtualBox y DNS Dinámico

Un panel de control integral desarrollado en **Go** y **Vanilla HTML/JS** para la gestión automatizada de máquinas virtuales, aprovisionamiento de software y orquestación de servicios en entornos de red local bajo el dominio `cloud.local`.

---

## 🚀 Requisitos Previos (Antes de empezar)

Para que el proyecto funcione correctamente, el usuario debe tener instalado:

1.  **VirtualBox:** Motor de virtualización (asegúrate de que `VBoxManage` esté en el PATH o usa la ruta por defecto).
2.  **Vagrant:** Para levantar la infraestructura base de forma automática.
3.  **Go (Golang) 1.22+:** Para ejecutar el servidor de gestión.
4.  **Red Host-Only:** Configura una red en VirtualBox llamada `vboxnet0` con la IP `192.168.10.1`.

---

## 🛠️ Guía de Configuración Rápida

### 1. Preparar la Infraestructura Base
Desde la raíz del proyecto, levanta el servidor DNS y la máquina plantilla:
```bash
vagrant up
```
*Nota: Esto creará automáticamente el dominio `cloud.local` y preparará una imagen de Debian lista para ser clonada.*

### 2. Configurar el Host (Resolución de Dominios)
Para que tu navegador reconozca los dominios `.cloud.local`, elige tu sistema operativo:

#### **En Linux (systemd-resolved):**
```bash
sudo mkdir -p /etc/systemd/resolved.conf.d
printf '[Resolve]\nDNS=192.168.10.10\nDomains=~cloud.local\n' | sudo tee /etc/systemd/resolved.conf.d/cloud-local.conf
sudo systemctl restart systemd-resolved
```

#### **En Windows:**
1. Ve a "Conexiones de Red".
2. Propiedades del adaptador "VirtualBox Host-Only Network".
3. TCP/IPv4 > Propiedades.
4. Servidor DNS preferido: `192.168.10.10`.

---

## 💻 Ejecución del Proyecto

Una vez que `vagrant up` termine con éxito:
1. Asegúrate de que la máquina `plantilla_http_base` esté apagada (Vagrant la deja encendida).
2. Ejecuta el gestor:
   ```bash
   go run .
   ```
3. Abre tu navegador en [http://localhost:8090](http://localhost:8090).

---

## 🤖 Prompt para Asistente AI (Análisis y Configuración)

Si necesitas ayuda para adaptar este proyecto a tu entorno específico, copia y pega el siguiente prompt en tu IA favorita (ChatGPT, Claude, Gemini):

> "Estoy configurando el proyecto 'Gestor de Demonios', un orquestador de VMs con Go y VirtualBox. El proyecto utiliza un Vagrantfile para levantar un DNS (192.168.10.10) y una plantilla (192.168.10.30). Necesito que analices mi sistema operativo actual y me des los pasos exactos para: 1. Configurar mi adaptador de red para alcanzar la IP del DNS. 2. Configurar la resolución de dominios para que '*.cloud.local' sea resuelto por la VM DNS. 3. Verificar que las rutas de VBoxManage y las llaves de Vagrant sean detectadas correctamente por el código de Go. Por favor, sé específico con los comandos según mi shell."

---

## 📂 Estructura del Proyecto
- `main.go`: Servidor central (Portable: detecta rutas de Windows/Linux automáticamente).
- `httpaas.go`: Lógica de clonación de discos e inyección de red.
- `dns.go`: Integración dinámica con BIND9.
- `index.html`: Dashboard web moderno con Glassmorphism.
- `Vagrantfile`: Definición de servidores base (DNS y Plantilla).

---
*Este proyecto es totalmente portátil y no requiere rutas hardcodeadas.*
