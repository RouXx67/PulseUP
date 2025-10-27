import { Component, createSignal, Show, onMount, lazy, Suspense } from 'solid-js';
import { setBasicAuth } from '@/utils/apiClient';
import { STORAGE_KEYS } from '@/constants';

// Force include FirstRunSetup with lazy loading
const FirstRunSetup = lazy(() =>
  import('./FirstRunSetup').then((m) => ({ default: m.FirstRunSetup })),
);

interface LoginProps {
  onLogin: () => void;
}

interface SecurityStatus {
  hasAuthentication: boolean;
  oidcEnabled?: boolean;
  oidcIssuer?: string;
  oidcClientId?: string;
  oidcEnvOverrides?: Record<string, boolean>;
}

export const Login: Component<LoginProps> = (props) => {
  const [username, setUsername] = createSignal('');
  const [password, setPassword] = createSignal('');
  const [error, setError] = createSignal('');
  const [loading, setLoading] = createSignal(false);
  const [authStatus, setAuthStatus] = createSignal<SecurityStatus | null>(null);
  const [loadingAuth, setLoadingAuth] = createSignal(true);
  const [oidcLoading, setOidcLoading] = createSignal(false);
  const [oidcError, setOidcError] = createSignal('');
  const [oidcMessage, setOidcMessage] = createSignal('');

  const supportsOIDC = () => Boolean(authStatus()?.oidcEnabled);

  const resolveOidcError = (reason?: string | null) => {
    switch (reason) {
      case 'email_restricted':
        return 'Your account email is not permitted to access Pulse.';
      case 'domain_restricted':
        return 'Your email domain is not allowed for Pulse access.';
      case 'group_restricted':
        return 'Your account is not part of an authorized group to use Pulse.';
      case 'invalid_state':
        return 'The sign-in attempt expired. Please try again.';
      case 'exchange_failed':
        return 'We could not complete the sign-in request. Please try again shortly.';
      case 'session_failed':
        return 'Login succeeded but we could not create a session. Try again.';
      case 'invalid_id_token':
        return 'ID token verification failed. Check that OIDC_ISSUER_URL matches the issuer claim in your provider tokens (check server logs for details).';
      case 'invalid_signature_alg':
        return 'The identity provider is issuing HS256 tokens. Configure it to sign ID tokens with RS256 (see your IdP\'s OIDC settings).';
      case 'invalid_nonce':
        return 'Security validation failed (nonce mismatch). Please try again.';
      default:
        return 'Single sign-on failed. Please try again or contact an administrator.';
    }
  };

  onMount(async () => {
    // Apply saved theme preference from localStorage
    const savedTheme = localStorage.getItem(STORAGE_KEYS.DARK_MODE);
    if (savedTheme === 'false') {
      document.documentElement.classList.remove('dark');
    } else if (savedTheme === 'true') {
      document.documentElement.classList.add('dark');
    } else {
      // No saved preference - use system preference
      if (window.matchMedia('(prefers-color-scheme: dark)').matches) {
        document.documentElement.classList.add('dark');
      } else {
        document.documentElement.classList.remove('dark');
      }
    }

    const params = new URLSearchParams(window.location.search);
    const oidcStatus = params.get('oidc');
    if (oidcStatus === 'error') {
      const reason = params.get('oidc_error');
      setOidcError(resolveOidcError(reason));
      setError('');
    } else if (oidcStatus === 'success') {
      setOidcMessage('Signed in successfully. Loading Pulse…');
      setError('');
    }
    if (oidcStatus) {
      params.delete('oidc');
      params.delete('oidc_error');
      const newQuery = params.toString();
      const newUrl = `${window.location.pathname}${newQuery ? `?${newQuery}` : ''}`;
      window.history.replaceState({}, document.title, newUrl);
    }

    console.log('[Login] Starting auth check...');
    try {
      const response = await fetch('/api/security/status');
      console.log('[Login] Auth check response:', response.status);
      if (response.ok) {
        const data = await response.json();
        console.log('[Login] Auth status data:', data);
        setAuthStatus(data);
      } else if (response.status === 429) {
        // Rate limited - wait a bit and assume auth is configured
        console.log('[Login] Rate limited, assuming auth is configured');
        setAuthStatus({ hasAuthentication: true });
      } else {
        console.log('[Login] Auth check failed, assuming no auth');
        // On error, assume no auth configured
        setAuthStatus({ hasAuthentication: false });
      }
    } catch (err) {
      console.error('[Login] Failed to check auth status:', err);
      // On error, assume no auth configured
      setAuthStatus({ hasAuthentication: false });
    } finally {
      console.log('[Login] Auth check complete, setting loading to false');
      setLoadingAuth(false);
    }
  });

  const startOidcLogin = async () => {
    if (!supportsOIDC()) return;

    setOidcError('');
    setOidcMessage('');
    setError('');
    setOidcLoading(true);

    let redirecting = false;
    try {
      const response = await fetch('/api/oidc/login', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({
          returnTo: `${window.location.pathname}${window.location.search}`,
        }),
      });

      if (!response.ok) {
        const message = await response.text();
        throw new Error(message || 'Failed to initiate OIDC login');
      }

      const data = await response.json();
      if (data.authorizationUrl) {
        redirecting = true;
        window.location.href = data.authorizationUrl;
        return;
      }

      throw new Error('OIDC response missing authorization URL');
    } catch (err) {
      console.error('[Login] Failed to start OIDC login:', err);
      setOidcError('Failed to start single sign-on. Please try again.');
    } finally {
      if (!redirecting) {
        setOidcLoading(false);
      }
    }
  };

  // Only auto-redirect to OIDC if password auth is disabled
  // This prevents redirect loops when both password and OIDC are configured
  // createEffect(() => {
  //   if (!loadingAuth() && supportsOIDC() && !autoOidcTriggered()) {
  //     setAutoOidcTriggered(true);
  //     startOidcLogin();
  //   }
  // });

  const handleSubmit = async (e: Event) => {
    e.preventDefault();
    setError('');
    setLoading(true);

    try {
      // Use the new login endpoint for better feedback
      const response = await fetch('/api/login', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Accept: 'application/json',
        },
        body: JSON.stringify({
          username: username(),
          password: password(),
        }),
        credentials: 'include', // Important for session cookie
      });

      const data = await response.json();

      if (response.ok && data.success) {
        // Credentials are valid, save them and notify parent
        setBasicAuth(username(), password());
        props.onLogin();
      } else if (response.status === 403) {
        // Account is locked
        if (data.remainingMinutes) {
          setError(
            `Account locked. Please try again in ${data.remainingMinutes} ${data.remainingMinutes === 1 ? 'minute' : 'minutes'}.`,
          );
        } else {
          setError(data.message || 'Account temporarily locked due to too many failed attempts.');
        }
        // Clear the input fields
        setUsername('');
        setPassword('');
      } else if (response.status === 429) {
        // Rate limited
        setError(data.message || 'Too many requests. Please wait a moment and try again.');
      } else if (response.status === 401) {
        // Invalid credentials with attempt information
        if (data.remaining !== undefined && data.remaining > 0) {
          setError(
            `${data.message || 'Invalid username or password.'} (${data.remaining} ${data.remaining === 1 ? 'attempt' : 'attempts'} remaining)`,
          );
        } else if (data.locked) {
          setError(data.message || 'Invalid username or password. Account is now locked.');
        } else {
          setError(data.message || 'Invalid username or password');
        }
        // Clear the input fields
        setUsername('');
        setPassword('');
      } else {
        setError(data.message || 'Server error. Please try again.');
      }
    } catch (_err) {
      // Try the old method as fallback
      try {
        const response = await fetch('/api/state', {
          headers: {
            Authorization: `Basic ${btoa(`${username()}:${password()}`)}`,
            'X-Requested-With': 'XMLHttpRequest',
            Accept: 'application/json',
          },
          credentials: 'include',
        });

        if (response.ok) {
          setBasicAuth(username(), password());
          props.onLogin();
        } else if (response.status === 401) {
          setError('Invalid username or password');
          setUsername('');
          setPassword('');
        } else {
          setError('Server error. Please try again.');
        }
      } catch (_fallbackErr) {
        setError('Failed to connect to server');
      }
    } finally {
      setLoading(false);
    }
  };

  // Debug logging
  console.log('[Login] Render - loadingAuth:', loadingAuth(), 'authStatus:', authStatus());

  return (
    <Show
      when={!loadingAuth()}
      fallback={
        <div class="min-h-screen flex items-center justify-center bg-gradient-to-br from-blue-50 via-white to-cyan-50 dark:from-gray-900 dark:via-gray-800 dark:to-blue-900">
          <div class="text-center">
            <div class="animate-spin h-12 w-12 border-4 border-blue-500 border-t-transparent rounded-full mx-auto mb-4"></div>
            <p class="text-gray-600 dark:text-gray-400">Checking authentication...</p>
          </div>
        </div>
      }
    >
      <Show
        when={authStatus()?.hasAuthentication === false}
        fallback={
          <LoginForm
            {...{
              username,
              setUsername,
              password,
              setPassword,
              error,
              loading,
              handleSubmit,
              supportsOIDC,
              startOidcLogin,
              oidcLoading,
              oidcError,
              oidcMessage,
            }}
          />
        }
      >
        <Suspense
          fallback={
            <div class="min-h-screen flex items-center justify-center bg-gradient-to-br from-blue-50 via-white to-cyan-50 dark:from-gray-900 dark:via-gray-800 dark:to-blue-900">
              <div class="text-center">
                <div class="animate-spin h-12 w-12 border-4 border-blue-500 border-t-transparent rounded-full mx-auto mb-4"></div>
                <p class="text-gray-600 dark:text-gray-400">Loading setup...</p>
              </div>
            </div>
          }
        >
          <FirstRunSetup />
        </Suspense>
      </Show>
    </Show>
  );
};

