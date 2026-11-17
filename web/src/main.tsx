// The SPA entry. The Go server (internal/webui) serves index.html with the
// page's metadata and a bootstrap JSON already rendered in; this mounts the
// app on top of it.
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./app/fonts/fonts.css";
import "./app/globals.css";
import { App } from "./App";
import { getLanguage, rememberLanguage } from "./app/lib/i18n";
import { updateNotificationLanguage } from "./app/lib/i18n/notifications";
import { captureEntryAddress } from "./app/shared/sidebar/home";

// The address fleet was opened at, for the sidebar's Home (docs/SIDEBAR.md),
// read before any page strips its link parameters.
captureEntryAddress();
document.documentElement.lang = getLanguage();
rememberLanguage(getLanguage());
void updateNotificationLanguage();

const root = document.getElementById("root");
if (root) {
  createRoot(root).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
}
