import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";

// useQueryFilter keeps a list's filter input in the page's own query string,
// so a filtered list survives a refresh, can be shared as a link, and can be
// the target of a label link from a detail page.
export function useQueryFilter() {
  const route = useRoute();
  const router = useRouter();

  const filter = ref(
    typeof route.query.query === "string" ? route.query.query : "",
  );

  watch(filter, (value) => {
    void router.replace({ query: value ? { query: value } : {} });
  });

  // Navigation can change the parameter while the list is already mounted —
  // a label link pointing at the page it is on — so the input follows it.
  watch(
    () => route.query.query,
    (value) => {
      const next = typeof value === "string" ? value : "";
      if (next !== filter.value) filter.value = next;
    },
  );

  const queries = computed(() => filter.value.split(/\s+/).filter(Boolean));

  return { filter, queries };
}
