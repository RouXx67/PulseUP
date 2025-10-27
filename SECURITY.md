# Pulse Security

This document is the canonical security policy for Pulse. It combines our
ongoing hardening guidance with the operational checklists that previously lived
in `docs/SECURITY.md`.

---

## Critical Security Notice for Container Deployments

### Container SSH Key Policy (BREAKING CHANGE)

**Effective immediately, SSH-based temperature monitoring is blocked in
containerized Pulse deployments.**

#### Why This Change?

Storing SSH private keys inside Docker/LXC containers creates an unacceptable
risk in production environments:

- **Container compromise = infrastructure compromise** – if an attacker gains
  shell access to the Pulse container they obtain the SSH private keys used to
  reach your Proxmox hosts.
- **Keys persist in images** – private keys survive in image layers and can leak
  when images are pushed to registries or shared.
- **No key rotation** – long-lived keys inside containers are difficult to
  rotate safely.
- **Violates least-privilege** – monitoring containers should not hold
  credentials that grant host-level access to the infrastructure they observe.

#### Affected Deployments

✅ **Not affected** – Pulse installed directly on a VM or bare-metal host (no
containers), or homelab environments where you explicitly accept the risk.

❌ **Blocked** – Pulse running in Docker containers, LXC containers, or any
environment where `PULSE_DOCKER=true`/`/.dockerenv` is detected.

#### Migration Path (Production)

1. **Deploy `pulse-sensor-proxy` on each Proxmox host**
   ```bash
   curl -o /usr/local/bin/pulse-sensor-proxy \
     https://github.com/RouXx67/PulseUp/releases/latest/download/pulse-sensor-proxy
   chmod +x /usr/local/bin/pulse-sensor-proxy
   ```
2. **Create a systemd unit** (`/etc/systemd/system/pulse-sensor-proxy.service`)
   ```ini
   [Unit]
   Description=Pulse Temperature Sensor Proxy
   After=network.target

   [Service]
   Type=simple
   User=root
   ExecStart=/usr/local/bin/pulse-sensor-proxy
   Restart=on-failure

   [Install]
   WantedBy=multi-user.target
   ```
3. **Enable and start the service**
   ```bash
   systemctl daemon-reload
   systemctl enable --now pulse-sensor-proxy
   ```
4. **Restart the Pulse container** so it binds to the proxy socket. The
   container will automatically fall back to socket-based temperature polling.

#### Removing Old SSH Keys

If you previously generated SSH keys inside containers:

```bash
# On each Proxmox host
sed -i '/# pulse-/d' /root/.ssh/authorized_keys

# Inside the Pulse container (or rebuild the container)
docker exec pulse rm -rf /home/pulse/.ssh/id_ed25519*
```

#### Security Boundary

```
┌─────────────────────────────────────┐
│  Proxmox Host                       │
│  ┌───────────────────────────────┐  │
│  │  pulse-sensor-proxy (root)    │  │
│  │  · Runs sensors -j            │  │
│  │  · Exposes Unix socket only   │  │
│  └───────────────────────────────┘  │
│            │                         │
│            │ /run/pulse-sensor-proxy.sock
│            │                         │
│  ┌─────────▼─────────────────────┐  │
│  │  Pulse container (bind mount) │  │
│  │  · No SSH keys                │  │
│  │  · No host root privileges    │  │
│  └───────────────────────────────┘  │
└─────────────────────────────────────┘
```

#### Homelab Exception

If you fully understand the risk and are **not** containerized (VM/bare-metal
install), the legacy SSH flow still works. Use a dedicated monitoring user,
restrict the key with `command="sensors -j"` and `from="<pulse-ip>"`, and
rotate keys regularly.

#### Auditing Your Deployment

```bash
# Detect vulnerable containers
ls /home/pulse/.ssh/id_ed25519* 2>/dev/null && echo "⚠️  SSH keys present"

# Check container logs for proxy detection
docker logs pulse | grep -i "temperature proxy detected"

# Verify the host service
systemctl status pulse-sensor-proxy
```

**Documentation:** https://docs.pulseapp.io/security/containerized-deployments  
**Issues:** https://github.com/RouXx67/PulseUp/issues  
**Private disclosures:** security@pulseapp.io

