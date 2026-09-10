<script setup lang="ts">
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
import { operator } from "../../auth";

const route = useRoute();
const router = useRouter();
// A snapshot rather than a computed: Suspense keeps this view on screen
// while the next one loads, and a reactive read would watch the route it
// is leaving and render the page empty. The view is remounted per path,
// so the value cannot go stale.
const name = route.params.name as string;

const volume = useVolume(() => name);
const deleteVolume = useDeleteVolume(() => name);

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
    <!-- Mutating controls exist only for a caller whose role covers
         them, so a viewer sees a read-only page rather than buttons
         that answer 403. -->
    <template #actions>
      <DeleteControl
        v-if="operator()"
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
