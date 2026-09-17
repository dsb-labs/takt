import { describe, expect, it } from "vitest";

import { bytes, cores, percent, pluralize, usageStyles } from "@/lib/format";

describe("cores", () => {
  it.each([
    [0, "0"],
    [2, "2"],
    [0.5, "0.5"],
    [0.35, "0.35"],
    [0.123, "0.12"],
    [1.999, "2"],
    // Thousandths of a core keep two significant figures rather than rounding
    // to nothing, which is what an idle process reads as.
    [0.0031, "0.0031"],
    [0.00312, "0.0031"],
    [0.0005, "0.0005"],
    [0.00025, "0.00025"],
    [1e-12, "0"],
  ])("writes %s as %s", (count, expected) => {
    expect(cores(count)).toBe(expected);
  });

  // Chart.js steps an axis by 1, 2 or 5 of a power of ten, so every tick it
  // places over a small range has to come out distinct.
  it.each([0.0001, 0.0002, 0.0005])("keeps ticks %s apart distinct", (step) => {
    const ticks = Array.from({ length: 11 }, (_, i) => cores(i * step));
    expect(new Set(ticks).size).toBe(ticks.length);
  });
});

describe("bytes", () => {
  it.each([
    [0, "0 B"],
    [512, "512 B"],
    [1024, "1 KiB"],
    [1536, "1.5 KiB"],
    [512 * 1024 * 1024, "512 MiB"],
    [1.5 * 1024 ** 3, "1.5 GiB"],
    [100.25 * 1024 ** 2, "100 MiB"],
    [1024 ** 4, "1 TiB"],
    // The units stop at TiB, so anything past it is written in TiB.
    [1024 ** 5, "1024 TiB"],
  ])("writes %s as %s", (count, expected) => {
    expect(bytes(count)).toBe(expected);
  });
});

describe("percent", () => {
  it.each([
    [42, 100, "42%"],
    [1, 3, "33%"],
    [0, 0, "0%"],
    [5, 0, "0%"],
  ])("writes %s of %s as %s", (part, whole, expected) => {
    expect(percent(part, whole)).toBe(expected);
  });
});

describe("pluralize", () => {
  it.each([
    [0, "0 workloads"],
    [1, "1 workload"],
    [3, "3 workloads"],
  ])("writes %s as %s", (count, expected) => {
    expect(pluralize(count, "workload")).toBe(expected);
  });
});

describe("usageStyles", () => {
  const muted = usageStyles(0, undefined);

  it("keeps the muted colour without a limit", () => {
    expect(usageStyles(100, undefined)).toBe(muted);
    expect(usageStyles(100, 0)).toBe(muted);
  });

  it("keeps the muted colour with room left", () => {
    expect(usageStyles(79, 100)).toBe(muted);
  });

  it("warns at four fifths and alarms at nineteen twentieths", () => {
    expect(usageStyles(80, 100)).toContain("amber");
    expect(usageStyles(95, 100)).toContain("rose");
  });
});
