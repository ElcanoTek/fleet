import { getLanguage } from "./index";

export function setNotificationLanguage(registration: ServiceWorkerRegistration): void {
  for (const worker of [registration.active, registration.waiting, registration.installing]) {
    worker?.postMessage({ type: "fleet-language", language: getLanguage() });
  }
}

// Do not register a worker just for localization; notifications remain opt-in.
export async function updateNotificationLanguage(): Promise<void> {
  try {
    const registration = await navigator.serviceWorker?.getRegistration("/sw.js");
    if (registration) setNotificationLanguage(registration);
  } catch { /* Browsers may deny service workers or storage. */ }
}
