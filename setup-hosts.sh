#!/bin/bash
# Setup script for /etc/hosts dynamic management
# Configures sudoers to allow gestor-daemon to update /etc/hosts without password

set -e

echo "🔧 Configurando permisos de sudoers para /etc/hosts dinámico..."

# Create sudoers file
SUDOERS_FILE="/etc/sudoers.d/gestor-daemon"

if [ -f "$SUDOERS_FILE" ]; then
    echo "✓ Archivo sudoers ya existe"
else
    echo "Creando archivo sudoers..."
    echo "%sudo ALL=(ALL) NOPASSWD: /usr/bin/tee /etc/hosts" | sudo tee "$SUDOERS_FILE" > /dev/null
fi

# Fix permissions (sudoers files must be readable but not writable by group/others)
sudo chmod 0440 "$SUDOERS_FILE"

# Verify
echo ""
echo "✓ Verificando configuración..."
ls -l "$SUDOERS_FILE"

# Test sudo access
echo ""
echo "Testeando acceso sudo..."
echo "# Test" | sudo tee /etc/hosts > /dev/null

echo ""
echo "✅ ¡Configuración completada! El daemon ya puede actualizar /etc/hosts."
echo ""
echo "El sistema ahora:"
echo "  • Agregará automáticamente líneas a /etc/hosts al desplegar"
echo "  • Resolverá dominio.cloud.local sin necesidad de DNS externo"
echo "  • Funcionará incluso al cambiar de red"
