import { createSignal, Show, For, createMemo, createEffect, onMount, onCleanup } from 'solid-js';
import { usePersistentSignal } from '@/hooks/usePersistentSignal';
import type { JSX } from 'solid-js';
import { EmailProviderSelect } from '@/components/Alerts/EmailProviderSelect';
import { WebhookConfig } from '@/components/Alerts/WebhookConfig';
import { ThresholdsTable } from '@/components/Alerts/ThresholdsTable';
import type { RawOverrideConfig, PMGThresholdDefaults, SnapshotAlertConfig, BackupAlertConfig } from '@/types/alerts';
import { Card } from '@/components/shared/Card';
import { SectionHeader } from '@/components/shared/SectionHeader';
import { SettingsPanel } from '@/components/shared/SettingsPanel';
import { Toggle } from '@/components/shared/Toggle';
import { formField, formControl, formHelpText, labelClass, controlClass } from '@/components/shared/Form';
import { ScrollableTable } from '@/components/shared/ScrollableTable';
import { useWebSocket } from '@/App';
import { showSuccess, showError } from '@/utils/toast';
import { showTooltip, hideTooltip } from '@/components/shared/Tooltip';
import { AlertsAPI } from '@/api/alerts';
import { NotificationsAPI, Webhook } from '@/api/notifications';
import type { EmailConfig, AppriseConfig } from '@/api/notifications';
import type { HysteresisThreshold } from '@/types/alerts';
import type { Alert, State, VM, Container, DockerHost, DockerContainer } from '@/types/api';
import { useNavigate, useLocation } from '@solidjs/router';
import { useAlertsActivation } from '@/stores/alertsActivation';
import LayoutDashboard from 'lucide-solid/icons/layout-dashboard';
import History from 'lucide-solid/icons/history';
import Gauge from 'lucide-solid/icons/gauge';
import Send from 'lucide-solid/icons/send';
import Calendar from 'lucide-solid/icons/calendar';

type AlertTab = 'overview' | 'thresholds' | 'destinations' | 'schedule' | 'history';

const ALERT_HEADER_META: Record<AlertTab, { title: string; description: string }> = {
  overview: {
    title: 'Alerts Overview',
    description: 'Monitor active alerts, acknowledgements, and recent status changes across platforms.',
  },
  thresholds: {
    title: 'Alert Thresholds',
    description: 'Tune resource thresholds and override rules for nodes, guests, and containers.',
  },
  destinations: {
    title: 'Notification Destinations',
    description: 'Configure email, webhooks, and escalation paths for alert delivery.',
  },
  schedule: {
    title: 'Maintenance Schedule',
    description: 'Set quiet hours and maintenance windows to suppress alerts when expected changes occur.',
  },
  history: {
    title: 'Alert History',
    description: 'Review previously triggered alerts and their resolution timeline.',
  },
};

export const ALERT_TAB_SEGMENTS: Record<AlertTab, string> = {
  overview: 'overview',
  thresholds: 'thresholds',
  destinations: 'destinations',
  schedule: 'schedule',
  history: 'history',
};

export const pathForTab = (
  tab: AlertTab,
  segments: Record<AlertTab, string> = ALERT_TAB_SEGMENTS,
): string => {
  const segment = segments[tab];
  return segment ? `/alerts/${segment}` : '/alerts';
};

export const tabFromPath = (
  pathname: string,
  segments: Record<AlertTab, string> = ALERT_TAB_SEGMENTS,
): AlertTab => {
  const normalizedPath = pathname.replace(/\/+$/, '') || '/alerts';
  const parts = normalizedPath.split('/').filter(Boolean);

  if (parts[0] !== 'alerts') {
    return 'overview';
  }

  const segment = parts[1] ?? '';
  if (!segment) {
    return 'overview';
  }

  const entry = (Object.entries(segments) as [AlertTab, string][])
    .find(([, value]) => value === segment);

  if (entry) {
    return entry[0];
  }

  if (segment === 'custom-rules') {
    return 'thresholds';
  }

  return 'overview';
};

// Store reference interfaces
interface DestinationsRef {
  emailConfig?: () => EmailConfig;
  appriseConfig?: () => AppriseConfig;
}

// Override interface for both guests and nodes
type OverrideType =
  | 'guest'
  | 'node'
  | 'storage'
  | 'pbs'
  | 'pmg'
  | 'dockerHost'
  | 'dockerContainer';

interface Override {
  id: string; // Full ID (e.g. "Main-node1-105" for guest, "node-node1" for node, "pbs-name" for PBS)
  name: string; // Display name
  type: OverrideType;
  resourceType?: string; // VM, CT, Node, Storage, or PBS
  vmid?: number; // Only for guests
  node?: string; // Node name (for guests and storage), undefined for nodes themselves
  instance?: string;
  disabled?: boolean; // Completely disable alerts for this guest/storage
  disableConnectivity?: boolean; // For nodes - disable offline/connectivity alerts
  poweredOffSeverity?: 'warning' | 'critical';
  thresholds: {
    cpu?: number;
    memory?: number;
    disk?: number;
    diskRead?: number;
    diskWrite?: number;
    networkIn?: number;
    networkOut?: number;
    usage?: number; // For storage devices
  };
}

// Local email config with UI-specific fields
interface UIEmailConfig {
  enabled: boolean;
  provider: string;
  server: string; // Fixed: use 'server' not 'smtpHost'
  port: number; // Fixed: use 'port' not 'smtpPort'
  username: string;
  password: string;
  from: string;
  to: string[];
  tls: boolean;
  startTLS: boolean;
  replyTo: string;
  maxRetries: number;
  retryDelay: number;
  rateLimit: number;
}

interface UIAppriseConfig {
  enabled: boolean;
  mode: 'cli' | 'http';
  targetsText: string;
  cliPath: string;
  timeoutSeconds: number;
  serverUrl: string;
  configKey: string;
  apiKey: string;
  apiKeyHeader: string;
  skipTlsVerify: boolean;
}

interface QuietHoursConfig {
  enabled: boolean;
  start: string;
  end: string;
  timezone: string;
  days: Record<string, boolean>;
  suppress: {
    performance: boolean;
    storage: boolean;
    offline: boolean;
  };
}

interface CooldownConfig {
  enabled: boolean;
  minutes: number;
  maxAlerts: number;
}

interface GroupingConfig {
  enabled: boolean;
  window: number;
  maxGroupSize?: number;
  byNode?: boolean;
  byGuest?: boolean;
}

type EscalationNotifyTarget = 'email' | 'webhook' | 'all';

interface EscalationLevel {
  after: number;
  notify: EscalationNotifyTarget;
}

interface EscalationConfig {
  enabled: boolean;
  levels: EscalationLevel[];
}

const getLocalTimezone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';

export const createDefaultQuietHours = (): QuietHoursConfig => ({
  enabled: false,
  start: '22:00',
  end: '08:00',
  timezone: getLocalTimezone(),
  days: {
    monday: true,
    tuesday: true,
    wednesday: true,
    thursday: true,
    friday: true,
    saturday: false,
    sunday: false,
  },
  suppress: {
    performance: false,
    storage: false,
    offline: false,
  },
});

export const createDefaultCooldown = (): CooldownConfig => ({
  enabled: true,
  minutes: 30,
  maxAlerts: 3,
});

export const createDefaultGrouping = (): GroupingConfig => ({
  enabled: true,
  window: 5,
  byNode: true,
  byGuest: false,
});

const createDefaultAppriseConfig = (): UIAppriseConfig => ({
  enabled: false,
  mode: 'cli',
  targetsText: '',
  cliPath: 'apprise',
  timeoutSeconds: 15,
  serverUrl: '',
  configKey: '',
  apiKey: '',
  apiKeyHeader: 'X-API-KEY',
  skipTlsVerify: false,
});

const parseAppriseTargets = (value: string): string[] =>
  value
    .split(/\r?\n|,/)
    .map((entry) => entry.trim())
    .filter((entry, index, arr) => entry.length > 0 && arr.indexOf(entry) === index);

const formatAppriseTargets = (targets: string[] | undefined | null): string =>
  targets && targets.length > 0 ? targets.join('\n') : '';

export const normalizeMetricDelayMap = (
  input: Record<string, Record<string, number>> | undefined | null,
): Record<string, Record<string, number>> => {
  if (!input) return {};
  const normalized: Record<string, Record<string, number>> = {};

  Object.entries(input).forEach(([rawType, metrics]) => {
    if (!metrics) return;
    const typeKey = rawType.trim().toLowerCase();
    if (!typeKey) return;

    const entries: Record<string, number> = {};
    Object.entries(metrics).forEach(([rawMetric, value]) => {
      if (typeof value !== 'number' || Number.isNaN(value) || value < 0) return;
      const metricKey = rawMetric.trim().toLowerCase();
      if (!metricKey) return;
      entries[metricKey] = Math.round(value);
    });

    if (Object.keys(entries).length > 0) {
      normalized[typeKey] = entries;
    }
  });

  return normalized;
};

export const createDefaultEscalation = (): EscalationConfig => ({
  enabled: false,
  levels: [],
});

export const getTriggerValue = (
  threshold: number | boolean | HysteresisThreshold | undefined,
): number => {
  if (typeof threshold === 'number') {
    return threshold; // Legacy format
  }
  if (typeof threshold === 'boolean') {
    return 0;
  }
  if (threshold && typeof threshold === 'object' && 'trigger' in threshold) {
    return threshold.trigger; // New hysteresis format
  }
  return 0; // Default fallback
};

export const extractTriggerValues = (
  thresholds: RawOverrideConfig,
): Record<string, number> => {
  const result: Record<string, number> = {};
  Object.entries(thresholds).forEach(([key, value]) => {
    // Skip non-threshold fields
    if (key === 'disabled' || key === 'disableConnectivity' || key === 'poweredOffSeverity') return;
    if (typeof value === 'string') return;
    result[key] = getTriggerValue(value);
  });
  return result;
};

const DEFAULT_DELAY_SECONDS = 5;

