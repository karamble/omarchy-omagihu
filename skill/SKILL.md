---
name: omagihu-alerts
description: "Arm, list, edit and disarm omagihu alerts: watches on the GitHub and local-checkout state omagihu already tracks (review requests, your open and merged pull requests, assigned issues, unread notifications, CI verdicts, unpushed and dirty checkouts, and the facts where GitHub and this machine disagree) that wake this agent through herdr, or notify the user, when a condition trips. Use when the user asks to be told, woken, alerted or notified once something happens on GitHub or in a checkout: a review lands, a pull request goes red, an issue is assigned, a branch goes stale, work stops being pushed. Do not use to read the current state (use the omagihu_* MCP tools), for timers or reminders unrelated to omagihu data, or to edit ~/.config/omagihu/triggers.json by hand."
---

# omagihu alerts

An omagihu alert is a standing question put to the daemon: ring me when this
happens, then stop bothering me. Every pass folds the fresh snapshot through the
triggers an agent armed and rings when a condition trips. It never answers a
data question, never acts on anything, and the wake-up carries no values. It
rings by handing the alarm to herdr, retrying for 60 seconds while that agent is
blocked on a dialog, and otherwise by raising a desktop notification.

## Find the tool

`omagihu-setup` is not on PATH on a normal install. Use `omagihu-setup` if
`command -v omagihu-setup` finds it, otherwise
`~/.config/omarchy/plugins/karamble.omagihu/bin/omagihu-setup`. Every example
here writes plain `omagihu-setup`.

If `HERDR_PANE_ID` is set you are in a herdr pane: `arm` records that pane as
`armedBy`, and alarms come back to this pane unless you pass `--deliver`.
Outside a pane, `armedBy` is `you` and alarms go to the user's desktop.

## Before arming

- Run `omagihu-setup alerts --json` first. Do not arm a trigger that duplicates
  one already there on the same path, operator and params; edit yours instead.
- Never disarm or edit a trigger whose `armedBy` is not you. Tell the user it
  exists and who armed it.
- Read the warnings `arm` prints on stderr and relay them. "monitoring is off"
  means the trigger is stored but nothing is sampling it until the user switches
  monitoring back on in the panel. The trigger stays armed either way.
- Write `--reason` as an instruction to your future self. The alarm carries the
  reason and nothing else about why you cared, so "the release branch is
  blocked on this review" beats "PR alert".

## Syntax

```bash
omagihu-setup catalogue
omagihu-setup alerts --json
omagihu-setup arm work.reviewRequests appears --expires 7d --reason "..." --json
omagihu-setup arm facts appears --where kind=ci-red-on-head --expires 4d --standing
omagihu-setup edit t-1a2b3c4d --expires 2d --json
omagihu-setup disarm t-1a2b3c4d
```

Params by operator:

- `crosses` and `count`: `--above X` or `--below X`, one of them and not both,
  with `--rearm R` to widen the re-arm margin.
- `becomes`: `--value V`, with `--hold 10m` to set how long the value must stay
  away before it can ring again (default 5m).
- `ages`: `--older-than 7d`, with `--field updatedAt` to choose which timestamp
  is measured when a path carries more than one.
- `appears`, `disappears` and `count`: `--where field=value` for an exact match
  or `--where field~=text` for a case-insensitive substring, repeatable, all of
  which must hold.

Common flags:

- `--expires` is required: a span such as `4d`, `12h` or `90m`, a date such as
  `2026-10-01` meaning the end of that local day, or an RFC 3339 time. Nothing
  stays armed for ever.
- `--reason <text>`: the one piece of context the alarm carries.
- `--deliver <agent|pane|repo|you>`: a herdr agent name or pane id, `repo` for
  whichever agent is working inside the checkout the alert concerns, or `you`
  for the user's desktop. Defaults to your pane, or `you` outside one.
- `--standing` rings every time the condition trips until it expires; `--once`
  is the default.
- `--json`: machine output on every verb.
- `--dry-run` on `arm`: validate fully and print the trigger as it would be
  stored, saving nothing. The preview carries no id, since nothing was created.

**The first sample after arming never fires.** A pull request already red is
history, not an event; if you want to know what is true now, read omagihu with
the `omagihu_*` MCP tools before you arm.

**One-shot is the default, and a fired one-shot is spent.** It stays in the list
as `fired` until you disarm it or edit it back to life. Pass `--standing` when
every occurrence matters.

## Picking an operator

| Kind of leaf | Operators |
| --- | --- |
| number | crosses |
| text, bool | becomes |
| list | appears, disappears, count, and ages where it carries a timestamp |

- `crosses`: fires on the sample that carries the value from one side of the
  bound to the other, never while it sits past it. After a fire it re-arms only
  once the value is back on the far side by more than the margin, which is one
  whole unit unless `--rearm` widens it. The re-arming sample never fires.
- `becomes`: fires when a text or bool takes `--value` having not had it on the
  previous sample. It re-arms after the value has been away for `--hold`
  (default 5m), so a flapping status rings once.
