# -*- mode: ruby -*-
# vi: set ft=ruby :

# =================================================================
# CONFIGURACIÓN DE VARIABLES GLOBALES
# =================================================================
NETWORK_PREFIX  = "192.168.10"
DNS_IP          = "#{NETWORK_PREFIX}.10"
PLANTILLA_IP    = "#{NETWORK_PREFIX}.30"
GESTION_IP      = "#{NETWORK_PREFIX}.50"
DNS_ZONE        = "cloud.local"
GO_VERSION      = "1.22.3"

# Función reutilizable: configura DNS interno rompiendo el enlace de systemd-resolved
$configure_dns = <<~SHELL
  echo ">>> Configurando DNS interno..."
  # Se elimina el symlink para evitar problemas con systemd-resolved en Debian 12
  rm -f /etc/resolv.conf 
  cat > /etc/resolv.conf <<EOF
nameserver #{DNS_IP}
search #{DNS_ZONE}
EOF
  # Protege el archivo contra cambios de otros servicios (DHCP/NetworkManager)
  chattr +i /etc/resolv.conf
SHELL

Vagrant.configure("2") do |config|

  config.vm.box = "debian/bookworm64"
  config.ssh.insert_key = false

  config.vbguest.auto_update = false if Vagrant.has_plugin?("vagrant-vbguest")

  # Previene montar vboxsf (incompatible con multiattach)
  config.vm.synced_folder ".", "/vagrant", disabled: true

  # Configuración base VirtualBox compartida
  config.vm.provider "virtualbox" do |vb|
    vb.linked_clone = false
    vb.customize ["modifyvm", :id, "--natdnshostresolver1", "on"]
    vb.customize ["modifyvm", :id, "--ioapic", "on"]
  end

  # =================================================================
  # 1. SERVIDOR DNS (BIND9) — 192.168.10.10
  # =================================================================
  config.vm.define "ns" do |ns|
    ns.vm.hostname = "ns.#{DNS_ZONE}"
    ns.vm.network "private_network", ip: DNS_IP, adapter: 2

    ns.vm.provider "virtualbox" do |vb|
      vb.name   = "servidor_dns_proyecto"
      vb.memory = "512"
      vb.cpus   = 1
    end

    ns.vm.provision "shell", name: "ns-setup", inline: <<~SHELL
      set -euo pipefail
      export DEBIAN_FRONTEND=noninteractive

      echo ">>> [ns] Actualizando repositorios..."
      apt-get update -qq

      echo ">>> [ns] Instalando BIND9..."
      apt-get install -y bind9 bind9utils bind9-doc dnsutils

      echo ">>> [ns] Configurando named.conf.options..."
      cat > /etc/bind/named.conf.options <<'EOF'
options {
    directory "/var/cache/bind";
    recursion no;
    allow-query { any; };
    listen-on { any; };
    dnssec-validation no;
    version "hidden";
};
EOF

      echo ">>> [ns] Configurando zona #{DNS_ZONE}..."
      cat > /etc/bind/named.conf.local <<'EOF'
zone "#{DNS_ZONE}" {
    type master;
    file "/var/lib/bind/db.#{DNS_ZONE}";
    allow-update { #{NETWORK_PREFIX}.0/24; };
    notify yes;
};
EOF

      echo ">>> [ns] Creando zona forward..."
      cat > /var/lib/bind/db.#{DNS_ZONE} <<'EOF'
$TTL 604800
@   IN  SOA ns.#{DNS_ZONE}. admin.#{DNS_ZONE}. (
            2026050301 ; Serial  (YYYYMMDDNN)
            604800     ; Refresh (7d)
            86400      ; Retry   (1d)
            2419200    ; Expire  (28d)
            604800 )   ; Negative Cache TTL

; Servidores de nombres
@       IN  NS  ns.#{DNS_ZONE}.

; Registros A
ns        IN  A   #{DNS_IP}
plantilla IN  A   #{PLANTILLA_IP}
gestion   IN  A   #{GESTION_IP}
EOF

      chown -R bind:bind /var/lib/bind

      echo ">>> [ns] Verificando configuración..."
      named-checkconf
      named-checkzone #{DNS_ZONE} /var/lib/bind/db.#{DNS_ZONE}

      echo ">>> [ns] Iniciando BIND9..."
      systemctl restart named

      echo ">>> [ns] Verificando resolución DNS..."
      sleep 2
      dig @#{DNS_IP} ns.#{DNS_ZONE} +short
      dig @#{DNS_IP} plantilla.#{DNS_ZONE} +short
      dig @#{DNS_IP} gestion.#{DNS_ZONE} +short
      echo ">>> [ns] DNS operativo."
    SHELL
  end

  # =================================================================
  # 2. PLANTILLA BASE HTTP (Apache2) — 192.168.10.30
  # =================================================================
  config.vm.define "plantilla" do |pl|
    pl.vm.hostname = "plantilla.#{DNS_ZONE}"
    pl.vm.network "private_network", ip: PLANTILLA_IP, adapter: 2

    pl.vm.provider "virtualbox" do |vb|
      vb.name   = "plantilla_http_base"
      vb.memory = "512"
      vb.cpus   = 1
    end

    pl.vm.provision "shell", name: "plantilla-setup", inline: <<~SHELL
      set -euo pipefail
      export DEBIAN_FRONTEND=noninteractive

      apt-get update -qq
      apt-get install -y apache2 unzip curl

      cat > /var/www/html/index.html <<'EOF'