export function Alerts() {
  const { state, activeAlerts, updateAlert, removeAlerts } = useWebSocket();
  const navigate = useNavigate();
  const location = useLocation();
  const alertsActivation = useAlertsActivation();
  const [isSwitchingActivation, setIsSwitchingActivation] = createSignal(false);
  const isAlertsActive = createMemo(() => alertsActivation.activationState() === 'active');

  const handleActivateAlerts = async () => {
    if (alertsActivation.isLoading() || isSwitchingActivation()) {
      return;
    }
    setIsSwitchingActivation(true);
    try {
      const success = await alertsActivation.activate();
      if (success) {
        showSuccess('Alerts activated! You\'ll now receive alerts when issues are detected.');
        try {
          await alertsActivation.refreshActiveAlerts();
        } catch (error) {
          console.error('Failed to refresh alerts after activation', error);
        }
      } else {
        showError('Unable to activate alerts. Please try again.');
      }
    } finally {
      setIsSwitchingActivation(false);
    }
  };

  const handleDeactivateAlerts = async () => {
    if (isSwitchingActivation()) {
      return;
    }
    setIsSwitchingActivation(true);
    try {
      const success = await alertsActivation.deactivate();
      if (success) {
        showSuccess('Alerts deactivated. Nothing will be sent until you activate them again.');
        try {
          await alertsActivation.refreshActiveAlerts();
        } catch (error) {
          console.error('Failed to refresh alerts after deactivation', error);
        }
      } else {
        showError('Unable to deactivate alerts. Please try again.');
      }
    } catch (error) {
      console.error('Deactivate alerts failed', error);
      showError('Unable to deactivate alerts. Please try again.');
    } finally {
      setIsSwitchingActivation(false);
    }
  };

  const [activeTab, setActiveTab] = createSignal<AlertTab>(tabFromPath(location.pathname));

  const headerMeta = () =>
    ALERT_HEADER_META[activeTab()] ?? {
      title: 'Alerts',
      description: 'Manage alerting configuration.',
    };

  createEffect(() => {
    const currentPath = location.pathname;
    const tab = tabFromPath(currentPath);

    if (tab !== activeTab()) {
      setActiveTab(tab);
    }

    const expectedPath = pathForTab(tab);

    // Allow sub-paths for thresholds tab (e.g., /alerts/thresholds/proxmox)
    const isThresholdsSubPath = tab === 'thresholds' && currentPath.startsWith('/alerts/thresholds/');

    if (currentPath !== expectedPath && !isThresholdsSubPath) {
      navigate(expectedPath, { replace: true });
    }
  });

  createEffect(() => {
    if (!isAlertsActive() && activeTab() !== 'overview') {
      handleTabChange('overview');
    }
  });

  const handleTabChange = (tab: AlertTab) => {
    const targetPath = pathForTab(tab);
    if (location.pathname !== targetPath) {
      navigate(targetPath);
    }
  };

  const [hasUnsavedChanges, setHasUnsavedChanges] = createSignal(false);
  const [isReloadingConfig, setIsReloadingConfig] = createSignal(false);
  const [showAcknowledged, setShowAcknowledged] = createSignal(true);
  // Quick tip visibility state
  const [showQuickTip, setShowQuickTip] = createSignal(
    localStorage.getItem('hideAlertsQuickTip') !== 'true',
  );

  const dismissQuickTip = () => {
    setShowQuickTip(false);
    localStorage.setItem('hideAlertsQuickTip', 'true');
  };

  // Store references to child component data
  let destinationsRef: DestinationsRef = {};

  const [overrides, setOverrides] = createSignal<Override[]>([]);
  const [rawOverridesConfig, setRawOverridesConfig] = createSignal<
    Record<string, RawOverrideConfig>
  >({}); // Store raw config

  // Email configuration state moved to parent to persist across tab changes
const [emailConfig, setEmailConfig] = createSignal<UIEmailConfig>({
  enabled: false,
  provider: '',
  server: '', // Fixed: use 'server' not 'smtpHost'
  port: 587, // Fixed: use 'port' not 'smtpPort'
    username: '',
    password: '',
    from: '',
    to: [] as string[],
    tls: true,
    startTLS: false,
    replyTo: '',
    maxRetries: 3,
    retryDelay: 5,
  rateLimit: 60,
});

const [appriseConfig, setAppriseConfig] = createSignal<UIAppriseConfig>(
  createDefaultAppriseConfig(),
);

  // Schedule configuration state moved to parent to persist across tab changes
  const [scheduleQuietHours, setScheduleQuietHours] =
    createSignal<QuietHoursConfig>(createDefaultQuietHours());

  const [scheduleCooldown, setScheduleCooldown] =
    createSignal<CooldownConfig>(createDefaultCooldown());

  const [scheduleGrouping, setScheduleGrouping] =
    createSignal<GroupingConfig>(createDefaultGrouping());

  const [scheduleEscalation, setScheduleEscalation] =
    createSignal<EscalationConfig>(createDefaultEscalation());

  // Set up destinationsRef.emailConfig function immediately
  destinationsRef.emailConfig = () => {
    const config = emailConfig();
    return {
      enabled: config.enabled,
      provider: config.provider,
      server: config.server, // Fixed: use correct property name
      port: config.port, // Fixed: use correct property name
      username: config.username,
      password: config.password,
      from: config.from,
      to: config.to,
      tls: config.tls,
      startTLS: config.startTLS,
    } as EmailConfig;
  };

  destinationsRef.appriseConfig = () => {
    const config = appriseConfig();
    return {
      enabled: config.enabled,
      mode: config.mode,
      targets: parseAppriseTargets(config.targetsText),
      cliPath: config.cliPath,
      timeoutSeconds: config.timeoutSeconds,
      serverUrl: config.serverUrl,
      configKey: config.configKey,
      apiKey: config.apiKey,
      apiKeyHeader: config.apiKeyHeader,
      skipTlsVerify: config.skipTlsVerify,
    } as AppriseConfig;
  };

  // Process raw overrides config when state changes
  createEffect(() => {
    // Skip this effect if there are unsaved changes to prevent losing focus
    if (hasUnsavedChanges()) {
      return;
    }

    const rawConfig = rawOverridesConfig();
    if (
      Object.keys(rawConfig).length > 0 &&
      state.nodes &&
      state.vms &&
      state.containers &&
      state.storage
    ) {
      // Convert overrides object to array format
      const overridesList: Override[] = [];

      const dockerHostsList: DockerHost[] = state.dockerHosts || [];
      const dockerHostMap = new Map<string, DockerHost>();
      const dockerContainerMap = new Map<string, { host: DockerHost; container: DockerContainer }>();

      dockerHostsList.forEach((host) => {
        dockerHostMap.set(host.id, host);
        (host.containers || []).forEach((container) => {
          const resourceId = `docker:${host.id}/${container.id}`;
          dockerContainerMap.set(resourceId, { host, container });
        });
      });

      Object.entries(rawConfig).forEach(([key, thresholds]) => {
        // Docker host override stored by host ID
        const dockerHost = dockerHostMap.get(key);
        if (dockerHost) {
          overridesList.push({
            id: key,
            name: dockerHost.displayName?.trim() || dockerHost.hostname || dockerHost.id,
            type: 'dockerHost',
            resourceType: 'Docker Host',
            disableConnectivity: thresholds.disableConnectivity || false,
            thresholds: extractTriggerValues(thresholds),
          });
          return;
        }

        // Docker container override stored as docker:hostId/containerId
        const dockerContainer = dockerContainerMap.get(key);
        if (dockerContainer) {
          const { host, container } = dockerContainer;
          const containerName = container.name?.replace(/^\/+/, '') || container.id;
          overridesList.push({
            id: key,
            name: containerName,
            type: 'dockerContainer',
            resourceType: 'Docker Container',
            node: host.hostname,
            instance: host.displayName,
            disabled: thresholds.disabled || false,
            disableConnectivity: thresholds.disableConnectivity || false,
            poweredOffSeverity:
              thresholds.poweredOffSeverity === 'critical'
                ? 'critical'
                : thresholds.poweredOffSeverity === 'warning'
                  ? 'warning'
                  : undefined,
            thresholds: extractTriggerValues(thresholds),
          });
          return;
        }

        if (key.startsWith('docker:')) {
          // Handle docker overrides where the host/container is no longer reporting
          const [, rest] = key.split(':', 2);
          const [hostId, containerId] = (rest || '').split('/', 2);

          if (containerId) {
            overridesList.push({
              id: key,
              name: containerId,
              type: 'dockerContainer',
              resourceType: 'Docker Container',
              node: hostId,
              disabled: thresholds.disabled || false,
              disableConnectivity: thresholds.disableConnectivity || false,
              poweredOffSeverity:
                thresholds.poweredOffSeverity === 'critical'
                  ? 'critical'
                  : thresholds.poweredOffSeverity === 'warning'
                    ? 'warning'
                    : undefined,
              thresholds: extractTriggerValues(thresholds),
            });
            return;
          }

          overridesList.push({
            id: hostId || key,
            name: hostId || key,
            type: 'dockerHost',
            resourceType: 'Docker Host',
            disableConnectivity: thresholds.disableConnectivity || false,
            thresholds: extractTriggerValues(thresholds),
          });
          return;
        }

        // Check if it's a PBS server override (starts with "pbs-")
        if (key.startsWith('pbs-')) {
          const pbs = (state.pbs || []).find((p) => p.id === key);
          if (pbs) {
            overridesList.push({
              id: key,
              name: pbs.name,
              type: 'pbs',
              resourceType: 'PBS',
              disableConnectivity: thresholds.disableConnectivity || false,
              thresholds: extractTriggerValues(thresholds),
            });
          }
        } else {
          // Check if it's a node override by looking for matching node
          const node = (state.nodes || []).find((n) => n.id === key);
          if (node) {
            overridesList.push({
              id: key,
              name: node.name,
              type: 'node',
              resourceType: 'Node',
              disableConnectivity: thresholds.disableConnectivity || false,
              thresholds: extractTriggerValues(thresholds),
            });
          } else {
            // Check if it's a storage device
            const storage = (state.storage || []).find((s) => s.id === key);
            if (storage) {
              overridesList.push({
                id: key,
                name: storage.name,
                type: 'storage',
                resourceType: 'Storage',
                node: storage.node,
                instance: storage.instance,
                disabled: thresholds.disabled || false,
                thresholds: extractTriggerValues(thresholds),
              });
            } else {
              // Find the guest by matching the full ID
              const vm = (state.vms || []).find((g) => g.id === key);
              const container = (state.containers || []).find((g) => g.id === key);
              const guest = vm || container;
              if (guest) {
                overridesList.push({
                  id: key,
                  name: guest.name,
                  type: 'guest',
                  resourceType: guest.type === 'qemu' ? 'VM' : 'CT',
                  vmid: guest.vmid,
                  node: guest.node,
                  instance: guest.instance,
                  disabled: thresholds.disabled || false,
                  poweredOffSeverity:
                    thresholds.poweredOffSeverity === 'critical'
                      ? 'critical'
                      : thresholds.poweredOffSeverity === 'warning'
                        ? 'warning'
                        : undefined,
                  thresholds: extractTriggerValues(thresholds),
                });
              }
            }
          }
        }
      });

      // Only update if there's an actual change to prevent losing edit state
      const currentOverrides = overrides();
      const hasChanged =
        overridesList.length !== currentOverrides.length ||
        overridesList.some((newOverride) => {
          const existing = currentOverrides.find((o) => o.id === newOverride.id);
          if (!existing) return true;
          // Check both thresholds and disableConnectivity for nodes/PBS
          const thresholdsChanged =
            JSON.stringify(newOverride.thresholds) !== JSON.stringify(existing.thresholds);
          const connectivityChanged =
            (newOverride.type === 'node' ||
              newOverride.type === 'pbs' ||
              newOverride.type === 'dockerContainer') &&
            newOverride.disableConnectivity !== existing.disableConnectivity;
          const disabledChanged =
            (newOverride.type === 'guest' || newOverride.type === 'storage') &&
            newOverride.disabled !== existing.disabled;
          const severityChanged =
            (newOverride.type === 'guest' || newOverride.type === 'dockerContainer') &&
            (newOverride.poweredOffSeverity ?? null) !==
              (existing.poweredOffSeverity ?? null);
          return (
            thresholdsChanged ||
            connectivityChanged ||
            disabledChanged ||
            severityChanged
          );
        });

      if (hasChanged) {
        setOverrides(overridesList);
      }
    }
  });

  const loadAlertConfiguration = async (options: { notify?: boolean } = {}) => {
    setIsReloadingConfig(true);
    setHasUnsavedChanges(false);

    // Reset to defaults before applying server state
    setGuestDefaults({
      cpu: 80,
      memory: 85,
      disk: 90,
      diskRead: -1,
      diskWrite: -1,
      networkIn: -1,
      networkOut: -1,
    });
    setGuestDisableConnectivity(false);
    setGuestPoweredOffSeverity('warning');
    setNodeDefaults({
      cpu: 80,
      memory: 85,
      disk: 90,
      temperature: 80,
    });
    setStorageDefault(85);
    setTimeThreshold(DEFAULT_DELAY_SECONDS);
    setTimeThresholds({
      guest: DEFAULT_DELAY_SECONDS,
      node: DEFAULT_DELAY_SECONDS,
      storage: DEFAULT_DELAY_SECONDS,
      pbs: DEFAULT_DELAY_SECONDS,
    });
    setMetricTimeThresholds({});
    setScheduleQuietHours(createDefaultQuietHours());
    setScheduleCooldown(createDefaultCooldown());
    setScheduleGrouping(createDefaultGrouping());
    setScheduleEscalation(createDefaultEscalation());

    setEmailConfig({
      enabled: false,
      provider: '',
      server: '',
      port: 587,
      username: '',
      password: '',
      from: '',
      to: [],
      tls: true,
      startTLS: false,
      replyTo: '',
      maxRetries: 3,
      retryDelay: 5,
      rateLimit: 60,
    });

    setAppriseConfig(createDefaultAppriseConfig());

    try {
      const config = await AlertsAPI.getConfig();

      if (config.guestDefaults) {
        setGuestDefaults({
          cpu: getTriggerValue(config.guestDefaults.cpu) ?? 80,
          memory: getTriggerValue(config.guestDefaults.memory) ?? 85,
          disk: getTriggerValue(config.guestDefaults.disk) ?? 90,
          diskRead: getTriggerValue(config.guestDefaults.diskRead) ?? -1,
          diskWrite: getTriggerValue(config.guestDefaults.diskWrite) ?? -1,
          networkIn: getTriggerValue(config.guestDefaults.networkIn) ?? -1,
          networkOut: getTriggerValue(config.guestDefaults.networkOut) ?? -1,
        });
        setGuestDisableConnectivity(Boolean(config.guestDefaults.disableConnectivity));
        setGuestPoweredOffSeverity(
          config.guestDefaults.poweredOffSeverity === 'critical' ? 'critical' : 'warning',
        );
      } else {
        setGuestDisableConnectivity(false);
        setGuestPoweredOffSeverity('warning');
      }

      if (config.nodeDefaults) {
        setNodeDefaults({
          cpu: getTriggerValue(config.nodeDefaults.cpu) ?? 80,
          memory: getTriggerValue(config.nodeDefaults.memory) ?? 85,
          disk: getTriggerValue(config.nodeDefaults.disk) ?? 90,
          temperature: getTriggerValue(config.nodeDefaults.temperature) ?? 80,
        });
      }

      if (config.dockerDefaults) {
        setDockerDefaults({
          cpu: getTriggerValue(config.dockerDefaults.cpu) ?? 80,
          memory: getTriggerValue(config.dockerDefaults.memory) ?? 85,
          restartCount: config.dockerDefaults.restartCount ?? 3,
          restartWindow: config.dockerDefaults.restartWindow ?? 300,
          memoryWarnPct: config.dockerDefaults.memoryWarnPct ?? 90,
          memoryCriticalPct: config.dockerDefaults.memoryCriticalPct ?? 95,
        });
      }
      setDockerIgnoredPrefixes(config.dockerIgnoredContainerPrefixes ?? []);

      if (config.storageDefault) {
        setStorageDefault(getTriggerValue(config.storageDefault) ?? 85);
      }
      if (config.timeThreshold !== undefined) {
        setTimeThreshold(config.timeThreshold);
      }
      if (config.timeThresholds) {
        setTimeThresholds({
          guest: config.timeThresholds.guest ?? DEFAULT_DELAY_SECONDS,
          node: config.timeThresholds.node ?? DEFAULT_DELAY_SECONDS,
          storage: config.timeThresholds.storage ?? DEFAULT_DELAY_SECONDS,
          pbs: config.timeThresholds.pbs ?? DEFAULT_DELAY_SECONDS,
        });
      } else {
        const fallback = config.timeThreshold && config.timeThreshold > 0 ? config.timeThreshold : DEFAULT_DELAY_SECONDS;
        setTimeThresholds({
          guest: fallback,
          node: fallback,
          storage: fallback,
          pbs: fallback,
        });
      }
      if (config.metricTimeThresholds) {
        setMetricTimeThresholds(normalizeMetricDelayMap(config.metricTimeThresholds));
      } else {
        setMetricTimeThresholds({});
      }

      // Load backup thresholds
      if (config.backupDefaults) {
        const enabled = Boolean(config.backupDefaults.enabled);
        const rawWarning = config.backupDefaults.warningDays ?? FACTORY_BACKUP_DEFAULTS.warningDays;
        const rawCritical = config.backupDefaults.criticalDays ?? FACTORY_BACKUP_DEFAULTS.criticalDays;
        const safeCritical = Math.max(0, rawCritical);
        const normalizedWarning = Math.max(0, rawWarning);
        const warningDays =
          safeCritical > 0 && normalizedWarning > safeCritical ? safeCritical : normalizedWarning;
        const criticalDays = Math.max(safeCritical, warningDays);
        setBackupDefaults({
          enabled,
          warningDays,
          criticalDays,
        });
      } else {
        setBackupDefaults({ ...FACTORY_BACKUP_DEFAULTS });
      }

      // Load snapshot thresholds
      if (config.snapshotDefaults) {
        const enabled = Boolean(config.snapshotDefaults.enabled);
        const rawWarning = config.snapshotDefaults.warningDays ?? 30;
        const rawCritical = config.snapshotDefaults.criticalDays ?? 45;
        const safeCritical = Math.max(0, rawCritical);
        const normalizedWarning = Math.max(0, rawWarning);
        const warningDays =
          safeCritical > 0 && normalizedWarning > safeCritical ? safeCritical : normalizedWarning;
        const criticalDays = Math.max(safeCritical, warningDays);
        setSnapshotDefaults({
          enabled,
          warningDays,
          criticalDays,
        });
      } else {
        setSnapshotDefaults({
          enabled: false,
          warningDays: 30,
          criticalDays: 45,
        });
      }

      // Load PMG thresholds
      if (config.pmgDefaults) {
        setPMGThresholds({
          queueTotalWarning: config.pmgDefaults.queueTotalWarning ?? 500,
          queueTotalCritical: config.pmgDefaults.queueTotalCritical ?? 1000,
          oldestMessageWarnMins: config.pmgDefaults.oldestMessageWarnMins ?? 30,
          oldestMessageCritMins: config.pmgDefaults.oldestMessageCritMins ?? 60,
          deferredQueueWarn: config.pmgDefaults.deferredQueueWarn ?? 200,
          deferredQueueCritical: config.pmgDefaults.deferredQueueCritical ?? 500,
          holdQueueWarn: config.pmgDefaults.holdQueueWarn ?? 100,
          holdQueueCritical: config.pmgDefaults.holdQueueCritical ?? 300,
          quarantineSpamWarn: config.pmgDefaults.quarantineSpamWarn ?? 2000,
          quarantineSpamCritical: config.pmgDefaults.quarantineSpamCritical ?? 5000,
          quarantineVirusWarn: config.pmgDefaults.quarantineVirusWarn ?? 2000,
          quarantineVirusCritical: config.pmgDefaults.quarantineVirusCritical ?? 5000,
          quarantineGrowthWarnPct: config.pmgDefaults.quarantineGrowthWarnPct ?? 25,
          quarantineGrowthWarnMin: config.pmgDefaults.quarantineGrowthWarnMin ?? 250,
          quarantineGrowthCritPct: config.pmgDefaults.quarantineGrowthCritPct ?? 50,
          quarantineGrowthCritMin: config.pmgDefaults.quarantineGrowthCritMin ?? 500,
        });
      }

      // Load global disable flags
      setDisableAllNodes(config.disableAllNodes ?? false);
      setDisableAllGuests(config.disableAllGuests ?? false);
      setDisableAllStorage(config.disableAllStorage ?? false);
      setDisableAllPBS(config.disableAllPBS ?? false);
      setDisableAllPMG(config.disableAllPMG ?? false);
      setDisableAllDockerHosts(config.disableAllDockerHosts ?? false);
      setDisableAllDockerContainers(config.disableAllDockerContainers ?? false);

      // Load global disable offline alerts flags
      setDisableAllNodesOffline(config.disableAllNodesOffline ?? false);
      setDisableAllGuestsOffline(config.disableAllGuestsOffline ?? false);
      setDisableAllPBSOffline(config.disableAllPBSOffline ?? false);
      setDisableAllPMGOffline(config.disableAllPMGOffline ?? false);
      setDisableAllDockerHostsOffline(config.disableAllDockerHostsOffline ?? false);

      setRawOverridesConfig(config.overrides || {});

      if (config.schedule) {
        if (config.schedule.quietHours) {
          const qh = config.schedule.quietHours;
          let days: Record<string, boolean>;
          if (Array.isArray(qh.days)) {
            days = {
              sunday: qh.days.includes(0),
              monday: qh.days.includes(1),
              tuesday: qh.days.includes(2),
              wednesday: qh.days.includes(3),
              thursday: qh.days.includes(4),
              friday: qh.days.includes(5),
              saturday: qh.days.includes(6),
            };
          } else {
            days = (qh.days as Record<string, boolean>) || createDefaultQuietHours().days;
          }
          const suppress = {
            performance: qh.suppress?.performance ?? false,
            storage: qh.suppress?.storage ?? false,
            offline: qh.suppress?.offline ?? false,
          };

          setScheduleQuietHours({
            enabled: qh.enabled || false,
            start: qh.start || '22:00',
            end: qh.end || '08:00',
            timezone: qh.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC',
            days,
            suppress,
          });
        }

        if (config.schedule.cooldown !== undefined) {
          setScheduleCooldown({
            enabled: config.schedule.cooldown > 0,
            minutes: config.schedule.cooldown,
            maxAlerts: config.schedule.maxAlertsHour || 3,
          });
        }

        if (config.schedule.grouping) {
          setScheduleGrouping({
            enabled: config.schedule.grouping.enabled || false,
            window: Math.floor((config.schedule.grouping.window || 300) / 60),
            byNode:
              config.schedule.grouping.byNode !== undefined
                ? config.schedule.grouping.byNode
                : true,
            byGuest:
              config.schedule.grouping.byGuest !== undefined
                ? config.schedule.grouping.byGuest
                : false,
          });
        } else if (config.schedule.groupingWindow !== undefined) {
          setScheduleGrouping({
            enabled: config.schedule.groupingWindow > 0,
            window: Math.floor(config.schedule.groupingWindow / 60),
            byNode: true,
            byGuest: false,
          });
        }

        if (config.schedule.escalation) {
          const rawLevels = config.schedule.escalation.levels || [];
          const levels = rawLevels.map((level) => ({
            after: typeof level.after === 'number' ? level.after : 15,
            notify: (level.notify as EscalationNotifyTarget) || 'all',
          }));
          setScheduleEscalation({
            enabled: Boolean(config.schedule.escalation.enabled),
            levels,
          });
        }
      }

      try {
        const emailConfigData = await NotificationsAPI.getEmailConfig();
        setEmailConfig({
          enabled: emailConfigData.enabled,
          provider: emailConfigData.provider || '',
          server: emailConfigData.server || '',
          port: emailConfigData.port || 587,
          username: emailConfigData.username || '',
          password: emailConfigData.password || '',
          from: emailConfigData.from || '',
          to: emailConfigData.to || [],
          tls: emailConfigData.tls !== undefined ? emailConfigData.tls : true,
          startTLS: emailConfigData.startTLS || false,
          replyTo: '',
          maxRetries: 3,
          retryDelay: 5,
          rateLimit: 60,
        });
      } catch (emailErr) {
        console.error('Failed to load email configuration:', emailErr);
      }

      try {
        const appriseData = await NotificationsAPI.getAppriseConfig();
        setAppriseConfig({
          enabled: appriseData.enabled ?? false,
          mode: appriseData.mode === 'http' ? 'http' : 'cli',
          targetsText: formatAppriseTargets(appriseData.targets),
          cliPath: appriseData.cliPath || 'apprise',
          timeoutSeconds:
            typeof appriseData.timeoutSeconds === 'number' && appriseData.timeoutSeconds > 0
              ? appriseData.timeoutSeconds
              : 15,
          serverUrl: appriseData.serverUrl || '',
          configKey: appriseData.configKey || '',
          apiKey: appriseData.apiKey || '',
          apiKeyHeader: appriseData.apiKeyHeader || 'X-API-KEY',
          skipTlsVerify: Boolean(appriseData.skipTlsVerify),
        });
      } catch (appriseErr) {
        console.error('Failed to load Apprise configuration:', appriseErr);
      }

      if (options.notify) {
        showSuccess('Changes discarded');
      }
    } catch (err) {
      console.error('Failed to load alert configuration:', err);
      if (options.notify) {
        showError('Failed to reload configuration');
      }
    } finally {
      setIsReloadingConfig(false);
    }
  };

  // Load existing alert configuration on mount (only once)
  onMount(() => {
    void loadAlertConfiguration();
  });

  // Reload email config when switching to destinations tab
  createEffect(() => {
    if (activeTab() === 'destinations') {
      // Reload email config from server when switching to destinations tab
      NotificationsAPI.getEmailConfig()
        .then((emailConfigData) => {
          setEmailConfig({
            enabled: emailConfigData.enabled,
            provider: emailConfigData.provider || '',
            server: emailConfigData.server || '',
            port: emailConfigData.port || 587,
            username: emailConfigData.username || '',
            password: emailConfigData.password || '',
            from: emailConfigData.from || '',
            to: emailConfigData.to || [],
            tls: emailConfigData.tls !== undefined ? emailConfigData.tls : true,
            startTLS: emailConfigData.startTLS || false,
            replyTo: '',
            maxRetries: 3,
            retryDelay: 5,
            rateLimit: 60,
          });
        })
        .catch((err) => {
          console.error('Failed to reload email configuration:', err);
        });

      NotificationsAPI.getAppriseConfig()
        .then((appriseData) => {
          setAppriseConfig({
            enabled: appriseData.enabled ?? false,
            mode: appriseData.mode === 'http' ? 'http' : 'cli',
            targetsText: formatAppriseTargets(appriseData.targets),
            cliPath: appriseData.cliPath || 'apprise',
            timeoutSeconds:
              typeof appriseData.timeoutSeconds === 'number' && appriseData.timeoutSeconds > 0
                ? appriseData.timeoutSeconds
                : 15,
            serverUrl: appriseData.serverUrl || '',
            configKey: appriseData.configKey || '',
            apiKey: appriseData.apiKey || '',
            apiKeyHeader: appriseData.apiKeyHeader || 'X-API-KEY',
            skipTlsVerify: Boolean(appriseData.skipTlsVerify),
          });
        })
        .catch((err) => {
          console.error('Failed to reload Apprise configuration:', err);
        });
    }
  });

  // Get all guests from state - memoize to prevent unnecessary updates
  const allGuests = createMemo(
    () => {
      const vms = state.vms || [];
      const containers = state.containers || [];
      return [...vms, ...containers];
    },
    [],
    {
      equals: (prev, next) => {
        // Only update if the actual guest list changed
        if (prev.length !== next.length) return false;
        return prev.every((p, i) => p.vmid === next[i].vmid && p.name === next[i].name);
      },
    },
  );

  // Factory defaults - constants for reset functionality
  const FACTORY_GUEST_DEFAULTS = {
    cpu: 80,
    memory: 85,
    disk: 90,
    diskRead: -1,
    diskWrite: -1,
    networkIn: -1,
    networkOut: -1,
  };

  const FACTORY_NODE_DEFAULTS = {
    cpu: 80,
    memory: 85,
    disk: 90,
    temperature: 80,
  };

  const FACTORY_DOCKER_DEFAULTS = {
    cpu: 80,
    memory: 85,
    restartCount: 3,
    restartWindow: 300,
    memoryWarnPct: 90,
    memoryCriticalPct: 95,
  };

  const FACTORY_STORAGE_DEFAULT = 85;
  const FACTORY_SNAPSHOT_DEFAULTS: SnapshotAlertConfig = {
    enabled: false,
    warningDays: 30,
    criticalDays: 45,
  };
  const FACTORY_BACKUP_DEFAULTS: BackupAlertConfig = {
    enabled: false,
    warningDays: 7,
    criticalDays: 14,
  };

  // Threshold states - using trigger values for display
  const [guestDefaults, setGuestDefaults] = createSignal<Record<string, number | undefined>>({ ...FACTORY_GUEST_DEFAULTS });
  const [guestDisableConnectivity, setGuestDisableConnectivity] = createSignal(false);
  const [guestPoweredOffSeverity, setGuestPoweredOffSeverity] = createSignal<'warning' | 'critical'>('warning');

  const [nodeDefaults, setNodeDefaults] = createSignal<Record<string, number | undefined>>({ ...FACTORY_NODE_DEFAULTS });

  const [dockerDefaults, setDockerDefaults] = createSignal({ ...FACTORY_DOCKER_DEFAULTS });
  const [dockerIgnoredPrefixes, setDockerIgnoredPrefixes] = createSignal<string[]>([]);

  const [storageDefault, setStorageDefault] = createSignal(FACTORY_STORAGE_DEFAULT);
  const [backupDefaults, setBackupDefaults] = createSignal<BackupAlertConfig>({
    ...FACTORY_BACKUP_DEFAULTS,
  });

  // Reset functions
  const resetGuestDefaults = () => {
    setGuestDefaults({ ...FACTORY_GUEST_DEFAULTS });
    setHasUnsavedChanges(true);
  };

  const resetNodeDefaults = () => {
    setNodeDefaults({ ...FACTORY_NODE_DEFAULTS });
    setHasUnsavedChanges(true);
  };

  const resetDockerDefaults = () => {
    setDockerDefaults({ ...FACTORY_DOCKER_DEFAULTS });
    setHasUnsavedChanges(true);
  };

  const resetDockerIgnoredPrefixes = () => {
    setDockerIgnoredPrefixes([]);
    setHasUnsavedChanges(true);
  };

  const resetStorageDefault = () => {
    setStorageDefault(FACTORY_STORAGE_DEFAULT);
    setHasUnsavedChanges(true);
  };
  const resetBackupDefaults = () => {
    setBackupDefaults({ ...FACTORY_BACKUP_DEFAULTS });
    setHasUnsavedChanges(true);
  };
  const resetSnapshotDefaults = () => {
    setSnapshotDefaults({ ...FACTORY_SNAPSHOT_DEFAULTS });
    setHasUnsavedChanges(true);
  };
  const [timeThreshold, setTimeThreshold] = createSignal(DEFAULT_DELAY_SECONDS); // Legacy
  const [timeThresholds, setTimeThresholds] = createSignal({
    guest: DEFAULT_DELAY_SECONDS,
    node: DEFAULT_DELAY_SECONDS,
    storage: DEFAULT_DELAY_SECONDS,
    pbs: DEFAULT_DELAY_SECONDS,
  });
  const [metricTimeThresholds, setMetricTimeThresholds] =
    createSignal<Record<string, Record<string, number>>>({});
  const [snapshotDefaults, setSnapshotDefaults] = createSignal<SnapshotAlertConfig>({
    ...FACTORY_SNAPSHOT_DEFAULTS,
  });

  const [pmgThresholds, setPMGThresholds] = createSignal({
    queueTotalWarning: 500,
    queueTotalCritical: 1000,
    oldestMessageWarnMins: 30,
    oldestMessageCritMins: 60,
    deferredQueueWarn: 200,
    deferredQueueCritical: 500,
    holdQueueWarn: 100,
    holdQueueCritical: 300,
    quarantineSpamWarn: 2000,
    quarantineSpamCritical: 5000,
    quarantineVirusWarn: 2000,
    quarantineVirusCritical: 5000,
    quarantineGrowthWarnPct: 25,
    quarantineGrowthWarnMin: 250,
    quarantineGrowthCritPct: 50,
    quarantineGrowthCritMin: 500,
  });

  // Global disable flags per resource type
  const [disableAllNodes, setDisableAllNodes] = createSignal(false);
  const [disableAllGuests, setDisableAllGuests] = createSignal(false);
  const [disableAllStorage, setDisableAllStorage] = createSignal(false);
  const [disableAllPBS, setDisableAllPBS] = createSignal(false);
  const [disableAllPMG, setDisableAllPMG] = createSignal(false);
  const [disableAllDockerHosts, setDisableAllDockerHosts] = createSignal(false);
  const [disableAllDockerContainers, setDisableAllDockerContainers] = createSignal(false);

  // Global disable offline alerts flags
  const [disableAllNodesOffline, setDisableAllNodesOffline] = createSignal(false);
  const [disableAllGuestsOffline, setDisableAllGuestsOffline] = createSignal(false);
  const [disableAllPBSOffline, setDisableAllPBSOffline] = createSignal(false);
  const [disableAllPMGOffline, setDisableAllPMGOffline] = createSignal(false);
  const [disableAllDockerHostsOffline, setDisableAllDockerHostsOffline] = createSignal(false);

  const tabGroups: {
    id: 'status' | 'configuration';
    label: string;
    items: { id: AlertTab; label: string; icon: JSX.Element }[];
  }[] = [
    {
      id: 'status',
      label: 'Status',
      items: [
        { id: 'overview', label: 'Overview', icon: <LayoutDashboard class="w-4 h-4" strokeWidth={2} /> },
        { id: 'history', label: 'History', icon: <History class="w-4 h-4" strokeWidth={2} /> },
      ],
    },
    {
      id: 'configuration',
      label: 'Configuration',
      items: [
        { id: 'thresholds', label: 'Thresholds', icon: <Gauge class="w-4 h-4" strokeWidth={2} /> },
        { id: 'destinations', label: 'Notifications', icon: <Send class="w-4 h-4" strokeWidth={2} /> },
        { id: 'schedule', label: 'Schedule', icon: <Calendar class="w-4 h-4" strokeWidth={2} /> },
      ],
    },
  ];

  const flatTabs = tabGroups.flatMap((group) => group.items);
  const [sidebarCollapsed, setSidebarCollapsed] = createSignal(true);

  return (
    <div class="space-y-4">
      {/* Header with better styling */}
      <Card padding="md">
        <div class="flex items-center justify-between gap-4">
          <SectionHeader
            title={headerMeta().title}
            description={headerMeta().description}
            size="lg"
          />
          <Show when={activeTab() === 'overview'}>
            <div class="flex items-center gap-3">
              <span
                class={`text-sm font-medium ${
                  isAlertsActive() ? 'text-green-600 dark:text-green-400' : 'text-gray-500 dark:text-gray-400'
                }`}
              >
                {isAlertsActive() ? 'Alerts enabled' : 'Alerts disabled'}
              </span>
              <label class="relative inline-flex items-center cursor-pointer">
                <span class="sr-only">Toggle alerts</span>
                <input
                  type="checkbox"
                  class="sr-only peer"
                  checked={isAlertsActive()}
                  disabled={alertsActivation.isLoading() || isSwitchingActivation()}
                  onChange={(event) => {
                    if (event.currentTarget.checked) {
                      void handleActivateAlerts();
                    } else {
                      void handleDeactivateAlerts();
                    }
                  }}
                />
                <div
                  class={`relative w-11 h-6 rounded-full transition ${
                    isAlertsActive() ? 'bg-blue-600' : 'bg-gray-200 dark:bg-gray-700'
                  } ${alertsActivation.isLoading() || isSwitchingActivation() ? 'opacity-50' : ''}`}
                >
                  <span
                    class={`absolute top-[2px] left-[2px] h-5 w-5 rounded-full bg-white transition-all shadow ${
                      isAlertsActive() ? 'translate-x-5' : 'translate-x-0'
                    }`}
                  />
                </div>
              </label>
            </div>
          </Show>
        </div>
      </Card>

      {/* Save notification bar - only show when there are unsaved changes */}
      <Show when={hasUnsavedChanges() && activeTab() !== 'overview' && activeTab() !== 'history'}>
        <Card tone="warning" padding="sm" class="border-yellow-200 dark:border-yellow-800 sm:p-4">
          <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
            <div class="flex items-center gap-2 text-yellow-800 dark:text-yellow-200">
              <svg
                width="16"
                height="16"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                stroke-width="2"
              >
                <circle cx="12" cy="12" r="10"></circle>
                <line x1="12" y1="8" x2="12" y2="12"></line>
                <line x1="12" y1="16" x2="12.01" y2="16"></line>
              </svg>
              <span class="text-sm font-medium">You have unsaved changes</span>
            </div>
            <div class="flex w-full gap-2 sm:w-auto">
              <button
                class="flex-1 px-4 py-2 text-sm text-white transition-colors sm:flex-initial bg-blue-600 rounded-lg hover:bg-blue-700 disabled:opacity-60 disabled:cursor-not-allowed"
                disabled={isReloadingConfig()}
                onClick={async () => {
                  try {
                    // Save alert configuration with hysteresis format
                    const createHysteresisThreshold = (
                      trigger: number | undefined,
                      clearMargin: number = 5,
                    ) => {
                      const normalized = typeof trigger === 'number' ? trigger : 0;
                      return {
                        trigger: normalized,
                        clear: Math.max(0, normalized - clearMargin),
                      };
                    };

                    const snapshotConfig = snapshotDefaults();
                    const normalizedSnapshotWarning = Math.max(0, snapshotConfig.warningDays ?? 0);
                    const normalizedSnapshotCritical = Math.max(0, snapshotConfig.criticalDays ?? 0);
                    const finalSnapshotCritical =
                      normalizedSnapshotCritical > 0
                        ? Math.max(normalizedSnapshotCritical, normalizedSnapshotWarning)
                        : normalizedSnapshotWarning;

                    const backupConfig = backupDefaults();
                    const normalizedBackupWarning = Math.max(0, backupConfig.warningDays ?? 0);
                    const normalizedBackupCritical = Math.max(0, backupConfig.criticalDays ?? 0);
                    const finalBackupCritical =
                      normalizedBackupCritical > 0
                        ? Math.max(normalizedBackupCritical, normalizedBackupWarning)
                        : normalizedBackupWarning;

                    const alertConfig = {
                      enabled: true,
                      // Global disable flags per resource type
                      disableAllNodes: disableAllNodes(),
                      disableAllGuests: disableAllGuests(),
                      disableAllStorage: disableAllStorage(),
                      disableAllPBS: disableAllPBS(),
                      disableAllPMG: disableAllPMG(),
                      disableAllDockerHosts: disableAllDockerHosts(),
                      disableAllDockerContainers: disableAllDockerContainers(),
                      // Global disable offline alerts flags
                      disableAllNodesOffline: disableAllNodesOffline(),
                      disableAllGuestsOffline: disableAllGuestsOffline(),
                      disableAllPBSOffline: disableAllPBSOffline(),
                      disableAllPMGOffline: disableAllPMGOffline(),
                      disableAllDockerHostsOffline: disableAllDockerHostsOffline(),
                      guestDefaults: {
                        cpu: createHysteresisThreshold(guestDefaults().cpu),
                        memory: createHysteresisThreshold(guestDefaults().memory),
                        disk: createHysteresisThreshold(guestDefaults().disk),
                        diskRead: createHysteresisThreshold(guestDefaults().diskRead),
                        diskWrite: createHysteresisThreshold(guestDefaults().diskWrite),
                        networkIn: createHysteresisThreshold(guestDefaults().networkIn),
                        networkOut: createHysteresisThreshold(guestDefaults().networkOut),
                        disableConnectivity: guestDisableConnectivity(),
                        poweredOffSeverity: guestPoweredOffSeverity(),
                      },
                      nodeDefaults: {
                        cpu: createHysteresisThreshold(nodeDefaults().cpu),
                        memory: createHysteresisThreshold(nodeDefaults().memory),
                        disk: createHysteresisThreshold(nodeDefaults().disk),
                        temperature: createHysteresisThreshold(nodeDefaults().temperature),
                      },
                      dockerDefaults: {
                        cpu: createHysteresisThreshold(dockerDefaults().cpu),
                        memory: createHysteresisThreshold(dockerDefaults().memory),
                        restartCount: dockerDefaults().restartCount,
                        restartWindow: dockerDefaults().restartWindow,
                        memoryWarnPct: dockerDefaults().memoryWarnPct,
                        memoryCriticalPct: dockerDefaults().memoryCriticalPct,
                      },
                      dockerIgnoredContainerPrefixes: dockerIgnoredPrefixes()
                        .map((prefix) => prefix.trim())
                        .filter((prefix) => prefix.length > 0),
                      storageDefault: createHysteresisThreshold(storageDefault()),
                      minimumDelta: 2.0,
                      suppressionWindow: 5,
                      hysteresisMargin: 5.0,
                      timeThreshold: timeThreshold() || 0, // Legacy
                      timeThresholds: timeThresholds(),
                      metricTimeThresholds: normalizeMetricDelayMap(metricTimeThresholds()),
                      snapshotDefaults: {
                        enabled: snapshotConfig.enabled,
                        warningDays: normalizedSnapshotWarning,
                        criticalDays: finalSnapshotCritical,
                      },
                      backupDefaults: {
                        enabled: backupConfig.enabled,
                        warningDays: normalizedBackupWarning,
                        criticalDays: finalBackupCritical,
                      },
                      pmgDefaults: pmgThresholds(),
                      // Use rawOverridesConfig which is already properly formatted with disabled flags
                      overrides: rawOverridesConfig(),
                      schedule: {
                        quietHours: scheduleQuietHours(),
                        cooldown: scheduleCooldown().enabled ? scheduleCooldown().minutes : 0,
                        groupingWindow:
                          scheduleGrouping().enabled && scheduleGrouping().window
                            ? scheduleGrouping().window * 60
                            : 30, // Convert minutes to seconds
                        maxAlertsHour: scheduleCooldown().maxAlerts || 10,
                        escalation: scheduleEscalation(),
                        grouping: {
                          enabled: scheduleGrouping().enabled,
                          window: scheduleGrouping().window * 60, // Convert minutes to seconds
                          byNode: scheduleGrouping().byNode,
                          byGuest: scheduleGrouping().byGuest,
                        },
                      },
                      // Add missing required fields
                      aggregation: {
                        enabled: true,
                        timeWindow: 10,
                        countThreshold: 3,
                        similarityWindow: 5.0,
                      },
                      flapping: {
                        enabled: true,
                        threshold: 5,
                        window: 10,
                        suppressionTime: 30,
                        minStability: 0.8,
                      },
                      ioNormalization: {
                        enabled: true,
                        vmDiskMax: 500.0,
                        containerDiskMax: 300.0,
                        networkMax: 1000.0,
                      },
                    };

                    await AlertsAPI.updateConfig(alertConfig);

                    // Save email config if it exists (regardless of active tab)
                    if (destinationsRef.emailConfig) {
                      const emailData = destinationsRef.emailConfig();
                      await NotificationsAPI.updateEmailConfig(emailData);
                    }

                    if (destinationsRef.appriseConfig) {
                      const appriseData = destinationsRef.appriseConfig();
                      const updatedApprise = await NotificationsAPI.updateAppriseConfig(appriseData);
                      setAppriseConfig({
                        enabled: updatedApprise.enabled ?? false,
                        mode: updatedApprise.mode === 'http' ? 'http' : 'cli',
                        targetsText: formatAppriseTargets(updatedApprise.targets),
                        cliPath: updatedApprise.cliPath || 'apprise',
                        timeoutSeconds:
                          typeof updatedApprise.timeoutSeconds === 'number' && updatedApprise.timeoutSeconds > 0
                            ? updatedApprise.timeoutSeconds
                            : 15,
                        serverUrl: updatedApprise.serverUrl || '',
                        configKey: updatedApprise.configKey || '',
                        apiKey: updatedApprise.apiKey || '',
                        apiKeyHeader: updatedApprise.apiKeyHeader || 'X-API-KEY',
                        skipTlsVerify: Boolean(updatedApprise.skipTlsVerify),
                      });
                    }

                    setHasUnsavedChanges(false);
                    showSuccess('Configuration saved successfully!');
                  } catch (err) {
                    console.error('Failed to save configuration:', err);
                    showError(err instanceof Error ? err.message : 'Failed to save configuration');
                  }
                }}
              >
                Save Changes
              </button>
              <button
                class="flex-1 px-4 py-2 text-sm transition-colors border border-gray-300 rounded-lg text-gray-700 dark:border-gray-600 dark:text-gray-300 hover:bg-gray-50 dark:hover:bg-gray-700 sm:flex-initial disabled:opacity-60 disabled:cursor-not-allowed"
                disabled={isReloadingConfig()}
                onClick={async () => {
                  await loadAlertConfiguration({ notify: true });
                }}
              >
                {isReloadingConfig() ? 'Discarding...' : 'Discard'}
              </button>
            </div>
          </div>
        </Card>
      </Show>

      <div class={`transition-opacity ${
        isAlertsActive() ? 'opacity-100' : 'opacity-50 pointer-events-none'
      }`}>

      <Card padding="none" class="relative lg:flex">
        <div
          class={`hidden lg:flex lg:flex-col ${sidebarCollapsed() ? 'w-16' : 'w-72'} ${sidebarCollapsed() ? 'lg:min-w-[4rem] lg:max-w-[4rem] lg:basis-[4rem]' : 'lg:min-w-[18rem] lg:max-w-[18rem] lg:basis-[18rem]'} relative border-b border-gray-200 dark:border-gray-700 lg:border-b-0 lg:border-r lg:border-gray-200 dark:lg:border-gray-700 lg:align-top flex-shrink-0 transition-all duration-300`}
          onMouseEnter={() => setSidebarCollapsed(false)}
          onMouseLeave={() => setSidebarCollapsed(true)}
          aria-label="Alerts navigation"
          aria-expanded={!sidebarCollapsed()}
        >
          <div class={`sticky top-24 ${sidebarCollapsed() ? 'px-2' : 'px-5'} py-6 space-y-6 transition-all duration-300`}>
            <div id="alerts-sidebar-menu" class="space-y-6">
              <For each={tabGroups}>
                {(group) => (
                  <div class="space-y-2">
                    <Show when={!sidebarCollapsed()}>
                      <p class="text-xs font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">
                        {group.label}
                      </p>
                    </Show>
                    <div class="space-y-1.5">
                      <For each={group.items}>
                        {(item) => {
                          const disabled = () => item.id !== 'overview' && !isAlertsActive();
                          return (
                            <button
                              type="button"
                              aria-current={activeTab() === item.id ? 'page' : undefined}
                              class={`flex w-full items-center ${sidebarCollapsed() ? 'justify-center' : 'gap-2.5'} rounded-md ${sidebarCollapsed() ? 'px-2 py-2.5' : 'px-3 py-2'} text-sm font-medium transition-colors ${
                                activeTab() === item.id
                                  ? 'bg-blue-50 text-blue-600 dark:bg-blue-900/30 dark:text-blue-200'
                                  : 'text-gray-600 hover:bg-gray-100 hover:text-gray-900 dark:text-gray-300 dark:hover:bg-gray-700/60 dark:hover:text-gray-100'
                              } ${disabled() ? 'opacity-50 cursor-not-allowed pointer-events-none' : ''}`}
                              disabled={disabled()}
                              onClick={() => {
                                if (disabled()) return;
                                handleTabChange(item.id);
                              }}
                              title={
                                sidebarCollapsed()
                                  ? disabled()
                                    ? 'Activate alerts to configure'
                                    : item.label
                                  : disabled()
                                    ? 'Activate alerts to configure'
                                    : undefined
                              }
                            >
                              {item.icon}
                              <Show when={!sidebarCollapsed()}>
                                <span class="truncate">{item.label}</span>
                              </Show>
                            </button>
                          );
                        }}
                      </For>
                    </div>
                  </div>
                )}
              </For>
            </div>
          </div>
        </div>

        <div class="flex-1 min-w-0">
          <Show when={flatTabs.length > 0}>
            <div class="lg:hidden border-b border-gray-200 dark:border-gray-700">
              <div class="p-1">
                <div
                  class="flex rounded-lg bg-gray-100 dark:bg-gray-700 p-0.5 w-full overflow-x-auto"
                  style="-webkit-overflow-scrolling: touch;"
                >
                  <For each={flatTabs}>
                    {(tab) => {
                      const disabled = () => tab.id !== 'overview' && !isAlertsActive();
                      return (
                        <button
                          type="button"
                          class={`flex-1 px-3 py-2 text-xs font-medium rounded-md transition-all whitespace-nowrap ${
                            activeTab() === tab.id
                              ? 'bg-white dark:bg-gray-800 text-gray-900 dark:text-gray-100 shadow-sm'
                              : 'text-gray-600 dark:text-gray-400 hover:text-gray-900 dark:hover:text-gray-100'
                          } ${disabled() ? 'opacity-50 cursor-not-allowed pointer-events-none' : ''}`}
                          disabled={disabled()}
                          onClick={() => {
                            if (disabled()) return;
                            handleTabChange(tab.id);
                          }}
                          title={disabled() ? 'Activate alerts to configure' : undefined}
                        >
                          {tab.label}
                        </button>
                      );
                    }}
                  </For>
                </div>
              </div>
            </div>
          </Show>

          {/* Tab Content */}
          <div class="p-3 sm:p-6">
            <Show when={activeTab() === 'overview'}>
              <OverviewTab
                overrides={overrides()}
                activeAlerts={activeAlerts}
                updateAlert={updateAlert}
              showQuickTip={showQuickTip}
              dismissQuickTip={dismissQuickTip}
              showAcknowledged={showAcknowledged}
              setShowAcknowledged={setShowAcknowledged}
            />
          </Show>

          <Show when={activeTab() === 'thresholds'}>
            <ThresholdsTab
              overrides={overrides}
              setOverrides={setOverrides}
              rawOverridesConfig={rawOverridesConfig}
              setRawOverridesConfig={setRawOverridesConfig}
              allGuests={allGuests}
              state={state}
              guestDefaults={guestDefaults}
              guestDisableConnectivity={guestDisableConnectivity}
              setGuestDefaults={setGuestDefaults}
              setGuestDisableConnectivity={setGuestDisableConnectivity}
              guestPoweredOffSeverity={guestPoweredOffSeverity}
              setGuestPoweredOffSeverity={setGuestPoweredOffSeverity}
              nodeDefaults={nodeDefaults}
              setNodeDefaults={setNodeDefaults}
              dockerDefaults={dockerDefaults}
              setDockerDefaults={setDockerDefaults}
              dockerIgnoredPrefixes={dockerIgnoredPrefixes}
              setDockerIgnoredPrefixes={setDockerIgnoredPrefixes}
              storageDefault={storageDefault}
              setStorageDefault={setStorageDefault}
              resetGuestDefaults={resetGuestDefaults}
              resetNodeDefaults={resetNodeDefaults}
              resetDockerDefaults={resetDockerDefaults}
              resetDockerIgnoredPrefixes={resetDockerIgnoredPrefixes}
              resetStorageDefault={resetStorageDefault}
              resetSnapshotDefaults={resetSnapshotDefaults}
              resetBackupDefaults={resetBackupDefaults}
              factoryGuestDefaults={FACTORY_GUEST_DEFAULTS}
              factoryNodeDefaults={FACTORY_NODE_DEFAULTS}
              factoryDockerDefaults={FACTORY_DOCKER_DEFAULTS}
              factoryStorageDefault={FACTORY_STORAGE_DEFAULT}
              snapshotFactoryDefaults={FACTORY_SNAPSHOT_DEFAULTS}
              backupFactoryDefaults={FACTORY_BACKUP_DEFAULTS}
              timeThresholds={timeThresholds}
              metricTimeThresholds={metricTimeThresholds}
              setMetricTimeThresholds={setMetricTimeThresholds}
              backupDefaults={backupDefaults}
              setBackupDefaults={setBackupDefaults}
              snapshotDefaults={snapshotDefaults}
              setSnapshotDefaults={setSnapshotDefaults}
              pmgThresholds={pmgThresholds}
              setPMGThresholds={setPMGThresholds}
              activeAlerts={activeAlerts}
              setHasUnsavedChanges={setHasUnsavedChanges}
              hasUnsavedChanges={hasUnsavedChanges}
              removeAlerts={removeAlerts}
              disableAllNodes={disableAllNodes}
              setDisableAllNodes={setDisableAllNodes}
              disableAllGuests={disableAllGuests}
              setDisableAllGuests={setDisableAllGuests}
              disableAllStorage={disableAllStorage}
              setDisableAllStorage={setDisableAllStorage}
              disableAllPBS={disableAllPBS}
              setDisableAllPBS={setDisableAllPBS}
              disableAllPMG={disableAllPMG}
              setDisableAllPMG={setDisableAllPMG}
              disableAllDockerHosts={disableAllDockerHosts}
              setDisableAllDockerHosts={setDisableAllDockerHosts}
              disableAllDockerContainers={disableAllDockerContainers}
              setDisableAllDockerContainers={setDisableAllDockerContainers}
              disableAllNodesOffline={disableAllNodesOffline}
              setDisableAllNodesOffline={setDisableAllNodesOffline}
              disableAllGuestsOffline={disableAllGuestsOffline}
              setDisableAllGuestsOffline={setDisableAllGuestsOffline}
              disableAllPBSOffline={disableAllPBSOffline}
              setDisableAllPBSOffline={setDisableAllPBSOffline}
              disableAllPMGOffline={disableAllPMGOffline}
              setDisableAllPMGOffline={setDisableAllPMGOffline}
              disableAllDockerHostsOffline={disableAllDockerHostsOffline}
              setDisableAllDockerHostsOffline={setDisableAllDockerHostsOffline}
            />
          </Show>

          <Show when={activeTab() === 'destinations'}>
            <DestinationsTab
              ref={destinationsRef}
              hasUnsavedChanges={hasUnsavedChanges}
              setHasUnsavedChanges={setHasUnsavedChanges}
              emailConfig={emailConfig}
              setEmailConfig={setEmailConfig}
              appriseConfig={appriseConfig}
              setAppriseConfig={setAppriseConfig}
            />
          </Show>

          <Show when={activeTab() === 'schedule'}>
            <ScheduleTab
              hasUnsavedChanges={hasUnsavedChanges}
              setHasUnsavedChanges={setHasUnsavedChanges}
              quietHours={scheduleQuietHours}
              setQuietHours={setScheduleQuietHours}
              cooldown={scheduleCooldown}
              setCooldown={setScheduleCooldown}
              grouping={scheduleGrouping}
              setGrouping={setScheduleGrouping}
              escalation={scheduleEscalation}
              setEscalation={setScheduleEscalation}
            />
          </Show>

          <Show when={activeTab() === 'history'}>
            <HistoryTab />
          </Show>
          </div>
        </div>
      </Card>
      </div>
    </div>
  );
}

