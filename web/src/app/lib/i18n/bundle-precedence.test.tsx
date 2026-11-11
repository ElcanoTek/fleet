import { afterEach, expect, it, vi } from "vitest";

afterEach(() => {
  document.getElementById("fleet-bootstrap")?.remove();
  localStorage.clear();
  window.history.replaceState({}, "", "/");
  vi.unstubAllGlobals();
  vi.resetModules();
});

for (const locale of ["es", "pt"] as const) {
  it(`${locale}: bundle branding wins on the login page without overriding engine UI`, async () => {
    window.history.replaceState({}, "", `/login?lang=${locale}`);
    vi.resetModules();
    const welcome = locale === "es" ? "Bienvenido a este equipo." : "Bem-vindo a esta equipa.";
    const bootstrap = document.createElement("script");
    bootstrap.id = "fleet-bootstrap";
    bootstrap.type = "application/json";
    bootstrap.textContent = JSON.stringify({ branding: { loginTitle: "Welcome aboard.", localizations: { [locale]: { "Welcome aboard.": welcome } } } });
    document.body.append(bootstrap);
    const { bundleText, t, translate, setBundleLocalizations } = await import("./index");
    const { default: LoginPage } = await import("@/app/login/page");
    const { renderToStaticMarkup } = await import("react-dom/server");
    expect(renderToStaticMarkup(LoginPage())).toContain(welcome);
    expect(bundleText("Welcome aboard.")).toBe(welcome);
    expect(t("Welcome aboard.")).toBe(translate(locale, "Welcome aboard."));
    expect(t("Welcome aboard.")).not.toBe(welcome);
    setBundleLocalizations({ [locale]: { Save: "Bundle-only save", "Unregistered engine text": "Must not override engine text" } });
    expect(bundleText("Save")).toBe("Bundle-only save");
    expect(t("Save")).toBe(translate(locale, "Save"));
    expect(t("Unregistered engine text")).toBe("Unregistered engine text");
    expect(bundleText("Cancel")).toBe(translate(locale, "Cancel"));
    setBundleLocalizations();
    expect(bundleText("Save")).toBe(t("Save"));
  });

  it(`${locale}: bundle collisions affect metadata labels and drafts, preserving identifiers and user content`, async () => {
    window.history.replaceState({}, "", `/?lang=${locale}`);
    vi.resetModules();
    const { t, setBundleLocalizations } = await import("./index");
    const { localizeConnector, localizeField, localizePrompt, localizeTaskTemplate, skillDescription } = await import("./bundle");
    const { localizeBranding, localizeBundlePill, refreshClientConfig } = await import("@/app/lib/useClientConfig");
    const { formatBundlePersona, personaLabel } = await import("@/app/chat/ui/userPersonas");
    const { categoryName, orderedCategories } = await import("@/app/chat/ui/toolsPicker");
    const custom = locale === "es" ? "Texto propio del equipo" : "Texto próprio da equipa";
    const catalog = { Save: custom, Assistant: `${custom} persona`, "Welcome aboard.": `${custom} bienvenida`, "Work on {topic}": `${custom} {topic}` };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => ({ localizations: { [locale]: catalog }, branding: { login_title: "Welcome aboard." } }) }));
    expect(await refreshClientConfig()).toBe(true);
    expect(localizeBranding({ app_name: "Save", login_title: "Welcome aboard.", login_tagline: "Save", share_title: "Save", share_description: "Save" })).toMatchObject({ app_name: "Save", login_title: `${custom} bienvenida`, login_tagline: custom });
    const field = { key: "Save", label: "Save", type: "select" as const, options: ["Save"], default: "Save", placeholder: "Save", hint: "Save" };
    expect(localizeField(field)).toEqual({ ...field, label: custom, placeholder: custom, hint: custom, optionLabels: { Save: custom } });
    const connector = { name: "Save", display_name: "Save", description: "Save", tools: ["Save"], category: "Save" };
    expect(localizeConnector(connector)).toEqual({ ...connector, display_name: custom, description: custom });
    const userConnector = { ...connector, remote: true };
    expect(localizeConnector(userConnector)).toBe(userConnector);
    const prompt = { id: "git:save.yaml", name: "Save", content: "Save", source: "git" as const, read_only: true, owned_by_caller: false, visibility: "workspace" as const, fields: [field], prompt_template: "Work on {topic}" };
    expect(localizePrompt(prompt)).toMatchObject({ id: prompt.id, name: custom, content: "Save", fields: [{ key: "Save", label: custom, options: ["Save"], default: "Save" }], prompt_template: `${custom} {topic}` });
    const authored = { ...prompt, source: "workspace" as const };
    expect(localizePrompt(authored)).toBe(authored);
    expect(localizeTaskTemplate({ name: "Save", variables: ["topic"], task: { title: "Save", prompt: "Work on {topic}", persona: "assistant", recurrence: "0 9 * * 1" } })).toMatchObject({ name: custom, variables: ["topic"], task: { title: custom, prompt: `${custom} {topic}`, persona: "assistant", recurrence: "0 9 * * 1" } });
    expect(localizeBundlePill({ id: "Save", type: "form", section: "Save", title: "Save", desc: "Save", cta: "Save", icon: "Save", fields: [field] })).toMatchObject({ id: "Save", icon: "Save", title: custom, cta: custom });
    expect(skillDescription({ source: "bundle", description: "Save" })).toBe(custom);
    expect(skillDescription({ source: "user", description: "Save" })).toBe("Save");
    expect(formatBundlePersona("assistant")).toBe(`${custom} persona`);
    expect(personaLabel("user:Save", [{ ref: "user:Save", name: "Save", display_name: "Save", description: "Save", kind: "assistant", autonomy: "advise" }])).toBe("Save");
    expect(categoryName("Save", orderedCategories([{ id: "Save", name: "Save" }]))).toBe(custom);
    expect(orderedCategories([{ id: "Save", name: "Save" }])[0]).toEqual({ id: "Save", name: custom });
    expect(t("Save")).not.toBe(custom);
    setBundleLocalizations({ [locale]: {} });
    expect(localizeConnector(connector).display_name).toBe(t("Save"));
  });
}

for (const locale of ["en", "es", "pt"] as const) {
  it(`${locale}: a category label is translated once even when it equals another source key`, async () => {
    window.history.replaceState({}, "", `/?lang=${locale}`);
    vi.resetModules();
    const { setBundleLocalizations, t } = await import("./index");
    const { orderedCategories, categoryName } = await import("@/app/chat/ui/toolsPicker");
    setBundleLocalizations({ [locale]: { Save: "Cancel", Cancel: "Must not replace the translated label", "Misc Tools": "Must not replace the engine fallback" } });
    const source = [{ id: "save", name: "Save" }];
    const categories = orderedCategories(source);
    expect(categories[0]).toEqual({ id: "save", name: "Cancel" });
    expect(categoryName("save", categories)).toBe("Cancel");
    expect(categoryName("missing", categories)).toBe(t("Misc Tools"));
    expect(source).toEqual([{ id: "save", name: "Save" }]);
  });
}
