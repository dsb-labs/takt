<script setup lang="ts">
import { useNode } from "@/api/queries";
import CapacityBar from "@/components/CapacityBar.vue";
import DetailCard from "@/components/DetailCard.vue";
import ErrorBanner from "@/components/ErrorBanner.vue";
import OverviewRow from "@/components/OverviewRow.vue";
import {
  absoluteTime,
  bytes,
  cores,
  percent,
  pluralize,
  relativeTime,
  usageStyles,
} from "@/lib/format";

const node = useNode();

// Awaited so Suspense holds the previous view until this one has its data.
// A failure is left for the error banner this view already renders.
await node.suspense().catch(() => {});
</script>

<template>
  <div>
    <ErrorBanner
      v-if="node.isError.value"
      :message="`Failed to read the node: ${node.error.value?.message}`"
    />

    <template v-if="node.data.value">
      <div class="grid gap-6 xl:grid-cols-2">
        <DetailCard title="Memory" class="min-w-0">
          <div class="px-4 pt-4 pb-2">
            <CapacityBar
              :value="node.data.value.allocated.memory"
              :total="node.data.value.memory.total"
              verb="allocated"
              :format="bytes"
            />
          </div>
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <OverviewRow label="Total">{{
              bytes(node.data.value.memory.total)
            }}</OverviewRow>
            <OverviewRow label="Allocated">
              <span
                :class="
                  usageStyles(
                    node.data.value.allocated.memory,
                    node.data.value.memory.total,
                  )
                "
                >{{ bytes(node.data.value.allocated.memory) }}</span
              >
              <span
                v-if="node.data.value.allocated.unlimitedMemory"
                class="ml-2 text-slate-500 dark:text-slate-400"
                >{{
                  pluralize(
                    node.data.value.allocated.unlimitedMemory,
                    "instance",
                  )
                }}
                with no limit</span
              >
            </OverviewRow>
            <OverviewRow
              label="Host used"
              title="Everything on the box, not only takt's workloads"
            >
              {{ bytes(node.data.value.memory.used) }}
              <span class="text-slate-500 dark:text-slate-400"
                >({{
                  percent(
                    node.data.value.memory.used,
                    node.data.value.memory.total,
                  )
                }})</span
              >
            </OverviewRow>
          </dl>
        </DetailCard>

        <DetailCard title="CPU" class="min-w-0">
          <div class="px-4 pt-4 pb-2">
            <CapacityBar
              :value="node.data.value.allocated.cpu"
              :total="node.data.value.cpus"
              verb="allocated"
              :format="cores"
            />
          </div>
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <OverviewRow label="Processors">{{
              node.data.value.cpus
            }}</OverviewRow>
            <OverviewRow label="Allocated">
              <span
                :class="
                  usageStyles(
                    node.data.value.allocated.cpu,
                    node.data.value.cpus,
                  )
                "
                >{{ cores(node.data.value.allocated.cpu) }}</span
              >
              <span
                v-if="node.data.value.allocated.unlimitedCpu"
                class="ml-2 text-slate-500 dark:text-slate-400"
                >{{
                  pluralize(node.data.value.allocated.unlimitedCpu, "instance")
                }}
                with no limit</span
              >
            </OverviewRow>
          </dl>
        </DetailCard>

        <DetailCard title="Disk" class="min-w-0">
          <!-- Used against total, since nothing allocates disk: a volume
               fills what it likes, which is the reason to watch it. -->
          <div class="space-y-4 px-4 pt-4 pb-2">
            <div>
              <p class="mb-1.5 text-xs text-slate-500 dark:text-slate-400">
                Data directory
                <span
                  class="ml-1 font-mono"
                  :title="node.data.value.disks.data.path"
                  >{{ node.data.value.disks.data.path }}</span
                >
              </p>
              <CapacityBar
                :value="
                  node.data.value.disks.data.total -
                  node.data.value.disks.data.free
                "
                :total="node.data.value.disks.data.total"
                verb="used"
                :format="bytes"
              />
            </div>
            <div>
              <p class="mb-1.5 text-xs text-slate-500 dark:text-slate-400">
                Volumes
                <span
                  class="ml-1 font-mono"
                  :title="node.data.value.disks.volumes.path"
                  >{{ node.data.value.disks.volumes.path }}</span
                >
              </p>
              <CapacityBar
                :value="
                  node.data.value.disks.volumes.total -
                  node.data.value.disks.volumes.free
                "
                :total="node.data.value.disks.volumes.total"
                verb="used"
                :format="bytes"
              />
            </div>
          </div>
        </DetailCard>

        <DetailCard title="Identity" class="min-w-0">
          <dl
            class="divide-y divide-slate-100 text-sm dark:divide-slate-800/50"
          >
            <OverviewRow label="Hostname" mono>{{
              node.data.value.hostname
            }}</OverviewRow>
            <OverviewRow label="System" mono
              >{{ node.data.value.os }}/{{ node.data.value.arch }}</OverviewRow
            >
            <OverviewRow label="Kernel" mono>{{
              node.data.value.kernel
            }}</OverviewRow>
            <OverviewRow label="Version" mono>{{
              node.data.value.version
            }}</OverviewRow>
            <OverviewRow
              label="Started"
              :title="absoluteTime(node.data.value.startedAt)"
              >{{ relativeTime(node.data.value.startedAt) }}</OverviewRow
            >
            <OverviewRow
              label="Booted"
              :title="absoluteTime(node.data.value.bootedAt)"
              >{{ relativeTime(node.data.value.bootedAt) }}</OverviewRow
            >
          </dl>
        </DetailCard>
      </div>
    </template>
  </div>
</template>
