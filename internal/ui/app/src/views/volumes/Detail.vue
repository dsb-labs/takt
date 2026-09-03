<script setup lang="ts">
import { computed } from "vue";
import { useRoute, useRouter } from "vue-router";

import { useDeleteVolume } from "../../api/mutations";
import { useVolume } from "../../api/queries";
import DeleteControl from "../../components/DeleteControl.vue";
import DetailCard from "../../components/DetailCard.vue";
import LabelsCard from "../../components/LabelsCard.vue";
import UsedByCard from "../../components/UsedByCard.vue";
import { absoluteTime, relativeTime } from "../../format";

const route = useRoute();
const router = useRouter();
const name = computed(() => route.params.name as string);

const volume = useVolume(() => name.value);
const deleteVolume = useDeleteVolume(() => name.value);

// Awaited so Suspense holds the previous view until this one has its
// data. A failure is left for the error banner this view already renders.
await volume.suspense().catch(() => {});
</script>

<template>
  <div>
    <nav class="text-sm text-slate-500 dark:text-slate-400">
      <RouterLink to="/volumes" class="hover:underline">Volumes</RouterLink>
      <span class="mx-1">/</span>
      <span class="text-slate-900 dark:text-slate-100">{{ name }}</span>
    </nav>

    <div
      v-if="volume.isError.value"
      class="mt-6 rounded-lg border border-rose-200 bg-rose-50 p-4 text-sm text-rose-800 dark:border-rose-900 dark:bg-rose-950 dark:text-rose-300"
    >
      Failed to read the volume: {{ volume.error.value?.message }}
    </div>

    <template v-else-if="volume.data.value">
      <header class="mt-4 flex flex-wrap items-center gap-3">
        <h1 class="text-xl font-semibold">{{ volume.data.value.name }}</h1>
        <div class="ml-auto">
          <DeleteControl
            :subject="`the volume ${name} and its data`"
            :remove="(force) => deleteVolume.mutateAsync(force)"
            @deleted="router.push('/volumes')"
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
                Path
              </dt>
              <dd class="font-mono text-xs leading-5 break-all">
                {{ volume.data.value.path }}
              </dd>
            </div>
            <div class="flex gap-4 px-4 py-2.5">
              <dt class="w-32 shrink-0 text-slate-500 dark:text-slate-400">
                Created
              </dt>
              <dd :title="absoluteTime(volume.data.value.createdAt)">
                {{ relativeTime(volume.data.value.createdAt) }}
              </dd>
            </div>
          </dl>
        </DetailCard>

        <UsedByCard :used-by="volume.data.value.usedBy" />

        <LabelsCard :labels="volume.data.value.labels" target="/volumes" />
      </div>
    </template>
  </div>
</template>
