<script setup lang="ts">
import { computed, ref } from "vue";
import { useRoute, useRouter } from "vue-router";

import { useDeleteSecret, useSetSecret } from "../api/mutations";
import { useSecret } from "../api/queries";
import DeleteControl from "../components/DeleteControl.vue";
import DetailCard from "../components/DetailCard.vue";
import ErrorBanner from "../components/ErrorBanner.vue";
import UsedByLinks from "../components/UsedByLinks.vue";
import ValueForm from "../components/ValueForm.vue";
import { absoluteTime, relativeTime } from "../format";

const route = useRoute();
const router = useRouter();
const name = computed(() => route.params.name as string);

const secret = useSecret(() => name.value);
const setSecret = useSetSecret();
const deleteSecret = useDeleteSecret(() => name.value);

const rotateError = ref("");
const rotated = ref(false);

async function rotate(_: string, value: string) {
  rotateError.value = "";
  rotated.value = false;
  try {
    await setSecret.mutateAsync({ name: name.value, value });
    rotated.value = true;
  } catch (cause) {
    rotateError.value = cause instanceof Error ? cause.message : String(cause);
  }
}
</script>

<template>
  <div>
    <nav class="text-sm text-slate-500 dark:text-slate-400">
      <RouterLink to="/secrets" class="hover:underline">Secrets</RouterLink>
      <span class="mx-1">/</span>
      <span class="text-slate-900 dark:text-slate-100">{{ name }}</span>
    </nav>

    <div
      v-if="secret.isError.value"
      class="mt-6 rounded-lg border border-rose-200 bg-rose-50 p-4 text-sm text-rose-800 dark:border-rose-900 dark:bg-rose-950 dark:text-rose-300"
    >
      Failed to read the secret: {{ secret.error.value?.message }}
    </div>

    <template v-else-if="secret.data.value">
      <h1 class="mt-4 text-xl font-semibold">{{ secret.data.value.name }}</h1>

      <div class="mt-6 grid gap-6 xl:grid-cols-2">
        <DetailCard title="Overview">
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Revision
              </dt>
              <dd class="font-mono text-xs leading-5">
                {{ secret.data.value.revision }}
              </dd>
            </div>
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Created
              </dt>
              <dd :title="absoluteTime(secret.data.value.createdAt)">
                {{ relativeTime(secret.data.value.createdAt) }}
              </dd>
            </div>
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Updated
              </dt>
              <dd :title="absoluteTime(secret.data.value.updatedAt)">
                {{ relativeTime(secret.data.value.updatedAt) }}
              </dd>
            </div>
          </dl>
        </DetailCard>

        <DetailCard title="Used by">
          <div class="px-4 py-3 text-sm">
            <UsedByLinks :used-by="secret.data.value.usedBy" />
          </div>
        </DetailCard>
      </div>

      <div class="mt-6">
        <DetailCard title="Rotate">
          <div class="px-4 py-4">
            <p class="mb-3 text-sm text-slate-500 dark:text-slate-400">
              Set a new value. The current value is never shown, and every
              workload reading this secret picks the new one up.
            </p>
            <ValueForm
              submit-label="Rotate"
              placeholder="The new value"
              :busy="setSecret.isPending.value"
              @submit="rotate"
            />
            <ErrorBanner v-if="rotateError" :message="rotateError" />
            <p
              v-if="rotated"
              class="mt-3 text-sm text-emerald-700 dark:text-emerald-400"
            >
              Rotated. The revision above changed with it.
            </p>
          </div>
        </DetailCard>
      </div>

      <div class="mt-6">
        <DeleteControl
          :subject="`the secret ${name}`"
          :remove="(force) => deleteSecret.mutateAsync(force)"
          @deleted="router.push('/secrets')"
        />
      </div>
    </template>
  </div>
</template>