---

## Mandatory Authentication

**Starting with v4.5.0, authentication setup is prompted for all new Pulse
installations.** This protects your Proxmox API credentials from unauthorized
access.

> **Service name note:** systemd deployments use `pulse.service`. If you're
> upgrading from an older install that still registers `pulse-backend.service`,
> substitute that name in the commands below.

### First-Run Security Setup
When you first access Pulse, you'll be guided through a mandatory security
setup:
- Create your admin username and password
- Automatic API token generation for automation
- Settings are applied immediately without restart
- **Your existing nodes and settings are preserved**

## Smart Security Context

### Public Access Detection
Pulse automatically detects when it's being accessed from public networks:
- **Private networks**: local/RFC1918 addresses (192.168.x.x, 10.x.x.x, etc.)
- **Public networks**: any non-private IP address
- **Stronger warnings**: red alerts when accessed from public IPs without
  authentication

### Trusted Networks Configuration (Deprecated)
**Note:** authentication is now mandatory regardless of network location.

Legacy configuration (no longer applicable):
```bash
# Environment variable (comma-separated CIDR blocks)
PULSE_TRUSTED_NETWORKS=192.168.1.0/24,10.0.0.0/24

# Or in systemd
sudo systemctl edit pulse
[Service]
Environment="PULSE_TRUSTED_NETWORKS=192.168.1.0/24,10.0.0.0/24"
```

When configured:
- Access from trusted networks: no auth required
- Access from outside: authentication enforced
- Useful for: mixed home/remote access scenarios

## Security Warning System

Pulse includes a non-intrusive security warning system that helps you
understand your security posture.

### Security Score
Your instance receives a score from 0‑5 based on:
- ✅ Credentials encrypted at rest (always enabled)
- ✅ Export/import protection
- ⚠️ Authentication enabled
- ⚠️ HTTPS connection
- ⚠️ Audit logging

### Dismissing Warnings
If you're comfortable with your security setup, you can dismiss warnings:
- **For 1 day** – reminder tomorrow
- **For 1 week** – reminder next week
- **Forever** – won't show again

## Credential Security

### Encrypted at Rest (AES-256-GCM)
- **Node credentials**: passwords and API tokens (`/etc/pulse/nodes.enc`)
- **Email settings**: SMTP passwords (`/etc/pulse/email.enc`)
- **Webhook data**: URLs and auth headers (`/etc/pulse/webhooks.enc`) – v4.1.9+
- **Encryption key**: auto-generated (`/etc/pulse/.encryption.key`)

### Security Features
- **Logs**: token values masked with `***` in all outputs
- **API**: frontend receives only `hasToken: true`, never actual values
- **Export**: requires a valid API token (`X-API-Token` header or `token`
  parameter) to extract credentials
- **Migration**: use passphrase-protected export/import (see
  [Migration Guide](docs/MIGRATION.md))
- **Auto-migration**: unencrypted configs automatically migrate to encrypted
  format

## Export/Import Protection

By default, configuration export/import is blocked. You have two options:

### Option 1: Set API Tokens (Recommended)
```bash
# Using systemd (secure)
sudo systemctl edit pulse
# Add:
[Service]
Environment="API_TOKENS=ansible-token,docker-agent-token"
Environment="API_TOKEN=legacy-token"

# Then restart:
sudo systemctl restart pulse

# Docker
docker run -e API_TOKENS=ansible-token,docker-agent-token rcourtman/pulse:latest
```

### Option 2: Allow Unprotected Export (Homelab)
```bash
# Using systemd
sudo systemctl edit pulse
# Add:
[Service]
Environment="ALLOW_UNPROTECTED_EXPORT=true"

# Docker
docker run -e ALLOW_UNPROTECTED_EXPORT=true rcourtman/pulse:latest
```

**Note:** for production, prefer Docker secrets or systemd environment files
for sensitive data.

## Security Features

### Core Protection
- **Encryption**: credentials encrypted at rest (AES-256-GCM)
- **Export protection**: exports always encrypted with a passphrase
- **Minimum passphrase**: 12 characters required for exports
- **Security tab**: check status in *Settings → Security*

