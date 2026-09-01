import {
  defineConfigWithVueTs,
  vueTsConfigs,
} from "@vue/eslint-config-typescript";
import prettierConfig from "eslint-config-prettier";
import pluginVue from "eslint-plugin-vue";

export default defineConfigWithVueTs(
  // The schema is generated from the OpenAPI document and the bundle is build
  // output, so neither is ours to lint.
  { ignores: ["src/api/schema.d.ts", "../dist/**"] },
  pluginVue.configs["flat/recommended"],
  vueTsConfigs.recommended,
  prettierConfig,
  {
    rules: {
      // Components are named by their path, views/variables/Detail.vue, so
      // single-word file names are the convention rather than a mistake.
      "vue/multi-word-component-names": "off",
    },
  },
);