// Overview Tab - Shows current alert status
function OverviewTab(props: {
  overrides: Override[];
  activeAlerts: Record<string, Alert>;
  updateAlert: (alertId: string, updates: Partial<Alert>) => void;
  showQuickTip: () => boolean;
  dismissQuickTip: () => void;
  showAcknowledged: () => boolean;
  setShowAcknowledged: (value: boolean) => void;
}) {
  // Loading states for buttons
  const [processingAlerts, setProcessingAlerts] = createSignal<Set<string>>(new Set());

  // Get alert stats from actual active alerts
  const alertStats = createMemo(() => {
    // Access the store properly for reactivity
    const alertIds = Object.keys(props.activeAlerts);
    const alerts = alertIds.map((id) => props.activeAlerts[id]);
    return {
      active: alerts.filter((a) => !a.acknowledged).length,
      acknowledged: alerts.filter((a) => a.acknowledged).length,
      total24h: alerts.length, // In real app, would filter by time
      overrides: props.overrides.length,
    };
  });

  const filteredAlerts = createMemo(() => {
    const alerts = Object.values(props.activeAlerts);
    // Sort: unacknowledged first, then by start time (newest first)
    return alerts
      .filter((alert) => props.showAcknowledged() || !alert.acknowledged)
      .sort((a, b) => {
        // Acknowledged status comparison first
        if (a.acknowledged !== b.acknowledged) {
          return a.acknowledged ? 1 : -1; // Unacknowledged first
        }
        // Then by time
        return new Date(b.startTime).getTime() - new Date(a.startTime).getTime();
      });
  });

  const unacknowledgedAlerts = createMemo(() =>
    Object.values(props.activeAlerts).filter((alert) => !alert.acknowledged),
  );

  const [bulkAckProcessing, setBulkAckProcessing] = createSignal(false);

  return (
    <div class="space-y-6">
      {/* Stats Cards */}
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 sm:gap-4 lg:grid-cols-4">
        <Card padding="sm" class="sm:p-4">
          <div class="flex items-center justify-between">
            <div>
              <p class="text-xs sm:text-sm text-gray-600 dark:text-gray-400">Active Alerts</p>
              <p class="text-xl sm:text-2xl font-semibold text-gray-600 dark:text-gray-300">
                {alertStats().active}
              </p>
            </div>
            <div class="w-8 h-8 sm:w-10 sm:h-10 bg-red-100 dark:bg-red-900/50 rounded-full flex items-center justify-center">
              <svg
                width="16"
                height="16"
                class="sm:w-5 sm:h-5 text-red-600 dark:text-red-400"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                stroke-width="2"
              >
                <path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9"></path>
                <path d="M13.73 21a2 2 0 0 1-3.46 0"></path>
              </svg>
            </div>
          </div>
        </Card>

        <Card padding="sm" class="sm:p-4">
          <div class="flex items-center justify-between">
            <div>
              <p class="text-xs sm:text-sm text-gray-600 dark:text-gray-400">Acknowledged</p>
              <p class="text-xl sm:text-2xl font-semibold text-yellow-600 dark:text-yellow-400">
                {alertStats().acknowledged}
              </p>
            </div>
            <div class="w-8 h-8 sm:w-10 sm:h-10 bg-yellow-100 dark:bg-yellow-900/50 rounded-full flex items-center justify-center">
              <svg
                width="16"
                height="16"
                class="sm:w-5 sm:h-5 text-yellow-600 dark:text-yellow-400"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                stroke-width="2"
              >
                <path d="M9 11L12 14L22 4"></path>
                <path d="M21 12V19C21 20.1046 20.1046 21 19 21H5C3.89543 21 3 20.1046 3 19V5C3 3.89543 3.89543 3 5 3H16"></path>
              </svg>
            </div>
          </div>
        </Card>

        <Card padding="sm" class="sm:p-4">
          <div class="flex items-center justify-between">
            <div>
              <p class="text-xs sm:text-sm text-gray-600 dark:text-gray-400">Last 24 Hours</p>
              <p class="text-xl sm:text-2xl font-semibold text-gray-700 dark:text-gray-300">
                {alertStats().total24h}
              </p>
            </div>
            <div class="w-8 h-8 sm:w-10 sm:h-10 bg-gray-200 dark:bg-gray-600 rounded-full flex items-center justify-center">
              <svg
                width="16"
                height="16"
                class="sm:w-5 sm:h-5 text-gray-600 dark:text-gray-400"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                stroke-width="2"
              >
                <circle cx="12" cy="12" r="10"></circle>
                <polyline points="12 6 12 12 16 14"></polyline>
              </svg>
            </div>
          </div>
        </Card>

        <Card padding="sm" class="sm:p-4">
          <div class="flex items-center justify-between">
            <div>
              <p class="text-xs sm:text-sm text-gray-600 dark:text-gray-400">Guest Overrides</p>
              <p class="text-xl sm:text-2xl font-semibold text-blue-600 dark:text-blue-400">
                {alertStats().overrides}
              </p>
            </div>
            <div class="w-8 h-8 sm:w-10 sm:h-10 bg-blue-100 dark:bg-blue-900/50 rounded-full flex items-center justify-center">
              <svg
                width="16"
                height="16"
                class="sm:w-5 sm:h-5 text-blue-600 dark:text-blue-400"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                stroke-width="2"
              >
                <path d="M11 4H4a2 2 0 00-2 2v14a2 2 0 002 2h14a2 2 0 002-2v-7"></path>
                <path d="M18.5 2.5a2.121 2.121 0 013 3L12 15l-4 1 1-4 9.5-9.5z"></path>
              </svg>
            </div>
          </div>
        </Card>
      </div>

      {/* Recent Alerts */}
      <div>
        <SectionHeader title="Active Alerts" size="md" class="mb-3" />
        <Show
          when={Object.keys(props.activeAlerts).length > 0}
          fallback={
            <div class="text-center py-8 text-gray-500 dark:text-gray-400">
              <div class="flex justify-center mb-3">
                <svg class="w-12 h-12 text-green-500 dark:text-green-400" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <circle cx="12" cy="12" r="10" stroke="currentColor" stroke-width="2" fill="none" />
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 12l2 2 4-4" />
                </svg>
              </div>
              <p class="text-sm">No active alerts</p>
              <p class="text-xs mt-1">Alerts will appear here when thresholds are exceeded</p>
            </div>
          }
        >
          <Show when={alertStats().acknowledged > 0 || alertStats().active > 0}>
            <div class="flex flex-wrap items-center justify-between gap-2 p-2 bg-gray-50 dark:bg-gray-800 rounded-t-lg border border-gray-200 dark:border-gray-700">
              <Show when={alertStats().acknowledged > 0}>
                <button
                  onClick={() => props.setShowAcknowledged(!props.showAcknowledged())}
                  class="text-xs text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-200 transition-colors"
                >
                  {props.showAcknowledged() ? 'Hide' : 'Show'} acknowledged
                </button>
              </Show>
              <Show when={alertStats().active > 0}>
                <button
                  type="button"
                  class="inline-flex items-center gap-1 px-3 py-1.5 text-xs font-medium rounded-lg border border-blue-200 dark:border-blue-700 bg-blue-50 dark:bg-blue-900/40 text-blue-700 dark:text-blue-200 transition-colors hover:bg-blue-100 dark:hover:bg-blue-900/60 disabled:opacity-60 disabled:cursor-not-allowed"
                  disabled={bulkAckProcessing()}
                  onClick={async () => {
                    if (bulkAckProcessing()) return;
                    const pending = unacknowledgedAlerts();
                    if (pending.length === 0) {
                      return;
                    }
                    setBulkAckProcessing(true);
                    try {
                      const result = await AlertsAPI.bulkAcknowledge(pending.map((alert) => alert.id));
                      const successes = result.results.filter((r) => r.success);
                      const failures = result.results.filter((r) => !r.success);

                      successes.forEach((res) => {
                        props.updateAlert(res.alertId, {
                          acknowledged: true,
                          ackTime: new Date().toISOString(),
                        });
                      });

                      if (successes.length > 0) {
                        showSuccess(
                          `Acknowledged ${successes.length} ${successes.length === 1 ? 'alert' : 'alerts'}.`,
                        );
                      }

                      if (failures.length > 0) {
                        showError(
                          `Failed to acknowledge ${failures.length} ${failures.length === 1 ? 'alert' : 'alerts'}.`,
                        );
                      }
                    } catch (error) {
                      console.error('Bulk acknowledge failed', error);
                      showError('Failed to acknowledge alerts');
                    } finally {
                      setBulkAckProcessing(false);
                    }
                  }}
                >
                  {bulkAckProcessing()
                    ? 'Acknowledging…'
                    : `Acknowledge all (${alertStats().active})`}
                </button>
              </Show>
            </div>
          </Show>
          <div class="space-y-2">
            <Show when={filteredAlerts().length === 0}>
              <div class="text-center py-8 text-gray-500 dark:text-gray-400">
                {props.showAcknowledged() ? 'No active alerts' : 'No unacknowledged alerts'}
              </div>
            </Show>
            <For each={filteredAlerts()}>
              {(alert) => (
                <div
                  class={`border rounded-lg p-4 transition-all ${
                    processingAlerts().has(alert.id) ? 'opacity-50' : ''
                  } ${
                    alert.acknowledged
                      ? 'opacity-60 border-gray-300 dark:border-gray-700 bg-gray-50 dark:bg-gray-900/20'
                      : alert.level === 'critical'
                        ? 'border-red-300 dark:border-red-800 bg-red-50 dark:bg-red-900/20'
                        : 'border-yellow-300 dark:border-yellow-800 bg-yellow-50 dark:bg-yellow-900/20'
                  }`}
                >
                  <div class="flex flex-col sm:flex-row sm:items-start">
                    <div class="flex items-start flex-1">
                      {/* Status icon */}
                      <div
                        class={`mr-3 mt-0.5 transition-all ${
                          alert.acknowledged
                            ? 'text-green-600 dark:text-green-400'
                            : alert.level === 'critical'
                              ? 'text-red-600 dark:text-red-400'
                              : 'text-yellow-600 dark:text-yellow-400'
                        }`}
                      >
                        {alert.acknowledged ? (
                          // Checkmark for acknowledged
                          <svg
                            class="w-5 h-5"
                            fill="none"
                            stroke="currentColor"
                            viewBox="0 0 24 24"
                          >
                            <path
                              stroke-linecap="round"
                              stroke-linejoin="round"
                              stroke-width="2"
                              d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z"
                            />
                          </svg>
                        ) : (
                          // Warning/Alert icon
                          <svg
                            class="w-5 h-5"
                            fill="none"
                            stroke="currentColor"
                            viewBox="0 0 24 24"
                          >
                            <path
                              stroke-linecap="round"
                              stroke-linejoin="round"
                              stroke-width="2"
                              d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z"
                            />
                          </svg>
                        )}
                      </div>
                      <div class="flex-1 min-w-0">
                        <div class="flex flex-wrap items-center gap-2">
                          <span
                            class={`text-sm font-medium truncate ${
                              alert.level === 'critical'
                                ? 'text-red-700 dark:text-red-400'
                                : 'text-yellow-700 dark:text-yellow-400'
                            }`}
                          >
                            {alert.resourceName}
                          </span>
                          <span class="text-xs text-gray-600 dark:text-gray-400">
                            ({alert.type})
                          </span>
                          <Show when={alert.node}>
                            <span class="text-xs text-gray-500 dark:text-gray-500">
                              on {alert.node}
                            </span>
                          </Show>
                          <Show when={alert.acknowledged}>
                            <span class="px-2 py-0.5 text-xs bg-yellow-200 dark:bg-yellow-800 text-yellow-800 dark:text-yellow-200 rounded">
                              Acknowledged
                            </span>
                          </Show>
                        </div>
                        <p class="text-sm text-gray-700 dark:text-gray-300 mt-1 break-words">
                          {alert.message}
                        </p>
                        <p class="text-xs text-gray-600 dark:text-gray-400 mt-1">
                          Started: {new Date(alert.startTime).toLocaleString()}
                        </p>
                      </div>
                    </div>
                    <div class="flex gap-2 mt-3 sm:mt-0 sm:ml-4 self-end sm:self-start">
                      <button
                        class={`px-3 py-1.5 text-xs font-medium border rounded-lg transition-all disabled:opacity-50 disabled:cursor-not-allowed ${
                          alert.acknowledged
                            ? 'bg-white dark:bg-gray-700 text-gray-700 dark:text-gray-300 border-gray-300 dark:border-gray-600 hover:bg-gray-50 dark:hover:bg-gray-600'
                            : 'bg-white dark:bg-gray-700 text-yellow-700 dark:text-yellow-300 border-yellow-300 dark:border-yellow-700 hover:bg-yellow-50 dark:hover:bg-yellow-900/20'
                        }`}
                        disabled={processingAlerts().has(alert.id)}
                        onClick={async (e) => {
                          e.preventDefault();
                          e.stopPropagation();

                          // Prevent double-clicks
                          if (processingAlerts().has(alert.id)) return;

                          setProcessingAlerts((prev) => new Set(prev).add(alert.id));

                          // Store current state to avoid race conditions
                          const wasAcknowledged = alert.acknowledged;

                          try {
                            if (wasAcknowledged) {
                              // Call API first, only update local state if successful
                              await AlertsAPI.unacknowledge(alert.id);
                              // Only update local state after successful API call
                              props.updateAlert(alert.id, {
                                acknowledged: false,
                                ackTime: undefined,
                                ackUser: undefined,
                              });
                              showSuccess('Alert restored');
                            } else {
                              // Call API first, only update local state if successful
                              await AlertsAPI.acknowledge(alert.id);
                              // Only update local state after successful API call
                              props.updateAlert(alert.id, {
                                acknowledged: true,
                                ackTime: new Date().toISOString(),
                              });
                              showSuccess('Alert acknowledged');
                            }
                          } catch (err) {
                            console.error(
                              `Failed to ${wasAcknowledged ? 'unacknowledge' : 'acknowledge'} alert:`,
                              err,
                            );
                            showError(
                              `Failed to ${wasAcknowledged ? 'restore' : 'acknowledge'} alert`,
                            );
                            // Don't update local state on error - let WebSocket keep the correct state
                          } finally {
                            // Keep button disabled for longer to prevent race conditions with WebSocket updates
                            setTimeout(() => {
                              setProcessingAlerts((prev) => {
                                const next = new Set(prev);
                                next.delete(alert.id);
                                return next;
                              });
                            }, 1500); // 1.5 seconds to allow server to process and WebSocket to sync
                          }
                        }}
                      >
                        {processingAlerts().has(alert.id)
                          ? 'Processing...'
                          : alert.acknowledged
                            ? 'Unacknowledge'
                            : 'Acknowledge'}
                      </button>
                    </div>
                  </div>
                </div>
              )}
            </For>
          </div>
        </Show>
      </div>
    </div>
  );
}

