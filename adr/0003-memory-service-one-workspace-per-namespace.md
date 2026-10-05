# ADR 0003: The memory service backend keeps one workspace per namespace

**Status:** Accepted (maintainer sign-off recorded by approval of the PR that adds it, per ADR 0000)

## Context

The memory service backend (`memory/honcho`, ADR 0001 §4) was released keeping one workspace per subject: a workspace named by the namespace and the subject, holding a peer for the user and one for the program. Deleting a subject deleted its workspace. That was chosen because the service has no call to delete a peer or a single message, and deleting the workspace removed everything in it.

Two things make that layout the wrong one:

- **A workspace per user does not scale.** The number of workspaces grows with the number of users of every program on the service. One workspace per namespace is the intended shape.
- **It needs a token that reaches every workspace.** Writing for a new user creates a workspace, and deleting what retention expired lists every workspace on the service to find the namespace's. A token the service scopes to one workspace can do neither. Such a token covers none of the per-user workspaces either, so writes, recall and erasure all fail with it.

The service can delete a workspace, a session and a single derived fact (a conclusion); it can set a peer's card and metadata. It still cannot delete a peer or a single message. So a user can no longer be erased by deleting a workspace, and erasing a user inside a shared workspace leaves the user's peer behind.

This changes the backend's storage layout and its erasure guarantee, after its first release, so it is decided here rather than by amending ADR 0001 (ADR 0000).

## Decision

### 1. Layout

- A Backend keeps all of its namespace's memory in **one workspace**. By default it is named by the prefix and the namespace (`<prefix>--<hex of the namespace>`). An option lets the program name the workspace instead, for a service where the operator creates the workspace and scopes a token to it.
- The workspace records the namespace it holds in its metadata. A Backend refuses to read, write or delete in a workspace that records another namespace, so one workspace never holds two namespaces and everything it does across users stays inside its own namespace (ADR 0001 §4).
- **Each user is a peer** in that workspace, its id the hex of the subject as bonyan passes it to the backend: already qualified by the namespace (ADR 0001 §4).
- **Each user's sessions carry the user's peer id** in their names, so a session belongs to exactly one user. A session holds the user's peer and the program's peer and nobody else.
- The program has **one peer** in the workspace. It is never observed and observes nobody, set explicitly on every session it joins, so the service derives nothing from one user's messages into a peer that other users share.
- Records, generations of sessions and the reserved facts session keep the form they have in the released version, inside the user's sessions.

### 2. Recall and isolation

Recall must never return one user's records or derived facts for another. The backend reads only:

- **the user's own sessions**, found by the user's peer id in their names; and
- **conclusions filtered to the user's peer**, as observer and observed.

The service applies those filters. The backend checks every result again on read and drops a message from a session that is not the user's, or a conclusion whose observer or observed peer is not the user's. The workspace-wide search and anything else that reads across peers is never used for recall.

### 3. Erasing a user

Deleting a subject removes everything the service holds for the user, except the peer itself:

- each of the user's sessions, with their messages and summaries;
- every conclusion the user's peer holds as observer or observed;
- the user's peer card, and the program's card about the user, both set empty;
- the user's peer metadata, set empty.

The service derives facts asynchronously, so a fact can be formed from a message after the erase began. After deleting, the backend waits until the service's queue holds no work for the user's peer, then deletes the user's conclusions again. Erasing returns only after that second pass. If the queue does not drain within the wait, erasing fails and says so, and running it again finishes it.

**Known residue: the peer stays.** The service cannot delete a peer, so an erased user's peer remains in the workspace: an empty record with no sessions, conclusions, card or metadata, whose id is the hex of the subject. It shows only that the subject once had memory in this namespace. The backend's documentation states this. A returning user reuses the empty peer and starts with nothing.

The conformance suite and the backend's own tests against a running service show:
- after deleting a subject, nothing written for it and no fact derived about it remains;
- deleting one subject leaves another subject's records and facts in the same workspace intact;
- recall for one subject never returns another's.

### 4. Retention

Deleting what retention expired works inside the one workspace. It lists the workspace's sessions and conclusions and never lists workspaces. Sessions are rewritten by generation as in the released version, under the lock of the user they belong to, so a purge never needs access beyond the namespace's workspace.

### 5. Access

Every call the backend makes names its one workspace. A token scoped to that workspace is enough for writes, recall, erasure and retention, provided it can also create the workspace when it is missing. Where the token cannot create it, the operator creates the workspace before the program starts, and the program names it with the option above.

### 6. Migration

- The new layout does not read workspaces in the old one. Their names begin with the new default workspace's name followed by `--`, so they never collide with it, but the new version never lists or opens them.
- A program that stored memory with the old layout **deletes it before upgrading**, using the old version: deleting each subject deletes its workspace; deleting everything written before a time after the last write empties every workspace but leaves them, with their peers. A program could store memory in the old layout only with a token that created workspaces, but that is not proof it may delete them or delete the derived facts in them. Where it may not, the operator deletes the old workspaces with a token that may, and checks that none remain before upgrading. The release notes of the version that changes the layout say so.
- Memory is not moved from the old layout to the new one. A migration would have to copy messages with their times and metadata, and lose the derived facts, which are formed again only from new messages.
- This is a breaking change to the backend module, released as a new minor version before 1.0 (ADR 0001, Consequences).

## Consequences

- One workspace per namespace, however many users it has, reachable with a token scoped to that workspace.
- **Erasing a user is no longer the deletion of a container**, but a sequence of deletions the backend must keep complete as the service changes. A future kind of per-peer data on the service is not covered until the backend deletes it too. The tests against a running service are where that shows.
- An erased user leaves an empty peer named by the subject's hex. Where the subject is itself identifying, the namespace's workspace keeps that identifier after erasure. That is accepted.
- Isolation between users rests on filtering inside a shared workspace, enforced by the service and checked again by the backend, rather than on separate workspaces.
- Programs with memory in the old layout must delete it before upgrading. Nothing is migrated.