### Enterprise Security (When Authentication Enabled)
- **Password security**
  - Bcrypt hashing with cost factor 12 (60‑character hash)
  - Passwords never stored in plain text
  - Automatic hashing during security setup
  - **Critical**: bcrypt hashes must be exactly 60 characters
- **API token security**
  - 64‑character hex tokens (32 bytes entropy)
  - SHA3-256 hashed before storage (64‑character hash)
  - Raw token shown only once
  - Tokens never stored in plain text
  - Live reloading when `.env` changes
  - API-only mode supported (no password auth required)
- **CSRF protection**: all state-changing operations require CSRF tokens
- **Rate limiting** (enhanced in v4.24.0)
  - Auth endpoints: 10 attempts/minute per IP (returns `Retry-After` header)
  - General API: 500 requests/minute per IP
  - Real-time endpoints exempt for functionality
  - **New in v4.24.0**: All responses include rate limit headers:
    - `X-RateLimit-Limit`: Maximum requests per window
    - `X-RateLimit-Remaining`: Requests remaining in current window
    - `Retry-After`: Seconds to wait before retrying (on 429 responses)
- **Account lockout**
  - Locks after 5 failed login attempts
  - 15-minute automatic lockout duration
  - Clear feedback showing remaining attempts
  - Time remaining displayed when locked
  - Manual reset available via API for admins
- **Session management**
  - Secure HttpOnly cookies
  - 24-hour session expiry
  - Session invalidation on password change
- **Security headers**
  - Content-Security-Policy
  - X-Frame-Options: DENY
  - X-Content-Type-Options: nosniff
  - X-XSS-Protection: 1; mode=block
  - Referrer-Policy: strict-origin-when-cross-origin
  - Permissions-Policy restricting sensitive APIs
- **Audit logging** (enhanced in v4.24.0)
  - Authentication events include IP addresses
  - **New**: Rollback actions are logged with timestamps and metadata
  - **New**: Scheduler health escalations recorded in audit trail
  - **New**: Runtime logging configuration changes tracked

### What's Encrypted in Exports
- Node credentials (passwords, API tokens)
- PBS credentials
- Email settings passwords
- Webhook URLs and authentication headers (v4.1.9+)

### What's **Not** Encrypted
- Node hostnames and IPs
- Threshold settings
- General configuration
- Alert rules and schedules

## Authentication Workflows

Pulse supports multiple authentication methods that can be used independently or
together.

### Password Authentication

#### Quick Security Setup (Recommended)
1. Navigate to *Settings → Security*.
2. Click **Enable Security Now**.
3. Enter username and password.
4. Save the generated API token (shown only once!).
5. Security is enabled immediately (no restart needed).

This automatically:
- Generates a secure random password
- Hashes it with bcrypt (cost factor 12)
- Creates secure API token (SHA3-256 hashed, raw token shown once)
- For systemd: Configures systemd with hashed credentials
- For Docker: Saves to `/data/.env` with hashed credentials (properly quoted to prevent shell expansion)
- Restarts service/container with authentication enabled

#### Manual Setup (Advanced)
```bash
# Using systemd (password will be hashed automatically)
sudo systemctl edit pulse
# Add:
[Service]
Environment="PULSE_AUTH_USER=admin"
Environment="PULSE_AUTH_PASS=$2a$12$..."  # Use bcrypt hash, not plain text!

# Docker (credentials persist in volume via .env file)
# IMPORTANT: Always quote bcrypt hashes to prevent shell expansion!
docker run -e PULSE_AUTH_USER=admin -e PULSE_AUTH_PASS='$2a$12$...' rcourtman/pulse:latest
# Or use Quick Security Setup and restart container
```

**Important**: Always use hashed passwords in configuration. Use the Quick Security Setup or generate bcrypt hashes manually.

#### Features
- Web UI login required when authentication enabled
- Change/remove password from Settings → Security  
- Passwords ALWAYS hashed with bcrypt (cost 12)
- Session-based authentication with secure HttpOnly cookies
- 24-hour session expiry
- CSRF protection for all state-changing operations
- Session invalidation on password change

### API Token Authentication  
For programmatic access and automation. API tokens are SHA3-256 hashed for security.

