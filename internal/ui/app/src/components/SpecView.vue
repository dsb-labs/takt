<script setup lang="ts">
import hljs from "highlight.js/lib/core";
import yamlLanguage from "highlight.js/lib/languages/yaml";
import { computed } from "vue";
import { stringify } from "yaml";

import type { WorkloadSpec } from "../api/types";

// Only the one language is registered, so the bundle carries the highlighter
// core and the yaml grammar rather than every language it knows.
hljs.registerLanguage("yaml", yamlLanguage);

const props = defineProps<{ spec: WorkloadSpec }>();

// The API returns the specification as JSON. Rendering it as YAML shows it
// the way the manifest was written. The highlighter escapes the content, so
// the output is safe to render as HTML.
const highlighted = computed(
  () => hljs.highlight(stringify(props.spec), { language: "yaml" }).value,
);
</script>

<template>
  <!-- The highlighter escapes the content, so the markup carries only its own
       token spans. -->
  <!-- eslint-disable vue/no-v-html -->
  <pre
    class="overflow-auto bg-slate-950 px-4 py-3 font-mono text-xs leading-relaxed text-slate-200"
  ><code v-html="highlighted"></code></pre>
</template>
