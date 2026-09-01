<script setup lang="ts">
import { computed, ref } from "vue";
import { useRoute, useRouter } from "vue-router";

import { useDeleteVariable, useSetVariable } from "../api/mutations";
import { useVariable } from "../api/queries";
import DeleteControl from "../components/DeleteControl.vue";
import DetailCard from "../components/DetailCard.vue";
import ErrorBanner from "../components/ErrorBanner.vue";
import UsedByLinks from "../components/UsedByLinks.vue";
import ValueForm from "../components/ValueForm.vue";
import { absoluteTime, relativeTime } from "../format";

const route = useRoute();
const router = useRouter();
const name = computed(() => route.params.name as string);

const variable = useVariable(() => name.value);
const setVariable = useSetVariable();
const deleteVariable = useDeleteVariable(() => name.value);

const editError = ref("");
const saved = ref(false);

async function save(_: string, value: string) {
  editError.value = "";
  saved.value = false;
  try {
    await setVariable.mutateAsync({ name: name.value, value });
    saved.value = true;
  } catch (cause) {
    editError.value = cause instanceof Error ? cause.message : String(cause);
  }
}
</script>

<template>
  <div>
    <nav class="text-sm text-slate-500 dark:text-slate-400">
      <RouterLink to="/variables" class="hover:underline">Variables</RouterLink>
      <span class="mx-1">/</span>
      <span class="text-slate-900 dark:text-slate-100">{{ name }}</span>
    </nav>

    <div
      v-if="variable.isError.value"
      class="mt-6 rounded-lg border border-rose-200 bg-rose-50 p-4 text-sm text-rose-800 dark:border-rose-900 dark:bg-rose-950 dark:text-rose-300"
    >
      Failed to read the variable: {{ variable.error.value?.message }}
    </div>

    <template v-else-if="variable.data.value">
      <header class="mt-4 flex flex-wrap items-center gap-3">
        <h1 class="text-xl font-semibold">{{ variable.data.value.name }}</h1>
        <div class="ml-auto">
          <DeleteControl
            :subject="`the variable ${name}`"
            :remove="(force) => deleteVariable.mutateAsync(force)"
            @deleted="router.push('/variables')"
          />
        </div>
      </header>

      <div class="mt-6 grid gap-6 xl:grid-cols-2">
        <DetailCard title="Overview">
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Created
              </dt>
              <dd :title="absoluteTime(variable.data.value.createdAt)">
                {{ relativeTime(variable.data.value.createdAt) }}
              </dd>
            </div>
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Updated
              </dt>
              <dd :title="absoluteTime(variable.data.value.updatedAt)">
                {{ relativeTime(variable.data.value.updatedAt) }}
              </dd>
            </div>
          </dl>
        </DetailCard>

        <DetailCard title="Used by">
          <div class="px-4 py-3 text-sm">
            <UsedByLinks :used-by="variable.data.value.usedBy" />
          </div>
        </DetailCard>
      </div>

      <div class="mt-6">
        <DetailCard title="Value">
          <div class="px-4 py-4">
            <ValueForm
              :key="variable.data.value.updatedAt"
              submit-label="Save"
              :initial-value="variable.data.value.value"
              :busy="setVariable.isPending.value"
              @submit="save"
            />
            <ErrorBanner v-if="editError" :message="editError" />
            <p
              v-if="saved"
              class="mt-3 text-sm text-emerald-700 dark:text-emerald-400"
            >
              Saved.
            </p>
          </div>
        </DetailCard>
      </div>
    </template>
  </div>
</template>
