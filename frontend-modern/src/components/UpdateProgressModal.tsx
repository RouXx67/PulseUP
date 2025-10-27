import { createSignal, Show, onMount, onCleanup, createEffect } from 'solid-js';
import { UpdatesAPI, type UpdateStatus } from '@/api/updates';

interface UpdateProgressModalProps {
  isOpen: boolean;
  onClose: () => void;
  onViewHistory: () => void;
  connected?: () => boolean;
  reconnecting?: () => boolean;
}

export function UpdateProgressModal(props: UpdateProgressModalProps) {
  const [status, setStatus] = createSignal<UpdateStatus | null>(null);
  const [isComplete, setIsComplete] = createSignal(false);
  const [hasError, setHasError] = createSignal(false);
  const [isRestarting, setIsRestarting] = createSignal(false);
  const [wsDisconnected, setWsDisconnected] = createSignal(false);
  let pollInterval: number | undefined;
  let healthCheckTimer: number | undefined;
  let healthCheckAttempts = 0;

  const clearHealthCheckTimer = () => {
    if (healthCheckTimer !== undefined) {
      clearTimeout(healthCheckTimer);
      healthCheckTimer = undefined;
    }
  };

  const pollStatus = async () => {
    try {
      const currentStatus = await UpdatesAPI.getUpdateStatus();
      setStatus(currentStatus);

      // Check if restarting
      if (currentStatus.status === 'restarting') {
        setIsRestarting(true);
        if (pollInterval) {
          clearInterval(pollInterval);
        }
        // Start health check polling
        startHealthCheckPolling();
        return;
      }

      // Check if complete or error
      if (
        currentStatus.status === 'completed' ||
        currentStatus.status === 'idle' ||
        currentStatus.status === 'error'
      ) {
        setIsComplete(true);
        if (currentStatus.status === 'error' || currentStatus.error) {
          setHasError(true);
        }
        if (pollInterval) {
          clearInterval(pollInterval);
        }
      }
    } catch (error) {
      console.error('Failed to poll update status:', error);
      // If we get errors during update, assume we're restarting
      const currentStatus = status();
      const shouldAssumeRestart =
        !isRestarting() &&
        (!currentStatus || (currentStatus.status !== 'idle' && currentStatus.status !== 'error'));

      if (shouldAssumeRestart) {
        if (!currentStatus) {
          setStatus({
            status: 'restarting',
            progress: 95,
            message: 'Restarting service...',
            updatedAt: new Date().toISOString(),
          });
        }
        setIsRestarting(true);
        if (pollInterval) {
          clearInterval(pollInterval);
        }
        startHealthCheckPolling();
      }
    }
  };

  const startHealthCheckPolling = () => {
    clearHealthCheckTimer();
    healthCheckAttempts = 0;

    const checkHealth = async () => {
      let isHealthy = false;

      try {
        const response = await fetch('/api/health', { cache: 'no-store' });
        if (response.ok) {
          isHealthy = true;
        }
      } catch (error) {
        console.warn('Health check request failed, will retry', error);
      }

      if (isHealthy) {
        // Backend is back! Reload the page to get the new version
        console.log('Backend is healthy again, reloading...');
        window.location.reload();
        return;
      }

      const attempt = Math.min(healthCheckAttempts, 3);
      const nextDelay = Math.min(2000 * Math.pow(2, attempt), 15000);
      healthCheckAttempts++;
      clearHealthCheckTimer();
      healthCheckTimer = window.setTimeout(checkHealth, nextDelay);
    };

    // Start checking immediately
    healthCheckTimer = window.setTimeout(checkHealth, 0);
  };

  // Watch websocket status during restart
  createEffect(() => {
    if (!isRestarting()) return;

    const connected = props.connected?.();
    const reconnecting = props.reconnecting?.();

    // Track if websocket disconnected during restart
    if (connected === false && !reconnecting) {
      setWsDisconnected(true);
    }

    // If websocket reconnected after being disconnected, the backend is likely back
    if (wsDisconnected() && connected === true && !reconnecting) {
      console.log('WebSocket reconnected after restart, verifying health...');
      // Give it a moment for the backend to fully initialize
      setTimeout(async () => {
        try {
          const response = await fetch('/api/health', { cache: 'no-store' });
          if (response.ok) {
            console.log('Backend healthy after websocket reconnect, reloading...');
            window.location.reload();
          }
        } catch (_error) {
          console.warn('Health check failed after websocket reconnect, will keep trying');
        }
      }, 1000);
    }
  });

  onMount(() => {
    if (props.isOpen) {
      // Start polling immediately
      pollStatus();
      // Then poll every 2 seconds
      pollInterval = setInterval(pollStatus, 2000) as unknown as number;
    }
  });

  onCleanup(() => {
    if (pollInterval) {
      clearInterval(pollInterval);
    }
    clearHealthCheckTimer();
  });

  const getStageIcon = () => {
    const currentStatus = status();
    if (!currentStatus) return null;

    if (hasError()) {
      return (
        <svg class="w-12 h-12 text-red-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 8v4m0 4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
        </svg>
      );
    }

    if (isComplete() && !hasError()) {
      return (
        <svg class="w-12 h-12 text-green-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z" />
        </svg>
      );
    }

    return (
      <svg class="w-12 h-12 text-blue-500 animate-spin" fill="none" viewBox="0 0 24 24">
        <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
        <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
      </svg>
    );
  };

  const getStatusText = () => {
    const currentStatus = status();

    if (isRestarting()) {
      return 'Pulse is restarting...';
    }

    if (!currentStatus) return 'Initializing...';

    if (hasError()) {
      return 'Update Failed';
    }

    if (isComplete() && !hasError()) {
      return 'Update Completed Successfully';
    }

    return currentStatus.message || 'Updating...';
  };

  return (
    <Show when={props.isOpen}>
      <div class="fixed inset-0 bg-black/50 flex items-center justify-center z-50 p-4">
        <div class="bg-white dark:bg-gray-800 rounded-lg shadow-xl max-w-2xl w-full">
          {/* Header */}
          <div class="px-6 py-4 border-b border-gray-200 dark:border-gray-700">
            <div class="flex items-center justify-between">
              <h2 class="text-xl font-semibold text-gray-900 dark:text-gray-100">
                Updating Pulse
              </h2>
              <Show when={isComplete()}>
                <button
                  onClick={props.onClose}
                  class="text-gray-400 hover:text-gray-600 dark:hover:text-gray-300"
                >
                  <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
                  </svg>
                </button>
              </Show>
            </div>
          </div>

          {/* Body */}
          <div class="px-6 py-8">
            {/* Icon and Status */}
            <div class="flex flex-col items-center text-center space-y-4">
              {getStageIcon()}
              <div>
                <div class="text-lg font-medium text-gray-900 dark:text-gray-100">
                  {getStatusText()}
                </div>
                <Show when={status()?.status && !isComplete()}>
                  <div class="text-sm text-gray-500 dark:text-gray-400 mt-1 capitalize">
                    {status()!.status.replace('-', ' ')}
                  </div>
                </Show>
              </div>
            </div>

            {/* Progress Bar */}
            <Show when={!isComplete() && status()?.progress !== undefined}>
              <div class="mt-6">
                <div class="flex items-center justify-between text-sm text-gray-600 dark:text-gray-400 mb-2">
                  <span>Progress</span>
                  <span>{status()!.progress}%</span>
                </div>
                <div class="w-full bg-gray-200 dark:bg-gray-700 rounded-full h-2">
                  <div
                    class="bg-blue-600 h-2 rounded-full transition-all duration-300"
                    style={{ width: `${status()!.progress}%` }}
                  />
                </div>
              </div>
            </Show>

            {/* Error Message */}
            <Show when={hasError() && status()?.error}>
              <div class="mt-6 bg-red-50 dark:bg-red-900/20 border border-red-200 dark:border-red-800 rounded-lg p-4">
                <div class="text-sm text-red-800 dark:text-red-200">
                  <div class="font-medium mb-1">Error Details:</div>
                  <div class="text-red-700 dark:text-red-300">{status()!.error}</div>
                </div>
              </div>
            </Show>

            {/* Warning / Info */}
            <Show when={!isComplete()}>
              <Show when={isRestarting()}>
                <div class="mt-6 bg-blue-50 dark:bg-blue-900/20 border border-blue-200 dark:border-blue-800 rounded-lg p-3">
                  <div class="flex items-start gap-2">
                    <svg class="w-5 h-5 text-blue-600 dark:text-blue-400 flex-shrink-0 mt-0.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                      <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
                    </svg>
                    <div class="text-sm text-blue-800 dark:text-blue-200">
                      <Show when={wsDisconnected()} fallback={
                        <span>Pulse is restarting with the new version...</span>
                      }>
                        <span>Waiting for Pulse to complete restart. This page will reload automatically.</span>
                      </Show>
                    </div>
                  </div>
                </div>
              </Show>
              <Show when={!isRestarting()}>
                <div class="mt-6 bg-yellow-50 dark:bg-yellow-900/20 border border-yellow-200 dark:border-yellow-800 rounded-lg p-3">
                  <div class="flex items-start gap-2">
                    <svg class="w-5 h-5 text-yellow-600 dark:text-yellow-400 flex-shrink-0 mt-0.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                      <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z" />
                    </svg>
                    <div class="text-sm text-yellow-800 dark:text-yellow-200">
                      Please do not close this window or refresh the page during the update.
                    </div>
                  </div>
                </div>
              </Show>
            </Show>
          </div>

          {/* Footer */}
          <Show when={isComplete()}>
            <div class="px-6 py-4 bg-gray-50 dark:bg-gray-900/50 border-t border-gray-200 dark:border-gray-700 flex items-center justify-end gap-3">
              <Show when={!hasError()}>
                <button
                  onClick={props.onViewHistory}
                  class="px-4 py-2 text-sm font-medium text-gray-700 dark:text-gray-300 hover:bg-gray-100 dark:hover:bg-gray-800 rounded-md transition-colors"
                >
                  View History
                </button>
              </Show>
              <Show when={hasError()}>
                <button
                  onClick={() => window.location.reload()}
                  class="px-4 py-2 text-sm font-medium text-white bg-blue-600 hover:bg-blue-700 rounded-md transition-colors"
                >
                  Retry
                </button>
              </Show>
              <button
                onClick={props.onClose}
                class="px-4 py-2 text-sm font-medium text-white bg-blue-600 hover:bg-blue-700 rounded-md transition-colors"
              >
                Close
              </button>
            </div>
          </Show>
        </div>
      </div>
    </Show>
  );
}