#### Token Setup via Quick Security
The Quick Security Setup automatically:
- Generates a cryptographically secure token
- Hashes it with SHA3-256
- Stores only the 64-character hash
- Adds the token to the managed token list

#### Manual Token Setup
```bash
# Using systemd (plain text values are auto-hashed on startup)
sudo systemctl edit pulse
# Add:
[Service]
Environment="API_TOKENS=ansible-token,docker-agent-token"

# Docker
docker run -e API_TOKENS=ansible-token,docker-agent-token rcourtman/pulse:latest

# To provide pre-hashed tokens instead, list the SHA3-256 hashes
# Environment="API_TOKENS=83c8...,b1de..."
```

**Security Note**: Tokens defined via environment variables are hashed with SHA3-256 before being stored on disk. Plain values never persist beyond startup.

#### Token Management (Settings → Security → API tokens)
- Issue dedicated tokens for automation/agents without sharing a global credential
- View prefixes/suffixes and last-used timestamps for auditing
- Revoke tokens individually without downtime
- Regenerate tokens when rotating credentials (new value displayed once)
- All tokens stored as SHA3-256 hashes

#### Usage
```bash
# Include the ORIGINAL token (not hash) in X-API-Token header
curl -H "X-API-Token: your-original-token" http://localhost:7655/api/health

# Or in query parameter for export/import
curl "http://localhost:7655/api/export?token=your-original-token"
```

### Auto-Registration Security

#### Default Mode
- All access requires authentication
- Nodes can auto-register with the API token
- Setup scripts work without additional configuration

#### Secure Mode
- Require API token for all operations
- Protects auto-registration endpoint
- Enable by setting at least one API token via `API_TOKENS` (or legacy `API_TOKEN`) environment variable

### Runtime Logging Configuration

**New in v4.24.0:** Adjust logging settings dynamically without restarting Pulse.

#### Security Benefits
- Enable debug logging temporarily for incident investigation
- Switch to JSON format for SIEM integration
- Adjust verbosity based on security posture
- Control file rotation to manage audit log retention

#### Configuration Options

**Via UI:**
Navigate to **Settings → System → Logging**:
- **Log Level**: `debug`, `info`, `warn`, `error`
- **Log Format**: `json` (for log aggregation), `text` (human-readable)
- **File Rotation**: size limits, retention policies

**Via Environment Variables:**
```bash
# Systemd
sudo systemctl edit pulse
[Service]
Environment="LOG_LEVEL=info"
Environment="LOG_FORMAT=json"
Environment="LOG_MAX_SIZE=100"        # MB per log file
Environment="LOG_MAX_BACKUPS=10"      # Number of rotated logs to keep
Environment="LOG_MAX_AGE=30"          # Days to retain logs

# Docker
docker run \
  -e LOG_LEVEL=info \
  -e LOG_FORMAT=json \
  -e LOG_MAX_SIZE=100 \
  -e LOG_MAX_BACKUPS=10 \
  -e LOG_MAX_AGE=30 \
  rcourtman/pulse:latest
```

**Security Considerations:**
- Debug logs may contain sensitive data—enable only when needed
- JSON format recommended for security monitoring and SIEM
- Adjust retention based on compliance requirements
- Changes are logged to audit trail

## CORS (Cross-Origin Resource Sharing)

By default, Pulse only allows same-origin requests (no CORS headers). This is the most secure configuration.

### Configuring CORS for External Access

If you need to access Pulse API from a different domain:

```bash
# Docker
docker run -e ALLOWED_ORIGINS="https://app.example.com" rcourtman/pulse:latest

# systemd
sudo systemctl edit pulse
[Service]
Environment="ALLOWED_ORIGINS=https://app.example.com"

# Multiple origins (comma-separated)
ALLOWED_ORIGINS="https://app.example.com,https://dashboard.example.com"

# Development mode (allows localhost)
PULSE_DEV=true
```

**Security Note**: Never use `ALLOWED_ORIGINS=*` in production as it allows any website to access your API.

## Monitoring and Observability

### Scheduler Health API

**New in v4.24.0:** Monitor Pulse's internal health and detect anomalies using the scheduler health API.

