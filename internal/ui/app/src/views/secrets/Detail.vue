<script setup lang="ts">
import { ref } from "vue";
import { useRoute, useRouter } from "vue-router";

import { useDeleteSecret, useSetSecret } from "../../api/mutations";
import { useSecret } from "../../api/queries";
import DeleteControl from "../../components/DeleteControl.vue";
import DetailCard from "../../components/DetailCard.vue";
import DetailPage from "../../components/DetailPage.vue";
import LabelsCard from "../../components/LabelsCard.vue";
import ErrorBanner from "../../components/ErrorBanner.vue";
import UsedByCard from "../../components/UsedByCard.vue";
import ValueForm from "../../components/ValueForm.vue";
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

const secret = useSecret(() => name);
const setSecret = useSetSecret();
const deleteSecret = useDeleteSecret(() => name);

const rotateError = ref("");
const rotated = ref(false);

async function rotate(_: string, value: string) {
  rotateError.value = "";
  rotated.value = false;
  try {
    await setSecret.mutateAsync({ name: name, value });
    rotated.value = true;
  } catch (cause) {
    rotateError.value = cause instanceof Error ? cause.message : String(cause);
  }
}

// Awaited so Suspense holds the previous view until this one has its
// data. A failure is left for the error banner this view already renders.
await secret.suspense().catch(() => {});
</script>

<template>
  <DetailPage
    section="Secrets"
    section-to="/secrets"
    :name="name"
    :error="
      secret.isError.value
        ? `Failed to read the secret: ${secret.error.value?.message}`
        : ''
    "
  >
    <!-- Mutating controls exist only for a caller whose role covers
         them, so a viewer sees a read-only page rather than buttons
         that answer 403. -->
    <template #actions>
      <DeleteControl
        v-if="operator()"
        :subject="`the secret ${name}`"
        :remove="(force) => deleteSecret.mutateAsync(force)"
        @deleted="router.push('/secrets')"
      />
    </template>

    <template v-if="secret.data.value">
      <div class="mt-6 grid gap-6 xl:grid-cols-2">
        <DetailCard title="Overview">
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <OverviewRow label="Revision" mono>{{
              secret.data.value.revision
            }}</OverviewRow>
            <OverviewRow
              label="Created"
              :title="absoluteTime(secret.data.value.createdAt)"
              >{{ relativeTime(secret.data.value.createdAt) }}</OverviewRow
            >
            <OverviewRow
              label="Updated"
              :title="absoluteTime(secret.data.value.updatedAt)"
              >{{ relativeTime(secret.data.value.updatedAt) }}</OverviewRow
            >
          </dl>
        </DetailCard>

        <UsedByCard :used-by="secret.data.value.usedBy" />

        <LabelsCard :labels="secret.data.value.labels" target="/secrets" />
      </div>

      <div v-if="operator()" class="mt-6">
        <DetailCard title="Rotate">
          <div class="px-4 py-4">
            <ValueForm
              submit-label="Rotate"
              placeholder="The new value. The current one is never shown."
              :busy="setSecret.isPending.value"
              @submit="rotate"
            />
            <ErrorBanner v-if="rotateError" :message="rotateError" />
            <p
              v-if="rotated"
              class="mt-3 text-sm text-emerald-700 dark:text-emerald-400"
            >
              Rotated.
            </p>
          </div>
        </DetailCard>
      </div>
    </template>
  </DetailPage>
</template>