- `appears` and `disappears`: compare the set of entry identities before and
  after the `--where` filter; order is ignored. Every new or missing entry is
  its own event, so these never disarm, and a one-shot is spent by the first.
- `count`: `crosses` over the number of entries left after the filter, with an
  integer re-arm margin of 1.
- `ages`: an entry whose timestamp is older than `--older-than` joins the set,
  and joining is the event. This is how a pull request going quiet is expressed:
  it is a property of one entry, not of a count.

When the daemon is asleep, a poll failed, or the path resolves to nothing, there
is no sample and nothing moves: a missing sample is not a transition, and
nothing fires while monitoring is off.

## When the alarm arrives

The wake-up is one line that starts with `omagihu alarm`:

```
omagihu alarm t-1a2b3c4d: a new entry in work.reviewRequests: repo=karamble/omarchy-omagihu number=12 at 2026-09-09T14:12:51Z. Reason: "the release branch is blocked on this review". Armed by w7:p1 at 2026-09-09T12:10:00Z. This message carries no values; read omagihu yourself with the omagihu_* MCP tools. This was a one-shot and is now spent.
```

A standing trigger ends instead with
`It stays armed until <expiry>; disarm with: omagihu-setup disarm <id>.` Then:

- Read omagihu yourself with your own MCP tools. The alarm says the condition
  tripped at that time, not what is true now.
- Act on the reason you wrote, and report to the user.
- When a standing trigger has done its job, disarm it. To move a bound or extend
  an expiry, edit it rather than arming a second trigger.
- Check `omagihu-setup alerts --json` for its status: `armed` is watched and
  ready; `rearming` has fired and waits for the condition to be clearly false
  again; `no-sample` means the last pass could not resolve the path; `fired` is
  a spent one-shot; `expired` passed its expiry without firing;
  `delivery-failed` means it rang and neither herdr nor a desktop notification
  could be raised, so tell the user.

## What alerts never do

Alerts observe and ring. Nothing is pushed, merged, closed, commented on or
marked read because one fired. Acting on the reason is your job, which is also
what keeps the wake-up free of values.

## Recipes

Worked examples for every operator, with the exact text each one delivers, are
in [`recipes.md`](recipes.md).

## Catalogue

Everything below is what the panel already renders, so anything visible there
can be waited on.

<!-- catalogue:begin -->
<!-- Generated from alerts.Catalogue() by make skill. Do not edit by hand. -->
19 paths. A number takes `crosses`. Text and bool take `becomes`. A list takes `appears`, `disappears`, `count` and, where it carries a timestamp, `ages`; it filters with `--where` on the fields shown, and two entries are the same entry when their identity fields match.

### attention
- `attention.reviews` number: crosses. Reviews requested of you.
- `attention.brokenPrs` number: crosses. Your pull requests failing checks or sent back.
- `attention.reconcile` number: crosses. Urgent drift between GitHub and this machine.
- `attention.unread` number: crosses. Unread notifications.
- `attention.reposAtRisk` number: crosses. Checkouts holding uncommitted or unpushed work.
- `attention.unpushedTotal` number: crosses. Commits that exist only on this machine.
- `attention.interrupted` number: crosses. Checkouts left mid rebase, merge or bisect.
- `attention.level` text: becomes. The bar's severity: urgent, warn, notice, clear or asleep.

### facts
- `facts` list: appears, disappears, count. Where GitHub and this machine disagree. Fields: kind, severity, repo, path, branch, number. Identity: kind, path, branch.

### health
- `health.repos` number: crosses. How many checkouts are watched.
- `health.rateLeft` number: crosses. GitHub rate budget remaining.
- `health.monitoring` bool: becomes. Whether polling is on at all.
- `health.lastError` text: becomes. The most recent poll error, empty when healthy.

### inbox
- `inbox` list: appears, disappears, count, ages. Unread GitHub notifications. Fields: repo, type, title, reason, accountId. Identity: id. Time fields: updatedAt.

### repos
- `repos` list: appears, disappears, count, ages. Watched local checkouts. Fields: name, path, branch, upstream, operation, unpushed, behind, upstreamBehind, staged, modified, untracked, conflicted. Identity: path. Time fields: observedAt.

### work
- `work.reviewRequests` list: appears, disappears, count, ages. Pull requests waiting on your review. Fields: repo, number, title, author, headRef, reviewDecision, checksState, isDraft. Identity: repo, number. Time fields: updatedAt, mergedAt.
- `work.authoredPrs` list: appears, disappears, count, ages. Your open pull requests. Fields: repo, number, title, author, headRef, reviewDecision, checksState, isDraft. Identity: repo, number. Time fields: updatedAt, mergedAt.
- `work.mergedPrs` list: appears, disappears, count, ages. Your recently merged pull requests. Fields: repo, number, title, author, headRef, reviewDecision, checksState, isDraft. Identity: repo, number. Time fields: updatedAt, mergedAt.
- `work.assignedIssues` list: appears, disappears, count, ages. Issues assigned to you. Fields: repo, number, title. Identity: repo, number. Time fields: updatedAt.

<!-- catalogue:end -->
