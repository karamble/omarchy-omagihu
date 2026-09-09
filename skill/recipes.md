# omagihu alert recipes

Worked examples, one per operator, with the text each one delivers. Every
command assumes `omagihu-setup` resolves as described in
[`SKILL.md`](SKILL.md).

## appears: a review lands on you

The workhorse. A review request is a thing that arrives, and arriving is the
event.

```bash
omagihu-setup arm work.reviewRequests appears \
  --expires 7d \
  --reason "the release branch is blocked on whatever comes in" \
  --standing
```

Delivers, once per new pull request:

```
omagihu alarm t-1a2b3c4d: a new entry in work.reviewRequests: repo=karamble/omarchy-omagihu number=12 at 2026-09-09T14:12:51Z. Reason: "the release branch is blocked on whatever comes in". Armed by w7:p1 at 2026-09-09T12:10:00Z. This message carries no values; read omagihu yourself with the omagihu_* MCP tools. It stays armed until 2026-09-16T12:10:00Z; disarm with: omagihu-setup disarm t-1a2b3c4d.
```

Narrow it to one repository with a filter:

```bash
omagihu-setup arm work.reviewRequests appears \
  --where repo=karamble/omarchy-omagihu \
  --expires 7d --reason "..." --standing
```

## appears on facts: your branch went red where you are sitting

`facts` is the correlation layer: where GitHub and this machine disagree. A
`ci-red-on-head` fact means the checks failed on exactly the commit checked out
here, which is the one case worth interrupting for.

```bash
omagihu-setup arm facts appears \
  --where kind=ci-red-on-head \
  --expires 4d \
  --deliver repo \
  --reason "stop and read the failing job before pushing again" \
  --standing
```

`--deliver repo` sends it to whichever agent is working inside that checkout,
falling back to the desktop when nobody is.

The other kinds worth watching: `missing-work` (a pull request whose branch has
no checkout here), `changes-requested`, `stale-branch` (merged upstream, still
here), `fork-behind`.

## disappears: your pull request left the open list

```bash
omagihu-setup arm work.authoredPrs disappears \
  --where repo=karamble/omarchy-omagihu \
  --expires 7d \
  --reason "merged or closed: tidy the local branch"
```

Delivers:

```
omagihu alarm t-2b3c4d5e: an entry left work.authoredPrs where repo=karamble/omarchy-omagihu: repo=karamble/omarchy-omagihu number=9 at ... This was a one-shot and is now spent.
```

## count: the backlog got past what you can hold

`count` is `crosses` over the size of a filtered list.

```bash
omagihu-setup arm inbox count \
  --above 25 \
  --expires 7d \
  --reason "triage the notification backlog before it stops being read"
```

It re-arms once the count is back below 24, so a number hovering on 25 rings
once rather than every pass.

## crosses: work is piling up locally

```bash
omagihu-setup arm attention.unpushedTotal crosses \
  --above 50 \
  --expires 7d \
  --reason "push before the machine becomes the only copy"
```

And the one worth having permanently, on the budget rather than the work:

```bash
omagihu-setup arm health.rateLeft crosses \
  --below 500 \
  --expires 30d \
  --reason "the GitHub rate budget is running down; check what is polling"
```

## becomes: the bar went urgent

```bash
omagihu-setup arm attention.level becomes \
  --value urgent \
  --hold 15m \
  --expires 7d \
  --reason "something needs answering now"
```

`--hold` is what stops a flapping status ringing repeatedly: the level has to
have been away from `urgent` for fifteen minutes before it can ring again.

The same operator watches the daemon itself:

```bash
omagihu-setup arm health.monitoring becomes --value false \
  --expires 30d --reason "polling stopped; alerts are blind until it is back"
```

## ages: a pull request went quiet

The one that cannot be expressed as a count. Staleness is a property of one
entry, so `ages` measures the entry, not the list.

```bash
omagihu-setup arm work.authoredPrs ages \
  --older-than 7d \
  --field updatedAt \
  --expires 30d \
  --reason "nudge the reviewer or close it" \
  --standing
```

Delivers, once per pull request as it goes past a week:

```
omagihu alarm t-3c4d5e6f: an entry in work.authoredPrs has been untouched for 7d: repo=karamble/omarchy-omagihu number=12 at ...
```

Waiting on your own review queue instead:

```bash
omagihu-setup arm work.reviewRequests ages \
  --older-than 2d --expires 30d \
  --reason "somebody has been waiting on me for two days" --standing
```

## Editing rather than re-arming

Move a bound, extend an expiry, change who gets woken. An edit keeps the id and
the ownership, and clears what the trigger had learned, so the next sample
teaches it again exactly as arming did.

```bash
omagihu-setup edit t-1a2b3c4d --above 75 --expires 14d --json
omagihu-setup edit t-1a2b3c4d --deliver you
```

Never edit a trigger whose `armedBy` is not you.

## Checking your work

```bash
omagihu-setup alerts
omagihu-setup arm inbox count --above 25 --expires 7d --reason "..." --dry-run
```

`--dry-run` validates against the catalogue and prints what would be stored,
without an id, because nothing was created.
