import { describe, expect, it } from "vitest";

import { useSort } from "@/composables/sort";

type Row = { name: string; age: number };

const rows: Row[] = [
  { name: "carol", age: 31 },
  { name: "alice", age: 45 },
  { name: "bob", age: 27 },
];

const columns = {
  name: (row: Row) => row.name,
  age: (row: Row) => row.age,
};

describe("useSort", () => {
  it("starts ascending by the initial column", () => {
    const { sorted, key, descending } = useSort(() => rows, "name", columns);

    expect(key.value).toBe("name");
    expect(descending.value).toBe(false);
    expect(sorted.value.map((row) => row.name)).toEqual([
      "alice",
      "bob",
      "carol",
    ]);
  });

  it("can start descending, for a list keyed by a time", () => {
    const { sorted } = useSort(() => rows, "age", columns, true);

    expect(sorted.value.map((row) => row.age)).toEqual([45, 31, 27]);
  });

  it("reverses when the same column is toggled", () => {
    const { sorted, toggle, descending } = useSort(() => rows, "name", columns);

    toggle("name");

    expect(descending.value).toBe(true);
    expect(sorted.value.map((row) => row.name)).toEqual([
      "carol",
      "bob",
      "alice",
    ]);
  });

  it("starts another column ascending", () => {
    const { sorted, toggle, key, descending } = useSort(
      () => rows,
      "name",
      columns,
    );

    toggle("name");
    toggle("age");

    expect(key.value).toBe("age");
    expect(descending.value).toBe(false);
    expect(sorted.value.map((row) => row.age)).toEqual([27, 31, 45]);
  });

  it("leaves the order alone under a column it does not know", () => {
    const { sorted } = useSort(() => rows, "missing", columns);

    expect(sorted.value).toEqual(rows);
  });

  it("sorts nothing when there is nothing yet", () => {
    const { sorted } = useSort(() => undefined, "name", columns);

    expect(sorted.value).toEqual([]);
  });

  it("does not reorder the list it was given", () => {
    const copy = [...rows];
    const { sorted } = useSort(() => rows, "name", columns);
    void sorted.value;

    expect(rows).toEqual(copy);
  });
});
