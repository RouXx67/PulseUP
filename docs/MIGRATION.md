# Migrating Pulse

**Updated for Pulse v4.24.0**

## Quick Migration Guide

### ❌ DON'T: Copy files directly
Never copy `/etc/pulse` or `/var/lib/pulse` directories between systems:
- The encryption key is tied to the files
- Credentials may be exposed
- Configuration may not work on different systems

### ✅ DO: Use Export/Import

#### Exporting (Old Server)
1. Open Pulse web interface
2. Go to **Settings** → **Configuration Management**
3. Click **Export Configuration**
4. Enter a strong passphrase (you'll need this for import!)
5. Save the downloaded file securely

#### Importing (New Server)
1. Install fresh Pulse instance
2. Open Pulse web interface
3. Go to **Settings** → **Configuration Management**
4. Click **Import Configuration**
5. Select your exported file
6. Enter the same passphrase
7. Click Import
8. **Post-migration verification (v4.24.0+)**:
   - Check scheduler health: `curl -s http://localhost:7655/api/monitoring/scheduler/health | jq`
   - Verify adaptive polling status: **Settings → System → Monitoring**
   - Confirm all nodes are connected and polling correctly

## What Gets Migrated

✅ **Included:**
- All PVE/PBS nodes and credentials
- Alert settings and thresholds
- Email configuration
- Webhook configurations
- System settings
- Guest metadata (custom URLs, notes)

❌ **Not Included:**
- Historical metrics data
- Alert history
- Authentication settings (passwords, API tokens)
- **Updates rollback history** (v4.24.0+)
- Each instance should configure its own authentication
- **Note:** Updates rollback data isn't transferred and must be rebuilt by running one successful update cycle on the new host

## Common Scenarios

### Moving to New Hardware
1. Export from old server
2. Shut down old Pulse instance
3. Install Pulse on new hardware
4. Import configuration
5. Verify all nodes are connected

### Docker to Systemd (or vice versa)
The export/import process works across all installation methods:
- Docker → Systemd ✅
- Systemd → Docker ✅
- Docker → LXC ✅

### Backup Strategy
**Weekly Backups:**
1. Export configuration weekly
2. Store exports with date: `pulse-backup-2024-01-15.enc`
3. Keep last 4 backups
4. Store passphrase securely (password manager)

### Disaster Recovery
1. Install Pulse: `curl -sL https://github.com/RouXx67/PulseUP/releases/latest/download/install.sh | bash`
2. Import latest backup
3. System restored in under 5 minutes!

## Security Notes

- **Passphrase Protection**: Exports are encrypted with PBKDF2 (100,000 iterations)
- **Safe to Store**: Encrypted exports can be stored in cloud backups
- **Minimum 12 characters**: Use a strong passphrase
- **Password Manager**: Store your passphrase securely
- **Rollback History**: Updates rollback data isn't included in exports; rebuild by running one successful update on the new host

## Troubleshooting

**"Invalid passphrase" error**
- Ensure you're using the exact same passphrase
- Check for extra spaces or capitalization

**Missing nodes after import**
- Verify the export was taken after adding the nodes
- Check Settings to ensure nodes are listed

**Connection errors after import**
- Node IPs may have changed
- Update node addresses in Settings

**Logging issues after migration (v4.24.0+)**
- If you lose logs after migration, ensure the runtime logging configuration persisted
- Toggle **Settings → System → Logging** to your desired level
- Check environment variables: `LOG_LEVEL`, `LOG_FORMAT`
- Verify log file rotation settings are correct

## Pro Tips

1. **Test imports**: Try importing on a test instance first
2. **Document changes**: Note any manual configs not in Pulse
3. **Version matching**: Best to import into same or newer Pulse version
4. **Network access**: Ensure new server can reach all nodes

---

*Remember: Export/Import is the ONLY supported migration method. Direct file copying is not supported and may result in data loss.*
