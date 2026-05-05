# Arquitectura: Resolución DNS con /etc/hosts Dinámico

## Problema Original
```
Escenario A (WiFi Oficina):
  Tu PC → DNS Query → 192.168.10.10 → "app.cloud.local" = 192.168.10.100 ✓

Cambias a WiFi Diferente o Red Móvil:
  Tu PC → DNS Query → ??? (No alcanza 192.168.10.10) → "Server Not Found" ✗
```

## Solución: /etc/hosts Dinámico

```
Gestor Daemon detecta despliegue → Ejecuta updateHosts()
  ↓
Lee /etc/hosts actual
  ↓
Busca entrada para "app.cloud.local" (si existe, la reemplaza)
  ↓
Agrega nueva línea: "192.168.10.100    app.cloud.local"
  ↓
Usa 'sudo tee /etc/hosts' para escribir cambios
  ↓
Tu navegador consulta /etc/hosts LOCALMENTE (sin red externa)
  ↓
Resuelve: "app.cloud.local" = 192.168.10.100 ✓ (funciona en cualquier red)
```

## Comparación de Opciones

### Opción A: /etc/hosts Dinámico (Recomendado)
```
┌─────────────────────────────────────────┐
│      Tu Máquina (Host)                  │
├─────────────────────────────────────────┤
│ /etc/hosts                              │
│ ├─ 127.0.0.1  localhost                │
│ ├─ 192.168.10.100  app.cloud.local     │ ← Daemon agrega automáticamente
│ ├─ 192.168.10.105  otra.cloud.local    │   cuando despliegas
│ └─ ...                                   │
│                                          │
│ Navegador                               │
│ └─ lookup("app.cloud.local")            │
│    └─ Consulta /etc/hosts (local)  ✓    │
└─────────────────────────────────────────┘

Ventajas:
  ✅ Funciona en CUALQUIER red (no depende de DNS externo)
  ✅ Automático - el daemon lo maneja
  ✅ Rápido - sin latencia de DNS
  ✅ Offline compatible
  
  ✅ One-time setup (sudoers)

Desventajas:
  ❌ Requiere permisos de sudoers
  ❌ No resuelve desde otras máquinas de la red
```

### Opción B: Servidor DNS Externo (BIND9)
```
┌──────────────────────┐        ┌──────────────────────┐
│   Tu Máquina (Host)  │        │  VM DNS (BIND9)      │
├──────────────────────┤        │  192.168.10.10       │
│ Navegador            │        ├──────────────────────┤
│ lookup("app...")     │────┐   │ Zona: cloud.local    │
│                      │    └──→ app.cloud.local       │
│ /etc/resolv.conf     │        = 192.168.10.100      │
│ nameserver 192...10  │        │                      │
└──────────────────────┘        └──────────────────────┘
        ↓
   ⚠️ Si cambias de red:
   - Pierde conectividad a 192.168.10.10
   - "Server Not Found"

Ventajas:
  ✅ Resuelve desde toda la red
  ✅ Soporte completo de DNS
  ✅ Compatible con otros hosts

Desventajas:
  ❌ Falla al cambiar de red
  ❌ Requiere servidor DNS corriendo
  ❌ Necesita configurar cliente DNS
```

## Flujo de Despliegue (Con /etc/hosts)

```
Usuario: Click "Desplegar app.zip"
  ↓
[1] Validar ZIP
[2] Asignar IP (ej: 192.168.10.100)
[3] Clonar disco de plantilla
[4] Configurar VM web-app
[5] Esperar booteo
[6] Configurar red (IP estática)
[7] Subir ZIP y descomprimir
[8] Configurar Apache
[9] Registrar en BIND9 DNS ← DNS externo (opcional)
[10] ✨ NUEVO: updateHosts("app", "192.168.10.100", true)
     └─ Agrega a /etc/hosts localmente
[11] Guardar metadatos
[12] Resultado: ✓ "Listo en http://app.cloud.local"
```

## Administración de /etc/hosts

### Ver entradas:
```bash
grep cloud.local /etc/hosts
# 192.168.10.100    app.cloud.local
# 192.168.10.105    otra.cloud.local
```

### Editar manualmente (si necesario):
```bash
sudo nano /etc/hosts
# Editar, guardar y cerrar
```

### Limpiar todo:
```bash
# Remover todas las entradas cloud.local
sudo sed -i '/cloud\.local/d' /etc/hosts
```

## Casos de Uso

### Caso 1: WiFi Oficina → WiFi Casa
```
WiFi Oficina (conectado a 192.168.10.0/24):
  ✓ app.cloud.local funciona (está en /etc/hosts)

Cambias a WiFi Casa (192.168.1.0/24):
  ✓ app.cloud.local SIGUE funcionando
  (porque está en /etc/hosts local, no depende de red)

Conectas VPN a Oficina:
  ✓ app.cloud.local funciona (VM es accesible vía VPN + /etc/hosts)
```

### Caso 2: IP de instancia cambia
```
Despliegas app1 → IP asignada 192.168.10.100
  /etc/hosts: "192.168.10.100    app1.cloud.local"
  ✓ Funciona

Eliminas app1, despliegas app2 en misma VM → IP 192.168.10.100
  daemon llama updateHosts("app2", "192.168.10.100", true)
  /etc/hosts: "192.168.10.100    app2.cloud.local"  ← Actualizado
  ✓ Funciona
```

## Configuración de Sudoers

Archivo: `/etc/sudoers.d/gestor-daemon`
```
%sudo ALL=(ALL) NOPASSWD: /usr/bin/tee /etc/hosts
```

Significa:
- `%sudo` = Cualquier usuario en grupo "sudo"
- `ALL=(ALL)` = Desde cualquier host, como cualquier usuario
- `NOPASSWD` = Sin pedir contraseña
- `/usr/bin/tee /etc/hosts` = Solo para este comando específico

Esto es **SEGURO** porque:
- Solo permite escribir `/etc/hosts` (no todo)
- Limitado a grupo sudo (no todos los usuarios)
- El daemon valida input antes de escribir

## Troubleshooting

### "Permission denied" escribiendo /etc/hosts
```
Solución:
1. Ejecutar: sudo ./setup-hosts.sh
2. Verificar: ls -l /etc/sudoers.d/gestor-daemon
3. Debería ser: -r--r----- 1 root root
```

### Cambios en /etc/hosts no se ven en navegador
```
Solución:
1. Limpiar caché DNS del navegador
   - Chrome: Ctrl+Shift+Delete → Limpiar datos
   - Firefox: about:preferences → Privacy → Limpiar datos
2. Ctrl+Shift+R (reload caché)
3. Reiniciar navegador
```

### Quiero usar AMBAS opciones (local + DNS externo)
```
✅ Está soportado:
- El daemon escribe en /etc/hosts (local)
- Y simultáneamente registra en BIND9 (DNS externo)
- Tu sistema intenta /etc/hosts primero (más rápido)
- Si no encuentra, consulta DNS externo como fallback
```
