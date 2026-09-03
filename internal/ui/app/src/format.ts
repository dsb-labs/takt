import type { HealthStatus } from "./api/types";

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

// healthStyles colours a health status the same way everywhere it appears.
// Keyed by the schema's own status type, so a renamed status fails the build
// rather than rendering uncoloured.
export const healthStyles: Record<HealthStatus, string> = {
  healthy: "text-emerald-700 dark:text-emerald-400",
  unhealthy: "text-rose-700 dark:text-rose-400",
  starting: "text-sky-700 dark:text-sky-400",
};
