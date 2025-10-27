# SelfUp Integration - Application Update Monitoring

Pulse includes integrated application update monitoring capabilities inspired by the SelfUp project. This feature allows you to track application versions across your infrastructure and receive notifications when updates are available.

## Overview

The SelfUp integration provides:
- **Multi-Provider Support**: Monitor updates from GitHub releases, Docker Hub, custom registries, and more
- **Automated Checking**: Configurable intervals for update checking
- **Visual Dashboard**: Dedicated interface showing all monitored applications and their update status
- **Alert Integration**: Notifications through Pulse's existing alert system
- **Version Tracking**: Historical view of application versions and update patterns
- **Privacy-First**: All update checking performed locally with no external telemetry

## Getting Started

### Accessing SelfUp

1. Log into your Pulse dashboard
2. Navigate to the **SelfUp** tab in the main navigation
3. You'll see the SelfUp dashboard with application overview and statistics

### Adding Applications

1. Click the **"Add App"** button in the SelfUp dashboard
2. Fill in the application details:
   - **Name**: Display name for your application
   - **Provider**: Select from GitHub, Docker Hub, or custom registry
   - **Repository/Image**: The repository or image identifier
   - **Current Version**: Your currently deployed version (optional)
   - **Check Interval**: How often to check for updates (hourly, daily, weekly)

3. Click **"Save"** to start monitoring the application

### Supported Providers

#### GitHub Releases
- **Format**: `owner/repository`
- **Example**: `rcourtman/Pulse`
- Monitors GitHub releases and tags

#### Docker Hub
- **Format**: `namespace/image` or `image` (for official images)
- **Example**: `rcourtman/pulse` or `nginx`
- Monitors Docker image tags

#### Custom Registry
- **Format**: Custom URL or identifier
- Configure custom endpoints for private registries

## Dashboard Features

### Application Overview
- **Application Cards**: Visual cards showing each monitored application
- **Update Status**: Clear indicators for up-to-date, update available, or checking status
- **Version Information**: Current and latest available versions
- **Provider Icons**: Visual identification of update sources

### Statistics Panel
- **Total Applications**: Number of monitored applications
- **Updates Available**: Count of applications with pending updates
- **Up to Date**: Count of applications on latest versions
- **Last Check**: Timestamp of most recent update check

### Actions
- **Check All**: Manually trigger update checks for all applications
- **Individual Checks**: Check specific applications on demand
- **Bulk Operations**: Manage multiple applications simultaneously

## Alert Configuration

### Enabling Alerts
1. Go to **Settings → Alerts** in Pulse
2. Configure SelfUp alert rules:
   - **Update Available**: Notify when new versions are detected
   - **Check Failures**: Alert on update checking errors
   - **Version Changes**: Notify when applications are updated

### Notification Channels
SelfUp alerts integrate with all existing Pulse notification methods:
- Email notifications
- Discord webhooks
- Slack integration
- Telegram messages
- Custom webhooks

## Configuration

### Update Check Intervals
- **Hourly**: For critical applications requiring immediate update awareness
- **Daily**: Standard interval for most applications
- **Weekly**: For stable applications with infrequent updates
- **Custom**: Define specific intervals as needed

### Version Comparison
- **Semantic Versioning**: Automatic parsing of semver versions (1.2.3)
- **Date-based**: Support for date-based versioning schemes
- **Custom Patterns**: Configure custom version comparison logic

## API Integration

### REST Endpoints
```bash
# List all monitored applications
GET /api/selfup/apps

# Add new application
POST /api/selfup/apps
{
  "name": "My App",
  "provider": "github",
  "repository": "owner/repo",
  "current_version": "1.0.0",
  "check_interval": "daily"
}

# Trigger update check
POST /api/selfup/apps/{id}/check

# Get application details
GET /api/selfup/apps/{id}

# Update application configuration
PUT /api/selfup/apps/{id}

# Delete application
DELETE /api/selfup/apps/{id}
```

### Webhook Payloads
```json
{
  "event": "update_available",
  "application": {
    "id": "123",
    "name": "My Application",
    "provider": "github",
    "repository": "owner/repo",
    "current_version": "1.0.0",
    "latest_version": "1.1.0",
    "update_url": "https://github.com/owner/repo/releases/tag/v1.1.0"
  },
  "timestamp": "2024-01-15T10:30:00Z"
}
```

## Security Considerations

### Privacy
- All update checking is performed locally by your Pulse instance
- No telemetry or usage data is sent to external services
- Application configurations stored securely in your Pulse database

### Authentication
- SelfUp features require Pulse authentication
- API access controlled by existing Pulse API token system
- All operations logged in Pulse audit trail

### Network Security
- Update checks use standard HTTPS connections
- Configurable proxy support for restricted networks
- Rate limiting to prevent API abuse

## Troubleshooting

### Common Issues

#### Update Checks Failing
1. Verify network connectivity to provider APIs
2. Check API rate limits (especially for GitHub)
3. Validate repository/image names and accessibility
4. Review Pulse logs for detailed error messages

#### Incorrect Version Detection
1. Ensure version format matches provider standards
2. Check for pre-release or beta versions
3. Verify semantic versioning compliance
4. Configure custom version patterns if needed

#### Missing Notifications
1. Confirm alert rules are properly configured
2. Test notification channels independently
3. Check alert thresholds and conditions
4. Verify application update status

### Logs and Debugging
```bash
# View SelfUp-specific logs
docker logs pulse | grep selfup

# Enable debug logging
# Add to environment: LOG_LEVEL=debug

# Check update check history
# Available in Pulse UI under SelfUp → History
```

## Migration from Standalone SelfUp

If you're migrating from a standalone SelfUp installation:

1. **Export Configuration**: Export your application list from standalone SelfUp
2. **Import to Pulse**: Use the bulk import feature in Pulse SelfUp
3. **Verify Settings**: Confirm all applications and intervals are correctly configured
4. **Test Notifications**: Ensure alert channels are working as expected
5. **Decommission**: Safely shut down standalone SelfUp instance

## Best Practices

### Application Management
- Use descriptive names for easy identification
- Group related applications with consistent naming
- Set appropriate check intervals based on update frequency
- Regularly review and clean up unused applications

### Alert Configuration
- Start with conservative alert settings
- Use different notification channels for different priority levels
- Test alert rules before deploying to production
- Document alert escalation procedures

### Performance Optimization
- Stagger update checks to avoid API rate limits
- Use longer intervals for stable applications
- Monitor Pulse resource usage with many applications
- Consider caching strategies for frequently checked applications

## Support

For SelfUp integration support:
- Check the [main Pulse documentation](../README.md)
- Review [troubleshooting guide](TROUBLESHOOTING.md)
- Submit issues on the [Pulse GitHub repository](https://github.com/RouXx67/PulseUP/issues)
- Join the community discussions for tips and best practices
