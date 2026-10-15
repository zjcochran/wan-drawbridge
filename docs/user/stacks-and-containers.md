# Choosing what to stop

You decide what stops during a failover at two levels: whole **stacks** (Docker
Compose projects) and single **containers**, in the web UI or with a label in
your Compose files. This page explains the rules precisely, because the levels
interact.

## How Drawbridge identifies containers

Containers are re-created all the time (`docker compose up` after an image
update gives a container a new ID), so Drawbridge does not remember them by ID.
It uses a **key**:

- **Compose containers:** `project/service`, from the labels Compose sets
  (`com.docker.compose.project`, `com.docker.compose.service`). For example
  `media/player`. Scaled replicas of one service share a key and are treated as
  one.
- **Standalone containers** (`docker run`): the container name, for example
  `dns`.
- **Docker Swarm services** (through a [swarm agent](hosts.md#adding-a-docker-swarm)):
  `stack/service`, from the stack label (`com.docker.stack.namespace`), for
  example `media/db`; a service outside a stack uses its name. A stack of a
  swarm is a stack here, with the same switch and rules.

Keys are per **host**: the local Docker socket is the host `local`, and the
same project on another host is a different stack with its own settings.
Settings belong to the key, so they survive re-creation. A container removed
from its host disappears from the lists at once. Its row, with its setting, is
deleted once it has been missing for 10 minutes, long enough for a re-create
(`docker compose up` after a pull) to bring it back. A stack is deleted, with
its switch, when it has no containers left. Renaming a standalone container or a Compose project creates a new
key with default settings.

## The three container settings

| Setting | Shown as | Effect |
| --- | --- | --- |
| `inherit` | Follow label / Follow stack / Default (keep running) | Do what the container's label says, if it has one (see [Labels](#labels-in-your-compose-files)); otherwise what the stack says. A standalone container without a label keeps running. |
| `disable` | Stop on failover | Always stop, whatever the stack says. |
| `keep` | Keep running | Never stop, whatever the stack says. |

The **effective** setting, shown in the "On failover" column, is:

| Container setting | Stack switch on | Stack switch off / standalone |
| --- | --- | --- |
| Follow stack | **Stops** | Keeps running |
| Stop on failover | **Stops** | **Stops** |
| Keep running | Keeps running | Keeps running |

With a label, "Follow label" takes the label's value in both columns. The
"On failover" column says where the setting came from: own setting, from
label, from stack, or default.

New containers start as *Follow stack*.

## Labels in your Compose files

Instead of choosing in the UI, you can declare it on the container with the
label `drawbridge.on-failover`:

```yaml
services:
  downloader:
    image: example/downloader
    labels:
      drawbridge.on-failover: stop   # stop this container on failover
  player:
    image: example/player
    labels:
      drawbridge.on-failover: keep   # never stop it
```

- Values are `stop` and `keep` (case does not matter). Any other value is
  ignored, as if there were no label.
- A label is more specific than the stack switch, so it wins over it: a `keep`
  container keeps running in a stack that is switched on.
- A setting chosen for the container in the UI wins over the label. Set the
  container back to **Follow label** to use the label again.
- Labels are read on every inventory sync (every 30 seconds, and about 2
  seconds after a container is created), so a `docker compose up -d` with a
  changed label takes effect within seconds. Removing the label goes back to
  the stack's setting.
- A new container with a label is never flagged **new, not configured**: the
  label says what to do. Adding a label to a flagged container clears the
  flag.

Labels need no restart of Drawbridge and keep working after you rebuild or
move hosts, because they travel with your Compose files.

## Rule 1: the stack switch resets the stack

Turning a stack's switch **on** or **off** sets every container in that stack
back to *Follow stack*. This is deliberate: the switch always means "the whole
stack", so you never end up with forgotten exceptions.

> Example: `media` has containers `player`, `indexer` and `fetcher`, and `player` is
> set to *Keep running*. Turning `media` on resets `player` to *Follow stack*: all
> three stop on failover.

## Rule 2: overriding one container

Setting one container in a switched-on stack to **Keep running** converts the
stack rule into per-container settings, so that only that container changes:

1. The stack's switch turns off.
2. Every other container in the stack that was following the stack is set to
   *Stop on failover* individually.
3. The chosen container is set to *Keep running*.

The Containers page asks you to confirm this, naming how many other containers
will stay disabled individually. The Stacks page then shows the stack as
**Partial**, for example "Partial · 4 of 5".

> Example: `media` (switch on) has `player`, `db`, `indexer`, `fetcher` and
> `downloader`. You set `player` to *Keep running*. Result: the `media` switch
> is off; `db`, `indexer`, `fetcher` and `downloader` are *Stop on
> failover*; `player` is *Keep running*. Behaviour on failover is exactly what
> you asked: everything except `player` stops.

Setting a container to *Stop on failover* or *Follow stack* never converts the
stack; only *Keep running* in a switched-on stack does.

To go back to a plain stack rule, flip the stack's switch (Rule 1).

## New containers

When Drawbridge sees a container key for the first time:

- In a stack that has **no** per-container settings (every container follows
  the stack), the new container also follows the stack. If the stack is
  switched on, it will stop on failover like the rest.
- A container with a `drawbridge.on-failover` label follows its label and is
  never flagged.
- In a stack that **has** per-container settings (it was converted, or you set
  containers individually), Drawbridge cannot know what you want. The new
  container is set to *Follow stack*, which in a converted stack means it keeps
  running, and is flagged **new, not configured**. The dashboard shows a
  banner until you choose a setting for it (any choice clears the flag), or
  until you flip the stack switch.

New containers are found within 30 seconds, or about 2 seconds after Docker
reports a container being created, removed or renamed.

## Drawbridge never stops itself

Drawbridge skips its own container, recognised by the `drawbridge.self=true`
label or by its container ID. It does not appear in the lists, even if it is
part of a selected stack.

## Order of stopping and starting

Drawbridge reads Compose's `depends_on` (from the
`com.docker.compose.depends_on` label) within each stack, on each host;
hosts are stopped and started at the same time:

- **Stopping:** dependents first. If `player` depends on `db`, `player` stops
  before `db`.
- **Starting:** dependencies first. `db` starts, Drawbridge waits for its
  healthcheck to report healthy (up to 2 minutes; containers without a
  healthcheck count as ready immediately), then starts `player`.

Up to 4 containers are stopped or started at the same time within each level.
Docker is given 30 seconds to stop each container gracefully before it kills
it. Details are in [Container control](../technical/container-control.md).

## What is started again at failback

Only containers that **Drawbridge stopped and that were running when the
failover began**. A container you had stopped yourself before the outage stays
stopped. A container you start by hand during the outage is not touched again
until the next failover.

## Changing settings during a failover

Changes made in the web UI while in **Failover** take effect immediately:

- A container that now has to stop is stopped and added to the run, so it is
  started again at failback.
- A container that Drawbridge stopped and that should no longer stop is
  started at once.

Changes made with the [command line](cli.md) during a failover are saved but
not applied until the next failover or failback.

## Common setups

**Stop everything that uses a lot of bandwidth.** Switch on the download and
media stacks; leave infrastructure (DNS, reverse proxy, home automation)
alone.

**A stack except one service.** Switch the stack on, then set the exception to
*Keep running* (Rule 2), or give it the label `drawbridge.on-failover: keep`.

**Declared in Compose.** Put `drawbridge.on-failover: stop` on each service
that should stop, and leave every stack switch off. New services get decided
when you write them.

**A few standalone containers.** Set each to *Stop on failover* on the
Containers page; use the "Standalone only" filter and bulk edit.

**Everything except Drawbridge and the proxy.** Switch on every stack, then set
the proxy's container to *Keep running*. Drawbridge excludes itself
automatically.
