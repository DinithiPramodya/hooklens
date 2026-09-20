import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],

  build: {
    // Build INTO a Go package, not into web/dist.
    //
    // //go:embed cannot reach outside its own package directory -- no "../" --
    // so the embed declaration has to live next to the files it embeds. Putting
    // a Go package inside web/ would mean `go build ./...` walking node_modules,
    // so instead the output lands in a package of its own.
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
  },

  server: {
    // The dev server proxies API calls to the Go server so that, in development,
    // the browser still sees a SINGLE origin (localhost:5173). Without this the
    // page is on :5173 and the API on :8080 -- two origins -- and every request
    // becomes a CORS problem that does not exist in production, where one binary
    // serves both. See docs/learn/12-cors.md.
    proxy: {
      '/api': 'http://localhost:8080',
      '/e': 'http://localhost:8080',
      '/healthz': 'http://localhost:8080',
    },
  },
})