// Thresholds Tab - Improved design
interface ThresholdsTabProps {
  allGuests: () => (VM | Container)[];
  state: State;
  guestDefaults: () => Record<string, number | undefined>;
  nodeDefaults: () => Record<string, number | undefined>;
  dockerDefaults: () => { cpu: number; memory: number; restartCount: number; restartWindow: number; memoryWarnPct: number; memoryCriticalPct: number };
  dockerIgnoredPrefixes: () => string[];
  storageDefault: () => number;
  timeThresholds: () => { guest: number; node: number; storage: number; pbs: number };
  metricTimeThresholds: () => Record<string, Record<string, number>>;
  overrides: () => Override[];
  rawOverridesConfig: () => Record<string, RawOverrideConfig>;
  pmgThresholds: () => PMGThresholdDefaults;
  setPMGThresholds: (
    value:
      | PMGThresholdDefaults
      | ((prev: PMGThresholdDefaults) => PMGThresholdDefaults),
  ) => void;
  setGuestDefaults: (
    value:
      | Record<string, number | undefined>
      | ((prev: Record<string, number | undefined>) => Record<string, number | undefined>),
  ) => void;
  guestDisableConnectivity: () => boolean;
  setGuestDisableConnectivity: (value: boolean) => void;
  guestPoweredOffSeverity: () => 'warning' | 'critical';
  setGuestPoweredOffSeverity: (value: 'warning' | 'critical') => void;
  setNodeDefaults: (
    value:
      | Record<string, number | undefined>
      | ((prev: Record<string, number | undefined>) => Record<string, number | undefined>),
  ) => void;
  setDockerDefaults: (
    value: { cpu: number; memory: number; restartCount: number; restartWindow: number; memoryWarnPct: number; memoryCriticalPct: number } | ((prev: { cpu: number; memory: number; restartCount: number; restartWindow: number; memoryWarnPct: number; memoryCriticalPct: number }) => { cpu: number; memory: number; restartCount: number; restartWindow: number; memoryWarnPct: number; memoryCriticalPct: number }),
  ) => void;
  setDockerIgnoredPrefixes: (value: string[] | ((prev: string[]) => string[])) => void;
  setStorageDefault: (value: number) => void;
  setMetricTimeThresholds: (
    value:
      | Record<string, Record<string, number>>
      | ((prev: Record<string, Record<string, number>>) => Record<string, Record<string, number>>),
  ) => void;
  snapshotDefaults: () => SnapshotAlertConfig;
  setSnapshotDefaults: (
    value:
      | SnapshotAlertConfig
      | ((prev: SnapshotAlertConfig) => SnapshotAlertConfig),
  ) => void;
  snapshotFactoryDefaults: SnapshotAlertConfig;
  resetSnapshotDefaults: () => void;
  backupDefaults: () => BackupAlertConfig;
  setBackupDefaults: (
    value:
      | BackupAlertConfig
      | ((prev: BackupAlertConfig) => BackupAlertConfig),
  ) => void;
  backupFactoryDefaults: BackupAlertConfig;
  resetBackupDefaults: () => void;
  setOverrides: (value: Override[]) => void;
  setRawOverridesConfig: (value: Record<string, RawOverrideConfig>) => void;
  activeAlerts: Record<string, Alert>;
  setHasUnsavedChanges: (value: boolean) => void;
  hasUnsavedChanges: () => boolean;
  removeAlerts: (predicate: (alert: Alert) => boolean) => void;
  // Global disable flags
  disableAllNodes: () => boolean;
  setDisableAllNodes: (value: boolean) => void;
  disableAllGuests: () => boolean;
  setDisableAllGuests: (value: boolean) => void;
  disableAllStorage: () => boolean;
  setDisableAllStorage: (value: boolean) => void;
  disableAllPBS: () => boolean;
  setDisableAllPBS: (value: boolean) => void;
  disableAllPMG: () => boolean;
  setDisableAllPMG: (value: boolean) => void;
  disableAllDockerHosts: () => boolean;
  setDisableAllDockerHosts: (value: boolean) => void;
  disableAllDockerContainers: () => boolean;
  setDisableAllDockerContainers: (value: boolean) => void;
  // Global disable offline alerts flags
  disableAllNodesOffline: () => boolean;
  setDisableAllNodesOffline: (value: boolean) => void;
  disableAllGuestsOffline: () => boolean;
  setDisableAllGuestsOffline: (value: boolean) => void;
  disableAllPBSOffline: () => boolean;
  setDisableAllPBSOffline: (value: boolean) => void;
  disableAllPMGOffline: () => boolean;
  setDisableAllPMGOffline: (value: boolean) => void;
  disableAllDockerHostsOffline: () => boolean;
  setDisableAllDockerHostsOffline: (value: boolean) => void;
  // Reset functions and factory defaults
  resetGuestDefaults?: () => void;
  resetNodeDefaults?: () => void;
  resetDockerDefaults?: () => void;
  resetDockerIgnoredPrefixes?: () => void;
  resetStorageDefault?: () => void;
  factoryGuestDefaults?: Record<string, number | undefined>;
  factoryNodeDefaults?: Record<string, number | undefined>;
  factoryDockerDefaults?: Record<string, number | undefined>;
  factoryStorageDefault?: number;
}

