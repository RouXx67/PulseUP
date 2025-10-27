import { Component, Show } from 'solid-js';
import { Card } from '@/components/shared/Card';
import { EmptyState } from '@/components/shared/EmptyState';
import { useWebSocket } from '@/App';
import UnifiedBackups from './UnifiedBackups';
import { ProxmoxSectionNav } from '@/components/Proxmox/ProxmoxSectionNav';

const Backups: Component = () => {
  const { state, connected } = useWebSocket();

  return (
    <div class="space-y-3">
      <ProxmoxSectionNav current="backups" />

      {/* Loading State */}
      <Show
        when={
          connected() &&
          !(state.backups?.pve?.guestSnapshots?.length ||
            state.backups?.pve?.storageBackups?.length ||
            state.backups?.pbs?.length ||
            state.backups?.pmg?.length)
        }
      >
        <Card padding="lg">
          <EmptyState
            icon={
              <div class="inline-flex items-center justify-center w-12 h-12">
                <svg
                  class="animate-spin h-8 w-8 text-blue-600 dark:text-blue-400"
                  xmlns="http://www.w3.org/2000/svg"
                  fill="none"
                  viewBox="0 0 24 24"
                >
                  <circle
                    class="opacity-25"
                    cx="12"
                    cy="12"
                    r="10"
                    stroke="currentColor"
                    stroke-width="4"
                  ></circle>
                  <path
                    class="opacity-75"
                    fill="currentColor"
                    d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"
                  ></path>
                </svg>
              </div>
            }
            title="Loading backup information..."
          />
        </Card>
      </Show>

      {/* Disconnected State */}
      <Show when={!connected()}>
        <Card padding="lg" tone="danger">
          <EmptyState
            icon={
              <svg
                class="h-12 w-12 text-red-400"
                fill="none"
                viewBox="0 0 24 24"
                stroke="currentColor"
              >
                <path
                  stroke-linecap="round"
                  stroke-linejoin="round"
                  stroke-width="2"
                  d="M12 8v4m0 4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z"
                />
              </svg>
            }
            title="Connection lost"
            description="Unable to connect to the backend server. Attempting to reconnect..."
            tone="danger"
          />
        </Card>
      </Show>

      {/* Main Content - Unified Backups View */}
      <Show when={connected() && (state.backups?.pve || state.backups?.pbs || state.pbs)}>
        <UnifiedBackups />
      </Show>
    </div>
  );
};

export default Backups;
