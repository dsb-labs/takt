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
