import { createSignal, createEffect, For, Show } from 'solid-js';
import { useWebSocket } from '../App';

interface App {
  id: string;
  name: string;
  provider: 'github' | 'dockerhub' | 'generic';
  checkUrl: string;
  currentVersion: string;
  latestVersion?: string;
  hasUpdate: boolean;
  enabled: boolean;
  lastChecked?: string;
  updateUrl?: string;
  changelogUrl?: string;
}

interface UpdateStats {
  totalApps: number;
  enabledApps: number;
  updatesAvailable: number;
  lastCheck?: string;
}

export function SelfUp() {
  const [apps, setApps] = createSignal<App[]>([]);
  const [stats, setStats] = createSignal<UpdateStats>({
    totalApps: 0,
    enabledApps: 0,
    updatesAvailable: 0
  });
  const [loading, setLoading] = createSignal(true);
  const [checking, setChecking] = createSignal(false);
  const [showAddForm, setShowAddForm] = createSignal(false);

  // Mock data for demonstration
  createEffect(() => {
    // Simulate loading data
    setTimeout(() => {
      const mockApps: App[] = [
        {
          id: '1',
          name: 'Pulse',
          provider: 'github',
          checkUrl: 'https://github.com/pulse-monitoring/pulse',
          currentVersion: '1.2.3',
          latestVersion: '1.2.4',
          hasUpdate: true,
          enabled: true,
          lastChecked: new Date().toISOString(),
          updateUrl: 'https://github.com/pulse-monitoring/pulse/releases/latest',
          changelogUrl: 'https://github.com/pulse-monitoring/pulse/releases/tag/v1.2.4'
        },
        {
          id: '2',
          name: 'Traefik',
          provider: 'dockerhub',
          checkUrl: 'https://hub.docker.com/r/traefik/traefik',
          currentVersion: '3.0.0',
          latestVersion: '3.0.0',
          hasUpdate: false,
          enabled: true,
          lastChecked: new Date().toISOString()
        },
        {
          id: '3',
          name: 'Portainer',
          provider: 'github',
          checkUrl: 'https://github.com/portainer/portainer',
          currentVersion: '2.19.0',
          latestVersion: '2.19.1',
          hasUpdate: true,
          enabled: false,
          lastChecked: new Date().toISOString()
        }
      ];
      
      setApps(mockApps);
      setStats({
        totalApps: mockApps.length,
        enabledApps: mockApps.filter(app => app.enabled).length,
        updatesAvailable: mockApps.filter(app => app.hasUpdate && app.enabled).length,
        lastCheck: new Date().toISOString()
      });
      setLoading(false);
    }, 1000);
  });

  const handleCheckAll = async () => {
    setChecking(true);
    // Simulate checking for updates
    setTimeout(() => {
      setChecking(false);
    }, 2000);
  };

  const getProviderIcon = (provider: string) => {
    switch (provider) {
      case 'github':
        return (
          <svg class="w-4 h-4" fill="currentColor" viewBox="0 0 24 24">
            <path d="M12 0c-6.626 0-12 5.373-12 12 0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.084 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23.957-.266 1.983-.399 3.003-.404 1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576 4.765-1.589 8.199-6.086 8.199-11.386 0-6.627-5.373-12-12-12z"/>
          </svg>
        );
      case 'dockerhub':
        return (
          <svg class="w-4 h-4" fill="currentColor" viewBox="0 0 24 24">
            <path d="M13.983 11.078h2.119a.186.186 0 00.186-.185V9.006a.186.186 0 00-.186-.186h-2.119a.185.185 0 00-.185.185v1.888c0 .102.083.185.185.185m-2.954-5.43h2.118a.186.186 0 00.186-.186V3.574a.186.186 0 00-.186-.185h-2.118a.185.185 0 00-.185.185v1.888c0 .102.082.185.185.186m0 2.716h2.118a.187.187 0 00.186-.186V6.29a.186.186 0 00-.186-.185h-2.118a.185.185 0 00-.185.185v1.887c0 .102.082.185.185.186m-2.93 0h2.12a.186.186 0 00.184-.186V6.29a.185.185 0 00-.185-.185H8.1a.185.185 0 00-.185.185v1.887c0 .102.083.185.185.186m-2.964 0h2.119a.186.186 0 00.185-.186V6.29a.185.185 0 00-.185-.185H5.136a.186.186 0 00-.186.185v1.887c0 .102.084.185.186.186m5.893 2.715h2.118a.186.186 0 00.186-.185V9.006a.186.186 0 00-.186-.186h-2.118a.185.185 0 00-.185.185v1.888c0 .102.082.185.185.185m-2.93 0h2.12a.185.185 0 00.184-.185V9.006a.185.185 0 00-.184-.186h-2.12a.185.185 0 00-.184.185v1.888c0 .102.083.185.185.185m-2.964 0h2.119a.185.185 0 00.185-.185V9.006a.185.185 0 00-.184-.186H5.136a.186.186 0 00-.186.186v1.887c0 .102.084.185.186.185m-2.92 0h2.12a.185.185 0 00.184-.185V9.006a.185.185 0 00-.184-.186h-2.12a.185.185 0 00-.184.185v1.888c0 .102.082.185.184.185M23.763 9.89c-.065-.051-.672-.51-1.954-.51-.338 0-.676.03-1.01.087-.248-1.7-1.653-2.53-1.716-2.566l-.344-.199-.226.327c-.284.438-.49.922-.612 1.43-.23.97-.09 1.882.403 2.661-.595.332-1.55.413-1.744.42H.751a.751.751 0 00-.75.748 11.376 11.376 0 00.692 4.062c.545 1.428 1.355 2.48 2.41 3.124 1.18.723 3.1 1.137 5.275 1.137.983.003 1.963-.086 2.93-.266a12.248 12.248 0 003.823-1.389c.98-.567 1.86-1.288 2.61-2.136 1.252-1.418 1.998-2.997 2.553-4.4h.221c1.372 0 2.215-.549 2.68-1.009.309-.293.55-.65.707-1.046l.098-.288Z"/>
          </svg>
        );
      default:
        return (
          <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9.663 17h4.673M12 3v1m6.364 1.636l-.707.707M21 12h-1M4 12H3m3.343-5.657l-.707-.707m2.828 9.9a5 5 0 117.072 0l-.548.547A3.374 3.374 0 0014 18.469V19a2 2 0 11-4 0v-.531c0-.895-.356-1.754-.988-2.386l-.548-.547z" />
          </svg>
        );
    }
  };

  return (
    <div class="space-y-6">
      {/* Header */}
      <div class="flex items-center justify-between">
        <div>
          <h1 class="text-2xl font-bold text-gray-900 dark:text-white">SelfUp</h1>
          <p class="text-gray-600 dark:text-gray-400 mt-1">
            Surveillez les mises à jour de vos applications auto-hébergées
          </p>
        </div>
        
        <div class="flex items-center space-x-3">
          <button
            onClick={handleCheckAll}
            disabled={checking()}
            class="btn btn-secondary"
          >
            <svg class={`w-4 h-4 mr-2 ${checking() ? 'animate-spin' : ''}`} fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
            </svg>
            {checking() ? 'Vérification...' : 'Vérifier tout'}
          </button>
          
          <button
            onClick={() => setShowAddForm(true)}
            class="btn btn-primary"
          >
            <svg class="w-4 h-4 mr-2" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 6v6m0 0v6m0-6h6m-6 0H6" />
            </svg>
            Ajouter une app
          </button>
        </div>
      </div>

      {/* Stats Cards */}
      <div class="grid grid-cols-1 md:grid-cols-4 gap-4">
        <div class="bg-white dark:bg-gray-800 rounded-lg p-6 shadow-sm border border-gray-200 dark:border-gray-700">
          <div class="flex items-center">
            <div class="flex-shrink-0">
              <svg class="w-8 h-8 text-blue-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 11H5m14 0a2 2 0 012 2v6a2 2 0 01-2 2H5a2 2 0 01-2-2v-6a2 2 0 012-2m14 0V9a2 2 0 00-2-2M5 11V9a2 2 0 012-2m0 0V5a2 2 0 012-2h6a2 2 0 012 2v2M7 7h10" />
              </svg>
            </div>
            <div class="ml-4">
              <p class="text-sm font-medium text-gray-600 dark:text-gray-400">Total Apps</p>
              <p class="text-2xl font-semibold text-gray-900 dark:text-white">{stats().totalApps}</p>
            </div>
          </div>
        </div>

        <div class="bg-white dark:bg-gray-800 rounded-lg p-6 shadow-sm border border-gray-200 dark:border-gray-700">
          <div class="flex items-center">
            <div class="flex-shrink-0">
              <svg class="w-8 h-8 text-green-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z" />
              </svg>
            </div>
            <div class="ml-4">
              <p class="text-sm font-medium text-gray-600 dark:text-gray-400">Activées</p>
              <p class="text-2xl font-semibold text-gray-900 dark:text-white">{stats().enabledApps}</p>
            </div>
          </div>
        </div>

        <div class="bg-white dark:bg-gray-800 rounded-lg p-6 shadow-sm border border-gray-200 dark:border-gray-700">
          <div class="flex items-center">
            <div class="flex-shrink-0">
              <svg class="w-8 h-8 text-orange-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M7 16a4 4 0 01-.88-7.903A5 5 0 1115.9 6L16 6a5 5 0 011 9.9M9 19l3 3m0 0l3-3m-3 3V10" />
              </svg>
            </div>
            <div class="ml-4">
              <p class="text-sm font-medium text-gray-600 dark:text-gray-400">Mises à jour</p>
              <p class="text-2xl font-semibold text-gray-900 dark:text-white">{stats().updatesAvailable}</p>
            </div>
          </div>
        </div>

        <div class="bg-white dark:bg-gray-800 rounded-lg p-6 shadow-sm border border-gray-200 dark:border-gray-700">
          <div class="flex items-center">
            <div class="flex-shrink-0">
              <svg class="w-8 h-8 text-gray-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 8v4l3 3m6-3a9 9 0 11-18 0 9 9 0 0118 0z" />
              </svg>
            </div>
            <div class="ml-4">
              <p class="text-sm font-medium text-gray-600 dark:text-gray-400">Dernière vérif.</p>
              <p class="text-sm font-semibold text-gray-900 dark:text-white">
                {stats().lastCheck ? new Date(stats().lastCheck).toLocaleTimeString('fr-FR', { 
                  hour: '2-digit', 
                  minute: '2-digit' 
                }) : 'Jamais'}
              </p>
            </div>
          </div>
        </div>
      </div>

      {/* Apps List */}
      <div class="bg-white dark:bg-gray-800 rounded-lg shadow-sm border border-gray-200 dark:border-gray-700">
        <div class="px-6 py-4 border-b border-gray-200 dark:border-gray-700">
          <h2 class="text-lg font-medium text-gray-900 dark:text-white">Applications surveillées</h2>
        </div>
        
        <Show when={!loading()} fallback={
          <div class="p-6">
            <div class="animate-pulse space-y-4">
              <For each={[1, 2, 3]}>
                {() => (
                  <div class="flex items-center space-x-4">
                    <div class="w-10 h-10 bg-gray-200 dark:bg-gray-700 rounded-lg"></div>
                    <div class="flex-1 space-y-2">
                      <div class="h-4 bg-gray-200 dark:bg-gray-700 rounded w-1/4"></div>
                      <div class="h-3 bg-gray-200 dark:bg-gray-700 rounded w-1/2"></div>
                    </div>
                  </div>
                )}
              </For>
            </div>
          </div>
        }>
          <div class="divide-y divide-gray-200 dark:divide-gray-700">
            <For each={apps()}>
              {(app) => (
                <div class="p-6 hover:bg-gray-50 dark:hover:bg-gray-700/50 transition-colors">
                  <div class="flex items-center justify-between">
                    <div class="flex items-center space-x-4">
                      <div class="flex-shrink-0">
                        <div class="w-10 h-10 bg-gray-100 dark:bg-gray-700 rounded-lg flex items-center justify-center">
                          {getProviderIcon(app.provider)}
                        </div>
                      </div>
                      
                      <div class="flex-1 min-w-0">
                        <div class="flex items-center space-x-2">
                          <h3 class="text-sm font-medium text-gray-900 dark:text-white truncate">
                            {app.name}
                          </h3>
                          <Show when={!app.enabled}>
                            <span class="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-gray-100 text-gray-800 dark:bg-gray-700 dark:text-gray-300">
                              Désactivée
                            </span>
                          </Show>
                          <Show when={app.hasUpdate && app.enabled}>
                            <span class="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-orange-100 text-orange-800 dark:bg-orange-900 dark:text-orange-300">
                              Mise à jour disponible
                            </span>
                          </Show>
                        </div>
                        
                        <div class="flex items-center space-x-4 mt-1">
                          <p class="text-sm text-gray-500 dark:text-gray-400">
                            {app.currentVersion}
                            <Show when={app.latestVersion && app.latestVersion !== app.currentVersion}>
                              <span class="text-orange-600 dark:text-orange-400"> → {app.latestVersion}</span>
                            </Show>
                          </p>
                          
                          <span class="text-gray-300 dark:text-gray-600">•</span>
                          
                          <p class="text-sm text-gray-500 dark:text-gray-400 capitalize">
                            {app.provider}
                          </p>
                          
                          <Show when={app.lastChecked}>
                            <span class="text-gray-300 dark:text-gray-600">•</span>
                            <p class="text-sm text-gray-500 dark:text-gray-400">
                              Vérifié {new Date(app.lastChecked!).toLocaleTimeString('fr-FR', { 
                                hour: '2-digit', 
                                minute: '2-digit' 
                              })}
                            </p>
                          </Show>
                        </div>
                      </div>
                    </div>
                    
                    <div class="flex items-center space-x-2">
                      <Show when={app.hasUpdate && app.changelogUrl}>
                        <a
                          href={app.changelogUrl}
                          target="_blank"
                          rel="noopener noreferrer"
                          class="text-blue-600 hover:text-blue-500 dark:text-blue-400 dark:hover:text-blue-300"
                        >
                          <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M10 6H6a2 2 0 00-2 2v10a2 2 0 002 2h10a2 2 0 002-2v-4M14 4h6m0 0v6m0-6L10 14" />
                          </svg>
                        </a>
                      </Show>
                      
                      <button class="text-gray-400 hover:text-gray-500 dark:text-gray-500 dark:hover:text-gray-400">
                        <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 5v.01M12 12v.01M12 19v.01M12 6a1 1 0 110-2 1 1 0 010 2zm0 7a1 1 0 110-2 1 1 0 010 2zm0 7a1 1 0 110-2 1 1 0 010 2z" />
                        </svg>
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