function ThresholdsTab(props: ThresholdsTabProps) {
  return (
    <ThresholdsTable
      overrides={props.overrides}
      setOverrides={props.setOverrides}
      rawOverridesConfig={props.rawOverridesConfig}
      setRawOverridesConfig={props.setRawOverridesConfig}
      allGuests={props.allGuests}
      nodes={props.state.nodes || []}
      storage={props.state.storage || []}
      dockerHosts={props.state.dockerHosts || []}
      pbsInstances={props.state.pbs || []}
      pmgInstances={props.state.pmg || []}
      backups={props.state.backups}
      pveBackups={props.state.pveBackups}
      pbsBackups={props.state.pbsBackups}
      pmgBackups={props.state.pmgBackups}
      pmgThresholds={props.pmgThresholds}
      setPMGThresholds={props.setPMGThresholds}
      guestDefaults={props.guestDefaults()}
      guestDisableConnectivity={props.guestDisableConnectivity}
      setGuestDefaults={props.setGuestDefaults}
      setGuestDisableConnectivity={props.setGuestDisableConnectivity}
      guestPoweredOffSeverity={props.guestPoweredOffSeverity}
      setGuestPoweredOffSeverity={props.setGuestPoweredOffSeverity}
      nodeDefaults={props.nodeDefaults()}
      setNodeDefaults={props.setNodeDefaults}
      dockerDefaults={props.dockerDefaults()}
      setDockerDefaults={props.setDockerDefaults}
      dockerIgnoredPrefixes={props.dockerIgnoredPrefixes}
      setDockerIgnoredPrefixes={props.setDockerIgnoredPrefixes}
      storageDefault={props.storageDefault}
      setStorageDefault={props.setStorageDefault}
      timeThresholds={props.timeThresholds}
      metricTimeThresholds={props.metricTimeThresholds}
      setMetricTimeThresholds={props.setMetricTimeThresholds}
      backupDefaults={props.backupDefaults}
      setBackupDefaults={props.setBackupDefaults}
      backupFactoryDefaults={props.backupFactoryDefaults}
      resetBackupDefaults={props.resetBackupDefaults}
      snapshotDefaults={props.snapshotDefaults}
      setSnapshotDefaults={props.setSnapshotDefaults}
      snapshotFactoryDefaults={props.snapshotFactoryDefaults}
      resetSnapshotDefaults={props.resetSnapshotDefaults}
      setHasUnsavedChanges={props.setHasUnsavedChanges}
      activeAlerts={props.activeAlerts}
      removeAlerts={props.removeAlerts}
      disableAllNodes={props.disableAllNodes}
      setDisableAllNodes={props.setDisableAllNodes}
      disableAllGuests={props.disableAllGuests}
      setDisableAllGuests={props.setDisableAllGuests}
      disableAllStorage={props.disableAllStorage}
      setDisableAllStorage={props.setDisableAllStorage}
      disableAllPBS={props.disableAllPBS}
      setDisableAllPBS={props.setDisableAllPBS}
      disableAllPMG={props.disableAllPMG}
      setDisableAllPMG={props.setDisableAllPMG}
      disableAllDockerHosts={props.disableAllDockerHosts}
      setDisableAllDockerHosts={props.setDisableAllDockerHosts}
      disableAllDockerContainers={props.disableAllDockerContainers}
      setDisableAllDockerContainers={props.setDisableAllDockerContainers}
      disableAllNodesOffline={props.disableAllNodesOffline}
      setDisableAllNodesOffline={props.setDisableAllNodesOffline}
      disableAllGuestsOffline={props.disableAllGuestsOffline}
      setDisableAllGuestsOffline={props.setDisableAllGuestsOffline}
      disableAllPBSOffline={props.disableAllPBSOffline}
      setDisableAllPBSOffline={props.setDisableAllPBSOffline}
      disableAllPMGOffline={props.disableAllPMGOffline}
      setDisableAllPMGOffline={props.setDisableAllPMGOffline}
      disableAllDockerHostsOffline={props.disableAllDockerHostsOffline}
      setDisableAllDockerHostsOffline={props.setDisableAllDockerHostsOffline}
      resetGuestDefaults={props.resetGuestDefaults}
      resetNodeDefaults={props.resetNodeDefaults}
      resetDockerDefaults={props.resetDockerDefaults}
      resetDockerIgnoredPrefixes={props.resetDockerIgnoredPrefixes}
      resetStorageDefault={props.resetStorageDefault}
      factoryGuestDefaults={props.factoryGuestDefaults}
      factoryNodeDefaults={props.factoryNodeDefaults}
      factoryDockerDefaults={props.factoryDockerDefaults}
      factoryStorageDefault={props.factoryStorageDefault}
    />
  );
}

// Destinations Tab - Notification settings
interface DestinationsTabProps {
  ref: DestinationsRef;
  hasUnsavedChanges: () => boolean;
  setHasUnsavedChanges: (value: boolean) => void;
  emailConfig: () => UIEmailConfig;
  setEmailConfig: (config: UIEmailConfig) => void;
  appriseConfig: () => UIAppriseConfig;
  setAppriseConfig: (config: UIAppriseConfig) => void;
}

function DestinationsTab(props: DestinationsTabProps) {
  const [webhooks, setWebhooks] = createSignal<Webhook[]>([]);
  const [testingEmail, setTestingEmail] = createSignal(false);
  const [testingWebhook, setTestingWebhook] = createSignal<string | null>(null);
  const appriseState = () => props.appriseConfig();
  const updateApprise = (partial: Partial<UIAppriseConfig>) => {
    props.setAppriseConfig({ ...props.appriseConfig(), ...partial });
  };
  // Load webhooks on mount (email config is now loaded in parent)
  onMount(async () => {
    try {
      const hooks = await NotificationsAPI.getWebhooks();
      // Map to local Webhook type - preserve the service type from backend
      setWebhooks(
        hooks.map((h) => ({
          ...h,
          service: h.service || 'generic', // Preserve service type or default to generic
        })),
      );
    } catch (err) {
      console.error('Failed to load webhooks:', err);
    }
  });

  const testEmailConfig = async () => {
    setTestingEmail(true);
    try {
      // Send the current form config for testing (including unsaved changes)
      const config = props.emailConfig();
      await NotificationsAPI.testNotification({
        type: 'email',
        config: { ...config } as Record<string, unknown>, // Send current form data, not saved config
      });
      showSuccess('Test email sent successfully!', 'Check your inbox.');
    } catch (err) {
      console.error('Failed to send test email:', err);
      showError('Failed to send test email', err instanceof Error ? err.message : 'Unknown error');
    } finally {
      setTestingEmail(false);
    }
  };

  const testWebhook = async (webhookId: string, webhookData?: Omit<Webhook, 'id'>) => {
    setTestingWebhook(webhookId);
    try {
      if (webhookData) {
        // Test unsaved webhook with provided configuration
        await NotificationsAPI.testWebhook(webhookData);
      } else {
        // Test existing webhook by ID
        await NotificationsAPI.testNotification({ type: 'webhook', webhookId });
      }
      showSuccess('Test webhook sent successfully!');
    } catch (err) {
      showError(
        'Failed to send test webhook',
        err instanceof Error ? err.message : 'Unknown error',
      );
    } finally {
      setTestingWebhook(null);
    }
  };

  return (
    <div class="flex w-full max-w-full flex-col gap-6 md:gap-8">
      <SettingsPanel
        title="Email notifications"
        description="Configure SMTP delivery for alert emails."
        action={
          <Toggle
            checked={props.emailConfig().enabled}
            onChange={(e) => {
              props.setEmailConfig({ ...props.emailConfig(), enabled: e.currentTarget.checked });
              props.setHasUnsavedChanges(true);
            }}
            containerClass="sm:self-start"
            label={
              <span class="text-xs font-medium text-gray-600 dark:text-gray-400">
                {props.emailConfig().enabled ? 'Enabled' : 'Disabled'}
              </span>
            }
          />
        }
        class="min-w-0"
        bodyClass=""
      >
        <div
          class={`${!props.emailConfig().enabled ? 'pointer-events-none opacity-50 transition-opacity' : 'transition-opacity'}`}
        >
          <EmailProviderSelect
            config={props.emailConfig()}
            onChange={(config) => {
              props.setEmailConfig(config);
              props.setHasUnsavedChanges(true);
            }}
            onTest={testEmailConfig}
            testing={testingEmail()}
          />
        </div>
      </SettingsPanel>

      <SettingsPanel
        title="Apprise notifications"
        description="Relay grouped alerts through the Apprise CLI."
        action={
          <Toggle
            checked={appriseState().enabled}
            onChange={(e) => {
              updateApprise({ enabled: e.currentTarget.checked });
              props.setHasUnsavedChanges(true);
            }}
            containerClass="sm:self-start"
            label={
              <span class="text-xs font-medium text-gray-600 dark:text-gray-400">
                {appriseState().enabled ? 'Enabled' : 'Disabled'}
              </span>
            }
          />
        }
        class="min-w-0"
        bodyClass="space-y-4"
      >
        <div class="space-y-4">
          <div class={formField}>
            <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>Delivery mode</label>
            <select
              class={formControl}
              value={appriseState().mode}
              onInput={(e) => {
                updateApprise({ mode: e.currentTarget.value as 'cli' | 'http' });
                props.setHasUnsavedChanges(true);
              }}
            >
              <option value="cli">Local Apprise CLI</option>
              <option value="http">Remote Apprise API</option>
            </select>
            <p class={formHelpText}>Choose how Pulse should execute Apprise notifications.</p>
          </div>

          <div class={formField}>
            <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>Delivery targets</label>
            <textarea
              rows={4}
              class={`${formControl} font-mono min-h-[120px]`}
              value={appriseState().targetsText}
              placeholder={`discord://token
mailto://alerts@example.com`}
              onInput={(e) => {
                updateApprise({ targetsText: e.currentTarget.value });
                props.setHasUnsavedChanges(true);
              }}
            />
            <p class={formHelpText}>
              {appriseState().mode === 'http'
                ? 'Optional: override the URLs defined on your Apprise API instance. Leave blank to use the server defaults.'
                : 'Enter one Apprise URL per line. Commas are also supported.'}
            </p>
          </div>
          <Show when={appriseState().mode === 'cli'}>
            <div class={formField}>
              <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>CLI path</label>
              <input
                type="text"
                value={appriseState().cliPath}
                class={formControl}
                placeholder="apprise"
                onInput={(e) => {
                  updateApprise({ cliPath: e.currentTarget.value });
                  props.setHasUnsavedChanges(true);
                }}
              />
              <p class={formHelpText}>Leave blank to use the default `apprise` executable.</p>
            </div>
          </Show>

          <Show when={appriseState().mode === 'http'}>
            <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div class={`${formField} sm:col-span-2`}>
                <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>Server URL</label>
                <input
                  type="text"
                  value={appriseState().serverUrl}
                  class={formControl}
                  placeholder="https://apprise-api.internal:8000"
                  onInput={(e) => {
                    updateApprise({ serverUrl: e.currentTarget.value });
                    props.setHasUnsavedChanges(true);
                  }}
                />
                <p class={formHelpText}>Point to an Apprise API endpoint such as https://host:8000.</p>
              </div>
              <div class={formField}>
                <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>Config key (optional)</label>
                <input
                  type="text"
                  value={appriseState().configKey}
                  class={formControl}
                  placeholder="default"
                  onInput={(e) => {
                    updateApprise({ configKey: e.currentTarget.value });
                    props.setHasUnsavedChanges(true);
                  }}
                />
                <p class={formHelpText}>Targets the /notify/&lt;key&gt; endpoint when provided.</p>
              </div>
              <div class={formField}>
                <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>API key</label>
                <input
                  type="password"
                  value={appriseState().apiKey}
                  class={formControl}
                  placeholder="Optional API key"
                  onInput={(e) => {
                    updateApprise({ apiKey: e.currentTarget.value });
                    props.setHasUnsavedChanges(true);
                  }}
                />
                <p class={formHelpText}>Included with each request when your Apprise API requires authentication.</p>
              </div>
              <div class={formField}>
                <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>API key header</label>
                <input
                  type="text"
                  value={appriseState().apiKeyHeader}
                  class={formControl}
                  placeholder="X-API-KEY"
                  onInput={(e) => {
                    updateApprise({ apiKeyHeader: e.currentTarget.value });
                    props.setHasUnsavedChanges(true);
                  }}
                />
                <p class={formHelpText}>Defaults to X-API-KEY for Apprise API deployments.</p>
              </div>
              <div class={`${formField} sm:col-span-2`}>
                <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>TLS verification</label>
                <label class="inline-flex items-center gap-2">
                  <input
                    type="checkbox"
                    class="h-4 w-4 rounded border border-gray-300 dark:border-gray-600"
                    checked={appriseState().skipTlsVerify}
                    onChange={(e) => {
                      updateApprise({ skipTlsVerify: e.currentTarget.checked });
                      props.setHasUnsavedChanges(true);
                    }}
                  />
                  <span class="text-sm text-gray-600 dark:text-gray-400">Allow self-signed certificates</span>
                </label>
                <p class={formHelpText}>Enable only when the Apprise API uses a self-signed certificate.</p>
              </div>
            </div>
          </Show>

          <div class={formField}>
            <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>Timeout (seconds)</label>
            <input
              type="number"
              min="5"
              max="120"
              value={appriseState().timeoutSeconds}
              class={formControl}
              onInput={(e) => {
                const raw = e.currentTarget.valueAsNumber;
                const safe = Number.isNaN(raw) ? 15 : Math.min(120, Math.max(5, Math.trunc(raw)));
                updateApprise({ timeoutSeconds: safe });
                props.setHasUnsavedChanges(true);
              }}
            />
            <p class={formHelpText}>Maximum time to wait for Apprise to respond.</p>
          </div>
        </div>
      </SettingsPanel>

      <SettingsPanel
        title="Webhooks"
        description="Push alerts to chat apps or automation systems."
        action={
          <span class="text-xs text-gray-500 dark:text-gray-400 whitespace-nowrap">
            {webhooks().length} configured
          </span>
        }
        class="min-w-0"
        bodyClass="space-y-4"
      >
        <WebhookConfig
          webhooks={webhooks()}
          onAdd={async (webhook) => {
            try {
              const created = await NotificationsAPI.createWebhook(webhook);
              setWebhooks([...webhooks(), created]);
              showSuccess('Webhook added successfully');
            } catch (err) {
              console.error('Failed to add webhook:', err);
              showError(err instanceof Error ? err.message : 'Failed to add webhook');
            }
          }}
          onUpdate={async (webhook) => {
            try {
              const updated = await NotificationsAPI.updateWebhook(webhook.id!, webhook);
              setWebhooks(webhooks().map((w) => (w.id === webhook.id ? updated : w)));
              showSuccess('Webhook updated successfully');
            } catch (err) {
              console.error('Failed to update webhook:', err);
              showError(err instanceof Error ? err.message : 'Failed to update webhook');
            }
          }}
          onDelete={async (id) => {
            try {
              await NotificationsAPI.deleteWebhook(id);
              setWebhooks(webhooks().filter((w) => w.id !== id));
              showSuccess('Webhook deleted successfully');
            } catch (err) {
              console.error('Failed to delete webhook:', err);
              showError(err instanceof Error ? err.message : 'Failed to delete webhook');
            }
          }}
          onTest={testWebhook}
          testing={testingWebhook()}
        />
      </SettingsPanel>
    </div>
  );
}

// History Tab - Alert history

// Schedule Tab - Quiet hours, cooldown, and grouping
interface ScheduleTabProps {
  hasUnsavedChanges: () => boolean;
  setHasUnsavedChanges: (value: boolean) => void;
  quietHours: () => QuietHoursConfig;
  setQuietHours: (value: QuietHoursConfig) => void;
  cooldown: () => CooldownConfig;
  setCooldown: (value: CooldownConfig) => void;
  grouping: () => GroupingConfig;
  setGrouping: (value: GroupingConfig) => void;
  escalation: () => EscalationConfig;
  setEscalation: (value: EscalationConfig) => void;
}

