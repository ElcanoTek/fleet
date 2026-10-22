// The SPA entry. The Go server (internal/webui) serves index.html with the
// page's metadata and a bootstrap JSON already rendered in; this mounts the
// app on top of it.
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./app/fonts/fonts.css";
import "./app/globals.css";
import { App } from "./App";

const root = document.getElementById("root");
if (root) {
  createRoot(root).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
