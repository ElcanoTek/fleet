import { describe, expect, it } from "vitest";
import {
  correctedModelAdoption,
  parseLockdownModelRefusal,
} from "./useTurnStream";

// #1588: a lockdown deployment refuses a model its allow-list forbids instead
// of quietly running the turn on another one, and names the slug to use in the
// same body. The parser is what turns that refusal into a self-correcting
// retry, so it has to be strict about what it recognises: mistaking an
// unrelated 400 for one would resend a turn the server never wanted.

const refusal = (body: unknown) => JSON.stringify(body);

describe("parseLockdownModelRefusal", () => {
  it("reads the correction off a refusal", () => {
    expect(
      parseLockdownModelRefusal(
        400,
        refusal({
          error: "model not allowed in lockdown mode",
          code: "lockdown_model_not_allowed",
          model: "vendor/allowed",
        }),
      ),
    ).toEqual({
      message: "model not allowed in lockdown mode",
      model: "vendor/allowed",
    });
  });

  it("a refusal with no correction still parses, with an empty model", () => {
    // The operator's allow-list named no literal slug, so the server had
    // nothing safe to offer: show the message, let the user pick.
    expect(
      parseLockdownModelRefusal(
        400,
        refusal({
          error: "model not allowed in lockdown mode",
          code: "lockdown_model_not_allowed",
        }),
      ),
    ).toEqual({ message: "model not allowed in lockdown mode", model: "" });
  });

  it("ignores any other 400 — plain text, other JSON, other codes", () => {
    expect(parseLockdownModelRefusal(400, "message is required")).toBeNull();
    expect(
      parseLockdownModelRefusal(400, refusal({ error: "bad json" })),
    ).toBeNull();
    expect(
      parseLockdownModelRefusal(400, refusal({ code: "something_else" })),
    ).toBeNull();
    expect(parseLockdownModelRefusal(400, "")).toBeNull();
    expect(parseLockdownModelRefusal(400, refusal(null))).toBeNull();
  });

  it("only 400 is a refusal — a 500 carrying the same body is not", () => {
    const body = refusal({
      error: "model not allowed in lockdown mode",
      code: "lockdown_model_not_allowed",
      model: "vendor/allowed",
    });
    expect(parseLockdownModelRefusal(500, body)).toBeNull();
    expect(parseLockdownModelRefusal(429, body)).toBeNull();
  });

  it("a non-string model is no correction, not a crash", () => {
    expect(
      parseLockdownModelRefusal(
        400,
        refusal({ code: "lockdown_model_not_allowed", model: 7 }),
      ),
    ).toEqual({ message: "", model: "" });
  });
});

// The picker belongs to the conversation on screen; the resend belongs to the
// submission. Conflating them loses one or the other: guard nothing and
// submitting in lockdown conversation A then navigating to B rewrites B's
// model, guard both and a user who navigates away has their message silently
// dropped.
describe("correctedModelAdoption", () => {
  const base = {
    refusalModel: "vendor/allowed-model",
    isModelRetry: false,
    activeConvKey: "conv-a",
    target: "conv-a",
  };

  it("adopts and resends while the refused conversation is still on screen", () => {
    expect(correctedModelAdoption(base)).toEqual({
      resend: true,
      adoptIntoPicker: true,
    });
  });

  it("still resends after the user navigates away, without touching that conversation's picker", () => {
    expect(
      correctedModelAdoption({ ...base, activeConvKey: "conv-b" }),
    ).toEqual({ resend: true, adoptIntoPicker: false });
  });

  it("does not touch the picker from a background submission with no active conversation", () => {
    expect(correctedModelAdoption({ ...base, activeConvKey: null })).toEqual({
      resend: true,
      adoptIntoPicker: false,
    });
  });

  it("a refusal naming no model corrects nothing — the user must pick", () => {
    expect(correctedModelAdoption({ ...base, refusalModel: "" })).toEqual({
      resend: false,
      adoptIntoPicker: false,
    });
  });

  it("a retry that is itself refused is not retried again", () => {
    expect(correctedModelAdoption({ ...base, isModelRetry: true })).toEqual({
      resend: false,
      adoptIntoPicker: false,
    });
  });

  // How the FIRST attempt actually calls it — streamTurn's parameter is
  // optional and omitted on the way in.
  it("an omitted retry flag is a first attempt, not a retry", () => {
    expect(
      correctedModelAdoption({ ...base, isModelRetry: undefined }),
    ).toEqual({ resend: true, adoptIntoPicker: true });
  });
});
