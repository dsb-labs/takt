<script setup lang="ts">
import { usePolicy, useTokens } from "../../api/queries";
import DetailCard from "../../components/DetailCard.vue";
import ErrorBanner from "../../components/ErrorBanner.vue";
import YamlView from "../../components/YamlView.vue";
import { absoluteTime, relativeTime } from "../../format";

const policy = usePolicy();
const tokens = useTokens();

// Awaited so Suspense holds the previous view until this one has its data. A
// failure is left for the error banner this view already renders.
await Promise.all([
  policy.suspense().catch(() => {}),
  tokens.suspense().catch(() => {}),
]);
</script>

<template>
  <div>
    <h1 class="text-xl font-semibold">Access</h1>

    <ErrorBanner
      v-if="policy.isError.value"
      :message="`Failed to read the policy: ${policy.error.value?.message}`"
    />
    <ErrorBanner
      v-if="tokens.isError.value"
      :message="`Failed to list credentials: ${tokens.error.value?.message}`"
    />

    <div v-if="policy.data.value" class="mt-6">
      <DetailCard title="Policy">
        <YamlView :document="policy.data.value" />
      </DetailCard>
    </div>

    <div v-if="tokens.data.value" class="mt-6">
      <DetailCard title="Credentials">
        <table class="w-full text-left text-sm">
          <thead
            class="border-b border-slate-100 text-xs text-slate-500 dark:border-slate-800 dark:text-slate-400"
          >
            <tr>
              <th class="px-4 py-2 font-medium">Principal</th>
              <th class="hidden px-4 py-2 font-medium sm:table-cell">Source</th>
              <th class="hidden px-4 py-2 font-medium md:table-cell">
                Created
              </th>
              <th class="px-4 py-2 font-medium">Last used</th>
              <th class="hidden px-4 py-2 font-medium sm:table-cell">
                Expires
              </th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-100 dark:divide-slate-800/50">
            <tr v-for="token in tokens.data.value" :key="token.id">
              <td class="px-4 py-2 font-mono">
                {{ token.type === "recovery" ? "(recovery)" : token.principal }}
              </td>
              <td class="hidden px-4 py-2 sm:table-cell">
                {{ token.source }}
              </td>
              <td
                class="hidden px-4 py-2 md:table-cell"
                :title="absoluteTime(token.createdAt)"
              >
                {{ relativeTime(token.createdAt) }}
              </td>
              <td
                class="px-4 py-2"
                :title="
                  token.lastUsedAt ? absoluteTime(token.lastUsedAt) : undefined
                "
              >
                {{
                  token.lastUsedAt ? relativeTime(token.lastUsedAt) : "never"
                }}
              </td>
              <td class="hidden px-4 py-2 sm:table-cell">
                {{ token.expiresAt ? relativeTime(token.expiresAt) : "never" }}
              </td>
            </tr>
            <tr v-if="tokens.data.value.length === 0">
              <td
                colspan="5"
                class="px-4 py-6 text-center text-slate-500 dark:text-slate-400"
              >
                No credentials.
              </td>
            </tr>
          </tbody>
        </table>
      </DetailCard>
    </div>
  </div>
</template>