#### Endpoint
```bash
curl -s http://localhost:7655/api/monitoring/scheduler/health | jq
```

#### Security Use Cases
1. **Anomaly Detection**
   - Watch for unusual queue depths (possible DoS)
   - Monitor circuit breaker trips (connectivity issues or attacks)
   - Track backoff patterns (rate limiting, potential probes)

2. **Performance Monitoring**
   - Identify performance degradation
   - Detect resource exhaustion
   - Track API response times

3. **Incident Response**
   - Real-time visibility into system health
   - Historical metrics for post-incident analysis
   - Circuit breaker status for failover decisions

#### Key Security Metrics
- **Queue Depth**: High values may indicate attack or overload
- **Circuit Breaker Status**: Half-open/open states suggest connectivity issues
- **Backoff Delays**: Increased backoff may indicate rate limiting or errors
- **Error Rates**: Track failed API calls and authentication attempts

**Dashboard Access:**
Navigate to **Settings → System → Monitoring** for visual representation of scheduler health.

## Security Best Practices

### Credential Storage
- ✅ **DO**: Use Quick Security Setup for automatic hashing
- ✅ **DO**: Store only bcrypt hashes for passwords
- ✅ **DO**: Store only SHA3-256 hashes for API tokens
- ❌ **DON'T**: Store plain text passwords in config files
- ❌ **DON'T**: Store plain text API tokens in config files
- ❌ **DON'T**: Log credentials or include them in backups

### Authentication Setup
- ✅ **DO**: Use strong, unique passwords (16+ characters)
- ✅ **DO**: Rotate API tokens periodically
- ✅ **DO**: Use HTTPS in production environments
- ❌ **DON'T**: Share API tokens between users/services
- ❌ **DON'T**: Embed credentials in client-side code

### Verification
Run the security verification script to ensure no plain text credentials:
```bash
/opt/pulse/testing-tools/security-verification.sh
```

This checks:
- No hardcoded credentials in code
- No credentials exposed in logs
- All passwords/tokens properly hashed
- Secure file permissions
- No credential leaks in API responses

## Account Lockout and Recovery

### Lockout Behavior
- After **5 failed login attempts**, the account is locked for **15 minutes**
- Lockout applies to both username and IP address
- Login form shows remaining attempts after each failure
- Clear message when locked with time remaining

### Automatic Recovery
- Lockouts automatically expire after 15 minutes
- No action needed - just wait for the timer to expire
- Successful login clears all failed attempt counters

### Manual Recovery (Admin)
Administrators with API access can manually reset lockouts:

```bash
# Reset lockout for a specific username
curl -X POST http://localhost:7655/api/security/reset-lockout \
  -H "X-API-Token: your-api-token" \
  -H "Content-Type: application/json" \
  -d '{"identifier":"username"}'

# Reset lockout for an IP address
curl -X POST http://localhost:7655/api/security/reset-lockout \
  -H "X-API-Token: your-api-token" \
  -H "Content-Type: application/json" \
  -d '{"identifier":"192.168.1.100"}'
```

## Troubleshooting

**Account locked?** Wait 15 minutes or contact admin for manual reset  
**Export blocked?** You're on a public network – login with password, set an API token (`API_TOKENS`), or set `ALLOW_UNPROTECTED_EXPORT=true`  
**Rate limited?** Wait 1 minute and try again  
**Can't login?** Check `PULSE_AUTH_USER` and `PULSE_AUTH_PASS` environment variables  
**API access denied?** Verify the token you supplied matches one of the values created in *Settings → Security → API tokens* (use the original token, not the hash)  
**CORS errors?** Configure `ALLOWED_ORIGINS` for your domain  
**Forgot password?** Start fresh – delete your Pulse data and restart

---

_Last updated: 2025-10-20_

**Version 4.24.0 Security Enhancements:**
- ✅ X-RateLimit-* headers for all API responses
- ✅ Runtime logging configuration for incident response
- ✅ Scheduler health API for anomaly detection
- ✅ Enhanced audit logging (rollback actions, scheduler events)
- ✅ Adaptive polling with circuit breakers and backoff
- ✅ Shared script library system (secure installer patterns)
