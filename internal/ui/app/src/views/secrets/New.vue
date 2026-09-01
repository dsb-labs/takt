<script setup lang="ts">
import { ref } from "vue";
import { useRouter } from "vue-router";

import { useSetSecret } from "../../api/mutations";
import DetailCard from "../../components/DetailCard.vue";
import ErrorBanner from "../../components/ErrorBanner.vue";
import ValueForm from "../../components/ValueForm.vue";

const router = useRouter();
const setSecret = useSetSecret();
const error = ref("");

async function save(name: string, value: string) {
  error.value = "";
  try {
    await setSecret.mutateAsync({ name, value });
    await router.push(`/secrets/${name}`);
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  }
}
</script>

<template>
  <div>
    <nav class="text-sm text-slate-500 dark:text-slate-400">
      <RouterLink to="/secrets" class="hover:underline">Secrets</RouterLink>
      <span class="mx-1">/</span>
      <span class="text-slate-900 dark:text-slate-100">new</span>
    </nav>

    <h1 class="mt-4 text-xl font-semibold">Set a secret</h1>

    <div class="mt-6 max-w-2xl">
      <DetailCard title="Secret">
        <div class="px-4 py-4">
          <ValueForm
            submit-label="Save"
            with-name
            :busy="setSecret.isPending.value"
            @submit="save"
          />
          <ErrorBanner v-if="error" :message="error" />
        </div>
      </DetailCard>
    </div>
  </div>
</template>
