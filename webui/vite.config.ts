import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// PROxyLLM WebUI build config (plan §9, research §10 P-2/B-2/B-3).
//
// - base '/': the SPA is served from the Go binary's root (embedded dist),
//   same origin as /api/admin — connect-src 'self' (P-5).
// - assetsInlineLimit 0: no data: URIs in HTML/CSS (P-2) so the CSP keeps
//   img-src/font-src at 'self' and self-hosted fonts ship as real files.
// - manualChunks: one vendor chunk (react + react-dom + react-router) per
//   B-2; route-level splitting happens via React.lazy in the app.
// - reportCompressedSize: Vite prints gzip sizes at build; the CI bundle
//   budget gate parses them (B-3, ≤ 200 KB gzip initial).
export default defineConfig({
  plugins: [react()],
  build: {
    assetsInlineLimit: 0,
    reportCompressedSize: true,
    rollupOptions: {
      output: {
        manualChunks: {
          vendor: ['react', 'react-dom', 'react-router-dom'],
        },
      },
    },
  },
});