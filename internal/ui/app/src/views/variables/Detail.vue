<script setup lang="ts">
import { ref } from "vue";
import { useRoute, useRouter } from "vue-router";

import { useDeleteVariable, useSetVariable } from "../../api/mutations";
import { useVariable } from "../../api/queries";
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

const variable = useVariable(() => name);
const setVariable = useSetVariable();
const deleteVariable = useDeleteVariable(() => name);

const editError = ref("");
const saved = ref(false);

async function save(_: string, value: string) {
  editError.value = "";
  saved.value = false;
  try {
    await setVariable.mutateAsync({ name: name, value });
    saved.value = true;
  } catch (cause) {
    editError.value = cause instanceof Error ? cause.message : String(cause);
  }
}

// Awaited so Suspense holds the previous view until this one has its
// data. A failure is left for the error banner this view already renders.
await variable.suspense().catch(() => {});
</script>

<template>
  <DetailPage
    section="Variables"
    section-to="/variables"
    :name="name"
    :error="
      variable.isError.value
        ? `Failed to read the variable: ${variable.error.value?.message}`
        : ''
    "
  >
    <!-- Mutating controls exist only for a caller whose role covers
         them, so a viewer sees a read-only page rather than buttons
         that answer 403. -->
    <template #actions>
      <DeleteControl
        v-if="operator()"
        :subject="`the variable ${name}`"
        :remove="(force) => deleteVariable.mutateAsync(force)"
        @deleted="router.push('/variables')"
      />
    </template>

    <template v-if="variable.data.value">
      <div class="mt-6 grid gap-6 xl:grid-cols-2">
        <DetailCard title="Overview">
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <OverviewRow
              label="Created"
              :title="absoluteTime(variable.data.value.createdAt)"
              >{{ relativeTime(variable.data.value.createdAt) }}</OverviewRow
            >
            <OverviewRow
              label="Updated"
              :title="absoluteTime(variable.data.value.updatedAt)"
              >{{ relativeTime(variable.data.value.updatedAt) }}</OverviewRow
            >
          </dl>
        </DetailCard>

        <UsedByCard :used-by="variable.data.value.usedBy" />

        <LabelsCard :labels="variable.data.value.labels" target="/variables" />
      </div>

      <div v-if="operator()" class="mt-6">
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
  </DetailPage>
</template>
