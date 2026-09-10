import { ref } from "vue";

import { client } from "./api/client";
import type { components } from "./api/schema";

export type Identity = components["schemas"]["GetAuthResult"];

// The identity the server resolved the session to: null before the first
// load and after a sign-out, which the router guard reads as "go to the
// login page". A server with authentication disabled still answers, as the
// anonymous admin, so the rest of the app runs one code path either way.
//
// A ref at the module root rather than a query hook, because the router
// guard reads it outside any component and the role helpers below need a
// synchronous answer while a view renders.
export const identity = ref<Identity | null>(null);

// loadIdentity asks the server who the caller is. A refusal reads as "not
// signed in" rather than an error, because that is what it means: the
// session expired, the token was revoked, or there never was one.
export async function loadIdentity(): Promise<Identity | null> {
  try {
    const { data } = await client.GET("/api/v1/auth");
    identity.value = data ?? null;
  } catch {
    identity.value = null;
  }
  return identity.value;
}

// forgetIdentity is the local half of signing out: the server-side
// revocation is useLogout's job, and forgetting the answer here is what
// makes the next navigation land on the login page.
export function forgetIdentity() {
  identity.value = null;
}

// operator reports whether the caller may drive resource lifecycle, which is
// what the views showing mutating controls branch on. Everything is
// permitted when authentication is disabled, and the recovery token sits
// above policy.
export function operator(): boolean {
  const id = identity.value;
  if (!id) return false;
  return (
    !id.enabled || id.recovery || id.role === "operator" || id.role === "admin"
  );
}

// admin reports whether the caller may read the policy and manage tokens.
export function admin(): boolean {
  const id = identity.value;
  if (!id) return false;
  return !id.enabled || id.recovery || id.role === "admin";
}
