# DigitalOcean Droplet Deployment & Pabbly Webhook Guide

This guide walks you through deploying the **Course Coupon Service** on a DigitalOcean Droplet, setting up **SSL (HTTPS)** with `sslip.io` and Nginx, and setting up the **Pabbly Webhook**.

> [!NOTE]
> **Using a 1GB RAM Fedora Droplet?** See the optimized [Fedora 1GB RAM Fast Track](#0-fedora-1gb-ram-droplet-fast-track) below, which includes swap configuration and avoids out-of-memory errors!

---

## 0. Fedora 1GB RAM Droplet Fast-Track

Because your Droplet has **1GB RAM** and runs **Fedora**:
1. Compiling Go inside the droplet can cause out-of-memory (OOM) crashes.
2. A 2GB Swap file is essential.
3. Native PostgreSQL + Go systemd services use under **80MB RAM total** (saving ~250MB compared to Docker).

### Step A: Run the one-step setup on your Fedora Droplet
SSH into your droplet and run:
```bash
# Upload or curl the setup script, or run:
curl -sSL https://raw.githubusercontent.com/<YOUR_USER>/<YOUR_REPO>/main/scripts/setup-fedora-droplet.sh | bash
```
*(Or copy `scripts/setup-fedora-droplet.sh` to your droplet and run `bash scripts/setup-fedora-droplet.sh`)*

This script automatically:
- Creates a **2GB swapfile** (no more out-of-memory kills).
- Installs and tunes **PostgreSQL** for 1GB RAM.
- Configures **Nginx** for `YOUR_DROPLET_IP.sslip.io` and sets SELinux permissions.
- Installs **systemd services** (`course-api` and `course-worker`).

### Step B: Deploy from your local machine (using your local Go compiler)
From your local machine terminal:
```bash
./scripts/deploy-to-droplet.sh YOUR_DROPLET_IP
```
This builds static binaries using your local Go compiler in 3 seconds, copies them to the droplet, and starts the services!

---

## 1. Quick Architecture Overview

```
                      Internet
                         │
                 HTTPS (Port 443)
                         │
        ┌────────────────▼────────────────┐
        │     Nginx Reverse Proxy + SSL   │ (Let's Encrypt Certbot)
        └────────────────┬────────────────┘
                         │ Proxy to Port 8080
        ┌────────────────▼────────────────┐
        │       Docker Compose Stack      │
        │                                 │
        │  ┌──────────┐     ┌──────────┐  │
        │  │ course-  │     │ course-  │  │
        │  │ coupon-  │     │ coupon-  │  │
        │  │ api      │     │ worker   │  │
        │  └────┬─────┘     └────┬─────┘  │
        │       │                │        │
        │  ┌────▼────────────────▼─────┐  │
        │  │   PostgreSQL 16 (db)      │  │
        │  └───────────────────────────┘  │
        └─────────────────────────────────┘
```

---

## 2. One-Time Droplet Setup

### Step 2.1: Connect to your Droplet
```bash
ssh root@YOUR_DROPLET_IP
```

### Step 2.2: Install Docker & Docker Compose
```bash
# Update package lists
sudo apt update && sudo apt upgrade -y

# Install Docker & Docker Compose plugin
curl -fsSL https://get.docker.com -o get-docker.sh
sudo sh get-docker.sh

# Verify installation
docker --version
docker compose version
```

### Step 2.3: Clone the Repository on the Droplet
Clone your repository into `/opt/course-coupon-service`:
```bash
cd /opt
git clone <YOUR_GITHUB_REPO_URL> course-coupon-service
cd /opt/course-coupon-service
```

---

## 3. Environment Configuration (`.env`)

Inside `/opt/course-coupon-service`, create your production `.env` file:
```bash
cp .env.example .env
nano .env
```

Configure the environment variables:
```env
APP_ENV=production
HTTP_ADDR=:8080
DATABASE_URL=postgres://postgres:postgres@db:5432/marketplace?sslmode=disable

# Authentication
DEV_AUTH_ENABLED=false
JWT_SECRET=your-secure-random-jwt-secret-here

# Pabbly API Credentials
PABBLY_BASE_URL=https://payments.pabbly.com/api/v1
PABBLY_API_KEY=your_pabbly_api_key_here
PABBLY_SECRET_KEY=your_pabbly_secret_key_here
PABBLY_TIMEOUT=15s

# Sync intervals
CATALOG_SYNC_INTERVAL=2m
CATALOG_HARD_STALE_AFTER=30m
PRICE_QUOTE_MAX_AGE=60s
REDEMPTION_TTL=30m

# Webhook Secret (Generate with: openssl rand -hex 24)
PABBLY_WEBHOOK_SECRET=your_generated_webhook_secret_here
```

> [!TIP]
> Generate a strong webhook secret using:
> ```bash
> openssl rand -hex 24
> ```
> Save this secret — you will use it in Pabbly's dashboard.

---

## 4. Launch the Application with Docker Compose

Run the entire stack (PostgreSQL, API, and Worker):
```bash
docker compose up -d --build
```

Check running containers:
```bash
docker compose ps
```

View application logs:
```bash
docker compose logs -f api
docker compose logs -f worker
```

---

## 5. Domain & HTTPS Setup with Nginx & Certbot

Pabbly requires a valid **HTTPS** endpoint. Set up Nginx as a reverse proxy:

### Step 5.1: Point DNS to Droplet
In your DNS provider (Namecheap, GoDaddy, Cloudflare, etc.), add an **A record**:
- **Name**: `api` (e.g. `api.yourdomain.com`)
- **Value**: `YOUR_DROPLET_IP`

### Step 5.2: Install Nginx & Certbot
```bash
sudo apt install -y nginx certbot python3-certbot-nginx
```

### Step 5.3: Configure Nginx Site
Create `/etc/nginx/sites-available/course-coupon`:
```bash
sudo nano /etc/nginx/sites-available/course-coupon
```

Paste the following configuration:
```nginx
server {
    server_name api.yourdomain.com; # Replace with your actual domain

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Enable the configuration and reload Nginx:
```bash
sudo ln -s /etc/nginx/sites-available/course-coupon /etc/nginx/sites-enabled/
sudo rm -f /etc/nginx/sites-enabled/default
sudo nginx -t
sudo systemctl reload nginx
```

### Step 5.4: Obtain Free SSL Certificate
```bash
sudo certbot --nginx -d api.yourdomain.com
```
Certbot will automatically install the SSL certificate and configure HTTPS redirection.

Test the public health check endpoint:
```bash
curl https://api.yourdomain.com/healthz
# Response: {"status":"ok"}
```

---

## 6. GitHub Actions Automated Deployment

A ready-to-use GitHub Actions workflow is located at [`.github/workflows/deploy.yml`](.github/workflows/deploy.yml).

### Step 6.1: Prepare SSH Key for GitHub
1. On your local machine (or on the droplet), generate a deploy key:
   ```bash
   ssh-keygen -t ed25519 -C "github-actions-deploy" -f ~/.ssh/github_actions
   ```
2. Add the public key (`~/.ssh/github_actions.pub`) to `/root/.ssh/authorized_keys` on your droplet:
   ```bash
   cat ~/.ssh/github_actions.pub >> ~/.ssh/authorized_keys
   ```

### Step 6.2: Add Secrets to GitHub Repository
Go to **GitHub Repo** $\rightarrow$ **Settings** $\rightarrow$ **Secrets and variables** $\rightarrow$ **Actions** $\rightarrow$ **New repository secret**:

| Secret Name | Value |
| :--- | :--- |
| `DROPLET_HOST` | Your Droplet's IP address (or domain `api.yourdomain.com`) |
| `DROPLET_USER` | `root` (or your droplet user with docker privileges) |
| `DROPLET_SSH_KEY` | Contents of the private key (`~/.ssh/github_actions`) |

Now, whenever you push code to `main`, GitHub Actions will SSH into the droplet, pull changes, and rebuild containers automatically!

---

## 7. Configuring Pabbly Webhook

Now that your server is live on HTTPS, configure the webhook on Pabbly:

1. Go to **[https://payments.pabbly.com/app/setting/webhook-create](https://payments.pabbly.com/app/setting/webhook-create)**.
2. Fill in the details:
   - **Webhook Name**: `Course Coupon Service - Production`
   - **Webhook URL**:
     ```text
     https://api.yourdomain.com/api/v1/webhooks/pabbly/<YOUR_PABBLY_WEBHOOK_SECRET>
     ```
     *(Replace `<YOUR_PABBLY_WEBHOOK_SECRET>` with the value from your droplet's `.env`)*
   - **Select Products**: `All Products`
   - **Authentication**: `None` (authentication is embedded in the URL secret)
3. **Select Events**:
   - Check **Successful Payment** (or *Payment Success* / *Payment Made*)
   - Check **Payment Failure** (or *Payment Failed*)
   - Check **Payment Refund** (or *Payment Refunded* / *Refund*)
4. Click **Save Webhook**.

Pabbly will ping your endpoint, receive `{"status":"received"}` with HTTP 200 OK, and save successfully.

---

## 8. Common Management Commands

```bash
# View live container logs
docker compose logs -f

# Force catalog sync from Pabbly right now
curl -X POST https://api.yourdomain.com/api/v1/admin/providers/pabbly/sync

# Force webhook processing right now
curl -X POST https://api.yourdomain.com/api/v1/admin/webhooks/process

# Restart containers
docker compose restart

# Stop the stack
docker compose down
```
