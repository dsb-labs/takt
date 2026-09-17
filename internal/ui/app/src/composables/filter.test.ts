import { describe, expect, it } from "vitest";

import { labelQuery } from "@/composables/filter";

describe("labelQuery", () => {
  it("quotes the key, so a dotted key is one path element", () => {
    expect(labelQuery("app.kubernetes.io/name", "api")).toBe(
      '$.labels."app.kubernetes.io/name"=api',
    );
  });

  it("writes a plain key the same way", () => {
    expect(labelQuery("app", "api")).toBe('$.labels."app"=api');
  });
});
