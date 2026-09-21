pragma ComponentBehavior: Bound

import QtQuick
import QtQuick.Layouts
import qs.Commons
import qs.Ui

// The local plane. Checkouts arrive already sorted, at-risk repositories
// first and a repository's worktrees adjacent with main first. A repository
// with several checkouts is one row that expands; the chips narrow the list
// by the kind of risk, because "unpushed" and "dirty" are different problems
// with different fixes.
Column {
  id: view

  required property var owner
  property var snap: null

  // "risk", "dirty", "interrupted", "all"
  property string filter: "risk"
  // Group keys the user has opened.
  property var expanded: ({})
  // Rows that are disclosed. A look, not a setting: it lives as long as the
  // view does, like expanded.
  property var disclosed: ({})
  readonly property int badgeCap: 4

  readonly property color foreground: owner.foreground
  readonly property string fontFamily: owner.fontFamily

  readonly property var allRepos: snap && snap.repos ? snap.repos : []
  readonly property var facts: snap && snap.facts ? snap.facts : []
  // Checkouts marked as deliberately local, so the missing remote is not
  // reported for them.
  readonly property var localOnly: snap && snap.localOnly ? snap.localOnly : []

  function hasRemote(r) {
    return !!r.remotes && Object.keys(r.remotes).length > 0
  }

  function isLocalOnly(r) {
    return view.localOnly.indexOf(r.path) >= 0
  }

  // A checkout with no remote has nothing to open, so its action slot is
  // free to say "this one is fine" and to take it back.
  function canMark(e) {
    return e.kind !== "header" && !e.repo.prunable && !view.hasRemote(e.repo)
  }

  // Facts keyed by checkout, so a row can wear the same badge the dashboard
  // shows and the two views never disagree.
  readonly property var factsByPath: {
    var m = ({})
    for (var i = 0; i < view.facts.length; i++) {
      var f = view.facts[i]
      if (!f.path) continue
      if (!m[f.path]) m[f.path] = []
      m[f.path].push(f)
    }
    return m
  }

  // Three weights of the one foreground, as alpha so they stay lighter than
  // the text on a light theme as well as a dark one: body for values, quiet
  // for labels and meta, quietTone for dirt, which never raises the
  // indicator and so never wears the urgent colour.
  readonly property color body: Util.alpha(view.foreground, 0.85)
  readonly property color quiet: Util.alpha(view.foreground, 0.6)
  readonly property color quietTone: Util.alpha(view.foreground, 0.7)

  // The number on the badge. Whether it counts as dirt is the daemon's call,
  // which arrives as r.dirty.
  function dirtyCount(r) {
    return (r.staged || 0) + (r.modified || 0) + (r.deleted || 0) + (r.untracked || 0) + (r.conflicted || 0)
  }

  function isInterrupted(r) {
    return r.operation !== undefined && r.operation !== null && r.operation !== ""
  }

  // atRisk and dirty are decided by the daemon and read here, so the chips
  // and the bar can never disagree.
  function matches(r, key) {
    if (key === "all") return true
    if (key === "dirty") return r.dirty === true
    if (key === "interrupted") return view.isInterrupted(r)
    return r.atRisk === true
  }

  // Checkouts folded into repositories, in the order they arrived. A plain
  // repository is a group of one. The group carries the sums its header
  // shows and the same state fields a checkout has, so one icon and tone
  // function serves both.
  readonly property var groups: {
    var out = []
    var byKey = ({})
    for (var i = 0; i < view.allRepos.length; i++) {
      var r = view.allRepos[i]
      var key = r.group || r.path
      var g = byKey[key]
      if (!g) {
        g = { key: key, name: r.name, remotes: r.remotes, members: [],
              checkouts: 0, risky: 0, stale: 0, followed: r.followed === true,
              unpushed: 0, changed: 0, operation: "", dirty: false, atRisk: false }
        byKey[key] = g
        out.push(g)
      }
      g.members.push(r)
      if (r.prunable) { g.stale++; continue }
      g.checkouts++
      if (r.atRisk === true) { g.risky++; g.atRisk = true }
      if (r.dirty === true) g.dirty = true
      g.unpushed += (r.unpushed || 0)
      g.changed += view.dirtyCount(r)
      if (g.operation === "" && view.isInterrupted(r)) g.operation = r.operation
    }
    return out
  }

  function groupMatches(g, key) {
    for (var i = 0; i < g.members.length; i++) {
      if (view.matches(g.members[i], key)) return true
    }
    return false
  }

  function countFor(key) {
    var n = 0
    for (var i = 0; i < view.groups.length; i++) {
      if (view.groupMatches(view.groups[i], key)) n++
    }
    return n
  }

  readonly property var shown: {
    var out = []
    for (var i = 0; i < view.groups.length; i++) {
      if (view.groupMatches(view.groups[i], view.filter)) out.push(view.groups[i])
    }
    return out
  }

  // A group is a repository with more than one checkout, not one whose
  // checkout is main: every plain repository is main of its own group.
  function isMulti(g) { return g.members.length > 1 }
  function isOpen(g) { return view.isMulti(g) && view.expanded[g.key] === true }

  function toggle(key) {
    var e = ({})
    for (var k in view.expanded) e[k] = view.expanded[k]
    e[key] = !e[key]
    view.expanded = e
  }

  // A header is keyed by its group, a checkout by its path.
  function rowKey(e) {
    return e.kind === "header" ? "group:" + e.group.key : e.repo.path
  }

  function toggleDisclosure(key) {
    var o = ({})
    for (var k in view.disclosed) o[k] = view.disclosed[k]
    o[key] = !o[key]
    view.disclosed = o
  }

  // Opening a row that can answer asks for its statistics; the panel keeps
  // the answer, so a second opening inside the hour costs nothing.
  function disclose(e) {
    var key = view.rowKey(e)
    var opening = view.disclosed[key] !== true
    view.toggleDisclosure(key)
    if (opening && view.hasStats(e)) view.owner.requestStats(view.originOf(e))
  }

  function rowBadges(e) {
    if (e.kind === "header") return view.groupBadges(e.group)
    if (e.repo.prunable) return [{ text: "STALE", tone: view.quiet }]
    return view.repoBadges(e.repo)
  }

  // The trailing slot holds one action, by priority: a header's chevron, a
  // remote-less checkout's mark, otherwise opening the remote. A stale
  // registration has no directory and gets none.
  function hasTrailing(e) {
    if (e.kind === "header" || view.canMark(e)) return true
    return !e.repo.prunable && view.remoteUrl(e.repo) !== ""
  }

  function ago(iso) {
    if (!iso) return ""
    var then = Date.parse(iso)
    if (isNaN(then)) return ""
    var secs = Math.max(0, Math.round((Date.now() - then) / 1000))
    if (secs < 60) return secs + "s ago"
    if (secs < 3600) return Math.round(secs / 60) + "m ago"
    if (secs < 86400) return Math.round(secs / 3600) + "h ago"
    return Math.round(secs / 86400) + "d ago"
  }

  // ---------- repository statistics ----------
  //
  // Asked of the daemon when a row is disclosed, and only for a row that
  // can answer: an origin on a forge host that one of the accounts speaks
  // to. A filesystem origin or an unknown host gets no card at all rather
  // than a failed one. The endpoint cannot see the row, so this is the
  // view's rule.
  readonly property var accountHosts: {
    var out = []
    var accounts = snap && snap.accounts ? snap.accounts : []
    for (var i = 0; i < accounts.length; i++) {
      var id = String(accounts[i].accountId || "")
      var at = id.lastIndexOf("@")
      if (at > 0) out.push(id.slice(at + 1).toLowerCase())
    }
    return out
  }

  function originOf(e) {
    var remotes = (e.kind === "header" ? e.group.remotes : e.repo.remotes) || {}
    return remotes["origin"] || ""
  }

  // The host in any of the forms a remote is written in, or "" for a path.
  function originHost(url) {
    var m = /^(?:[a-z]+:\/\/)?(?:[^@\/]+@)?([^\/:]+\.[^\/:]+)[:\/]/.exec(String(url))
    return m ? m[1].toLowerCase() : ""
  }

  function hasStats(e) {
    if (e.kind !== "header" && e.repo.prunable) return false
    var host = view.originHost(view.originOf(e))
    return host !== "" && view.accountHosts.indexOf(host) >= 0
  }

  function minutesLeft(iso) {
    var t = Date.parse(iso)
    if (isNaN(t)) return 0
    return Math.max(0, Math.round((t - Date.now()) / 60000))
  }

  function statsNumbers(st) {
    return [
      { value: st.stars || 0, label: "stars", icon: view.owner.iconStar },
      { value: st.forks || 0, label: "forks", icon: view.owner.iconBranch },
      { value: st.watchers || 0, label: "watchers", icon: view.owner.iconEye },
      { value: st.openIssues || 0, label: "open issues", icon: view.owner.iconIssue },
      { value: st.openPrs || 0, label: "open PRs", icon: view.owner.iconPr }
    ]
  }

  // A count with thousands grouped, so 1284 stars reads at a glance.
  function figure(n) {
    return Number(n || 0).toLocaleString(Qt.locale(), "f", 0)
  }

  // What the clock says, in the order the states matter.
  function clockText(answer) {
    if (answer.paused) return "monitoring is off: these figures cannot update until it is switched back on"
    if (answer.stale && answer.error) return "figures from " + view.ago(answer.fetchedAt) + "; the refresh failed"
    if (answer.cached) return "cached for another " + view.minutesLeft(answer.expiresAt) + " minutes"
    return ""
  }

  // The grid under the figures, from what the daemon returned. Short values,
  // so the hero lays them out in two columns.
  function statsRows(answer) {
    var out = []
    var body = view.body
    var st = answer.stats
    if (st.release) out.push({ label: "release", value: st.release + (st.releasedAt ? " · " + view.ago(st.releasedAt) : ""), tone: body })
    if (st.pushedAt) out.push({ label: "pushed", value: view.ago(st.pushedAt), tone: body })
    if (st.defaultBranch) out.push({ label: "branch", value: st.defaultBranch, tone: body })
    if (st.license) out.push({ label: "licence", value: st.license, tone: body })
    if (st.archived) out.push({ label: "status", value: "archived", tone: Color.urgent, bold: true })
    if (st.private) out.push({ label: "visibility", value: "private", tone: body })
    if (answer.stale && answer.error) out.push({ label: "refresh", value: "failed: " + answer.error, tone: Color.urgent })
    return out
  }

  // The first half of a row list, for the left column of a two column grid.
  function leftHalf(rows) { return rows.slice(0, Math.ceil(rows.length / 2)) }
  function rightHalf(rows) { return rows.slice(Math.ceil(rows.length / 2)) }

  // The counts in the tree, as one line, or clean.
  function treeText(r) {
    var work = []
    var counts = [["staged", r.staged], ["modified", r.modified], ["deleted", r.deleted],
                  ["untracked", r.untracked], ["conflicted", r.conflicted]]
    for (var c = 0; c < counts.length; c++) if ((counts[c][1] || 0) > 0) work.push(counts[c][1] + " " + counts[c][0])
    return work.length > 0 ? work.join(" · ") : "clean"
  }

  // What a disclosed row says, as sections in a fixed order so every row
  // reads the same way: the figures, then the facts and their fix and
  // anything that could not be read, then where the checkout is and how far
  // it stands from its remotes, then what is in the tree, the last commit
  // and how old this reading is. A grid section carries label and value
  // rows; the attention section carries facts, each a summary over a detail.
  function detailSections(e) {
    var out = []
    var body = view.body
    var quiet = view.quiet
    var r = e.repo
    var g = e.group

    if (view.hasStats(e)) out.push({ key: "stats", title: "STATISTICS", icon: view.owner.iconGithub, origin: view.originOf(e) })

    if (e.kind === "header") {
      var about = [{ label: "git dir", value: g.key, tone: body }]
      for (var m = 0; m < g.members.length; m++) {
        var mem = g.members[m]
        var role = mem.prunable ? "stale" : (mem.main ? "main" : "linked")
        about.push({ label: role, value: mem.name + "  [" + (mem.branch || "?") + "]  " + mem.path, tone: mem.prunable ? quiet : body })
      }
      var origin = g.remotes || {}
      for (var rk in origin) about.push({ label: rk, value: origin[rk], tone: body })
      out.push({ key: "repository", title: "REPOSITORY", icon: view.owner.iconGit, rows: about })
      return out
    }

    var attention = []
    var drift = view.factsByPath[r.path] || []
    var worst = quiet
    for (var i = 0; i < drift.length; i++) {
      var urgent = drift[i].severity === "urgent"
      attention.push({ summary: drift[i].summary, detail: drift[i].detail || "", tone: urgent ? Color.urgent : Color.accent })
      if (urgent) worst = Color.urgent
      else if (worst !== Color.urgent) worst = Color.accent
    }
    if (r.error) { attention.push({ summary: "could not inspect: " + r.error, detail: "", tone: Color.urgent }); worst = Color.urgent }
    if (r.prunable) attention.push({ summary: "stale registration: " + r.prunable, detail: "git worktree prune clears it", tone: body })
    if (attention.length > 0) out.push({ key: "attention", title: "ATTENTION", icon: view.owner.iconWarn, facts: attention, tone: worst })

    var repository = []
    var where = r.path
    if (view.isMulti(g)) where += r.main ? "  (main checkout of " + g.name + ")" : "  (linked worktree of " + g.name + ")"
    repository.push({ label: "path", value: where, tone: body })
    // Detached says where HEAD is, not whether the work can be lost. A HEAD
    // parked on the tip of a branch is detached and nothing is stranded, so
    // only the stranded case claims no branch names it. Nothing is counted as
    // stranded in a repository with no remotes, so the quiet wording there
    // claims nothing either way.
    if (r.detached)
      repository.push({ label: "HEAD",
        value: (r.stranded || 0) > 0 ? "detached, on no branch" : "detached",
        tone: Color.accent })
    var remotes = r.remotes || {}
    var names = Object.keys(remotes)
    if (names.length === 0) repository.push({ label: "remote", value: "none", tone: quiet })
    for (var n = 0; n < names.length; n++) repository.push({ label: names[n], value: remotes[names[n]], tone: body })
    if (!r.prunable) {
      if (r.upstream) {
        repository.push({ label: "tracking", value: r.upstream + " · ahead " + (r.ahead || 0) + " · behind " + (r.behind || 0), tone: body,
                          bar: { ahead: r.ahead || 0, behind: r.behind || 0 } })
      } else if (r.noUpstream) {
        repository.push({ label: "tracking", value: "no upstream · " + (r.unpushed || 0) + " commits exist only here", tone: body,
                          bar: { ahead: r.unpushed || 0, behind: 0 } })
      }
      // The fork-behind fact above already states this distance.
      var saidBehind = false
      for (var b = 0; b < drift.length; b++) if (drift[b].kind === "fork-behind") saidBehind = true
      if ((r.upstreamBehind || 0) > 0 && !saidBehind) repository.push({ label: "fork", value: r.upstreamBehind + " commits behind upstream/HEAD", tone: body })
    }
    out.push({ key: "repository", title: "REPOSITORY", icon: view.owner.iconGit, rows: repository })
    if (r.prunable) return out

    var state = []
    state.push({ label: "tree", value: view.treeText(r), tone: r.dirty === true ? body : quiet })
    if ((r.stashes || 0) > 0) state.push({ label: "stash", value: r.stashes + (r.stashes === 1 ? " entry" : " entries"), tone: body })
    if (r.last && r.last.subject) {
      var who = r.last.author ? " · " + r.last.author : ""
      var when = r.last.at ? " · " + view.ago(r.last.at) : ""
      state.push({ label: "last commit", value: r.last.subject + who + when, tone: body })
    }
    state.push({ label: "inspected", value: r.observedAt ? view.ago(r.observedAt) : "not yet", tone: quiet })
    out.push({ key: "state", title: "STATE", icon: view.owner.iconTree, rows: state })
    return out
  }

  // The rows the list draws, one entry each: a plain repository, a group
  // header, or a checkout under an open header.
  readonly property var rows: {
    var out = []
    for (var i = 0; i < view.shown.length; i++) {
      var g = view.shown[i]
      if (!view.isMulti(g)) {
        out.push({ kind: "single", repo: g.members[0], group: g })
        continue
      }
      out.push({ kind: "header", repo: g.members[0], group: g })
      if (!view.isOpen(g)) continue
      for (var j = 0; j < g.members.length; j++) {
        out.push({ kind: "child", repo: g.members[j], group: g })
      }
    }
    return out
  }

  // Badges say what is wrong. The checkout's own state leads, the operation
  // and the count that put it on the local tier, so a capped row never
  // loses the reason it is listed; then what GitHub adds; then housekeeping.
  // A clean repo says so plainly.
  function repoBadges(r) {
    var out = []
    if (view.isInterrupted(r))
      out.push({ text: String(r.operation).toUpperCase(), tone: Color.urgent, loud: true })
    if ((r.unpushed || 0) > 0)
      out.push({ text: r.unpushed + " UNPUSHED", tone: Color.accent, loud: (r.unpushed || 0) > 20 })
    var drift = view.factsByPath[r.path] || []
    for (var i = 0; i < drift.length; i++) {
      // Detached work is a notice so the bar stays on the local tier, but it
      // is the one local state where commits can vanish, so the badge shouts.
      var loud = drift[i].severity === "urgent" || drift[i].kind === "detached-work"
      out.push({ text: view.owner.factLabel(drift[i].kind),
                 tone: loud ? Color.urgent : Color.accent,
                 loud: loud })
    }
    var d = view.dirtyCount(r)
    if (d > 0) out.push({ text: d + " CHANGED", tone: view.quietTone })
    if ((r.behind || 0) > 0) out.push({ text: r.behind + " BEHIND", tone: view.quietTone })
    if ((r.stashes || 0) > 0) out.push({ text: r.stashes + " STASH", tone: view.quiet })
    if (view.isLocalOnly(r)) out.push({ text: "LOCAL ONLY", tone: view.quiet })
    if (out.length === 0) out.push({ text: "CLEAN", tone: view.owner.toneOk })
    // Somebody else's repository. Quiet, and after the state: what is wrong
    // with it matters more than whose it is.
    if (r.followed === true) out.push({ text: "FOLLOWED", tone: view.quiet })
    return out
  }

  // A header sums its checkouts; the facts stay on the checkout they name.
  function groupBadges(g) {
    var out = []
    if (g.operation !== "")
      out.push({ text: String(g.operation).toUpperCase(), tone: Color.urgent, loud: true })
    if (g.unpushed > 0)
      out.push({ text: g.unpushed + " UNPUSHED", tone: Color.accent, loud: g.unpushed > 20 })
    if (g.changed > 0) out.push({ text: g.changed + " CHANGED", tone: view.quietTone })
    if (g.stale > 0) out.push({ text: g.stale + " STALE", tone: view.quiet })
    if (out.length === 0) out.push({ text: "CLEAN", tone: view.owner.toneOk })
    if (g.followed) out.push({ text: "FOLLOWED", tone: view.quiet })
    return out
  }

  function rowIcon(r) {
    if (view.isInterrupted(r)) return view.owner.iconWarn
    if ((r.unpushed || 0) > 0) return view.owner.iconBranch
    if (r.dirty === true) return view.owner.iconDot
    return view.owner.iconCheck
  }

  function rowTone(r) {
    if (view.isInterrupted(r)) return Color.urgent
    if ((r.unpushed || 0) > 0) return Color.accent
    if (r.dirty === true) return view.quietTone
    return view.owner.toneOk
  }

  // A local checkout has no url of its own, so open its remote when there is
  // one, normalising the ssh form on the way.
  function remoteUrl(r) {
    var remotes = r.remotes || {}
    var url = remotes["origin"] || remotes["upstream"] || ""
    if (url.indexOf("git@") === 0)
      return "https://" + url.replace(":", "/").replace("git@", "").replace(/\.git$/, "")
    if (url.indexOf("http") === 0)
      return url.replace(/\.git$/, "")
    return ""
  }

  spacing: Style.space(10)

  // ---------- the pieces a disclosed row is drawn with ----------
  //
  // HeroStat, StatLine and SplitBar are shared with the other views; these
  // bind them to this view's foreground and font once, so every use is short.
  readonly property int labelColumn: Style.space(84)

  component Figure: HeroStat {
    foreground: view.foreground
    fontFamily: view.fontFamily
  }

  component Line: StatLine {
    foreground: view.foreground
    fontFamily: view.fontFamily
    labelColumn: view.labelColumn
  }

  // ---------- the keyboard contract ----------
  //
  // Row zero is the chip strip, whose actions are its chips. Every row after
  // it is an entry of view.rows. Its first action discloses it: the card
  // opens to show the folded badges and the checkout's detail. Its second,
  // when it has one, is the row's one contextual verb: a header's chevron,
  // which expands the repository into its checkouts as rows right after it
  // so everything below shifts; a remote-less checkout's mark; or opening
  // the remote.
  readonly property var filters: [
    { key: "risk", label: "At risk", urgent: false },
    { key: "dirty", label: "Dirty", urgent: false },
    { key: "interrupted", label: "Stuck", urgent: true },
    { key: "all", label: "All", urgent: false }
  ]

  readonly property int rowCount: 1 + view.rows.length
  readonly property bool formFocused: false

  // Actions on a row: the row itself discloses; the trailing action, when
  // there is one, is the row's one contextual verb.
  function actionCount(row) {
    if (row === 0) return view.filters.length
    var e = view.rows[row - 1]
    if (!e) return 1
    return 1 + (view.hasTrailing(e) ? 1 : 0)
  }

  function activateRow(row, action) {
    if (row === 0) {
      view.filter = String(view.filters[action].key)
      return
    }
    var e = view.rows[row - 1]
    if (!e) return
    if (action === 0) {
      view.disclose(e)
      return
    }
    if (e.kind === "header") {
      view.toggle(e.group.key)
      return
    }
    if (view.canMark(e)) {
      view.owner.setLocalOnly(e.repo.path, !view.isLocalOnly(e.repo))
      return
    }
    // A stale registration has no directory, so there is nothing to open.
    if (e.repo.prunable) return
    var url = view.remoteUrl(e.repo)
    if (url !== "") view.owner.openUrl(url)
  }

  // ---------- filter chips ----------
  RowLayout {
    width: parent.width
    spacing: Style.space(6)

    Repeater {
      model: view.filters
      delegate: Button {
        required property var modelData
        required property int index
        Layout.fillWidth: true
        readonly property int n: view.countFor(modelData.key)
        text: modelData.label + " (" + n + ")"
        selected: view.filter === modelData.key
        hasCursor: view.owner.cursor === 0 && view.owner.actionIndex === index
        bordered: true
        foreground: modelData.urgent && n > 0 ? Color.urgent : view.foreground
        accent: modelData.urgent && n > 0 ? Color.urgent : Color.accent
        fontFamily: view.fontFamily
        fontSize: Style.font.caption
        horizontalPadding: Style.space(8)
        verticalPadding: Style.space(5)
        onClicked: view.filter = modelData.key
      }
    }
  }

  // ---------- list. The card itself scrolls, so there is no inner viewport
  // competing for the wheel. ----------
  Column {
    id: repoColumn
    width: parent.width
    spacing: Style.space(6)

    Repeater {
      model: view.rows
      delegate: ListRow {
        id: repoRow
        required property var modelData
        required property int index
        readonly property var entry: repoRow.modelData
        readonly property var repo: entry.repo
        readonly property var group: entry.group
        readonly property bool header: entry.kind === "header"
        readonly property bool child: entry.kind === "child"
        readonly property bool stale: child && !!repo.prunable
        readonly property color quiet: view.quiet

        // Checkouts sit in from the header that owns them.
        x: child ? Style.space(18) : 0
        width: repoColumn.width - x
        hasCursor: view.owner.cursor === index + 1
        actionIndex: view.owner.actionIndex
        icon: stale ? view.owner.iconCross : view.rowIcon(header ? group : repo)
        tone: stale ? quiet : view.rowTone(header ? group : repo)
        urgent: header ? group.atRisk : repo.atRisk === true
        fontFamily: view.fontFamily
        title: header ? group.name : repo.name + "  [" + (repo.branch || "?") + "]"
        subtitle: {
          if (header) return group.risky + " of " + group.checkouts + " checkouts need attention"
          if (stale) return "stale registration, " + repo.prunable + " · git worktree prune clears it"
          var bits = []
          if (repo.upstream) bits.push(repo.upstream)
          else bits.push("no upstream")
          if (repo.last && repo.last.subject) bits.push(repo.last.subject)
          return bits.join(" · ")
        }
        badges: view.rowBadges(entry)
        maxBadges: view.badgeCap
        disclosable: true
        disclosed: view.disclosed[view.rowKey(entry)] === true
        discloseIcon: view.owner.iconCaretRight
        discloseOpenIcon: view.owner.iconCaretDown
        readonly property bool markable: view.canMark(entry)
        readonly property bool marked: markable && view.isLocalOnly(repo)
        readonly property bool openable: !header && !markable && view.hasTrailing(entry)
        actionIcon: {
          if (header) return view.isOpen(group) ? view.owner.iconChevronDown : view.owner.iconChevronRight
          if (markable) return marked ? view.owner.iconCross : view.owner.iconCheck
          if (openable) return view.owner.iconOpen
          return ""
        }
        actionTone: Color.accent
        actionTooltip: {
          if (header) return view.isOpen(group) ? "Collapse" : "Show " + group.members.length + " checkouts"
          if (marked) return "Report the missing remote again"
          if (markable) return "No remote wanted: stop reporting it"
          if (openable) return "Open on GitHub"
          return ""
        }
        onActivated: view.activateRow(index + 1, 0)
        onActionTriggered: view.activateRow(index + 1, 1)

        // The detail exists only while the row is disclosed: a collapsed
        // row costs nothing, however many rows there are.
        detailContent: Loader {
          width: parent.width
          active: repoRow.disclosed
          sourceComponent: Column {
            width: parent ? parent.width : 0
            spacing: Style.space(8)

            // One section per entry, in the order detailSections gives
            // them, a rule between each pair.
            Repeater {
              model: view.detailSections(repoRow.entry)
              delegate: Column {
                id: section
                required property var modelData
                required property int index
                readonly property string sectionKey: section.modelData.key
                readonly property bool statsCard: section.sectionKey === "stats"
                readonly property var answer: section.statsCard ? view.owner.statsFor(section.modelData.origin) : null
                readonly property var figures: section.answer && section.answer.stats ? section.answer.stats : null
                readonly property color body: view.body
                readonly property color quiet: view.quiet
                // The attention section is the one filled surface, in the
                // tone of its worst fact; every other section sits on the
                // row.
                readonly property color sectionTone: section.modelData.tone !== undefined ? section.modelData.tone : section.quiet
                readonly property bool filled: section.sectionKey === "attention"
                width: parent.width
                spacing: Style.space(4)

                PanelSeparator {
                  width: parent.width
                  foreground: view.foreground
                }

                Rectangle {
                  width: parent.width
                  implicitHeight: sectionBody.implicitHeight + (section.filled ? Style.space(16) : Style.space(2))
                  radius: Style.cornerRadius > 0 ? Style.space(6) : 0
                  color: section.filled ? Qt.rgba(section.sectionTone.r, section.sectionTone.g, section.sectionTone.b, 0.10) : "transparent"
                  border.width: section.filled ? 1 : 0
                  border.color: Qt.rgba(section.sectionTone.r, section.sectionTone.g, section.sectionTone.b, 0.35)

                  Column {
                    id: sectionBody
                    anchors.top: parent.top
                    anchors.left: parent.left
                    anchors.right: parent.right
                    anchors.margins: section.filled ? Style.space(8) : 0
                    anchors.topMargin: section.filled ? Style.space(6) : 0
                    spacing: Style.space(4)

                    // The title line. The statistics clock hangs at its right
                    // end, and only when there is something to say: reused
                    // from the cache, held while monitoring is paused, or
                    // kept after a refresh failed.
                    Item {
                      width: parent.width
                      implicitHeight: title.implicitHeight

                      // The title carries the section's glyph before its
                      // word. The colour is set here because the shell's
                      // header darkens its foreground, which on a light
                      // theme makes the title heavier than the values.
                      PanelSectionHeader {
                        id: title
                        text: (section.modelData.icon ? section.modelData.icon + "  " : "") + section.modelData.title
                        foreground: section.filled ? section.sectionTone : view.foreground
                        color: section.filled ? section.sectionTone : view.quiet
                        fontFamily: view.fontFamily
                      }

                      // Which repository the figures belong to: for a fork,
                      // the fork, which is not what the row's badges are about.
                      Text {
                        id: statsRepo
                        visible: section.statsCard && !!section.figures && !!section.answer.repo
                        anchors.right: clock.visible ? clock.left : parent.right
                        anchors.rightMargin: clock.visible ? Style.space(2) : 0
                        anchors.verticalCenter: parent.verticalCenter
                        textFormat: Text.PlainText
                        text: section.answer && section.answer.repo ? String(section.answer.repo) : ""
                        color: section.quiet
                        font.family: view.fontFamily
                        font.pixelSize: Style.font.caption
                      }

                      Item {
                        id: clock
                        objectName: "statsClock"
                        visible: section.statsCard && !!section.answer && section.answer.loading !== true
                                 && (section.answer.cached === true || section.answer.paused === true || section.answer.stale === true)
                        anchors.right: parent.right
                        anchors.verticalCenter: parent.verticalCenter
                        width: Style.space(20)
                        height: Style.space(20)

                        Text {
                          anchors.centerIn: parent
                          textFormat: Text.PlainText
                          text: view.owner.iconClock
                          color: clockMouse.containsMouse ? Color.accent : section.quiet
                          font.family: view.fontFamily
                          font.pixelSize: Style.font.caption
                        }

                        MouseArea {
                          id: clockMouse
                          anchors.fill: parent
                          hoverEnabled: true
                        }

                        PanelToolTip {
                          objectName: "statsClockTip"
                          visible: clockMouse.containsMouse
                          text: section.answer ? view.clockText(section.answer) : ""
                          fontFamily: view.fontFamily
                        }
                      }
                    }

                    // ---- statistics: the hero. Figures first, then the
                    // short facts in two columns, then what the repository
                    // says it is.
                    Text {
                      visible: section.statsCard && !section.figures && (!section.answer || section.answer.loading === true)
                      textFormat: Text.PlainText
                      text: "asking GitHub"
                      color: section.quiet
                      font.family: view.fontFamily
                      font.pixelSize: Style.font.caption
                    }

                    Text {
                      visible: section.statsCard && !section.figures && !!section.answer && section.answer.loading !== true
                      width: parent.width
                      wrapMode: Text.WordWrap
                      textFormat: Text.PlainText
                      text: section.answer && section.answer.error
                            ? "GitHub does not show this repository to " + (section.answer.account || "this account")
                            : "no figures yet"
                      color: section.body
                      font.family: view.fontFamily
                      font.pixelSize: Style.font.caption
                    }

                    Text {
                      visible: section.statsCard && !section.figures && !!section.answer && section.answer.loading !== true && !!section.answer.error
                      width: parent.width
                      wrapMode: Text.WrapAnywhere
                      textFormat: Text.PlainText
                      text: section.answer ? String(section.answer.error || "") : ""
                      color: section.quiet
                      font.family: view.fontFamily
                      font.pixelSize: Style.font.caption
                    }

                    Row {
                      visible: section.statsCard && !!section.figures
                      spacing: Style.space(18)

                      Repeater {
                        model: section.figures ? view.statsNumbers(section.figures) : []
                        delegate: Figure {
                          required property var modelData
                          value: view.figure(modelData.value)
                          label: modelData.label
                          icon: modelData.icon
                        }
                      }
                    }

                    Item { width: 1; height: Style.space(2); visible: section.statsCard && !!section.figures }

                    Row {
                      id: statsGrid
                      visible: section.statsCard && !!section.figures
                      width: parent.width
                      spacing: Style.space(20)
                      readonly property var rows: section.statsCard && section.figures ? view.statsRows(section.answer) : []
                      readonly property real columnWidth: (statsGrid.width - statsGrid.spacing) / 2

                      Column {
                        width: statsGrid.columnWidth
                        spacing: Style.space(3)
                        Repeater {
                          model: view.leftHalf(statsGrid.rows)
                          delegate: Line { required property var modelData; row: modelData }
                        }
                      }

                      Column {
                        width: statsGrid.columnWidth
                        spacing: Style.space(3)
                        Repeater {
                          model: view.rightHalf(statsGrid.rows)
                          delegate: Line { required property var modelData; row: modelData }
                        }
                      }
                    }

                    Item { width: 1; height: Style.space(2); visible: section.statsCard && !!section.figures }

                    Text {
                      visible: section.statsCard && !!section.figures && !!section.figures.description
                      width: parent.width
                      wrapMode: Text.WordWrap
                      textFormat: Text.PlainText
                      text: section.figures ? String(section.figures.description || "") : ""
                      color: section.body
                      font.family: view.fontFamily
                      font.pixelSize: Style.font.caption
                    }

                    Flow {
                      visible: section.statsCard && !!section.figures && (section.figures.topics || []).length > 0
                      width: parent.width
                      spacing: Style.space(4)

                      Repeater {
                        model: section.figures ? (section.figures.topics || []) : []
                        delegate: Badge {
                          id: topic
                          required property var modelData
                          text: String(topic.modelData)
                          tone: section.quiet
                          compact: true
                          fontFamily: view.fontFamily
                        }
                      }
                    }

                    // ---- attention: each fact, a summary over its detail
                    Repeater {
                      model: section.modelData.facts || []
                      delegate: Column {
                        id: fact
                        required property var modelData
                        width: parent.width
                        spacing: Style.space(1)

                        Text {
                          width: parent.width
                          textFormat: Text.PlainText
                          wrapMode: Text.WordWrap
                          text: fact.modelData.summary
                          color: fact.modelData.tone
                          font.family: view.fontFamily
                          font.pixelSize: Style.font.caption
                          font.bold: true
                        }

                        Text {
                          visible: fact.modelData.detail !== ""
                          width: parent.width
                          textFormat: Text.PlainText
                          wrapMode: Text.WordWrap
                          text: fact.modelData.detail
                          color: section.body
                          font.family: view.fontFamily
                          font.pixelSize: Style.font.caption
                        }
                      }
                    }

                    // ---- repository and state: the grid
                    Repeater {
                      model: section.modelData.rows || []
                      delegate: Line { required property var modelData; row: modelData }
                    }
                  }
                }
              }
            }
          }
        }
      }
    }

    Rectangle {
      visible: view.rows.length === 0
      width: repoColumn.width
      implicitHeight: Style.space(70)
      radius: Style.cornerRadius > 0 ? Style.space(6) : 0
      color: Qt.rgba(view.foreground.r, view.foreground.g, view.foreground.b, 0.04)

      ColumnLayout {
        anchors.centerIn: parent
        spacing: Style.space(4)

        Text {
          Layout.alignment: Qt.AlignHCenter
          textFormat: Text.PlainText
          text: view.filter === "all" ? "No repositories found" : "Nothing in this filter"
          color: view.filter === "all" ? Color.urgent : view.owner.toneOk
          font.family: view.fontFamily
          font.pixelSize: Style.font.body
          font.bold: true
        }

        Text {
          Layout.alignment: Qt.AlignHCenter
          textFormat: Text.PlainText
          text: view.filter === "all"
                ? "Check the roots the daemon is watching."
                : "Everything here is pushed and clean."
          color: Util.alpha(view.foreground, 0.55)
          font.family: view.fontFamily
          font.pixelSize: Style.font.caption
        }
      }
    }
  }

  Text {
    textFormat: Text.PlainText
    width: parent.width
    text: view.shown.length + " of " + view.groups.length + " watched repositories"
    color: view.quiet
    font.family: view.fontFamily
    font.pixelSize: Style.font.caption
  }
}
