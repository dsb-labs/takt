import Anser, { type AnserJsonEntry } from "anser";

// Turning the escape sequences a program writes into styled spans, so the log
// view shows what a terminal would show rather than the sequences themselves.
//
// Anser does the parsing. This module decides what the colours look like: a
// program picks one of sixteen names, and which colour a name means is the
// terminal's business. The log view is dark in both schemes, so one palette
// serves, and it is the palette the rest of the interface is drawn from.

export type Span = {
  text: string;
  // The Tailwind classes the span is drawn with.
  classes: string;
  // A colour no palette entry covers, as an inline style. A 256-colour or
  // truecolour sequence names a colour outright rather than naming a slot.
  style?: string;
};

// The sixteen named colours, mapped onto the takt palette. Yellow is the
// brass the interface accents with, and the rest are the status colours the
// badges and health rows already use, at the stops that hold their contrast
// on the near-black the log box is drawn on.
const foreground: Record<string, string> = {
  "ansi-black": "text-slate-600",
  "ansi-red": "text-rose-400",
  "ansi-green": "text-emerald-400",
  "ansi-yellow": "text-pulse-400",
  "ansi-blue": "text-sky-400",
  "ansi-magenta": "text-violet-400",
  "ansi-cyan": "text-cyan-400",
  "ansi-white": "text-slate-300",
  "ansi-bright-black": "text-slate-500",
  "ansi-bright-red": "text-rose-300",
  "ansi-bright-green": "text-emerald-300",
  "ansi-bright-yellow": "text-pulse-300",
  "ansi-bright-blue": "text-sky-300",
  "ansi-bright-magenta": "text-violet-300",
  "ansi-bright-cyan": "text-cyan-300",
  "ansi-bright-white": "text-slate-100",
};

const background: Record<string, string> = {
  "ansi-black": "bg-slate-900",
  "ansi-red": "bg-rose-900",
  "ansi-green": "bg-emerald-900",
  "ansi-yellow": "bg-pulse-900",
  "ansi-blue": "bg-sky-900",
  "ansi-magenta": "bg-violet-900",
  "ansi-cyan": "bg-cyan-900",
  "ansi-white": "bg-slate-700",
  "ansi-bright-black": "bg-slate-800",
  "ansi-bright-red": "bg-rose-800",
  "ansi-bright-green": "bg-emerald-800",
  "ansi-bright-yellow": "bg-pulse-800",
  "ansi-bright-blue": "bg-sky-800",
  "ansi-bright-magenta": "bg-violet-800",
  "ansi-bright-cyan": "bg-cyan-800",
  "ansi-bright-white": "bg-slate-600",
};

const decorations: Record<string, string> = {
  bold: "font-bold",
  dim: "opacity-60",
  italic: "italic",
  underline: "underline",
  strikethrough: "line-through",
  blink: "",
  hidden: "invisible",
};

// A style carried from one chunk to the next. A colour opened at the end of
// one read stays open until the program closes it, which may be several reads
// later.
type Carried = { classes: string; style?: string };

const none: Carried = { classes: "" };

// A parser reads the stream a chunk at a time, because that is how the log
// view receives it. State is kept between calls for two reasons: a colour
// stays open across a chunk boundary, and an escape sequence can be cut in
// half by one.
export function parser(): (chunk: string) => Span[] {
  let carried = none;
  let held = "";

  return (chunk: string): Span[] => {
    const text = held + chunk;
    // A sequence cut in half reads as text, and the half that arrives next
    // reads as a stray letter. Waiting for the rest costs one read.
    const cut = /\x1b(\[[\d;]*|\][^\x07\x1b]*)?$/.exec(text);
    held = cut ? cut[0] : "";

    const body = strip(cut ? text.slice(0, cut.index) : text);
    if (!body) return [];

    // Anser reads each call from a clean state, so a colour left open by the
    // read before this one is carried in by hand. It applies to the run
    // before this read's first sequence and to nothing after it, since from
    // there Anser knows what is open and what a reset has closed.
    const leading = !body.startsWith("\x1b[");

    return Anser.ansiToJson(body, {
      json: true,
      remove_empty: true,
      use_classes: true,
    }).map((entry, at) => {
      const style =
        at === 0 && leading && !entry.was_processed ? carried : styleOf(entry);
      carried = style;

      return { text: entry.content, ...style };
    });
  };
}

// strip removes the sequences that are neither text nor colour. Anser drops
// the ones that move the cursor or erase a line, and leaves an operating
// system command — a program setting the window title — in the text.
function strip(text: string): string {
  return text.replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g, "");
}

function styleOf(entry: AnserJsonEntry): Carried {
  const classes: string[] = [];
  const colours: string[] = [];

  // Anser has already swapped the pair for a run in reverse video, so the
  // two colours are read as they are.
  const { fg, bg, fg_truecolor: fgTrue, bg_truecolor: bgTrue } = entry;

  if (fg) {
    const named = foreground[slot(fg)];
    const exact = exactly(fg, fgTrue);
    if (named) classes.push(named);
    else if (exact) colours.push(`color:${exact}`);
  }

  if (bg) {
    const named = background[slot(bg)];
    const exact = exactly(bg, bgTrue);
    if (named) classes.push(named);
    else if (exact) colours.push(`background-color:${exact}`);
  }

  for (const decoration of entry.decorations) {
    const style = decorations[decoration];
    if (style) classes.push(style);
  }

  return {
    classes: classes.join(" "),
    style: colours.length ? colours.join(";") : undefined,
  };
}

// The first sixteen entries of the 256-colour palette are the named colours
// written another way, so they are read as the name rather than as a colour
// of their own.
const slots = [
  "ansi-black",
  "ansi-red",
  "ansi-green",
  "ansi-yellow",
  "ansi-blue",
  "ansi-magenta",
  "ansi-cyan",
  "ansi-white",
  "ansi-bright-black",
  "ansi-bright-red",
  "ansi-bright-green",
  "ansi-bright-yellow",
  "ansi-bright-blue",
  "ansi-bright-magenta",
  "ansi-bright-cyan",
  "ansi-bright-white",
];

function slot(name: string): string {
  const palette = /^ansi-palette-(\d+)$/.exec(name);
  if (!palette) return name;

  return slots[Number(palette[1])] ?? name;
}

// exactly reads a colour a sequence named outright rather than by slot. A
// truecolour sequence carries its channels, and a 256-colour one carries an
// index into a cube of 216 colours followed by 24 greys.
function exactly(name: string, truecolour: string | null): string | undefined {
  if (name === "ansi-truecolor" && truecolour) return `rgb(${truecolour})`;

  const palette = /^ansi-palette-(\d+)$/.exec(name);
  if (!palette) return undefined;

  const index = Number(palette[1]);
  if (index < 16 || index > 255) return undefined;

  if (index >= 232) {
    const grey = 8 + (index - 232) * 10;
    return `rgb(${grey}, ${grey}, ${grey})`;
  }

  const steps = [0, 95, 135, 175, 215, 255];
  const at = index - 16;
  const red = steps[Math.floor(at / 36)];
  const green = steps[Math.floor(at / 6) % 6];
  const blue = steps[at % 6];

  return `rgb(${red}, ${green}, ${blue})`;
}