// Extract login form to separate component for cleaner code
const LoginForm: Component<{
  username: () => string;
  setUsername: (v: string) => void;
  password: () => string;
  setPassword: (v: string) => void;
  error: () => string;
  loading: () => boolean;
  handleSubmit: (e: Event) => void;
  supportsOIDC: () => boolean;
  startOidcLogin: () => void | Promise<void>;
  oidcLoading: () => boolean;
  oidcError: () => string;
  oidcMessage: () => string;
}> = (props) => {
  const {
    username,
    setUsername,
    password,
    setPassword,
    error,
    loading,
    handleSubmit,
    supportsOIDC,
    startOidcLogin,
    oidcLoading,
    oidcError,
    oidcMessage,
  } = props;

  return (
    <div class="min-h-screen flex items-center justify-center bg-gradient-to-br from-blue-50 via-white to-cyan-50 dark:from-gray-900 dark:via-gray-800 dark:to-blue-900 py-12 px-4 sm:px-6 lg:px-8">
      <div class="max-w-md w-full space-y-8">
        <div class="animate-fade-in">
          <div class="flex justify-center mb-4">
            <div class="relative group">
              <div class="absolute -inset-1 bg-gradient-to-r from-blue-600 to-cyan-600 rounded-full blur opacity-25 group-hover:opacity-75 transition duration-1000 group-hover:duration-200 animate-pulse-slow"></div>
              <img
                src="/logo.svg"
                alt="Pulse Logo"
                class="relative w-24 h-24 transform transition duration-500 group-hover:scale-110"
              />
            </div>
          </div>
          <h2 class="mt-6 text-center text-3xl font-extrabold bg-gradient-to-r from-blue-600 to-cyan-600 bg-clip-text text-transparent">
            Welcome to Pulse
          </h2>
          <p class="mt-2 text-center text-sm text-gray-600 dark:text-gray-400">
            Enter your credentials to continue
          </p>
        </div>
        <form
          class="mt-8 space-y-6 bg-white/80 dark:bg-gray-800/80 backdrop-blur-lg rounded-lg p-8 shadow-xl animate-slide-up"
          onSubmit={handleSubmit}
        >
          <Show when={supportsOIDC()}>
            <div class="space-y-3">
              <button
                type="button"
                class={`w-full inline-flex items-center justify-center gap-2 px-4 py-3 rounded-lg border border-blue-500 text-blue-600 hover:bg-blue-50 transition dark:border-blue-400 dark:text-blue-200 dark:hover:bg-blue-900/40 ${oidcLoading() ? 'opacity-75 cursor-wait' : ''}`}
                disabled={oidcLoading()}
                onClick={() => startOidcLogin()}
              >
                <Show
                  when={!oidcLoading()}
                  fallback={
                    <span class="inline-flex items-center gap-2">
                      <span class="h-4 w-4 border-2 border-current border-t-transparent rounded-full animate-spin" />
                      Redirecting…
                    </span>
                  }
                >
                  <span class="inline-flex items-center gap-2">
                    <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                      <path
                        stroke-linecap="round"
                        stroke-linejoin="round"
                        stroke-width="1.8"
                        d="M21 12c0 4.97-4.03 9-9 9m9-9c0-4.97-4.03-9-9-9m9 9H3m9 9c-4.97 0-9-4.03-9-9m9 9c-1.5-1.35-3-4.5-3-9s1.5-7.65 3-9m0 18c1.5-1.35 3-4.5 3-9s-1.5-7.65-3-9"
                      />
                    </svg>
                    Continue with Single Sign-On
                  </span>
                </Show>
              </button>
              <Show when={oidcError()}>
                <div class="rounded-md bg-red-50 dark:bg-red-900/40 border border-red-200 dark:border-red-800 px-3 py-2 text-sm text-red-600 dark:text-red-300">
                  {oidcError()}
                </div>
              </Show>
              <Show when={oidcMessage()}>
                <div class="rounded-md bg-green-50 dark:bg-green-900/30 border border-green-200 dark:border-green-700 px-3 py-2 text-sm text-green-600 dark:text-green-300">
                  {oidcMessage()}
                </div>
              </Show>
              <div class="flex items-center gap-3 pt-2">
                <span class="flex-1 h-px bg-gray-200 dark:bg-gray-700" />
                <span class="text-xs uppercase tracking-wide text-gray-400 dark:text-gray-500">
                  or
                </span>
                <span class="flex-1 h-px bg-gray-200 dark:bg-gray-700" />
              </div>
              <p class="text-xs text-center text-gray-500 dark:text-gray-400">
                Use your admin credentials to sign in below.
              </p>
            </div>
          </Show>
          <input type="hidden" name="remember" value="true" />
          <div class="space-y-4">
            <div class="relative">
              <label for="username" class="sr-only">
                Username
              </label>
              <div class="absolute inset-y-0 left-0 pl-3 flex items-center pointer-events-none">
                <svg
                  class="h-5 w-5 text-gray-400"
                  fill="none"
                  stroke="currentColor"
                  viewBox="0 0 24 24"
                >
                  <path
                    stroke-linecap="round"
                    stroke-linejoin="round"
                    stroke-width="2"
                    d="M16 7a4 4 0 11-8 0 4 4 0 018 0zM12 14a7 7 0 00-7 7h14a7 7 0 00-7-7z"
                  />
                </svg>
              </div>
              <input
                id="username"
                name="username"
                type="text"
                autocomplete="username"
                required
                class="appearance-none relative block w-full pl-10 pr-3 py-3 border border-gray-300 placeholder-gray-500 text-gray-900 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent transition-all sm:text-sm dark:bg-gray-700 dark:border-gray-600 dark:text-white dark:placeholder-gray-400"
                placeholder="Username"
                value={username()}
                onInput={(e) => setUsername(e.currentTarget.value)}
              />
            </div>
            <div class="relative">
              <label for="password" class="sr-only">
                Password
              </label>
              <div class="absolute inset-y-0 left-0 pl-3 flex items-center pointer-events-none">
                <svg
                  class="h-5 w-5 text-gray-400"
                  fill="none"
                  stroke="currentColor"
                  viewBox="0 0 24 24"
                >
                  <path
                    stroke-linecap="round"
                    stroke-linejoin="round"
                    stroke-width="2"
                    d="M12 15v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2zm10-10V7a4 4 0 00-8 0v4h8z"
                  />
                </svg>
              </div>
              <input
                id="password"
                name="password"
                type="password"
                autocomplete="current-password"
                required
                class="appearance-none relative block w-full pl-10 pr-3 py-3 border border-gray-300 placeholder-gray-500 text-gray-900 rounded-lg focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent transition-all sm:text-sm dark:bg-gray-700 dark:border-gray-600 dark:text-white dark:placeholder-gray-400"
                placeholder="Password"
                value={password()}
                onInput={(e) => setPassword(e.currentTarget.value)}
              />
            </div>
          </div>

          <Show when={error()}>
            <div
              class={`rounded-md p-4 ${
                error().includes('locked')
                  ? 'bg-orange-50 dark:bg-orange-900/20'
                  : 'bg-red-50 dark:bg-red-900/20'
              }`}
            >
              <div class="flex">
                <div class="flex-shrink-0">
                  <Show
                    when={error().includes('locked')}
                    fallback={
                      <svg class="h-5 w-5 text-red-400" viewBox="0 0 20 20" fill="currentColor">
                        <path
                          fill-rule="evenodd"
                          d="M10 18a8 8 0 100-16 8 8 0 000 16zM8.707 7.293a1 1 0 00-1.414 1.414L8.586 10l-1.293 1.293a1 1 0 101.414 1.414L10 11.414l1.293 1.293a1 1 0 001.414-1.414L11.414 10l1.293-1.293a1 1 0 00-1.414-1.414L10 8.586 8.707 7.293z"
                          clip-rule="evenodd"
                        />
                      </svg>
                    }
                  >
                    <svg class="h-5 w-5 text-orange-400" viewBox="0 0 20 20" fill="currentColor">
                      <path
                        fill-rule="evenodd"
                        d="M5 9V7a5 5 0 0110 0v2a2 2 0 012 2v5a2 2 0 01-2 2H5a2 2 0 01-2-2v-5a2 2 0 012-2zm8-2v2H7V7a3 3 0 016 0z"
                        clip-rule="evenodd"
                      />
                    </svg>
                  </Show>
                </div>
                <div class="ml-3">
                  <p
                    class={`text-sm ${
                      error().includes('locked')
                        ? 'text-orange-800 dark:text-orange-200'
                        : 'text-red-800 dark:text-red-200'
                    }`}
                  >
                    {error()}
                  </p>
                  <Show when={error().includes('locked') && error().includes('minute')}>
                    <p class="text-xs mt-1 text-orange-700 dark:text-orange-300">
                      Lockouts automatically expire after the specified time. If you need immediate
                      access, contact your administrator.
                    </p>
                  </Show>
                </div>
              </div>
            </div>
          </Show>

          <div>
            <button
              type="submit"
              disabled={loading()}
              class="group relative w-full flex justify-center py-3 px-4 border border-transparent text-sm font-medium rounded-lg text-white bg-gradient-to-r from-blue-600 to-cyan-600 hover:from-blue-700 hover:to-cyan-700 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-blue-500 disabled:opacity-50 disabled:cursor-not-allowed transform transition hover:scale-105 shadow-lg"
            >
              <Show when={loading()}>
                <svg
                  class="animate-spin -ml-1 mr-3 h-5 w-5 text-white"
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
              </Show>
              <Show when={loading()} fallback="Sign in to Pulse">
                Authenticating...
              </Show>
            </button>
          </div>
        </form>
      </div>
    </div>
  );
};
