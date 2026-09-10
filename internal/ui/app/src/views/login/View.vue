<script setup lang="ts">
import { ref } from "vue";
import { useRoute, useRouter } from "vue-router";

import { useLogin } from "../../api/mutations";
import { useOIDC } from "../../api/queries";
import { loadIdentity } from "../../auth";
import ErrorBanner from "../../components/ErrorBanner.vue";

const route = useRoute();
const router = useRouter();

const token = ref("");
const error = ref("");

const oidc = useOIDC();
const login = useLogin();

async function submit() {
  error.value = "";

  try {
    await login.mutateAsync(token.value);
    await loadIdentity();
    await router.push(String(route.query.next ?? "/"));
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : String(cause);
  }
}

function ssoLogin() {
  window.location.href = "/api/v1/auth/oidc/login";
}
</script>

<template>
  <div class="flex min-h-screen items-center justify-center p-4">
    <div
      class="w-full max-w-sm rounded-lg border border-slate-200 bg-white p-6 dark:border-slate-800 dark:bg-slate-900"
    >
      <div class="flex items-center gap-2.5">
        <img src="/takt.svg" alt="" class="h-7 w-7" />
        <span class="text-lg font-semibold tracking-tight">takt</span>
      </div>

      <p class="mt-4 text-sm text-slate-600 dark:text-slate-400">
        This server requires a credential.
      </p>

      <button
        v-if="oidc.data.value"
        class="bg-pulse-600 hover:bg-pulse-700 mt-4 w-full rounded-md px-3 py-2 text-sm font-medium text-white"
        @click="ssoLogin"
      >
        Sign in with SSO
      </button>

      <div
        v-if="oidc.data.value"
        class="mt-4 flex items-center gap-3 text-xs text-slate-400 dark:text-slate-500"
      >
        <span class="h-px flex-1 bg-slate-200 dark:bg-slate-800"></span>
        or paste a token
        <span class="h-px flex-1 bg-slate-200 dark:bg-slate-800"></span>
      </div>

      <form class="mt-4" @submit.prevent="submit">
        <label class="block text-sm font-medium" for="token">Token</label>
        <input
          id="token"
          v-model="token"
          type="password"
          autocomplete="off"
          placeholder="takt_c_…"
          class="focus:border-pulse-500 mt-1 w-full rounded-md border border-slate-300 bg-transparent px-3 py-2 font-mono text-sm focus:outline-none dark:border-slate-700"
        />

        <ErrorBanner v-if="error" :message="error" />

        <button
          :disabled="login.isPending.value || token === ''"
          type="submit"
          class="mt-4 w-full rounded-md border border-slate-300 px-3 py-2 text-sm font-medium hover:bg-slate-100 disabled:opacity-50 dark:border-slate-700 dark:hover:bg-slate-800"
        >
          Sign in
        </button>
      </form>
    </div>
  </div>
</template>
