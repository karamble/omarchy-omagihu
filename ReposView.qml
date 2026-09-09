import QtQuick
import QtQuick.Layouts
import qs.Commons
import qs.Ui

// The local plane. Repositories arrive already sorted with the at-risk ones
// first; the chips narrow that by the kind of risk, because "unpushed" and
// "dirty" are different problems with different fixes.
Column {
  id: view

  required property var owner
  property var snap: null

  // "risk", "unpushed", "dirty", "interrupted", "all"
  property string filter: "risk"

  readonly property color foreground: owner.foreground
  readonly property string fontFamily: owner.fontFamily

  readonly property var allRepos: snap && snap.repos ? snap.repos : []
  readonly property var facts: snap && snap.facts ? snap.facts : []

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

  function factLabel(kind) {
    return ({
      "missing-work": "MISSING WORK",
      "ci-red-on-head": "CI RED HERE",
      "changes-requested": "CHANGES",
      "stale-branch": "MERGED",
      "fork-behind": "BEHIND UPSTREAM"
    })[kind] || String(kind).toUpperCase()
  }

  function dirtyCount(r) {
    return (r.staged || 0) + (r.modified || 0) + (r.deleted || 0) + (r.untracked || 0) + (r.conflicted || 0)
  }

  function isInterrupted(r) {
    return r.operation !== undefined && r.operation !== null && r.operation !== ""
  }

  function atRisk(r) {
    return (r.unpushed || 0) > 0 || view.isInterrupted(r) || view.dirtyCount(r) > 0
  }

  function matches(r, key) {
    if (key === "all") return true
    if (key === "unpushed") return (r.unpushed || 0) > 0
    if (key === "dirty") return view.dirtyCount(r) > 0
    if (key === "interrupted") return view.isInterrupted(r)
    return view.atRisk(r)
  }

  function countFor(key) {
    var n = 0
    for (var i = 0; i < view.allRepos.length; i++) {
      if (view.matches(view.allRepos[i], key)) n++
    }
    return n
  }

  readonly property var repos: {
    var out = []
    for (var i = 0; i < view.allRepos.length; i++) {
      if (view.matches(view.allRepos[i], view.filter)) out.push(view.allRepos[i])
    }
    return out
  }

  // Badges say what is wrong, loudest first. A clean repo says so plainly.
  function repoBadges(r) {
    var out = []
    var drift = view.factsByPath[r.path] || []
    for (var i = 0; i < drift.length; i++) {
      out.push({ text: view.factLabel(drift[i].kind),
                 tone: drift[i].severity === "urgent" ? Color.urgent : Color.accent,
                 loud: drift[i].severity === "urgent" })
    }
    if (view.isInterrupted(r))
      out.push({ text: String(r.operation).toUpperCase(), tone: Color.urgent, loud: true })
    if ((r.unpushed || 0) > 0)
      out.push({ text: r.unpushed + " UNPUSHED", tone: Color.accent, loud: (r.unpushed || 0) > 20 })
    var d = view.dirtyCount(r)
    if (d > 0) out.push({ text: d + " CHANGED", tone: Qt.lighter(Color.urgent, 1.3) })
    if ((r.behind || 0) > 0) out.push({ text: r.behind + " BEHIND", tone: Qt.darker(view.foreground, 1.3) })
    if ((r.stashes || 0) > 0) out.push({ text: r.stashes + " STASH", tone: Qt.darker(view.foreground, 1.4) })
    if (out.length === 0) out.push({ text: "CLEAN", tone: view.owner.toneOk })
    return out
  }

  function rowIcon(r) {
    if (view.isInterrupted(r)) return view.owner.iconWarn
    if ((r.unpushed || 0) > 0) return view.owner.iconBranch
    if (view.dirtyCount(r) > 0) return view.owner.iconDot
    return view.owner.iconCheck
  }

  function rowTone(r) {
    if (view.isInterrupted(r)) return Color.urgent
    if ((r.unpushed || 0) > 0) return Color.accent
    if (view.dirtyCount(r) > 0) return Qt.lighter(Color.urgent, 1.3)
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
  // Row zero is the chip strip, whose actions are its chips; every row after it
  // is a checkout, whose one action is to open its remote.
  readonly property var filters: [
    { key: "risk", label: "At risk", urgent: false },
    { key: "unpushed", label: "Unpushed", urgent: false },
    { key: "dirty", label: "Dirty", urgent: false },
    { key: "interrupted", label: "Stuck", urgent: true },
    { key: "all", label: "All", urgent: false }
  ]

  readonly property int rowCount: 1 + view.repos.length
  readonly property bool formFocused: false

  function actionCount(row) { return row === 0 ? view.filters.length : 1 }

  function activateRow(row, action) {
    if (row === 0) {
      view.filter = String(view.filters[action].key)
      return
    }
    var r = view.repos[row - 1]
    if (!r) return
    var url = view.remoteUrl(r)
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
      model: view.repos
      delegate: ListRow {
        width: repoColumn.width
        hasCursor: view.owner.cursor === index + 1
        icon: view.rowIcon(modelData)
        tone: view.rowTone(modelData)
        urgent: view.isInterrupted(modelData) || (modelData.unpushed || 0) > 0
        fontFamily: view.fontFamily
        title: modelData.name + "  [" + (modelData.branch || "?") + "]"
        subtitle: {
          var bits = []
          if (modelData.upstream) bits.push(modelData.upstream)
          else bits.push("no upstream")
          if (modelData.last && modelData.last.subject) bits.push(modelData.last.subject)
          return bits.join(" • ")
        }
        badges: view.repoBadges(modelData)
        onActivated: {
          var url = view.remoteUrl(modelData)
          if (url !== "") view.owner.openUrl(url)
        }
      }
    }

    Rectangle {
      visible: view.repos.length === 0
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
    text: view.repos.length + " of " + view.allRepos.length + " watched repositories"
    color: Qt.darker(view.foreground, 1.4)
    font.family: view.fontFamily
    font.pixelSize: Style.font.caption
  }
}
