# Design

## Desired state and observed state

orca stores what you asked for. What is actually running is observed from the runtime
when something asks, and the two are merged on read.

Nothing about the running state is persisted, so nothing persisted can go stale
against reality. A restarted server has no bookkeeping of its own to recover. It asks
the runtimes what they have and compares that against the database.

This is why ownership lives in the runtime rather than in the database. A container
carries labels naming its workload, the specification hash it was created from, and
its version. An exec workload has a record in its own directory.

Either way the runtime is the source of truth about what is running. orca stores no
identifier that could point at something gone.

## Reconciliation

A pass reads the full desired state, asks every driver what it is running, and acts on
the difference:

1. A workload with nothing running is started.
2. An instance whose specification hash differs from the stored one is replaced.
3. An instance that ended is restarted, subject to the workload's restart policy.
4. An instance nothing asked for is stopped.
5. A workload marked for deletion is torn down, and its row removed once nothing is
   left running for it.

The pass is level-triggered. It re-derives everything each time rather than responding
to changes, so a missed event costs responsiveness and never correctness. A pass that
fails part way leaves the node in a state the next pass acts on.

Passes run on a ticker, when a driver reports a change, and when a workload is applied
or deleted. The ticker guarantees convergence and the other two make it prompt. Passes
never overlap, so a burst of events collapses into one pass rather than racing.

Workloads inside a pass are converged several at a time. They are independent of one
another, and most of what converging one costs is waiting. Handling them in turn made
the slowest workload the rate at which any of them could be handled.

## Replacing rather than mutating

A specification change replaces the instance running the old one. Docker cannot change
most of a container's configuration in place, and reasoning about which fields can be
mutated is more subtle than starting again.

The specification hash recorded on an instance is what identifies it as outdated. That
hash covers the resolved specification, host ports included, so a reallocated port reads
as an ordinary change and replaces the instance bound to the old one.

## The schedule outranks the restart policy

A workload can say both when to run and what to do when a run ends. Those can disagree,
so one has to win, and it is the schedule.

An occurrence coming due starts the workload whatever the last run did. The restart
policy applies only between occurrences, where it retries a run that failed. A failed
occurrence did not achieve what it asked for, so trying again before the next one is
due is worth doing.

A run that ended cleanly is not restarted between occurrences, whatever the policy
says. Starting it again would run the workload at a time its schedule does not name,
which is the thing a schedule exists to prevent.

Nothing about when a workload ran is stored. The occurrence is derived from the
expression and the time the last instance started, which the runtime already reports.
A container that has ended is left in place until the next occurrence replaces it, and
a workload that has not run yet counts from when its specification was applied.

Occurrences missed while the server was down are missed. The occurrence orca runs is
the first after the last run, so a workload down for several does not run once for
each. For a nightly job down a week, that is the difference between one run and seven.

## orca owns restarts

Neither runtime is asked to restart anything. Docker's restart policy is left unset and
an exec process is not respawned by anything but orca.

Restarting is therefore visible in the workload's reported state. It is paced by orca's
own backoff, which widens after each consecutive failure, so a workload that cannot
start does not spin the daemon.

A workload that starts and exits at once is paced the same way. Such a container is
briefly observed as running, so the backoff clears only once an instance has stayed
up.

## Health is orca's question

Whether a workload is working is different from whether its runtime says it started. A
process listening and answering errors looks healthy to Docker, and Docker's own health
support reports a verdict without acting on it.

orca performs the check itself, from the host, against the port the workload
publishes. An image carrying no shell can still be checked. A driver inherits the
behaviour by publishing an address rather than implementing checks of its own.

A failing check makes the instance failed, which routes it into the same paced restart a
crashed one takes. The reaction to "not working" is the same whether the process died or
merely stopped answering.

## Only the reconciler touches the runtime

Deleting a workload records the intent and returns. The reconciler performs the
teardown and removes the desired state last.

Nothing races the reconciler for the same containers. A failure part way through
leaves a workload that gets torn down again, rather than containers nothing records.

## The driver boundary

A driver turns a workload into running work, reports what it has, and stops it again.
The reconciler and the service each define the part of that they use, and the drivers
satisfy both.

Which driver runs a workload comes from the runtime block its manifest names. Each
driver declares its own name and the server maps one onto the other, so a driver
identifies itself without knowing anything about the wire format.

A workload whose runtime has no driver is stored and left alone rather than reported as
broken.

## Specifications are queryable

Labels and specifications are stored as SQLite JSONB, so a filter runs in the database
rather than by loading every workload and discarding most of them. Storing JSONB also
means a malformed specification is rejected as it is written rather than when something
later tries to read it.

## Host ports are orca's to allocate

A container port that names no host port gets one from orca rather than from the
runtime. A workload's address is therefore known when it is applied rather than
discovered afterwards, and a driver whose runtime has no allocator of its own inherits
the behaviour.

An exec workload names its own host port, because the process binds one directly and
there is no mapping to make. orca records it so that no other workload is given it.
