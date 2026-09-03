import type { components } from "./schema";

// Aliases for the generated schema types the views render, so a component
// imports a name rather than an index expression.
export type Workload = components["schemas"]["Workload"];
export type WorkloadState = components["schemas"]["WorkloadState"];
export type Instance = components["schemas"]["Instance"];
export type InstanceHealth = components["schemas"]["InstanceHealth"];
export type ResolvedPort = components["schemas"]["ResolvedPort"];
export type WorkloadSpec = components["schemas"]["WorkloadSpec"];
export type VolumeMount = components["schemas"]["VolumeMount"];
export type Secret = components["schemas"]["Secret"];
export type Variable = components["schemas"]["Variable"];
export type Volume = components["schemas"]["Volume"];
export type Service = components["schemas"]["Service"];
export type Readiness = components["schemas"]["GetReadinessResult"];
