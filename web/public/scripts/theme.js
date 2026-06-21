(() => {
  const storageKey = "chat-theme-preference";
  const root = document.documentElement;
  try {
    const stored = window.localStorage.getItem(storageKey);
    const theme = stored === "light" || stored === "dark"
      ? stored
      : window.matchMedia("(prefers-color-scheme: dark)").matches
        ? "dark"
        : "light";
    root.setAttribute("data-theme", theme);
  } catch {
    root.setAttribute("data-theme", "dark");
  }
})();