function ScheduleTab(props: ScheduleTabProps) {
  // Use props instead of local state
  const quietHours = props.quietHours;
  const setQuietHours = props.setQuietHours;
  const cooldown = props.cooldown;
  const setCooldown = props.setCooldown;
  const grouping = props.grouping;
  const setGrouping = props.setGrouping;
  const escalation = props.escalation;
  const setEscalation = props.setEscalation;
  const resetToDefaults = () => {
    setQuietHours(createDefaultQuietHours());
    setCooldown(createDefaultCooldown());
    setGrouping(createDefaultGrouping());
    setEscalation(createDefaultEscalation());
    props.setHasUnsavedChanges(true);
  };

  // Comprehensive list of common IANA timezones
  const timezones = [
    'UTC',
    // Africa
    'Africa/Cairo',
    'Africa/Johannesburg',
    'Africa/Lagos',
    'Africa/Nairobi',
    // Americas
    'America/Anchorage',
    'America/Argentina/Buenos_Aires',
    'America/Bogota',
    'America/Caracas',
    'America/Chicago',
    'America/Denver',
    'America/Halifax',
    'America/Lima',
    'America/Los_Angeles',
    'America/Mexico_City',
    'America/New_York',
    'America/Phoenix',
    'America/Santiago',
    'America/Sao_Paulo',
    'America/St_Johns',
    'America/Toronto',
    'America/Vancouver',
    // Asia
    'Asia/Bangkok',
    'Asia/Dhaka',
    'Asia/Dubai',
    'Asia/Hong_Kong',
    'Asia/Jakarta',
    'Asia/Jerusalem',
    'Asia/Karachi',
    'Asia/Kolkata',
    'Asia/Kuala_Lumpur',
    'Asia/Manila',
    'Asia/Riyadh',
    'Asia/Seoul',
    'Asia/Shanghai',
    'Asia/Singapore',
    'Asia/Taipei',
    'Asia/Tehran',
    'Asia/Tokyo',
    // Australia
    'Australia/Adelaide',
    'Australia/Brisbane',
    'Australia/Melbourne',
    'Australia/Perth',
    'Australia/Sydney',
    // Europe
    'Europe/Amsterdam',
    'Europe/Athens',
    'Europe/Berlin',
    'Europe/Brussels',
    'Europe/Budapest',
    'Europe/Copenhagen',
    'Europe/Dublin',
    'Europe/Helsinki',
    'Europe/Istanbul',
    'Europe/Lisbon',
    'Europe/London',
    'Europe/Madrid',
    'Europe/Moscow',
    'Europe/Oslo',
    'Europe/Paris',
    'Europe/Prague',
    'Europe/Rome',
    'Europe/Stockholm',
    'Europe/Vienna',
    'Europe/Warsaw',
    'Europe/Zurich',
    // Pacific
    'Pacific/Auckland',
    'Pacific/Fiji',
    'Pacific/Guam',
    'Pacific/Honolulu',
  ];

  const quietHourSuppressOptions: Array<{
    key: keyof QuietHoursConfig['suppress'];
    label: string;
    description: string;
  }> = [
    {
      key: 'performance',
      label: 'Performance alerts',
      description: 'CPU, memory, disk, and network thresholds stay quiet.',
    },
    {
      key: 'storage',
      label: 'Storage alerts',
      description: 'Silence storage usage, disk health, and ZFS events.',
    },
    {
      key: 'offline',
      label: 'Offline & power state',
      description: 'Skip connectivity and powered-off alerts during backups.',
    },
  ];

  const days = [
    { id: 'monday', label: 'M', fullLabel: 'Monday' },
    { id: 'tuesday', label: 'T', fullLabel: 'Tuesday' },
    { id: 'wednesday', label: 'W', fullLabel: 'Wednesday' },
    { id: 'thursday', label: 'T', fullLabel: 'Thursday' },
    { id: 'friday', label: 'F', fullLabel: 'Friday' },
    { id: 'saturday', label: 'S', fullLabel: 'Saturday' },
    { id: 'sunday', label: 'S', fullLabel: 'Sunday' },
  ];

  return (
    <div class="space-y-6">
      <div class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div>
          <h3 class="text-base font-semibold text-gray-900 dark:text-gray-100">
            Alert scheduling
          </h3>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            Configure when and how alerts are delivered
          </p>
        </div>
        <button
          type="button"
          onClick={resetToDefaults}
          class="inline-flex items-center gap-2 self-start rounded-md border border-gray-300 bg-white px-3 py-2 text-sm font-medium text-gray-700 shadow-sm transition-colors hover:bg-gray-50 dark:border-gray-600 dark:bg-gray-800 dark:text-gray-300 dark:hover:bg-gray-700"
          title="Restore quiet hours, cooldown, grouping, and escalation settings to their defaults"
        >
          <svg
            class="h-4 w-4"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="2"
          >
            <path
              stroke-linecap="round"
              stroke-linejoin="round"
              d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
            />
          </svg>
          Reset to defaults
        </button>
      </div>

      <div class="grid gap-6 xl:grid-cols-2">
        {/* Quiet Hours */}
        <SettingsPanel
          title="Quiet hours"
          description="Pause non-critical alerts during specific times."
          action={
            <Toggle
              checked={quietHours().enabled}
              onChange={(e) => {
                setQuietHours({ ...quietHours(), enabled: e.currentTarget.checked });
                props.setHasUnsavedChanges(true);
              }}
              containerClass="sm:self-start"
              label={
                <span class="text-xs font-medium text-gray-600 dark:text-gray-400">
                  {quietHours().enabled ? 'Enabled' : 'Disabled'}
                </span>
              }
            />
          }
          class="space-y-4"
        >
          <Show when={quietHours().enabled}>
            <div class="space-y-4">
              <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
                <div class={formField}>
                  <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>
                    Start time
                  </label>
                  <input
                    type="time"
                    value={quietHours().start}
                    onChange={(e) => {
                      setQuietHours({ ...quietHours(), start: e.currentTarget.value });
                      props.setHasUnsavedChanges(true);
                    }}
                    class={controlClass('font-mono')}
                  />
                </div>
                <div class={formField}>
                  <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>End time</label>
                  <input
                    type="time"
                    value={quietHours().end}
                    onChange={(e) => {
                      setQuietHours({ ...quietHours(), end: e.currentTarget.value });
                      props.setHasUnsavedChanges(true);
                    }}
                    class={controlClass('font-mono')}
                  />
                </div>
                <div class={formField}>
                  <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>Timezone</label>
                  <select
                    value={quietHours().timezone}
                    onChange={(e) => {
                      setQuietHours({ ...quietHours(), timezone: e.currentTarget.value });
                      props.setHasUnsavedChanges(true);
                    }}
                    class={controlClass('pr-8')}
                  >
                    <For each={timezones}>{(tz) => <option value={tz}>{tz}</option>}</For>
                  </select>
                </div>
              </div>

              <div>
                <span class={`${labelClass('text-xs uppercase tracking-[0.08em]')} mb-2 block`}>
                  Quiet days
                </span>
                <div class="grid grid-cols-7 gap-1">
                  <For each={days}>
                    {(day) => (
                      <button
                        type="button"
                        onClick={() => {
                          const currentDays = quietHours().days;
                          setQuietHours({
                            ...quietHours(),
                            days: { ...currentDays, [day.id]: !currentDays[day.id] },
                          });
                          props.setHasUnsavedChanges(true);
                        }}
                        title={day.fullLabel}
                        class={`px-2 py-2 text-xs font-medium transition-all duration-200 ${
                          quietHours().days[day.id]
                            ? 'rounded-md bg-blue-500 text-white shadow-sm'
                            : 'rounded-md bg-gray-100 text-gray-600 hover:bg-gray-200 dark:bg-gray-700 dark:text-gray-400 dark:hover:bg-gray-600'
                        }`}
                      >
                        {day.label}
                      </button>
                    )}
                  </For>
                </div>
                <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">
                  <Show
                    when={
                      quietHours().days.monday &&
                      quietHours().days.tuesday &&
                      quietHours().days.wednesday &&
                      quietHours().days.thursday &&
                      quietHours().days.friday &&
                      !quietHours().days.saturday &&
                      !quietHours().days.sunday
                    }
                  >
                    Weekdays only
                  </Show>
                  <Show
                    when={
                      !quietHours().days.monday &&
                      !quietHours().days.tuesday &&
                      !quietHours().days.wednesday &&
                      !quietHours().days.thursday &&
                      !quietHours().days.friday &&
                      quietHours().days.saturday &&
                      quietHours().days.sunday
                    }
                  >
                    Weekends only
                  </Show>
                </p>
              </div>

              <div class="space-y-3 border-t border-gray-200 pt-4 dark:border-gray-700">
                <span class={`${labelClass('text-xs uppercase tracking-[0.08em]')} block`}>
                  Suppress categories
                </span>
                <p class="text-xs text-gray-500 dark:text-gray-400">
                  Critical alerts in selected categories will stay silent during quiet hours.
                </p>
                <div class="flex flex-col gap-2 sm:flex-row sm:flex-wrap sm:gap-3">
                  <For each={quietHourSuppressOptions}>
                    {(option) => (
                      <label
                        class={`flex cursor-pointer items-start gap-3 rounded-lg border px-3 py-2 transition-colors ${
                          quietHours().suppress[option.key]
                            ? 'border-blue-500 bg-blue-50 dark:border-blue-400 dark:bg-blue-500/10'
                            : 'border-gray-200 hover:bg-gray-50 dark:border-gray-600 dark:hover:bg-gray-700'
                        }`}
                      >
                        <input
                          type="checkbox"
                          checked={quietHours().suppress[option.key]}
                          onChange={(e) => {
                            setQuietHours({
                              ...quietHours(),
                              suppress: {
                                ...quietHours().suppress,
                                [option.key]: e.currentTarget.checked,
                              },
                            });
                            props.setHasUnsavedChanges(true);
                          }}
                          class="sr-only"
                        />
                        <div
                          class={`mt-1 flex h-4 w-4 items-center justify-center rounded border-2 ${
                            quietHours().suppress[option.key]
                              ? 'border-blue-500 bg-blue-500'
                              : 'border-gray-300 dark:border-gray-600'
                          }`}
                        >
                          <Show when={quietHours().suppress[option.key]}>
                            <svg
                              class="h-3 w-3 text-white"
                              fill="none"
                              viewBox="0 0 24 24"
                              stroke="currentColor"
                              stroke-width="3"
                            >
                              <path stroke-linecap="round" stroke-linejoin="round" d="M5 13l4 4L19 7" />
                            </svg>
                          </Show>
                        </div>
                        <div>
                          <p class="text-sm font-medium text-gray-700 dark:text-gray-200">
                            {option.label}
                          </p>
                          <p class="text-xs text-gray-500 dark:text-gray-400">
                            {option.description}
                          </p>
                        </div>
                      </label>
                    )}
                  </For>
                </div>
              </div>
            </div>
          </Show>
        </SettingsPanel>

        {/* Cooldown Period */}
        <SettingsPanel
          title="Alert cooldown"
          description="Limit alert frequency to prevent spam."
          action={
            <Toggle
              checked={cooldown().enabled}
              onChange={(e) => {
                setCooldown({ ...cooldown(), enabled: e.currentTarget.checked });
                props.setHasUnsavedChanges(true);
              }}
              containerClass="sm:self-start"
              label={
                <span class="text-xs font-medium text-gray-600 dark:text-gray-400">
                  {cooldown().enabled ? 'Enabled' : 'Disabled'}
                </span>
              }
            />
          }
          class="space-y-4"
        >
          <Show when={cooldown().enabled}>
            <div class="space-y-4">
              <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <div class={formField}>
                  <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>
                    Cooldown period
                  </label>
                  <div class="relative">
                    <input
                      type="number"
                      min="5"
                      max="120"
                      value={cooldown().minutes}
                      onChange={(e) => {
                        setCooldown({ ...cooldown(), minutes: parseInt(e.currentTarget.value) });
                        props.setHasUnsavedChanges(true);
                      }}
                      class={controlClass('pr-16')}
                    />
                    <span class="pointer-events-none absolute inset-y-0 right-3 flex items-center text-sm text-gray-500 dark:text-gray-400">
                      minutes
                    </span>
                  </div>
                  <p class={`${formHelpText} mt-1`}>
                    Minimum time between alerts for the same issue
                  </p>
                </div>

                <div class={formField}>
                  <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>
                    Max alerts / hour
                  </label>
                  <div class="relative">
                    <input
                      type="number"
                      min="1"
                      max="10"
                      value={cooldown().maxAlerts}
                      onChange={(e) => {
                        setCooldown({ ...cooldown(), maxAlerts: parseInt(e.currentTarget.value) });
                        props.setHasUnsavedChanges(true);
                      }}
                      class={controlClass('pr-16')}
                    />
                    <span class="pointer-events-none absolute inset-y-0 right-3 flex items-center text-sm text-gray-500 dark:text-gray-400">
                      alerts
                    </span>
                  </div>
                  <p class={`${formHelpText} mt-1`}>Per guest/metric combination</p>
                </div>
              </div>
            </div>
          </Show>
        </SettingsPanel>

        {/* Alert Grouping */}
        <SettingsPanel
          title="Smart grouping"
          description="Bundle similar alerts together."
          action={
            <Toggle
              checked={grouping().enabled}
              onChange={(e) => {
                setGrouping({ ...grouping(), enabled: e.currentTarget.checked });
                props.setHasUnsavedChanges(true);
              }}
              containerClass="sm:self-start"
              label={
                <span class="text-xs font-medium text-gray-600 dark:text-gray-400">
                  {grouping().enabled ? 'Enabled' : 'Disabled'}
                </span>
              }
            />
          }
          class="space-y-4"
        >
          <Show when={grouping().enabled}>
            <div class="space-y-4">
              <div class={formField}>
                <label class={labelClass('text-xs uppercase tracking-[0.08em]')}>
                  Grouping window
                </label>
                <div class="flex items-center gap-3">
                  <input
                    type="range"
                    min="1"
                    max="30"
                    value={grouping().window}
                    onChange={(e) => {
                      setGrouping({ ...grouping(), window: parseInt(e.currentTarget.value) });
                      props.setHasUnsavedChanges(true);
                    }}
                    class="flex-1"
                  />
                  <div class="w-16 rounded-md bg-gray-100 px-2 py-1 text-center text-sm text-gray-700 dark:bg-gray-700 dark:text-gray-200">
                    {grouping().window} min
                  </div>
                </div>
                <p class={`${formHelpText} mt-1`}>
                  Alerts within this window are grouped together.
                </p>
              </div>

              <div>
                <span class={`${labelClass('text-xs uppercase tracking-[0.08em]')} mb-2 block`}>
                  Grouping strategy
                </span>
                <div class="grid grid-cols-1 gap-2 sm:grid-cols-2">
                  <label
                    class={`relative flex items-center gap-2 rounded-lg border-2 p-3 transition-all ${
                      grouping().byNode
                        ? 'border-blue-500 bg-blue-50 shadow-sm dark:bg-blue-900/20'
                        : 'border-gray-200 hover:bg-gray-50 dark:border-gray-600 dark:hover:bg-gray-700'
                    }`}
                  >
                    <input
                      type="checkbox"
                      checked={grouping().byNode}
                      onChange={(e) => {
                        setGrouping({ ...grouping(), byNode: e.currentTarget.checked });
                        props.setHasUnsavedChanges(true);
                      }}
                      class="sr-only"
                    />
                    <div
                      class={`flex h-4 w-4 items-center justify-center rounded border-2 ${
                        grouping().byNode
                          ? 'border-blue-500 bg-blue-500'
                          : 'border-gray-300 dark:border-gray-600'
                      }`}
                    >
                      <Show when={grouping().byNode}>
                        <svg
                          class="h-3 w-3 text-white"
                          fill="none"
                          viewBox="0 0 24 24"
                          stroke="currentColor"
                          stroke-width="3"
                        >
                          <path stroke-linecap="round" stroke-linejoin="round" d="M5 13l4 4L19 7" />
                        </svg>
                      </Show>
                    </div>
                    <span class="text-sm font-medium text-gray-700 dark:text-gray-300">
                      By Node
                    </span>
                  </label>

                  <label
                    class={`relative flex items-center gap-2 rounded-lg border-2 p-3 transition-all ${
                      grouping().byGuest
                        ? 'border-blue-500 bg-blue-50 shadow-sm dark:bg-blue-900/20'
                        : 'border-gray-200 hover:bg-gray-50 dark:border-gray-600 dark:hover:bg-gray-700'
                    }`}
                  >
                    <input
                      type="checkbox"
                      checked={grouping().byGuest}
                      onChange={(e) => {
                        setGrouping({ ...grouping(), byGuest: e.currentTarget.checked });
                        props.setHasUnsavedChanges(true);
                      }}
                      class="sr-only"
                    />
                    <div
                      class={`flex h-4 w-4 items-center justify-center rounded border-2 ${
                        grouping().byGuest
                          ? 'border-blue-500 bg-blue-500'
                          : 'border-gray-300 dark:border-gray-600'
                      }`}
                    >
                      <Show when={grouping().byGuest}>
                        <svg
                          class="h-3 w-3 text-white"
                          fill="none"
                          viewBox="0 0 24 24"
                          stroke="currentColor"
                          stroke-width="3"
                        >
                          <path stroke-linecap="round" stroke-linejoin="round" d="M5 13l4 4L19 7" />
                        </svg>
                      </Show>
                    </div>
                    <span class="text-sm font-medium text-gray-700 dark:text-gray-300">
                      By Guest
                    </span>
                  </label>
                </div>
              </div>
            </div>
          </Show>
        </SettingsPanel>

        {/* Escalation Rules */}
        <SettingsPanel
          title="Alert escalation"
          description="Notify additional contacts for persistent issues."
          action={
            <Toggle
              checked={escalation().enabled}
              onChange={(e) => {
                setEscalation({ ...escalation(), enabled: e.currentTarget.checked });
                props.setHasUnsavedChanges(true);
              }}
              containerClass="sm:self-start"
              label={
                <span class="text-xs font-medium text-gray-600 dark:text-gray-400">
                  {escalation().enabled ? 'Enabled' : 'Disabled'}
                </span>
              }
            />
          }
          class="space-y-4"
        >
          <Show when={escalation().enabled}>
            <div class="space-y-3">
              <p class={formHelpText}>Define escalation levels for unresolved alerts:</p>
              <For each={escalation().levels}>
                {(level, index) => (
                  <div class="flex items-center gap-3 rounded-lg border border-gray-200 bg-gray-50 p-3 dark:border-gray-600 dark:bg-gray-700/40">
                    <div class="flex flex-1 flex-col gap-3 sm:grid sm:grid-cols-2 sm:items-center sm:gap-2">
                      <div class="flex items-center gap-2">
                        <span class="text-xs font-medium text-gray-600 dark:text-gray-400">
                          After
                        </span>
                        <input
                          type="number"
                          min="5"
                          max="180"
                          value={level.after}
                          onChange={(e) => {
                            const newLevels = [...escalation().levels];
                            const parsed = Number.parseInt(e.currentTarget.value, 10);
                            newLevels[index()] = {
                              ...level,
                              after: Number.isNaN(parsed) ? level.after : parsed,
                            };
                            setEscalation({ ...escalation(), levels: newLevels });
                            props.setHasUnsavedChanges(true);
                          }}
                          class={`${controlClass('px-2 py-1 text-sm')} w-20`}
                        />
                        <span class="text-xs text-gray-600 dark:text-gray-400">min</span>
                      </div>
                      <div class="flex items-center gap-2">
                        <span class="text-xs font-medium text-gray-600 dark:text-gray-400">
                          Notify
                        </span>
                        <select
                          value={level.notify}
                          onChange={(e) => {
                            const newLevels = [...escalation().levels];
                            newLevels[index()] = {
                              ...level,
                              notify: e.currentTarget.value as EscalationNotifyTarget,
                            };
                            setEscalation({ ...escalation(), levels: newLevels });
                            props.setHasUnsavedChanges(true);
                          }}
                          class={`${controlClass('px-2 py-1 text-sm')} flex-1`}
                        >
                          <option value="email">Email</option>
                          <option value="webhook">Webhooks</option>
                          <option value="all">All Channels</option>
                        </select>
                      </div>
                    </div>
                    <button
                      type="button"
                      onClick={() => {
                        const newLevels = escalation().levels.filter((_, i) => i !== index());
                        setEscalation({ ...escalation(), levels: newLevels });
                        props.setHasUnsavedChanges(true);
                      }}
                      class="rounded-md p-1.5 text-red-600 transition-colors hover:bg-red-100 dark:hover:bg-red-900/30"
                      title="Remove escalation level"
                    >
                      <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                        <path
                          stroke-linecap="round"
                          stroke-linejoin="round"
                          stroke-width="2"
                          d="M19 7l-.867 12.142A2 2 0 0116.138 21H7.862a2 2 0 01-1.995-1.858L5 7m5 4v6m4-6v6m1-10V4a1 1 0 00-1-1h-4a1 1 0 00-1 1v3M4 7h16"
                        />
                      </svg>
                    </button>
                  </div>
                )}
              </For>

              <button
                type="button"
                onClick={() => {
                  const lastLevel = escalation().levels[escalation().levels.length - 1];
                  const newAfter = typeof lastLevel?.after === 'number' ? lastLevel.after + 30 : 15;
                  setEscalation({
                    ...escalation(),
                    levels: [
                      ...escalation().levels,
                      { after: newAfter, notify: 'all' as EscalationNotifyTarget },
                    ],
                  });
                  props.setHasUnsavedChanges(true);
                }}
                class="flex w-full items-center justify-center gap-2 rounded-lg border-2 border-dashed border-gray-300 py-2 text-sm text-gray-600 transition-all hover:border-gray-400 hover:bg-gray-50 dark:border-gray-600 dark:text-gray-400 dark:hover:border-gray-500 dark:hover:bg-gray-700"
              >
                <svg class="h-4 w-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path
                    stroke-linecap="round"
                    stroke-linejoin="round"
                    stroke-width="2"
                    d="M12 6v6m0 0v6m0-6h6m-6 0H6"
                  />
                </svg>
                Add Escalation Level
              </button>
            </div>
          </Show>
        </SettingsPanel>

        {/* Configuration Summary */}
        <SettingsPanel
          title="Configuration summary"
          description="Preview of the active schedule settings."
          tone="muted"
          padding="lg"
          bodyClass="space-y-1 text-sm text-blue-800 dark:text-blue-300"
          class="lg:col-span-2"
        >
          <Show when={quietHours().enabled}>
            <p>
              • Quiet hours active from {quietHours().start} to {quietHours().end} (
              {quietHours().timezone})
            </p>
          </Show>
          <Show
            when={
              quietHours().enabled &&
              (quietHours().suppress.performance ||
                quietHours().suppress.storage ||
                quietHours().suppress.offline)
            }
          >
            <p>
              • Suppressing{' '}
              {quietHourSuppressOptions
                .filter((option) => quietHours().suppress[option.key])
                .map((option) => option.label)
                .join(', ')}{' '}
              during quiet hours
            </p>
          </Show>
          <Show when={cooldown().enabled}>
            <p>
              • {cooldown().minutes} minute cooldown between alerts, max {cooldown().maxAlerts}{' '}
              alerts per hour
            </p>
          </Show>
          <Show when={grouping().enabled}>
            <p>
              • Grouping alerts within {grouping().window} minute windows
              <Show when={grouping().byNode || grouping().byGuest}>
                {' '}
                by{' '}
                {[grouping().byNode && 'node', grouping().byGuest && 'guest']
                  .filter(Boolean)
                  .join(' and ')}
              </Show>
            </p>
          </Show>
          <Show when={escalation().enabled && escalation().levels.length > 0}>
            <p>
              • {escalation().levels.length} escalation level
              {escalation().levels.length > 1 ? 's' : ''} configured
            </p>
          </Show>
          <Show
            when={
              !quietHours().enabled &&
              !cooldown().enabled &&
              !grouping().enabled &&
              !escalation().enabled
            }
          >
            <p>• All notification controls are disabled - alerts will be sent immediately</p>
          </Show>
        </SettingsPanel>
      </div>
    </div>
  );
}
// History Tab - Comprehensive alert table
function HistoryTab() {
  const { state, activeAlerts } = useWebSocket();

  // Filter states with localStorage persistence
  const [timeFilter, setTimeFilter] = usePersistentSignal<'24h' | '7d' | '30d' | 'all'>(
    'alertHistoryTimeFilter',
    '7d',
    {
      deserialize: (raw) => (raw === '24h' || raw === '7d' || raw === '30d' || raw === 'all' ? raw : '7d'),
    },
  );
  const [severityFilter, setSeverityFilter] = usePersistentSignal<'all' | 'warning' | 'critical'>(
    'alertHistorySeverityFilter',
    'all',
    {
      deserialize: (raw) => (raw === 'warning' || raw === 'critical' ? raw : 'all'),
    },
  );
  const [searchTerm, setSearchTerm] = createSignal('');
  const [alertHistory, setAlertHistory] = createSignal<Alert[]>([]);
  const [loading, setLoading] = createSignal(true);
  const [selectedBarIndex, setSelectedBarIndex] = createSignal<number | null>(null);
  const MS_PER_HOUR = 60 * 60 * 1000;
  const userLocale =
    Intl.DateTimeFormat().resolvedOptions().locale ||
    (typeof navigator !== 'undefined' ? navigator.language : undefined) ||
    'en-US';

  const buildHistoryParams = (range: string) => {
    const params: { limit?: number; startTime?: string } = {};
    const now = Date.now();

    switch (range) {
      case '24h':
        params.limit = 2000;
        params.startTime = new Date(now - 24 * MS_PER_HOUR).toISOString();
        break;
      case '7d':
        params.limit = 10000;
        params.startTime = new Date(now - 7 * 24 * MS_PER_HOUR).toISOString();
        break;
      case '30d':
        params.limit = 10000;
        params.startTime = new Date(now - 30 * 24 * MS_PER_HOUR).toISOString();
        break;
      case 'all':
        params.limit = 0;
        break;
      default:
        params.limit = 1000;
    }

    return params;
  };

  let fetchRequestId = 0;
  const fetchAlertHistory = async (range: string) => {
    const requestId = ++fetchRequestId;
    setLoading(true);

    try {
      const history = await AlertsAPI.getHistory(buildHistoryParams(range));
      if (requestId === fetchRequestId) {
        setAlertHistory(history);
      }
    } catch (err) {
      if (requestId === fetchRequestId) {
        console.error('Failed to load alert history:', err);
      }
    } finally {
      if (requestId === fetchRequestId) {
        setLoading(false);
      }
    }
  };

  // Ref for search input
  let searchInputRef: HTMLInputElement | undefined;

  // Persist filter changes to localStorage

  // Clear chart selection when high-level filters change
  let lastTimeFilterValue: string | null = null;
  createEffect(() => {
    const current = timeFilter();
    if (lastTimeFilterValue !== null && current !== lastTimeFilterValue) {
      setSelectedBarIndex(null);
    }
    lastTimeFilterValue = current;
  });

  let lastSeverityFilterValue: string | null = null;
  createEffect(() => {
    const current = severityFilter();
    if (lastSeverityFilterValue !== null && current !== lastSeverityFilterValue) {
      setSelectedBarIndex(null);
    }
    lastSeverityFilterValue = current;
  });

  // Load alert history on mount
  onMount(() => {
    fetchAlertHistory(timeFilter());

    // Add keyboard event listeners
    const handleKeydown = (e: KeyboardEvent) => {
      // If already focused on an input, select, or textarea, don't interfere
      const activeElement = document.activeElement;
      if (
        activeElement &&
        (activeElement.tagName === 'INPUT' ||
          activeElement.tagName === 'TEXTAREA' ||
          activeElement.tagName === 'SELECT')
      ) {
        // Handle Escape to clear and unfocus
        if (e.key === 'Escape' && activeElement === searchInputRef) {
          setSearchTerm('');
          searchInputRef.blur();
        }
        return;
      }

      // If typing a letter, number, or space, focus the search input
      if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
        searchInputRef?.focus();
      }
    };

    document.addEventListener('keydown', handleKeydown);

    onCleanup(() => {
      document.removeEventListener('keydown', handleKeydown);
      // Prevent pending requests from updating state after unmount
      fetchRequestId++;
    });
  });

  let skipInitialFetchEffect = true;
  createEffect(() => {
    const range = timeFilter();
    if (skipInitialFetchEffect) {
      skipInitialFetchEffect = false;
      return;
    }
    fetchAlertHistory(range);
  });

  // Format duration for display
  const formatDuration = (startTime: string, endTime?: string) => {
    const start = new Date(startTime).getTime();
    const end = endTime ? new Date(endTime).getTime() : Date.now();
    const duration = end - start;

    // Handle negative durations (clock skew or timezone issues)
    if (duration < 0) {
      return '0m';
    }

    const minutes = Math.floor(duration / 60000);
    const hours = Math.floor(minutes / 60);
    const days = Math.floor(hours / 24);

    if (days > 0) return `${days}d ${hours % 24}h`;
    if (hours > 0) return `${hours}h ${minutes % 60}m`;
    return `${minutes}m`;
  };

  const formatBucketRange = (startMs: number, endMs: number) => {
    const start = new Date(startMs);
    const end = new Date(endMs);

    const sameDay =
      start.getFullYear() === end.getFullYear() &&
      start.getMonth() === end.getMonth() &&
      start.getDate() === end.getDate();

    const startDay = start.toLocaleDateString(userLocale, {
      month: 'short',
      day: 'numeric',
      year: start.getFullYear() !== end.getFullYear() ? 'numeric' : undefined,
    });
    const endDay = end.toLocaleDateString(userLocale, {
      month: 'short',
      day: 'numeric',
      year: 'numeric',
    });

    const timeFormatter: Intl.DateTimeFormatOptions = {
      hour: 'numeric',
      minute: '2-digit',
    };

    const startTimeStr = start.toLocaleTimeString(userLocale, timeFormatter);
    const endTimeStr = end.toLocaleTimeString(userLocale, timeFormatter);

    if (sameDay) {
      return `${startDay}, ${startTimeStr} – ${endTimeStr}`;
    }

    return `${startDay}, ${startTimeStr} → ${endDay}, ${endTimeStr}`;
  };

  // Get resource type (VM, CT, Node, Storage, Docker, PBS, etc.)
  const getResourceType = (
    resourceName: string,
    metadata?: Record<string, unknown> | undefined,
  ) => {
    const metadataType =
      typeof metadata?.resourceType === 'string'
        ? (metadata.resourceType as string)
        : undefined;
    if (metadataType && metadataType.trim().length > 0) {
      return metadataType;
    }

    // Check VMs and containers
    const vm = state.vms?.find((v) => v.name === resourceName);
    if (vm) return 'VM';

    const container = state.containers?.find((c) => c.name === resourceName);
    if (container) return 'CT';

    // Check nodes
    const node = state.nodes?.find((n) => n.name === resourceName);
    if (node) return 'Node';

    // Check storage
    const storage = state.storage?.find((s) => s.name === resourceName || s.id === resourceName);
    if (storage) return 'Storage';

    // Docker hosts
    const dockerHost = state.dockerHosts?.find(
      (host) =>
        host.displayName === resourceName ||
        host.hostname === resourceName ||
        host.agentId === resourceName ||
        host.id === resourceName,
    );
    if (dockerHost) return 'Docker Host';

    // Docker containers (via known hosts)
    const dockerContainer = state.dockerHosts
      ?.flatMap((host) => host.containers || [])
      .find((c) => c.name === resourceName || c.id === resourceName);
    if (dockerContainer) return 'Docker Container';

    // PBS instances
    const pbsInstance = state.pbs?.find(
      (pbs) => pbs.name === resourceName || pbs.host === resourceName || pbs.id === resourceName,
    );
    if (pbsInstance) return 'PBS';

    // Ceph clusters
    const cephCluster = state.cephClusters?.find(
      (cluster) => cluster.name === resourceName || cluster.id === resourceName,
    );
    if (cephCluster) return 'Ceph';

    return 'Unknown';
  };

  // Extended alert type for display
  interface ExtendedAlert extends Alert {
    status?: string;
    duration?: string;
    resourceType?: string;
  }

  // Prepare all alerts without filtering
  const allAlertsData = createMemo(() => {
    // Combine active and historical alerts
    const allAlerts: ExtendedAlert[] = [];

    // Add active alerts
    Object.values(activeAlerts || {}).forEach((alert) => {
      allAlerts.push({
        ...alert,
        status: 'active',
        duration: formatDuration(alert.startTime),
        resourceType: getResourceType(alert.resourceName, alert.metadata),
      });
    });

    // Create a set of active alert IDs for quick lookup
    const activeAlertIds = new Set(Object.keys(activeAlerts || {}));

    // Add historical alerts
    alertHistory().forEach((alert) => {
      // Skip if this alert is already in active alerts (avoid duplicates)
      if (activeAlertIds.has(alert.id)) {
        return;
      }

      allAlerts.push({
        ...alert,
        status: alert.acknowledged ? 'acknowledged' : 'resolved',
        duration: formatDuration(alert.startTime, alert.lastSeen),
        resourceType: getResourceType(alert.resourceName, alert.metadata),
      });
    });

    return allAlerts;
  });

  // Apply severity & search filters (time filtering is layered separately)
  const severityAndSearchFilteredAlerts = createMemo(() => {
    let filtered = allAlertsData();

    if (severityFilter() !== 'all') {
      filtered = filtered.filter((a) => a.level === severityFilter());
    }

    if (searchTerm()) {
      const term = searchTerm().toLowerCase();
      filtered = filtered.filter((alert) => {
        const name = alert.resourceName?.toLowerCase() ?? '';
        const message = alert.message?.toLowerCase() ?? '';
        const type = alert.type?.toLowerCase() ?? '';
        const nodeName = alert.node?.toLowerCase() ?? '';
        return (
          name.includes(term) || message.includes(term) || type.includes(term) || nodeName.includes(term)
        );
      });
    }

    return filtered;
  });

  // Apply filters to get the final alert data
  const alertData = createMemo(() => {
    let filtered = severityAndSearchFilteredAlerts();
    const currentTimeFilter = timeFilter();

    // Selected bar filter (takes precedence over time filter)
    if (selectedBarIndex() !== null) {
      const trends = alertTrends();
      const index = selectedBarIndex()!;
      const bucketStart = trends.bucketTimes[index];
      const bucketEnd = bucketStart + trends.bucketSize * MS_PER_HOUR;

      filtered = filtered.filter((alert) => {
        const alertTime = new Date(alert.startTime).getTime();
        return alertTime >= bucketStart && alertTime < bucketEnd;
      });
    } else if (currentTimeFilter !== 'all') {
      const now = Date.now();
      const cutoffMap: Record<'24h' | '7d' | '30d', number> = {
        '24h': now - 24 * 60 * 60 * 1000,
        '7d': now - 7 * 24 * 60 * 60 * 1000,
        '30d': now - 30 * 24 * 60 * 60 * 1000,
      };
      const cutoff = cutoffMap[currentTimeFilter];

      if (cutoff) {
        filtered = filtered.filter((a) => new Date(a.startTime).getTime() > cutoff);
      }
    }

    // Sort by start time (newest first)
    return [...filtered].sort(
      (a, b) => new Date(b.startTime).getTime() - new Date(a.startTime).getTime(),
    );
  });

  const monthNames = [
    'January',
    'February',
    'March',
    'April',
    'May',
    'June',
    'July',
    'August',
    'September',
    'October',
    'November',
    'December',
  ];

  const getDaySuffix = (day: number) => {
    if (day >= 11 && day <= 13) return 'th';
    switch (day % 10) {
      case 1:
        return 'st';
      case 2:
        return 'nd';
      case 3:
        return 'rd';
      default:
        return 'th';
    }
  };

  const formatAlertGroupLabel = (date: Date, todayStart: number, yesterdayStart: number) => {
    const month = monthNames[date.getMonth()];
    const day = date.getDate();
    const suffix = getDaySuffix(day);
    const absoluteDate = `${month} ${day}${suffix}`;

    if (date.getTime() === todayStart) {
      return `Today (${absoluteDate})`;
    }

    if (date.getTime() === yesterdayStart) {
      return `Yesterday (${absoluteDate})`;
    }

    return absoluteDate;
  };

  type AlertHistoryRow = ReturnType<typeof alertData>[number];

  // Group alerts by day for display
  const groupedAlerts = createMemo(() => {
    const now = new Date();
    const todayDate = new Date(now.getFullYear(), now.getMonth(), now.getDate());
    const todayStart = todayDate.getTime();
    const yesterdayDate = new Date(todayDate);
    yesterdayDate.setDate(yesterdayDate.getDate() - 1);
    const yesterdayStart = yesterdayDate.getTime();

    const groups = new Map<
      number,
      {
        date: Date;
        label: string;
        fullLabel: string;
        alerts: AlertHistoryRow[];
      }
    >();

    alertData().forEach((alert) => {
      const alertDate = new Date(alert.startTime);
      const dateOnly = new Date(
        alertDate.getFullYear(),
        alertDate.getMonth(),
        alertDate.getDate(),
      );
      const dateKey = dateOnly.getTime();

      if (!groups.has(dateKey)) {
        groups.set(dateKey, {
          date: dateOnly,
          label: formatAlertGroupLabel(dateOnly, todayStart, yesterdayStart),
          fullLabel: dateOnly.toLocaleDateString('en-US', {
            weekday: 'long',
            year: 'numeric',
            month: 'long',
            day: 'numeric',
          }),
          alerts: [],
        });
      }

      groups.get(dateKey)!.alerts.push(alert);
    });

    return Array.from(groups.values()).sort((a, b) => b.date.getTime() - a.date.getTime());
  });

  // Calculate alert trends for mini-chart
  const alertTrends = createMemo(() => {
    const now = Date.now();
    const msPerHour = MS_PER_HOUR;
    const filteredAlerts = severityAndSearchFilteredAlerts();
    const niceBucketSizes = [1, 2, 3, 6, 12, 24, 48, 72, 168, 336, 720, 1440]; // hours
    const maxBuckets = 30;

    let bucketSizeHours: number;
    let computedRangeHours: number;
    let startTime: number;

    const filter = timeFilter();
    if (filter === '24h') {
      bucketSizeHours = 1;
      computedRangeHours = 24;
      startTime = now - computedRangeHours * msPerHour;
    } else if (filter === '7d') {
      bucketSizeHours = 6;
      computedRangeHours = 7 * 24;
      startTime = now - computedRangeHours * msPerHour;
    } else if (filter === '30d') {
      bucketSizeHours = 24;
      computedRangeHours = 30 * 24;
      startTime = now - computedRangeHours * msPerHour;
    } else {
      if (!filteredAlerts.length) {
        bucketSizeHours = 24;
        computedRangeHours = 24;
        startTime = now - computedRangeHours * msPerHour;
      } else {
        const earliest = filteredAlerts.reduce((min, alert) => {
          const alertTime = new Date(alert.startTime).getTime();
          return Math.min(min, alertTime);
        }, now);
        const rawRangeHours = Math.max(1, Math.ceil((now - earliest) / msPerHour));
        const rawBucketSize = Math.max(1, Math.ceil(rawRangeHours / maxBuckets));
        bucketSizeHours =
          niceBucketSizes.find((size) => size >= rawBucketSize) ?? rawBucketSize;
        computedRangeHours = Math.max(rawRangeHours, bucketSizeHours);
        const bucketsNeeded = Math.min(
          Math.max(1, Math.ceil(computedRangeHours / bucketSizeHours)),
          maxBuckets,
        );
        startTime = now - bucketsNeeded * bucketSizeHours * msPerHour;
      }
    }

    const bucketCount = Math.min(
      Math.max(1, Math.ceil(computedRangeHours / bucketSizeHours)),
      maxBuckets,
    );
    startTime = Math.min(startTime, now - bucketCount * bucketSizeHours * msPerHour);

    const buckets = new Array(bucketCount).fill(0);
    const bucketTimes = new Array(bucketCount)
      .fill(0)
      .map((_, i) => startTime + i * bucketSizeHours * msPerHour);

    const windowStart = startTime;
    const windowEnd = now;

    filteredAlerts.forEach((alert) => {
      const alertTime = new Date(alert.startTime).getTime();
      if (alertTime < windowStart || alertTime > windowEnd) {
        return;
      }
      const rawIndex = Math.floor((alertTime - windowStart) / (bucketSizeHours * msPerHour));
      const bucketIndex = Math.min(bucketCount - 1, Math.max(0, rawIndex));
      if (bucketIndex >= 0 && bucketIndex < bucketCount) {
        buckets[bucketIndex]++;
      }
    });

    const max = Math.max(...buckets, 1);

    return {
      buckets,
      max,
      bucketSize: bucketSizeHours,
      bucketTimes,
      rangeStart: windowStart,
      rangeHours: bucketCount * bucketSizeHours,
    };
  });

  const bucketDurationLabel = createMemo(() => {
    const bucketHours = alertTrends().bucketSize;
    if (!Number.isFinite(bucketHours) || bucketHours <= 0) {
      return '—';
    }
    if (bucketHours % 24 === 0) {
      const days = bucketHours / 24;
      return `${days} day${days === 1 ? '' : 's'}`;
    }
    return `${bucketHours} hour${bucketHours === 1 ? '' : 's'}`;
  });

  const formatAxisTickLabel = (
    timestamp: number,
    bucketHours: number,
    totalHours: number,
    isEnd = false,
  ) => {
    if (!Number.isFinite(timestamp)) return '—';

    if (isEnd && Math.abs(Date.now() - timestamp) < bucketHours * MS_PER_HOUR * 0.75) {
      return 'Now';
    }

    const date = new Date(timestamp);
    const options: Intl.DateTimeFormatOptions = {};

    if (totalHours <= 48) {
      options.month = 'short';
      options.day = 'numeric';
      options.hour = '2-digit';
      options.minute = '2-digit';
    } else if (totalHours <= 24 * 90) {
      options.month = 'short';
      options.day = 'numeric';
      if (bucketHours <= 12 || totalHours <= 24 * 14) {
        options.hour = '2-digit';
      }
    } else {
      options.year = 'numeric';
      options.month = 'short';
      options.day = 'numeric';
    }

    return date.toLocaleString(userLocale, options);
  };

  const rangeSummary = createMemo(() => {
    const trends = alertTrends();
    if (!trends.bucketTimes.length || trends.bucketSize <= 0) {
      return null;
    }

    const bucketHours = trends.bucketSize;
    const totalHours = Math.max(trends.rangeHours ?? bucketHours, bucketHours);
    const start = trends.bucketTimes[0];
    const end = start + trends.buckets.length * bucketHours * MS_PER_HOUR;

    return {
      startLabel: formatAxisTickLabel(start, bucketHours, totalHours),
      endLabel: formatAxisTickLabel(end, bucketHours, totalHours, true),
    };
  });

  const axisTicks = createMemo(() => {
    const trends = alertTrends();
    if (!trends.bucketTimes.length || trends.bucketSize <= 0) {
      return [] as Array<{ position: number; label: string; align: 'start' | 'center' | 'end' }>;
    }

    const bucketHours = trends.bucketSize;
    const totalHours = Math.max(trends.rangeHours ?? bucketHours, bucketHours);
    const start = trends.bucketTimes[0];
    const totalDurationMs = Math.max(
      trends.buckets.length * bucketHours * MS_PER_HOUR,
      bucketHours * MS_PER_HOUR,
    );
    const end = start + totalDurationMs;

    const desiredTicks = Math.min(5, trends.bucketTimes.length + 1);
    const step = Math.max(
      1,
      Math.round(trends.bucketTimes.length / Math.max(1, desiredTicks - 1)),
    );
    const ticks: Array<{ position: number; label: string }> = [];

    for (let index = 0; index < trends.bucketTimes.length; index += step) {
      const ts = trends.bucketTimes[index];
      const position = Math.min(
        1,
        Math.max(0, (ts - start) / (totalDurationMs || 1)),
      );
      ticks.push({
        position,
        label: formatAxisTickLabel(ts, bucketHours, totalHours),
      });
    }

    if (!ticks.length || ticks[0].position > 0.01) {
      ticks.unshift({
        position: 0,
        label: formatAxisTickLabel(start, bucketHours, totalHours),
      });
    } else {
      ticks[0] = {
        position: 0,
        label: formatAxisTickLabel(start, bucketHours, totalHours),
      };
    }

    const lastTick = ticks[ticks.length - 1];
    if (!lastTick || Math.abs(lastTick.position - 1) > 0.01) {
      ticks.push({
        position: 1,
        label: formatAxisTickLabel(end, bucketHours, totalHours, true),
      });
    } else {
      ticks[ticks.length - 1] = {
        position: 1,
        label: formatAxisTickLabel(end, bucketHours, totalHours, true),
      };
    }

    return ticks.map((tick, index, arr) => ({
      position: tick.position,
      label: tick.label,
      align: index === 0 ? 'start' : index === arr.length - 1 ? 'end' : 'center',
    }));
  });

  const selectedBucketDetails = createMemo(() => {
    const index = selectedBarIndex();
    if (index === null) return null;
    const trends = alertTrends();
    const bucketStart = trends.bucketTimes[index];
    const bucketEnd = bucketStart + trends.bucketSize * MS_PER_HOUR;
    return {
      rangeLabel: formatBucketRange(bucketStart, bucketEnd),
      start: bucketStart,
      end: bucketEnd,
    };
  });

  return (
    <div class="space-y-4">
      {/* Alert Trends Mini-Chart */}
      <Card padding="md">
        <div class="mb-3 flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between sm:gap-3">
          <SectionHeader
            title="Alert frequency"
            description={
              <span class="text-xs text-gray-500 dark:text-gray-400">
                {alertData().length} alerts
              </span>
            }
            size="sm"
            class="flex-1"
          />
          <div class="flex flex-col items-start gap-2 sm:items-end">
            <Show when={selectedBucketDetails()}>
              {(selection) => (
                <div class="inline-flex items-center gap-2 rounded-full border border-blue-200 dark:border-blue-700 bg-blue-50 dark:bg-blue-900/30 px-3 py-1 text-xs text-blue-700 dark:text-blue-200">
                  <span class="font-medium uppercase tracking-wide text-[10px] text-blue-600 dark:text-blue-300">
                    Filtered Range
                  </span>
                  <span class="font-mono text-[11px]">{selection().rangeLabel}</span>
                </div>
              )}
            </Show>
            <div class="flex flex-col items-start gap-1 text-xs text-gray-500 dark:text-gray-400 sm:items-end">
              <div>
                <span class="font-medium text-gray-600 dark:text-gray-300">Bar size:</span>{' '}
                {bucketDurationLabel()}
              </div>
              <Show when={rangeSummary()}>
                {(summary) => (
                  <div class="flex items-center gap-1 whitespace-nowrap">
                    <span class="font-medium text-gray-600 dark:text-gray-300">Range:</span>
                    <span>{summary().startLabel}</span>
                    <span class="text-gray-400 dark:text-gray-500">→</span>
                    <span>{summary().endLabel}</span>
                  </div>
                )}
              </Show>
            </div>
            <div class="flex flex-wrap items-center justify-end gap-2">
              <Show when={selectedBarIndex() !== null}>
                <button
                  type="button"
                  onClick={() => setSelectedBarIndex(null)}
                  class="px-2 py-0.5 text-xs bg-blue-100 dark:bg-blue-900/50 text-blue-700 dark:text-blue-300 rounded hover:bg-blue-200 dark:hover:bg-blue-800/50 transition-colors"
                >
                  Clear filter
                </button>
              </Show>
              <div class="flex items-center gap-2 text-xs text-gray-500 dark:text-gray-400">
                <span class="flex items-center gap-1">
                  <div class="h-2 w-2 rounded-full bg-yellow-500"></div>
                  {alertData().filter((a) => a.level === 'warning').length} warnings
                </span>
                <span class="flex items-center gap-1">
                  <div class="h-2 w-2 rounded-full bg-red-500"></div>
                  {alertData().filter((a) => a.level === 'critical').length} critical
                </span>
              </div>
            </div>
          </div>
        </div>

        {/* Mini sparkline chart */}
        <div class="mb-1 text-[10px] text-gray-400 dark:text-gray-500">
          Showing {alertTrends().buckets.length} time periods ({bucketDurationLabel()} each) ·
          Total: {alertData().length} alerts
        </div>

        {/* Alert frequency chart */}
        {(() => {
          const trends = alertTrends();
          return (
            <div class="rounded bg-gray-100 p-1 dark:bg-gray-800">
              <div class="flex h-12 items-end gap-1">
                {trends.buckets.map((val, i) => {
                  const scaledHeight =
                    val > 0 ? Math.min(100, Math.max(20, Math.log(val + 1) * 20)) : 0;
                  const pixelHeight =
                    val > 0 ? Math.max(8, (scaledHeight / 100) * 40) : 0; // 40px is roughly the inner height
                  const isSelected = selectedBarIndex() === i;
                  const bucketStart = trends.bucketTimes[i];
                  const bucketEnd = bucketStart + trends.bucketSize * MS_PER_HOUR;
                  const bucketRangeLabel = formatBucketRange(bucketStart, bucketEnd);
                  const bucketDurationText =
                    trends.bucketSize % 24 === 0
                      ? `${trends.bucketSize / 24} day${trends.bucketSize / 24 === 1 ? '' : 's'}`
                      : `${trends.bucketSize} hour${trends.bucketSize === 1 ? '' : 's'}`;
                  const countLabel =
                    val === 0 ? 'No alerts' : `${val} alert${val === 1 ? '' : 's'}`;
                  const tooltipContent = [countLabel, `${bucketDurationText} period`, bucketRangeLabel].join('\n');
                  return (
                    <div
                      class="flex-1 relative flex items-end cursor-pointer"
                      role="button"
                      tabIndex={0}
                      aria-pressed={isSelected}
                      aria-label={`${countLabel} between ${bucketRangeLabel}`}
                      onClick={() => setSelectedBarIndex(i === selectedBarIndex() ? null : i)}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter' || e.key === ' ') {
                          e.preventDefault();
                          setSelectedBarIndex(i === selectedBarIndex() ? null : i);
                        }
                      }}
                    >
                      {/* Background track for all slots */}
                      <div class="absolute bottom-0 h-1 w-full rounded-full bg-gray-300 opacity-30 dark:bg-gray-600"></div>
                      {/* Actual bar */}
                      <div
                        class="relative w-full rounded-sm transition-all"
                        style={{
                          height: `${pixelHeight}px`,
                          'background-color':
                            val > 0 ? (isSelected ? '#2563eb' : '#3b82f6') : 'transparent',
                          opacity: isSelected ? '1' : '0.8',
                          'box-shadow': isSelected ? '0 0 0 2px rgba(37, 99, 235, 0.4)' : 'none',
                        }}
                        title={bucketRangeLabel}
                        onMouseEnter={(e) => {
                          if (val <= 0) {
                            hideTooltip();
                            return;
                          }
                          const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
                          showTooltip(tooltipContent, rect.left + rect.width / 2, rect.top, {
                            align: 'center',
                            direction: 'up',
                          });
                        }}
                        onMouseLeave={() => hideTooltip()}
                      />
                    </div>
                  );
                })}
              </div>
            </div>
          );
        })()}

        <Show when={axisTicks().length > 0}>
          <div class="relative mt-3 h-10">
            <div class="absolute inset-x-0 top-0 h-px bg-gray-200 dark:bg-gray-700"></div>
            <For each={axisTicks()}>
              {(tick) => (
                <div
                  class="pointer-events-none absolute top-0 flex h-full flex-col items-center"
                  style={{ left: `${tick.position * 100}%` }}
                >
                  <div class="h-3 w-px bg-gray-300 dark:bg-gray-600"></div>
                  <div
                    class="mt-1 whitespace-nowrap text-[10px] text-gray-500 dark:text-gray-400 transform"
                    classList={{
                      '-translate-x-1/2': tick.align === 'center',
                      '-translate-x-full': tick.align === 'end',
                    }}
                  >
                    {tick.label}
                  </div>
                </div>
              )}
            </For>
          </div>
        </Show>
      </Card>

      {/* Filters */}
      <div class="flex flex-wrap gap-2 mb-4">
        <select
          value={timeFilter()}
          onChange={(e) => setTimeFilter(e.currentTarget.value as '24h' | '7d' | '30d' | 'all')}
          class="w-full sm:w-auto px-3 py-2 text-sm border rounded-lg dark:bg-gray-700 dark:border-gray-600"
        >
          <option value="24h">Last 24h</option>
          <option value="7d">Last 7d</option>
          <option value="30d">Last 30d</option>
          <option value="all">All Time</option>
        </select>

        <select
          value={severityFilter()}
          onChange={(e) => setSeverityFilter(e.currentTarget.value as 'warning' | 'critical' | 'all')}
          class="w-full sm:w-auto px-3 py-2 text-sm border rounded-lg dark:bg-gray-700 dark:border-gray-600"
        >
          <option value="all">All Levels</option>
          <option value="critical">Critical Only</option>
          <option value="warning">Warning Only</option>
        </select>

        <div class="w-full sm:flex-1 sm:max-w-xs">
          <input
            ref={searchInputRef}
            type="text"
            placeholder="Search alerts..."
            value={searchTerm()}
            onInput={(e) => setSearchTerm(e.currentTarget.value)}
            onKeyDown={(e) => {
              if (e.key === 'Escape') {
                setSearchTerm('');
                e.currentTarget.blur();
              }
            }}
            class="w-full px-3 py-2 text-sm border rounded-lg dark:bg-gray-700 dark:border-gray-600 
                   dark:text-gray-200 placeholder-gray-400 dark:placeholder-gray-500"
          />
        </div>
      </div>

      {/* Alert History Table */}
      <Show
        when={loading()}
        fallback={
          <Show
            when={alertData().length > 0}
            fallback={
              <div class="text-center py-12 text-gray-500 dark:text-gray-400">
                <p class="text-sm">No alerts found</p>
                <p class="text-xs mt-1">Try adjusting your filters or check back later</p>
              </div>
            }
          >
            {/* Table */}
            <div class="mb-2 border border-gray-200 dark:border-gray-700 rounded overflow-hidden">
              <ScrollableTable minWidth="900px">
                <table class="w-full min-w-[900px] text-xs sm:text-sm">
                  <thead>
                    <tr class="bg-gray-100 dark:bg-gray-700 text-gray-600 dark:text-gray-300 border-b border-gray-300 dark:border-gray-600">
                      <th class="p-1 px-2 text-left text-[10px] sm:text-xs font-medium uppercase tracking-wider">
                        Timestamp
                      </th>
                      <th class="p-1 px-2 text-left text-[10px] sm:text-xs font-medium uppercase tracking-wider">
                        Resource
                      </th>
                      <th class="p-1 px-2 text-left text-[10px] sm:text-xs font-medium uppercase tracking-wider">
                        Type
                      </th>
                      <th class="p-1 px-2 text-center text-[10px] sm:text-xs font-medium uppercase tracking-wider">
                        Severity
                      </th>
                      <th class="p-1 px-2 text-left text-[10px] sm:text-xs font-medium uppercase tracking-wider">
                        Message
                      </th>
                      <th class="p-1 px-2 text-center text-[10px] sm:text-xs font-medium uppercase tracking-wider">
                        Duration
                      </th>
                      <th class="p-1 px-2 text-center text-[10px] sm:text-xs font-medium uppercase tracking-wider">
                        Status
                      </th>
                      <th class="p-1 px-2 text-left text-[10px] sm:text-xs font-medium uppercase tracking-wider">
                        Node
                      </th>
                    </tr>
                  </thead>
                  <tbody>
                    <For each={groupedAlerts()}>
                      {(group) => (
                        <>
                          {/* Date divider */}
                          <tr class="bg-gray-50 dark:bg-gray-900/40">
                            <td
                              colspan="8"
                              class="py-1.5 pr-3 pl-4 text-[12px] sm:text-sm font-semibold text-slate-700 dark:text-slate-100"
                            >
                              <div class="flex flex-col gap-1 sm:flex-row sm:items-center sm:justify-between sm:gap-3">
                                <span class="truncate" title={group.fullLabel}>
                                  {group.label}
                                </span>
                                <span class="text-[10px] font-medium text-slate-500 dark:text-slate-400">
                                  {group.alerts.length}{' '}
                                  {group.alerts.length === 1 ? 'alert' : 'alerts'}
                                </span>
                              </div>
                            </td>
                          </tr>

                          {/* Alerts for this day */}
                          <For each={group.alerts}>
                            {(alert) => (
                              <tr
                                class={`border-b border-gray-200 dark:border-gray-600 hover:bg-gray-50 dark:hover:bg-gray-700 ${
                                  alert.status === 'active' ? 'bg-red-50 dark:bg-red-900/10' : ''
                                }`}
                              >
                                {/* Timestamp */}
                                <td class="p-1 px-2 text-gray-600 dark:text-gray-400 font-mono">
                                  {new Date(alert.startTime).toLocaleTimeString('en-US', {
                                    hour: '2-digit',
                                    minute: '2-digit',
                                  })}
                                </td>

                                {/* Resource */}
                                <td class="p-1 px-2 font-medium text-gray-900 dark:text-gray-100 truncate max-w-[150px]">
                                  {alert.resourceName}
                                </td>

                                {/* Type */}
                                <td class="p-1 px-2">
                                  <span
                                    class={`text-xs px-1 py-0.5 rounded ${
                                      alert.resourceType === 'VM'
                                        ? 'bg-blue-100 dark:bg-blue-900/50 text-blue-700 dark:text-blue-300'
                                        : alert.resourceType === 'CT'
                                          ? 'bg-green-100 dark:bg-green-900/50 text-green-700 dark:text-green-300'
                                          : alert.resourceType === 'Node'
                                            ? 'bg-purple-100 dark:bg-purple-900/50 text-purple-700 dark:text-purple-300'
                                            : alert.resourceType === 'Storage'
                                              ? 'bg-orange-100 dark:bg-orange-900/50 text-orange-700 dark:text-orange-300'
                                              : 'bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300'
                                    }`}
                                  >
                                    {alert.type}
                                  </span>
                                </td>

                                {/* Severity */}
                                <td class="p-1 px-2 text-center">
                                  <span
                                    class={`text-xs px-2 py-0.5 rounded font-medium ${
                                      alert.level === 'critical'
                                        ? 'bg-red-100 dark:bg-red-900/50 text-red-700 dark:text-red-300'
                                        : 'bg-yellow-100 dark:bg-yellow-900/50 text-yellow-700 dark:text-yellow-300'
                                    }`}
                                  >
                                    {alert.level}
                                  </span>
                                </td>

                                {/* Message */}
                                <td
                                  class="p-1 px-2 text-gray-700 dark:text-gray-300 truncate max-w-[300px]"
                                  title={alert.message}
                                >
                                  {alert.message}
                                </td>

                                {/* Duration */}
                                <td class="p-1 px-2 text-center text-gray-600 dark:text-gray-400">
                                  {alert.duration}
                                </td>

                                {/* Status */}
                                <td class="p-1 px-2 text-center">
                                  <span
                                    class={`text-xs px-2 py-0.5 rounded ${
                                      alert.status === 'active'
                                        ? 'bg-red-100 dark:bg-red-900/50 text-red-700 dark:text-red-300 font-medium'
                                        : alert.status === 'acknowledged'
                                          ? 'bg-yellow-100 dark:bg-yellow-900/50 text-yellow-700 dark:text-yellow-300'
                                          : 'bg-gray-100 dark:bg-gray-700 text-gray-700 dark:text-gray-300'
                                    }`}
                                  >
                                    {alert.status}
                                  </span>
                                </td>

                                {/* Node */}
                                <td class="p-1 px-2 text-gray-600 dark:text-gray-400 truncate">
                                  {alert.node || '—'}
                                </td>
                              </tr>
                            )}
                          </For>
                        </>
                      )}
                    </For>
                  </tbody>
                </table>
              </ScrollableTable>
            </div>
          </Show>
        }
      >
        <div class="text-center py-12 text-gray-500 dark:text-gray-400">
          <p class="text-sm">Loading alert history...</p>
        </div>
      </Show>

      {/* Administrative Actions - Only show if there's history to clear */}
      <Show when={alertHistory().length > 0}>
        <div class="mt-8 pt-6 border-t border-gray-200 dark:border-gray-700">
          <div class="bg-gray-50 dark:bg-gray-800/50 rounded-lg p-4">
            <div class="flex items-start justify-between">
              <div>
                <h4 class="text-sm font-medium text-gray-800 dark:text-gray-200 mb-1">
                  Administrative Actions
                </h4>
                <p class="text-xs text-gray-600 dark:text-gray-400">
                  Permanently clear all alert history. Use with caution - this action cannot be
                  undone.
                </p>
              </div>
              <button
                type="button"
                onClick={async () => {
                  if (
                    confirm(
                      'Are you sure you want to clear all alert history?\n\nThis will permanently delete all historical alert data and cannot be undone.\n\nThis is typically only used for system maintenance or when starting fresh with a new monitoring setup.',
                    )
                  ) {
                    try {
                      await AlertsAPI.clearHistory();
                      setAlertHistory([]);
                      // Alert history cleared successfully
                    } catch (err) {
                      console.error('Error clearing alert history:', err);
                      showError(
                        'Error clearing alert history',
                        'Please check your connection and try again.',
                      );
                    }
                  }
                }}
                class="px-3 py-2 text-xs border border-red-300 dark:border-red-600 text-red-600 dark:text-red-400 
                       rounded-md hover:bg-red-50 dark:hover:bg-red-900/20 transition-colors flex-shrink-0"
              >
                Clear All History
              </button>
            </div>
          </div>
        </div>
      </Show>
    </div>
  );
}
