import { pollInterval } from "@/api/queries";
import type { Point, Series } from "@/composables/series";

// The arithmetic behind the usage chart: where a line breaks, how far the
// axis reaches, and where its ticks fall. It sits apart from the component so
// it can be tested without drawing anything.

// broken joins the points a chart draws, with a gap left wherever the page
// stopped polling — a hidden tab, or a request that failed. Without it the
// line is drawn straight across the pause, which reads as a slow climb or
// fall the instance never made.
export function broken(points: Point[]): (Point | { x: number; y: null })[] {
  const gap = pollInterval * 3;

  return points.flatMap((point, at) => {
    const previous = points[at - 1];
    if (previous && point.x - previous.x > gap) {
      return [{ x: previous.x + gap / 2, y: null }, point];
    }

    return [point];
  });
}

// extent reports the first and last instant any series has a reading at,
// which is how far the limit line has to reach.
export function extent(series: Series[]): [number, number] | null {
  const instants = series.flatMap((line) => line.points.map((p) => p.x));
  if (!instants.length) return null;

  return [Math.min(...instants), Math.max(...instants)];
}

// ago writes an instant as how long before the latest reading it was, which is
// what a rolling window wants on its axis: "now", "2m ago".
export function ago(instant: number, latest: number): string {
  const seconds = Math.round((latest - instant) / 1000);
  if (seconds < 5) return "now";
  if (seconds < 60) return `${seconds}s ago`;

  return `${Math.round(seconds / 60)}m ago`;
}

// marks places a tick every so often back from the latest reading, rather
// than leaving the library to pick round numbers of milliseconds. Two of
// those can land inside the same second of history and both read "1m ago",
// and the ticks it adds past the ends leave the lines short of the walls.
export function marks(span: [number, number]): number[] {
  const [oldest, latest] = span;
  const step =
    latest - oldest <= 60_000
      ? 15_000
      : latest - oldest <= 180_000
        ? 30_000
        : 60_000;

  const ticks: number[] = [];
  for (let at = latest; at >= oldest; at -= step) ticks.push(at);

  return ticks.reverse();
}
