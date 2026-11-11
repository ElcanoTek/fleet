import { test, expect } from "@playwright/test";
import { loginViaCookie } from "./_session";
import { mockChatBoot, mockShellBasics } from "./_mocks";

const languages = [
  { code: "en", language: "Language", settings: "Settings", signIn: "Sign in", email: "Email", password: "Password" },
  { code: "es", language: "Idioma", settings: "Configuración", signIn: "Iniciar sesión", email: "Correo electrónico", password: "Contraseña" },
  { code: "pt", language: "Idioma", settings: "Definições", signIn: "Iniciar sessão", email: "Email", password: "Palavra-passe" },
] as const;

for (const language of languages) {
  test(`${language.code}: login and settings use the selected language`, async ({ page, context }) => {
    await page.goto(`/login?lang=${language.code}`);
    await expect(page.locator("html")).toHaveAttribute("lang", language.code);
    await expect(page.getByRole("combobox", { name: language.language })).toHaveValue(language.code);
    await expect(page.getByRole("button", { name: language.signIn, exact: true })).toBeVisible();
    await expect(page.getByLabel(language.email, { exact: true })).toBeVisible();
    await expect(page.getByLabel(language.password, { exact: true })).toBeVisible();
    await loginViaCookie(context);
    await mockShellBasics(page);
    await page.goto("/settings");
    await expect(page.getByRole("heading", { name: language.settings, exact: true })).toBeVisible();
    await expect(page.getByRole("combobox", { name: language.language })).toHaveValue(language.code);
    await page.setViewportSize({ width: 390, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  });
}

test("settings selector persists across reloads and ordinary navigation", async ({ page, context }) => {
  await loginViaCookie(context);
  await mockShellBasics(page);
  await page.goto("/settings?lang=en#language");
  await page.getByRole("combobox", { name: "Language" }).selectOption("es");
  await expect(page).toHaveURL(/lang=es#language$/);
  await expect(page.getByRole("heading", { name: "Configuración", exact: true })).toBeVisible();
  expect(await page.evaluate(() => localStorage.getItem("elcano.language"))).toBe("es");
  await page.goto("/settings");
  await expect(page.locator("html")).toHaveAttribute("lang", "es");
  await page.getByRole("combobox", { name: "Idioma" }).selectOption("pt");
  await expect(page.getByRole("heading", { name: "Definições", exact: true })).toBeVisible();
});

test("language selection works with denied localStorage and leaves user titles unchanged", async ({ page, context }) => {
  await page.addInitScript(() => {
    Object.defineProperty(window, "localStorage", { get() { throw new DOMException("Denied", "SecurityError"); } });
  });
  await page.goto("/login?lang=en");
  await page.getByRole("combobox", { name: "Language" }).selectOption("es");
  await expect(page.getByRole("button", { name: "Iniciar sesión", exact: true })).toBeVisible();
  await loginViaCookie(context);
  await mockChatBoot(page, { conversations: [{ id: "user-title", title: "Save", created_at: Date.now() / 1000, updated_at: Date.now() / 1000 }] });
  await page.goto("/chat");
  await expect(page.locator("html")).toHaveAttribute("lang", "es");
  await expect(page.getByRole("button", { name: "Save", exact: true })).toHaveCount(2);
  await expect(page.getByRole("button", { name: "Save", exact: true }).first()).toBeVisible();
});

for (const language of [
  { code: "es", title: "Flujo personalizado", field: "Canal del paquete", option: "Opción propia", unit: "unidad", unavailable: "podman no está en PATH" },
  { code: "pt", title: "Fluxo personalizado", field: "Canal do pacote", option: "Opção própria", unit: "unidade", unavailable: "podman não está no PATH" },
]) {
  test(`${language.code}: bundle forms translate display labels and preserve option values`, async ({ page, context }) => {
    await loginViaCookie(context);
    await mockChatBoot(page, { personaDefault: "victoria" });
    await page.route("**/api/client-config", (route) => route.fulfill({ json: {
      localizations: { [language.code]: {
        "Custom bundle workflow": language.title,
        "Bundle channel": language.field,
        "Unique option": language.option,
        "Prepare custom audit": language.code === "es" ? "Preparar auditoría" : "Preparar auditoria",
        "Custom workspace audit": language.code === "es" ? "Auditoría del espacio" : "Auditoria do espaço",
        "Audit {channel}": language.code === "es" ? "Audita {channel}" : "Audite {channel}",
      } },
      empty_state: { cards: [{ id: "custom-workflow", section: "", type: "form", icon: "search", title: "Custom bundle workflow", desc: "Custom workspace audit", cta: "Prepare custom audit", promptTemplate: "Audit {channel}", fields: [{ key: "channel", type: "select", label: "Bundle channel", options: ["Unique option"], default: "Unique option" }] }] },
    } }));
    await page.goto(`/chat?lang=${language.code}`);
    await page.getByRole("button", { name: new RegExp(language.title) }).click();
    const select = page.getByRole("combobox", { name: language.field });
    await expect(select).toHaveValue("Unique option");
    await expect(select.locator("option:checked")).toHaveText(language.option);
    await page.setViewportSize({ width: 390, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });

  test(`${language.code}: system diagnostics localize prose and decimals while retaining commands`, async ({ page, context }) => {
    await loginViaCookie(context);
    await mockShellBasics(page, { admin: true });
    await page.route("**/api/admin/doctor", (route) => route.fulfill({ json: {
      generated_at: "2026-11-10T12:00:00Z", duration_ms: 1250, deep: false, healthy: false,
      summary: { ok: 0, warn: 0, fail: 1, skip: 0 },
      checks: [{ name: "unit custom.service", status: "fail", detail: "podman not on PATH", fix: "run on the box: journalctl -u custom.service -n 100" }],
    } }));
    await page.goto(`/settings/admin/doctor?lang=${language.code}`);
    const panel = page.getByTestId("doctor-panel");
    await expect(panel).toContainText(`${language.unit} custom.service`);
    await expect(panel).toContainText(language.unavailable);
    await expect(panel).toContainText("journalctl -u custom.service -n 100");
    await expect(page.getByText(/1,3s/)).toBeVisible();
    await page.setViewportSize({ width: 390, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}
