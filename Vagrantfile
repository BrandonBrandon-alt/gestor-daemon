Vagrant.configure("2") do |config|
  config.vm.box = "debian/bookworm64"
  config.ssh.insert_key = false

  # Deshabilitar vbguest: la box trae Guest Additions 6.0.0 pero VirtualBox es 7.x.
  # El plugin intenta instalar linux-headers que ya no están en los repos.
  if Vagrant.has_plugin?("vagrant-vbguest")
    config.vbguest.auto_update = false
  end

  # Deshabilitar synced folder (también previene el intento de montar vboxsf)
  config.vm.synced_folder ".", "/vagrant", disabled: true

  # =================================================================
  # 1. SERVIDOR DNS (BIND9)
  # IP: 192.168.10.10
  # =================================================================
  config.vm.define "ns" do |ns|
    ns.vm.hostname = "ns.cloud.local"

    ns.vm.provider "virtualbox" do |vb|
      vb.name   = "servidor_dns_proyecto"
      vb.memory = "512"
    end

    ns.vm.network "private_network", ip: "192.168.10.10", adapter: 2

    ns.vm.provision "shell", name: "ns-setup", inline: <<-'SHELL'
      set -e
      echo ">>> [ns] Actualizando repos..."
      apt-get update -qq

      echo ">>> [ns] Instalando BIND9..."
      apt-get install -y bind9 bind9utils bind9-doc dnsutils

      cat > /etc/bind/named.conf.options <<'EOF'
options {
    directory "/var/cache/bind";
    recursion no;
    allow-query { any; };
    listen-on { any; };
    dnssec-validation no;
};
EOF

      cat > /etc/bind/named.conf.local <<'EOF'
zone "cloud.local" {
    type master;
    file "/var/lib/bind/db.cloud.local";
    allow-update { 192.168.10.0/24; };
};
EOF

      cat > /var/lib/bind/db.cloud.local <<'EOF'
$TTL 604800
@   IN  SOA ns.cloud.local. admin.cloud.local. (
            2026050301 ; Serial
            604800     ; Refresh
            86400      ; Retry
            2419200    ; Expire
            604800 )   ; Negative TTL

@           IN  NS  ns.cloud.local.

ns          IN  A   192.168.10.10
plantilla   IN  A   192.168.10.30
gestion     IN  A   192.168.10.50
EOF

      chown -R bind:bind /var/lib/bind
      named-checkconf
      named-checkzone cloud.local /var/lib/bind/db.cloud.local
      systemctl restart bind9
      echo ">>> [ns] Bind9 levantado."
      sleep 1
      dig @192.168.10.10 ns.cloud.local +short || true
    SHELL
  end

  # =================================================================
  # 2. PLANTILLA BASE HTTP (Apache2)
  # IP: 192.168.10.30
  # Instalar paquetes PRIMERO (NAT con internet), DNS interno AL FINAL
  # =================================================================
  config.vm.define "plantilla" do |pl|
    pl.vm.hostname = "plantilla.cloud.local"

    pl.vm.provider "virtualbox" do |vb|
      vb.name   = "plantilla_http_base"
      vb.memory = "512"
    end

    pl.vm.network "private_network", ip: "192.168.10.30", adapter: 2

    pl.vm.provision "shell", name: "plantilla-setup", inline: <<-'SHELL'
      set -e
      echo ">>> [plantilla] Actualizando repos..."
      apt-get update -qq

      echo ">>> [plantilla] Instalando Apache2..."
      apt-get install -y apache2 unzip curl

      cat > /var/www/html/index.html <<'EOF'
<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>Plantilla HTTP Base</title></head>
<body>
  <h1>Plantilla Base HTTP</h1>
  <p>Host: plantilla.cloud.local (192.168.10.30)</p>
  <p>Estado: lista para clonar</p>
</body>
</html>
EOF

      systemctl enable apache2
      systemctl start apache2

      echo ">>> [plantilla] Configurando DNS interno (al final para no romper apt)..."
      printf 'nameserver 192.168.10.10\nsearch cloud.local\n' > /etc/resolv.conf

      echo ">>> [plantilla] Listo."
    SHELL
  end

  # =================================================================
  # 3. SERVIDOR DE GESTIÓN (Golang)
  # IP: 192.168.10.50
  # Instalar paquetes PRIMERO, DNS interno AL FINAL
  # =================================================================
  config.vm.define "gestion" do |ges|
    ges.vm.hostname = "gestion.cloud.local"

    ges.vm.provider "virtualbox" do |vb|
      vb.name   = "servidor_gestion_golang"
      vb.memory = "1024"
    end

    ges.vm.network "private_network", ip: "192.168.10.50", adapter: 2

    ges.vm.provision "shell", name: "gestion-setup", inline: <<-'SHELL'
      set -e
      echo ">>> [gestion] Actualizando repos..."
      apt-get update -qq

      echo ">>> [gestion] Instalando dependencias..."
      apt-get install -y git curl wget dnsutils

      GO_VERSION="1.22.3"
      echo ">>> [gestion] Descargando Go ${GO_VERSION}..."
      wget -q "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -O /tmp/go.tar.gz

      echo ">>> [gestion] Instalando Go..."
      rm -rf /usr/local/go
      tar -C /usr/local -xzf /tmp/go.tar.gz
      rm /tmp/go.tar.gz

      cat > /etc/profile.d/golang.sh <<'EOF'
export PATH=$PATH:/usr/local/go/bin
export GOPATH=/home/vagrant/go
export PATH=$PATH:$GOPATH/bin
EOF
      chmod +x /etc/profile.d/golang.sh
      export PATH=$PATH:/usr/local/go/bin
      echo ">>> [gestion] Go instalado: $(go version)"
    
      echo ">>> [gestion] Configurando DNS interno (al final para no romper wget)..."
      printf 'nameserver 192.168.10.10\nsearch cloud.local\n' > /etc/resolv.conf

      echo ">>> [gestion] Listo."
    SHELL
  end

end
