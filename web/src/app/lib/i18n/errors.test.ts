import { expect, it } from "vitest";
import { localizeError, localizeServerMessage } from "./errors";

it("localizes known HTTP errors and preserves unknown diagnostic content", () => {
  expect(localizeError("File not found", "es")).toBe("No se encontró el archivo");
  expect(localizeError("Invalid task ID", "pt")).toBe("Identificador de tarefa inválido");
  expect(localizeError("Save", "es")).toBe("Save");
  expect(localizeError("File not found", "en")).toBe("File not found");
});

it("preserves captured filenames, markup and slot-looking values without recursion", () => {
  const value = "<img src=x>{1}.csv";
  expect(localizeError(`${value} is over this server's 10 MB upload limit`, "es"))
    .toBe(`${value} supera el límite de subida de 10 MB de este servidor`);
  expect(localizeError("3 items, over the 2-item limit", "pt"))
    .toBe("3 elementos; excede o limite de 2 elementos");
  expect(localizeError("many items, over the two-item limit", "es"))
    .toBe("many items, over the two-item limit");
});

it("bounds matching and leaves a long unknown server response unchanged", () => {
  const diagnostic = "A".repeat(5000) + " is required";
  expect(localizeError(diagnostic, "es")).toBe(diagnostic);
});

it("localizes owned diagnostics separately from arbitrary error or user content", () => {
  expect(localizeServerMessage("unit fleet", "pt")).toBe("unidade fleet");
  expect(localizeServerMessage("run on the box: journalctl -u fleet -n 100", "es"))
    .toBe("ejecuta en el servidor: journalctl -u fleet -n 100");
  expect(localizeServerMessage("pricing for model my/model is unknown; cost fields are null. Add the slug to agentcore.KnownModelPricing to get a dollar estimate.", "es"))
    .toContain("modelo my/model");
  expect(localizeError("unit fleet", "pt")).toBe("unit fleet");
  expect(localizeError("my task is active", "es")).toBe("my task is active");
});
