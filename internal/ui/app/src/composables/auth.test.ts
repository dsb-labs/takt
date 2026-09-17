import { afterEach, describe, expect, it } from "vitest";

import { admin, forgetIdentity, identity, operator } from "@/composables/auth";

function signIn(partial: Partial<NonNullable<typeof identity.value>>) {
  identity.value = {
    enabled: true,
    principal: "someone",
    role: "viewer",
    groups: [],
    recovery: false,
    ...partial,
  };
}

afterEach(() => forgetIdentity());

describe("operator", () => {
  it("refuses before anyone has signed in", () => {
    expect(operator()).toBe(false);
  });

  it.each([
    ["viewer", false],
    ["operator", true],
    ["admin", true],
  ])("answers %s with %s", (role, expected) => {
    signIn({ role });
    expect(operator()).toBe(expected);
  });

  it("permits everything when authentication is disabled", () => {
    signIn({ enabled: false, role: "viewer" });
    expect(operator()).toBe(true);
  });

  it("permits the recovery token whatever its role", () => {
    signIn({ recovery: true, role: "viewer" });
    expect(operator()).toBe(true);
  });
});

describe("admin", () => {
  it("refuses before anyone has signed in", () => {
    expect(admin()).toBe(false);
  });

  it.each([
    ["viewer", false],
    ["operator", false],
    ["admin", true],
  ])("answers %s with %s", (role, expected) => {
    signIn({ role });
    expect(admin()).toBe(expected);
  });

  it("permits everything when authentication is disabled", () => {
    signIn({ enabled: false, role: "viewer" });
    expect(admin()).toBe(true);
  });

  it("permits the recovery token whatever its role", () => {
    signIn({ recovery: true, role: "viewer" });
    expect(admin()).toBe(true);
  });
});

describe("forgetIdentity", () => {
  it("leaves the session as it was before signing in", () => {
    signIn({ role: "admin" });
    forgetIdentity();

    expect(identity.value).toBeNull();
    expect(admin()).toBe(false);
  });
});
