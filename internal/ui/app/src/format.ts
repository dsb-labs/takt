import type { HealthStatus } from "@/api/types";

// Formatting helpers shared by the views.

const relative = new Intl.RelativeTimeFormat("en", { numeric: "auto" });

const divisions: { amount: number; unit: Intl.RelativeTimeFormatUnit }[] = [
  { amount: 60, unit: "seconds" },
  { amount: 60, unit: "minutes" },
  { amount: 24, unit: "hours" },
  { amount: 7, unit: "days" },
  { amount: 4.34524, unit: "weeks" },
  { amount: 12, unit: "months" },
  { amount: Number.POSITIVE_INFINITY, unit: "years" },
];

// relativeTime renders an instant relative to now: "3 minutes ago",
// "in 2 hours".
export function relativeTime(instant: string): string {
  let duration = (new Date(instant).getTime() - Date.now()) / 1000;

  for (const division of divisions) {
    if (Math.abs(duration) < division.amount) {
      return relative.format(Math.round(duration), division.unit);
    }
    duration /= division.amount;
  }

  return instant;
}

// absoluteTime renders an instant as a full local date and time, for titles
// and detail rows where the exact moment matters.
export function absoluteTime(instant: string): string {
  return new Date(instant).toLocaleString();
}

// pluralize renders a count with its noun: "1 workload", "3 workloads".
export function pluralize(count: number, noun: string): string {
  return count === 1 ? `1 ${noun}` : `${count} ${noun}s`;
}

const units = ["B", "KiB", "MiB", "GiB", "TiB"];

// bytes renders a byte count in the largest unit that leaves a figure worth
// reading: "512 MiB", "1.5 GiB". Binary units, because that is what a memory
// limit is written and enforced in.
export function bytes(count: number): string {
  let value = count;
  let unit = 0;

  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }

  // A whole number needs no decimal, and a fraction reads better with one
  // than with the six a division leaves behind.
  const rendered = unit === 0 || value >= 100 ? Math.round(value) : value;
  return `${Number(rendered.toFixed(1))} ${units[unit]}`;
}

// cores renders a processor figure the way a limit is written: "0.35", "2".
export function cores(count: number): string {
  return String(Number(count.toFixed(2)));
}

// usageStyles colours a figure by how close it is to the limit it answers
// to, so a workload about to be killed reads as such before it is. A figure
// with no limit, or one with room left, keeps the muted colour the rest of
// the row is written in.
export function usageStyles(used: number, limit: number | undefined): string {
  const muted = "text-slate-500 dark:text-slate-400";
  if (!limit) return muted;

  const fraction = used / limit;
  if (fraction >= 0.95) return "text-rose-700 dark:text-rose-400";
  if (fraction >= 0.8) return "text-amber-700 dark:text-amber-400";

  return muted;
}

// healthStyles colours a health status the same way everywhere it appears.
// Keyed by the schema's own status type, so a renamed status fails the build
// rather than rendering uncoloured.
export const healthStyles: Record<HealthStatus, string> = {
  healthy: "text-emerald-700 dark:text-emerald-400",
  unhealthy: "text-rose-700 dark:text-rose-400",
  starting: "text-sky-700 dark:text-sky-400",
};
