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

That cuts both ways. What a runtime reports is not something orca validated, so a name
read back from a label or a record is treated as a value rather than as a path. The
directories the exec runtime keeps are named for the identifier orca assigned, and a
name is read from inside a record rather than from the directory holding it.

One thing a driver reports is not work it is doing. Replacing a workload means stopping
it and starting it again, which would destroy the output of the attempt being replaced —
so a driver keeps that attempt rather than removing it, and `orca workload logs
--previous` reads it.

Such an instance has ended and nothing will restart it, so it is reported as retained
and left out of every decision about what to run. Counted as an instance it would read
as a stale one to replace, as a failure to pace, or as work already present that needs
nothing done.

It is reported rather than hidden because one part of a pass does need to see it: a
workload deleted while the server was down leaves a retained instance behind, and an
instance absent from an observation would never be reaped. So the sweep that removes
work nothing asked for sees everything, and convergence sees only what is live.

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

## Volumes have a lifetime of their own

A volume is a resource rather than a field on a workload. It is created before the
workload that mounts it, and deleting that workload leaves it alone.

The alternative is storage scoped to a workload, which reads as simpler until deleting
a workload destroys data. Then every delete is a decision about data, and correcting a
typo in a manifest is one too. Removing stored data is instead something asked for
directly, and `orca volume delete` is the only thing that does it.

That is also why mounting a volume which does not exist is rejected rather than
creating one. A mistyped name would otherwise become a second empty volume, which reads
as success while the data the workload wanted sits under the name that was meant.

Deleting a volume a workload mounts is refused unless forced, and a workload being torn
down still counts as mounting it. Its work runs until the reconciler has stopped it, so
the data is still in use.

## A volume is a directory orca owns

A volume is a directory under the data directory, bind-mounted into a container and
symlinked into an exec workload's working directory.

Docker has named volumes, and using them for containers would work. But an exec process
runs on the host and needs a real path, so orca has to own a directory whatever it does
for containers. One mechanism means a volume means the same thing wherever it is
mounted, and orca can say where a volume's data actually is.

The cost is that the Docker daemon has to share the filesystem, so a volume cannot be
mounted into a container on a daemon reached over the network.

An exec workload gets a symlink rather than a mount because mounting needs privileges
orca does not have. It runs as an ordinary user, and a workload reads and writes through
a link perfectly well. The links are made fresh on every start, since stopping a
workload removes the directory holding the last set — the link is disposable, and the
volume it points at is not.

The mount path is written the same way for either runtime, so a workload moved between
them keeps its manifest. Where it resolves to cannot be: an exec workload reaches its
volume by the path taken as relative to the directory it runs in, because making the
absolute path resolve there would mean giving the process a filesystem root of its own,
which needs privileges orca has not got. Landlock restricts which paths a process may
reach, not what they resolve to, so it cannot stand in for that.

The path a volume resolves to is part of the stored specification, so it is covered by
the specification hash. A volume whose path changed therefore replaces the instances
bound to where it was, the same way a reallocated port does.

Nothing tells a workload where its volume is on the host. It does not need telling —
the path in its manifest is the path that works — and an exec workload that knew would
know it sits inside orca's data directory.

## Host ports are orca's to allocate

A container port that names no host port gets one from orca rather than from the
runtime. A workload's address is therefore known when it is applied rather than
discovered afterwards, and a driver whose runtime has no allocator of its own inherits
the behaviour.

An exec workload names its own host port, because the process binds one directly and
there is no mapping to make. orca records it so that no other workload is given it.

## A secret's revision is hashed, not its value

A workload is replaced when its specification hash changes. A secret a workload reads
is therefore part of what that hash covers, or rotating one would leave the old value
running until something else happened to change the workload.

The value cannot be what the hash covers. A hash is reported by the API, so a hash
computed over a value would let a guess at that value be tested offline. Instead each
secret carries a revision, which moves whenever its value moves and says nothing about
it. The revisions of the secrets a workload reads are mixed into that workload's hash.

The revisions are hashed without being stored. What goes into the database is the
specification alone, exactly as submitted, with the reference text still in it. A
resolved value there would be readable through the API, which echoes a specification
back.

A workload that reads no secret hashes exactly as it would if none of this existed.
That is deliberate rather than incidental: any other choice would replace every
running instance the first time an operator upgraded orca.

The revision is random rather than a counter. A counter would restart at one for a
secret deleted and created again, so a workload would hash the same as it did against
the value that is gone, and would keep running against a secret orca no longer holds.

## A variable's value is hashed, and a secret's is not

A variable reaches a workload's hash the same way a secret does, and for the same
reason: changing one has to replace the instances reading the old value. What reaches
it differs. A secret contributes a revision, and a variable contributes its value.

The reasoning that keeps a secret's value out of the hash does not apply to a
variable. A hash over a secret's value would let a guess at it be tested offline
against a hash the API reports. A variable's value is returned by that same API, so
there is nothing left for the indirection to protect, and a revision column would only
be a second thing to keep in step with the value.

Hashing the value is also better behaved. A variable deleted and created again with the
same value hashes as it did before, so the workloads reading it are left alone — which
is correct, because nothing they read has changed. A random revision would have
replaced them all. This is the one place where a secret's design is a compromise the
variable does not have to make.

Both contributions are omitted from the hash when there are none. A workload reading
neither hashes exactly as it would if none of this existed, and a workload reading only
secrets hashes exactly as it did before variables were added. Without that, adding this
feature would have replaced every running instance that reads a secret.

## A secret is decrypted as late as possible

The value is decrypted when a workload starts, and nowhere else. Nothing else asks:
the API is given an interface that cannot read one, and the type it reports has no
field to put one in.

Resolution therefore sits in the reconciler, immediately before the driver is handed
the environment. Starting a workload is also the only path to a driver, so one call
covers every way a workload comes to run.

It happens ahead of the start rather than in its error path. A workload that fails to
start gives up the host ports orca chose for it, because something outside orca may
have taken one. A secret that cannot be resolved has nothing to do with ports, and
moving a workload's address for that reason would be a change an operator could not
account for.

## The kernel confines an exec workload, and there is no opt-out

An exec workload runs as the same user as the server, so file permissions draw no
boundary around it. Without one it reads `secret.key`, reads the database, reads every
other workload's mounted plaintext, and writes to every volume. A container gets that
boundary from the runtime. An exec process had none.

Running workloads as separate users would be the conventional answer, and orca cannot
take it: allocating users and changing to them needs privileges orca deliberately does
not ask for. Landlock needs none. An unprivileged process applies a ruleset to itself,
and the kernel enforces it from then on.

It is applied by orca executing itself. A ruleset has to land after the fork, so it
restricts the workload rather than the server, and before the command runs, so nothing
runs unconfined. Go exposes no hook between the two, so the driver starts orca, that
process confines itself, and it then becomes the command. Executing a command keeps the
process identifier, so the record the driver wrote still describes the running workload
and adoption is unaffected.

Confinement is mandatory rather than best-effort. A ruleset that quietly did nothing on
some hosts would be a guarantee that could not be reasoned about, and every mention of
it in these documents would need qualifying. So a host whose kernel offers less refuses
to run exec workloads, and says so at startup. It still runs containers.

The cost is a kernel floor: Linux 6.2, which is where a read-only grant also stops a
file being truncated. Requiring less would mean a workload could empty a mounted value
it cannot rewrite, which is not what "read-only" should mean.

What confinement covers is filesystem paths, and `ptrace`, which Landlock scopes
between domains — without that, restricting paths alone could be defeated by attaching
to the server. It does not cover signals or the network. Those remain what running
several workloads as one user costs.
