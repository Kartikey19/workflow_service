import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/v1": "http://127.0.0.1:8080",
      "/hooks": "http://127.0.0.1:8080",
    },
  },
  build: { sourcemap: false },
});
