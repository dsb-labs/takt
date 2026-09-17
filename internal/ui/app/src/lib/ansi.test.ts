import { describe, expect, it } from "vitest";

import { parser } from "@/lib/ansi";

const esc = "\x1b[";

describe("parser", () => {
  it("passes plain text through unstyled", () => {
    expect(parser()("hello")).toEqual([{ text: "hello", classes: "" }]);
  });

  it("yields nothing for an empty chunk", () => {
    expect(parser()("")).toEqual([]);
  });

  it("styles a run by its named colours and decorations", () => {
    const spans = parser()(`${esc}1;31;44mwarn${esc}0m ok`);

    expect(spans).toEqual([
      { text: "warn", classes: "text-rose-400 bg-sky-900 font-bold" },
      { text: " ok", classes: "" },
    ]);
  });

  it("reads a bright colour", () => {
    expect(parser()(`${esc}92mok`)[0]?.classes).toBe("text-emerald-300");
  });

  it("reads the first sixteen palette entries as the named colours", () => {
    expect(parser()(`${esc}38;5;3mx`)[0]?.classes).toBe("text-pulse-400");
    expect(parser()(`${esc}48;5;12mx`)[0]?.classes).toBe("bg-sky-800");
  });

  it("writes a colour from the cube or the greys as a style", () => {
    const [cube] = parser()(`${esc}38;5;196mx`);
    expect(cube?.classes).toBe("");
    expect(cube?.style).toBe("color:rgb(255, 0, 0)");

    const [grey] = parser()(`${esc}48;5;232mx`);
    expect(grey?.style).toBe("background-color:rgb(8, 8, 8)");
  });

  it("writes a truecolour sequence as a style", () => {
    const [span] = parser()(`${esc}38;2;12;34;56mx`);
    expect(span?.style).toBe("color:rgb(12, 34, 56)");
  });

  it("carries an open colour into the next chunk", () => {
    const parse = parser();
    parse(`${esc}32mfirst`);

    expect(parse("second")).toEqual([
      { text: "second", classes: "text-emerald-400" },
    ]);
  });

  it("stops carrying a colour once a reset closes it", () => {
    const parse = parser();
    parse(`${esc}32mfirst`);
    parse(`${esc}0mplain`);

    expect(parse("third")).toEqual([{ text: "third", classes: "" }]);
  });

  it("waits for the rest of a sequence cut in half", () => {
    const parse = parser();

    expect(parse(`before${esc}3`)).toEqual([{ text: "before", classes: "" }]);
    expect(parse("1mred")).toEqual([{ text: "red", classes: "text-rose-400" }]);
  });

  it("waits for the rest of an escape cut after its first byte", () => {
    const parse = parser();

    expect(parse("a\x1b")).toEqual([{ text: "a", classes: "" }]);
    expect(parse("[1mb")).toEqual([{ text: "b", classes: "font-bold" }]);
  });

  it("strips a window title a program sets", () => {
    expect(parser()("\x1b]0;title\x07text")).toEqual([
      { text: "text", classes: "" },
    ]);
    expect(parser()("\x1b]0;title\x1b\\text")).toEqual([
      { text: "text", classes: "" },
    ]);
  });

  it("waits for the rest of a window title cut in half", () => {
    const parse = parser();

    expect(parse("a\x1b]0;tit")).toEqual([{ text: "a", classes: "" }]);
    expect(parse("le\x07b")).toEqual([{ text: "b", classes: "" }]);
  });
});
