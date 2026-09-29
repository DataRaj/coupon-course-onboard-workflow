#!/usr/bin/env bash
# ==============================================================================
# setup-fedora-droplet.sh
# Optimized for Fedora Linux on a 1 vCPU / 1 GB RAM Droplet
# Sets up:
# 1. 2GB Swap (Prevents Out-Of-Memory crashes on 1GB RAM)
# 2. Native lightweight PostgreSQL (Uses ~35MB RAM, saves 200MB vs Docker daemon)
# 3. Nginx with auto-detected <IP>.sslip.io domain
# 4. SELinux permissions for reverse proxying to port 8080
# 5. Systemd service definitions for api and worker
# ==============================================================================

set -euo pipefail

echo "===> [1/6] Setting up 2GB Swap space (Crucial for 1GB RAM)..."
if [ "$(swapon --show | wc -l)" -le 1 ]; then
    fallocate -l 2G /swapfile || dd if=/dev/zero of=/swapfile bs=1M count=2048
    chmod 600 /swapfile
    mkswap /swapfile
    swapon /swapfile
    if ! grep -q '/swapfile' /etc/fstab; then
        echo '/swapfile none swap sw 0 0' >> /etc/fstab
    fi
    sysctl vm.swappiness=10
    echo 'vm.swappiness=10' >> /etc/sysctl.d/99-swap.conf
    echo "✓ 2GB Swap created and active."
else
    echo "✓ Swap space already active."
fi

echo "===> [2/6] Installing packages (Nginx, Certbot, PostgreSQL)..."
dnf install -y nginx certbot python3-certbot-nginx postgresql-server postgresql-contrib curl

echo "===> [3/6] Configuring lightweight native PostgreSQL..."
if [ ! -f /var/lib/pgsql/data/PG_VERSION ]; then
    postgresql-setup --initdb
fi

# Allow local password authentication
sed -i 's/host    all             all             127.0.0.1\/32            ident/host    all             all             127.0.0.1\/32            scram-sha-256/' /var/lib/pgsql/data/pg_hba.conf || true
sed -i 's/host    all             all             ::1\/128                 ident/host    all             all             ::1\/128                 scram-sha-256/' /var/lib/pgsql/data/pg_hba.conf || true

# Memory tuning for 1GB RAM: keep Postgres lean
mkdir -p /var/lib/pgsql/data/conf.d
cat << 'EOF' > /var/lib/pgsql/data/conf.d/memory.conf
# Memory optimizations for 1GB RAM
shared_buffers = 64MB
work_mem = 4MB
maintenance_work_mem = 16MB
max_connections = 30
EOF

if ! grep -q "include_dir = 'conf.d'" /var/lib/pgsql/data/postgresql.conf; then
    echo "include_dir = 'conf.d'" >> /var/lib/pgsql/data/postgresql.conf
fi

systemctl enable --now postgresql

# Wait for postgres to be ready
until sudo -u postgres pg_isready -q 2>/dev/null; do
    sleep 1
done

# Create database and user if not existing
sudo -u postgres psql -tc "SELECT 1 FROM pg_roles WHERE rolname='postgres'" | grep -q 1 || \
    sudo -u postgres psql -c "CREATE USER postgres WITH SUPERUSER PASSWORD 'postgres';"
sudo -u postgres psql -c "ALTER USER postgres PASSWORD 'postgres';"
sudo -u postgres psql -tc "SELECT 1 FROM pg_database WHERE datname='marketplace'" | grep -q 1 || \
    sudo -u postgres psql -c "CREATE DATABASE marketplace OWNER postgres;"
echo "✓ PostgreSQL ready on localhost:5432."

echo "===> [4/6] Configuring Nginx with sslip.io..."
DROPLET_IP=$(curl -s -4 https://ifconfig.me || curl -s -4 https://api.ipify.org || hostname -I | awk '{print $1}')
DROPLET_IP=$(echo "${DROPLET_IP}" | tr -d '[:space:]')
DOMAIN="${DROPLET_IP}.sslip.io"

echo "Detected domain: ${DOMAIN}"

cat << EOF > /etc/nginx/conf.d/course-coupon.conf
server {
    listen 80;
    server_name ${DOMAIN};

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        client_max_body_size 10M;
    }
}
EOF

# Allow Nginx network connections under Fedora SELinux
echo "Configuring SELinux permissions for Nginx..."
setsebool -P httpd_can_network_connect 1

systemctl enable --now nginx
nginx -t
systemctl reload nginx
echo "✓ Nginx configured for ${DOMAIN}."

echo "===> [5/6] Creating application directories & systemd services..."
APP_DIR="/opt/course-coupon-service"
mkdir -p "${APP_DIR}/bin" "${APP_DIR}/logs"

cat << EOF > /etc/systemd/system/course-api.service
[Unit]
Description=Course Coupon HTTP API
After=network.target postgresql.service

[Service]
Type=simple
User=root
WorkingDirectory=${APP_DIR}
EnvironmentFile=${APP_DIR}/.env
ExecStart=${APP_DIR}/bin/api
Restart=always
RestartSec=5s
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

cat << EOF > /etc/systemd/system/course-worker.service
[Unit]
Description=Course Coupon Background Worker
After=network.target postgresql.service

[Service]
Type=simple
User=root
WorkingDirectory=${APP_DIR}
EnvironmentFile=${APP_DIR}/.env
ExecStart=${APP_DIR}/bin/worker
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
echo "✓ Systemd services course-api and course-worker installed."

echo "===> [6/6] Droplet setup finished!"
echo ""
echo "NEXT STEPS:"
echo "1. Run Certbot to enable HTTPS:"
echo "   sudo certbot --nginx -d ${DOMAIN}"
echo ""
echo "2. Copy your built binaries into ${APP_DIR}/bin/ and your .env into ${APP_DIR}/.env"
echo "3. Start the services:"
echo "   systemctl enable --now course-api course-worker"
echo ""
echo "4. Pabbly Webhook URL:"
echo "   https://${DOMAIN}/api/v1/webhooks/pabbly/<YOUR_PABBLY_WEBHOOK_SECRET>"
