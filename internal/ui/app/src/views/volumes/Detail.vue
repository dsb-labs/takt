<script setup lang="ts">
import { computed } from "vue";
import { useRoute, useRouter } from "vue-router";

import { useDeleteVolume } from "../../api/mutations";
import { useVolume } from "../../api/queries";
import DeleteControl from "../../components/DeleteControl.vue";
import DetailCard from "../../components/DetailCard.vue";
import DetailPage from "../../components/DetailPage.vue";
import LabelsCard from "../../components/LabelsCard.vue";
import UsedByCard from "../../components/UsedByCard.vue";
import OverviewRow from "../../components/OverviewRow.vue";
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
  <DetailPage
    section="Volumes"
    section-to="/volumes"
    :name="name"
    :error="
      volume.isError.value
        ? `Failed to read the volume: ${volume.error.value?.message}`
        : ''
    "
  >
    <template #actions>
      <DeleteControl
        :subject="`the volume ${name} and its data`"
        :remove="(force) => deleteVolume.mutateAsync(force)"
        @deleted="router.push('/volumes')"
      />
    </template>

    <template v-if="volume.data.value">
      <div class="mt-6 grid gap-6 xl:grid-cols-2">
        <DetailCard title="Overview">
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <OverviewRow label="Path" mono>{{
              volume.data.value.path
            }}</OverviewRow>
            <OverviewRow v-if="volume.data.value.owner" label="Owner" mono>{{
              volume.data.value.owner
            }}</OverviewRow>
            <OverviewRow v-if="volume.data.value.mode" label="Mode" mono>{{
              volume.data.value.mode
            }}</OverviewRow>
            <OverviewRow
              label="Created"
              :title="absoluteTime(volume.data.value.createdAt)"
              >{{ relativeTime(volume.data.value.createdAt) }}</OverviewRow
            >
          </dl>
        </DetailCard>

        <UsedByCard :used-by="volume.data.value.usedBy" />

        <LabelsCard :labels="volume.data.value.labels" target="/volumes" />
      </div>
    </template>
  </DetailPage>
</template>
