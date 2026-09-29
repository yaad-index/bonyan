# ADR 0000: Record architecture decisions

**Status:** Accepted, amended (maintainer sign-off recorded by approval of the PR that sets this status and of each PR that amends it; see Amendments)

## Context

bonyan is a library other programs build on, so its interfaces and guarantees change slowly and deliberately. The reasoning behind each significant decision needs to stay readable after the discussion that produced it is gone.

Before the first release no program depends on bonyan, and building a part can show that its ADR missed something. Superseding a whole ADR to correct one section would leave the current decision spread across a chain of files while nothing yet relies on the old text.

## Decision

Significant decisions are recorded as Architecture Decision Records: numbered Markdown files in `adr/`, each with Status, Context, Decision and Consequences. Read the ADR governing an area before changing that area.

**Until the first release**, meaning the first version the release process publishes, an Accepted ADR may be amended in place by a pull request the maintainer approves. This includes amendments made during implementation, when building a part shows something the planning missed. An amended ADR keeps its number, its Status says it was amended, and an Amendments section at its end lists what changed. The earlier text stays in version control.

**From the first release on**, an Accepted ADR is not changed; a later ADR supersedes it.

## Consequences

Decisions are reviewable when they are made and traceable afterwards. Changing one is itself a recorded act, whether it is an amendment or a superseding ADR.

Before the first release a reader finds the current decision in one file. The cost is that the file alone no longer shows what an amended decision used to say: the Amendments section says what changed, and version control holds the earlier text. From the first release on, programs depend on the decisions, so a change goes through supersession and the old ADR stays readable as it was.

## Amendments

- The rule allowing in-place amendment before the first release, with the maintainer's approval, was added; supersession applies from the first release on.
