import createClient from "openapi-fetch";

import type { paths } from "./schema";

// The UI is served from the same listener as the API, so requests are relative
// and whatever address reached the page reaches the API.
export const client = createClient<paths>({ baseUrl: "/" });
