import { computed, ref, type ComputedRef, type Ref } from "vue";

// The Sort type is what useSort returns: the sorted rows, which column holds
// the order, and the toggle a header calls.
export type Sort<T> = {
  sorted: ComputedRef<T[]>;
  key: Ref<string>;
  descending: Ref<boolean>;
  toggle: (name: string) => void;
};

// useSort orders a list by a named column. Clicking the same column again
// reverses the order, and clicking another column starts it ascending.
export function useSort<T>(
  items: () => T[] | undefined,
  initial: string,
  columns: Record<string, (item: T) => string | number>,
): Sort<T> {
  const key = ref(initial);
  const descending = ref(false);

  function toggle(name: string) {
    if (key.value === name) {
      descending.value = !descending.value;
      return;
    }
    key.value = name;
    descending.value = false;
  }

  const sorted = computed(() => {
    const list = [...(items() ?? [])];
    const by = columns[key.value];
    if (!by) return list;

    list.sort((a, b) => {
      const left = by(a);
      const right = by(b);
      if (left < right) return -1;
      if (left > right) return 1;
      return 0;
    });
    if (descending.value) list.reverse();

    return list;
  });

  return { sorted, key, descending, toggle };
}