<!DOCTYPE html>
<html lang="es">
<head>
  <meta charset="utf-8">
  <title>Plantilla HTTP Base</title>
</head>
<body>
  <h1>Plantilla Base HTTP</h1>
  <p>Host: plantilla.#{DNS_ZONE} (#{PLANTILLA_IP})</p>
</body>
</html>
EOF

      a2dissite 000-default.conf 2>/dev/null || true
      cat > /etc/apache2/sites-available/plantilla.conf <<'EOF'
<VirtualHost *:80>
    ServerName plantilla.#{DNS_ZONE}
    DocumentRoot /var/www/html
</VirtualHost>
EOF
      a2ensite plantilla.conf
      systemctl enable --now apache2

      #{$configure_dns}
      echo ">>> [plantilla] Listo."
    SHELL

    pl.trigger.after :up do |trigger|
      trigger.info = "Configurando disco de plantilla como MULTIATTACH..."
      trigger.run = {
        inline: "bash -c '
          VM_NAME=\"plantilla_http_base\"
          
          # Obtenemos el disco REALMENTE adjunto a la VM, ignorando fantasmas del registro
          DISK_UUID=$(VBoxManage showvminfo \"$VM_NAME\" --machinereadable | grep \"SATA Controller-ImageUUID-0-0\" | cut -d\"=\" -f2 | tr -d \"\\\"\")
          
          if [ -z \"$DISK_UUID\" ]; then
            echo \">>> ERROR: No se encontró disco adjunto.\"
            exit 1
          fi

          # Si es un disco de diferenciación, el base ya es multiattach
          PARENT_UUID=$(VBoxManage showmediuminfo \"$DISK_UUID\" | grep \"Parent UUID:\" | awk \"{print \\$3}\")
          if [ \"$PARENT_UUID\" != \"base\" ]; then
            echo \">>> El disco actual es un snapshot/diferenciación. El base ya está protegido.\"
            exit 0
          fi

          # Si llegamos aquí, es el disco base. Verificamos si ya es multiattach
          CURRENT_TYPE=$(VBoxManage showmediuminfo \"$DISK_UUID\" | grep \"Type:\" | awk \"{print \\$2}\")
          if [ \"$CURRENT_TYPE\" = \"multiattach\" ]; then
            echo \">>> El disco base ya es MULTIATTACH.\"
            exit 0
          fi

          echo \">>> Configurando disco base $DISK_UUID como MULTIATTACH...\"
          VBoxManage controlvm \"$VM_NAME\" poweroff 2>/dev/null || true
          sleep 3
          VBoxManage storageattach \"$VM_NAME\" --storagectl \"SATA Controller\" --port 0 --device 0 --medium none
          VBoxManage modifymedium disk \"$DISK_UUID\" --type multiattach
          VBoxManage storageattach \"$VM_NAME\" --storagectl \"SATA Controller\" --port 0 --device 0 --type hdd --medium \"$DISK_UUID\"
          VBoxManage startvm \"$VM_NAME\" --type headless
          echo \">>> MULTIATTACH configurado con éxito en el disco real.\"
        '"
      }
    end
  end

  # =================================================================
  # 3. SERVIDOR DE GESTIÓN (Golang) — 192.168.10.50
  # =================================================================
  config.vm.define "gestion" do |ges|
    ges.vm.hostname = "gestion.#{DNS_ZONE}"
    ges.vm.network "private_network", ip: GESTION_IP, adapter: 2

    ges.vm.provider "virtualbox" do |vb|
      vb.name   = "servidor_gestion_golang"
      vb.memory = "1024"
      vb.cpus   = 2
    end

    ges.vm.provision "shell", name: "gestion-setup", inline: <<~SHELL
      set -euo pipefail
      export DEBIAN_FRONTEND=noninteractive

      apt-get update -qq
      apt-get install -y git curl wget dnsutils build-essential

      GO_VERSION=\"#{GO_VERSION}\"
      GO_TAR=\"go${GO_VERSION}.linux-amd64.tar.gz\"
      
      echo \">>> [gestion] Descargando Go...\"
      wget -q \"https://go.dev/dl/${GO_TAR}\" -O \"/tmp/${GO_TAR}\"

      EXPECTED=\"8920ea521bad8f6b7bc377b4824982e011c19af27df88a815e3586ea895f1b36\"
      ACTUAL=$(sha256sum \"/tmp/${GO_TAR}\" | awk '{print $1}')
      if [ \"$EXPECTED\" != \"$ACTUAL\" ]; then exit 1; fi

      rm -rf /usr/local/go
      tar -C /usr/local -xzf \"/tmp/${GO_TAR}\"
      
      cat > /etc/profile.d/golang.sh <<'EOF'
export PATH=$PATH:/usr/local/go/bin
export GOPATH=/home/vagrant/go
export PATH=$PATH:$GOPATH/bin
EOF
      chmod +x /etc/profile.d/golang.sh
      export PATH=$PATH:/usr/local/go/bin
      
      mkdir -p /home/vagrant/go/{bin,src,pkg}
      chown -R vagrant:vagrant /home/vagrant/go

      #{$configure_dns}
      echo \">>> [gestion] Listo.\"
    SHELL
  end
end