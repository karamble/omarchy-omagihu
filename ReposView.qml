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
  // Rows whose folded badges are shown. A look, not a setting: it lives as
  // long as the view does, like expanded.
  property var badgesOpen: ({})
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

  // Dirt never raises the indicator, so it never wears the urgent colour.
  readonly property color quietTone: Qt.darker(view.foreground, 1.3)

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

  function toggleBadges(key) {
    var o = ({})
    for (var k in view.badgesOpen) o[k] = view.badgesOpen[k]
    o[key] = !o[key]
    view.badgesOpen = o
  }

  function rowBadges(e) {
    if (e.kind === "header") return view.groupBadges(e.group)
    if (e.repo.prunable) return [{ text: "STALE", tone: Qt.darker(view.foreground, 1.4) }]
    return view.repoBadges(e.repo)
  }

  function overflows(e) {
    return view.rowBadges(e).length > view.badgeCap
  }

  // The trailing slot: a header's chevron or a remote-less checkout's mark.
  function hasTrailing(e) {
    return e.kind === "header" || view.canMark(e)
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
    if ((r.stashes || 0) > 0) out.push({ text: r.stashes + " STASH", tone: Qt.darker(view.foreground, 1.4) })
    if (view.isLocalOnly(r)) out.push({ text: "LOCAL ONLY", tone: Qt.darker(view.foreground, 1.4) })
    if (out.length === 0) out.push({ text: "CLEAN", tone: view.owner.toneOk })
    // Somebody else's repository. Quiet, and after the state: what is wrong
    // with it matters more than whose it is.
    if (r.followed === true) out.push({ text: "FOLLOWED", tone: Qt.darker(view.foreground, 1.4) })
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
    if (g.stale > 0) out.push({ text: g.stale + " STALE", tone: Qt.darker(view.foreground, 1.4) })
    if (out.length === 0) out.push({ text: "CLEAN", tone: view.owner.toneOk })
    if (g.followed) out.push({ text: "FOLLOWED", tone: Qt.darker(view.foreground, 1.4) })
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

  // ---------- the keyboard contract ----------
  //
  // Row zero is the chip strip, whose actions are its chips. Every row after
  // it is an entry of view.rows: a plain repository or a checkout opens its
  // remote; a group header opens the remote too and carries a second action,
  // the chevron, which expands or collapses it. Expanding inserts the
  // checkouts as rows right after the header, so everything below shifts.
  readonly property var filters: [
    { key: "risk", label: "At risk", urgent: false },
    { key: "dirty", label: "Dirty", urgent: false },
    { key: "interrupted", label: "Stuck", urgent: true },
    { key: "all", label: "All", urgent: false }
  ]

  readonly property int rowCount: 1 + view.rows.length
  readonly property bool formFocused: false

  // Actions on a row: the row itself, then the trailing action when there is
  // one, then the badge fold when the row overflows.
  function actionCount(row) {
    if (row === 0) return view.filters.length
    var e = view.rows[row - 1]
    if (!e) return 1
    return 1 + (view.hasTrailing(e) ? 1 : 0) + (view.overflows(e) ? 1 : 0)
  }

  function activateRow(row, action) {
    if (row === 0) {
      view.filter = String(view.filters[action].key)
      return
    }
    var e = view.rows[row - 1]
    if (!e) return
    var trailing = view.hasTrailing(e)
    if (action === (trailing ? 2 : 1) && view.overflows(e)) {
      view.toggleBadges(view.rowKey(e))
      return
    }
    if (e.kind === "header" && action === 1) {
      view.toggle(e.group.key)
      return
    }
    if (action === 1 && view.canMark(e)) {
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
        readonly property var entry: modelData
        readonly property var repo: entry.repo
        readonly property var group: entry.group
        readonly property bool header: entry.kind === "header"
        readonly property bool child: entry.kind === "child"
        readonly property bool stale: child && !!repo.prunable
        readonly property color quiet: Qt.darker(view.foreground, 1.4)

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
          if (stale) return "stale registration, " + repo.prunable + " • git worktree prune clears it"
          var bits = []
          if (repo.upstream) bits.push(repo.upstream)
          else bits.push("no upstream")
          if (repo.last && repo.last.subject) bits.push(repo.last.subject)
          return bits.join(" • ")
        }
        badges: view.rowBadges(entry)
        maxBadges: view.badgeCap
        badgesExpanded: view.badgesOpen[view.rowKey(entry)] === true
        onOverflowTriggered: view.toggleBadges(view.rowKey(entry))
        readonly property bool markable: view.canMark(entry)
        readonly property bool marked: markable && view.isLocalOnly(repo)
        actionIcon: {
          if (header) return view.isOpen(group) ? view.owner.iconChevronDown : view.owner.iconChevronRight
          if (markable) return marked ? view.owner.iconCross : view.owner.iconCheck
          return ""
        }
        actionTone: Color.accent
        actionTooltip: {
          if (header) return view.isOpen(group) ? "Collapse" : "Show " + group.members.length + " checkouts"
          if (marked) return "Report the missing remote again"
          if (markable) return "No remote wanted: stop reporting it"
          return ""
        }
        onActivated: view.activateRow(index + 1, 0)
        onActionTriggered: view.activateRow(index + 1, 1)
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
          color: Qt.darker(view.foreground, 1.5)
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
    color: Qt.darker(view.foreground, 1.4)
    font.family: view.fontFamily
    font.pixelSize: Style.font.caption
  }
}
