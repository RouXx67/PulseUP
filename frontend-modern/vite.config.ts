import { defineConfig } from 'vite';
import solid from 'vite-plugin-solid';
import path from 'path';
import { URL } from 'node:url';

const frontendDevHost = process.env.FRONTEND_DEV_HOST ?? '0.0.0.0';
const frontendDevPort = Number(
  process.env.FRONTEND_DEV_PORT ?? process.env.VITE_PORT ?? process.env.PORT ?? 5173,
);

const backendProtocol = process.env.PULSE_DEV_API_PROTOCOL ?? 'http';
const backendHost = process.env.PULSE_DEV_API_HOST ?? '127.0.0.1';
const backendPort = Number(
  process.env.PULSE_DEV_API_PORT ??
    process.env.FRONTEND_PORT ??
    process.env.PORT ??
    7655,
);

const backendUrl =
  process.env.PULSE_DEV_API_URL ?? `${backendProtocol}://${backendHost}:${backendPort}`;

const backendWsUrl =
  process.env.PULSE_DEV_WS_URL ??
  (() => {
    try {
      const parsed = new URL(backendUrl);
      parsed.protocol = parsed.protocol === 'https:' ? 'wss:' : 'ws:';
      return parsed.toString();
    } catch {
      return backendUrl
        .replace(/^http:\/\//i, 'ws://')
        .replace(/^https:\/\//i, 'wss://');
    }
  })();

export default defineConfig({
  plugins: [solid()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
      'lucide-solid/icons': path.resolve(__dirname, './node_modules/lucide-solid/dist/esm/icons'),
    },
    conditions: ['import', 'browser', 'default'],
  },
  optimizeDeps: {
    include: ['lucide-solid'],
    force: true,
  },
  ssr: {
    noExternal: ['lucide-solid'],
  },
  server: {
    port: frontendDevPort,
    host: frontendDevHost, // Listen on all interfaces for remote access
    strictPort: true,
    proxy: {
      '/ws': {
        target: backendWsUrl,
        ws: true,
        changeOrigin: true,
      },
      '/api': {
        target: backendUrl,
        changeOrigin: true,
      },
      '/install-docker-agent.sh': {
        target: backendUrl,
        changeOrigin: true,
      },
      '/download': {
        target: backendUrl,
        changeOrigin: true,
      },
    },
  },
  build: {
    target: 'esnext',
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
  },
});
