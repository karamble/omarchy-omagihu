import QtQuick
import QtQuick.Layouts
import qs.Commons
import qs.Ui

// The landing view, ordered by who is blocked: other people first, then your
// own broken work, then the inbox. Filter chips carry their own counts so the
// shape of the backlog is visible before anything is clicked.
Column {
  id: view

  required property var owner
  property var snap: null

  // "all", "reviews", "broken", "inbox"
  property string filter: "all"

  readonly property color foreground: owner.foreground
  readonly property string fontFamily: owner.fontFamily

  readonly property var work: snap && snap.work ? snap.work : null
  readonly property var reviews: work && work.reviewRequests ? work.reviewRequests : []
  readonly property var authored: work && work.authoredPrs ? work.authoredPrs : []
  readonly property var issues: work && work.assignedIssues ? work.assignedIssues : []
  // Issues you opened. Their labels are where a submission's state lives, which
  // is invisible from the notification alone.
  readonly property var allOpened: work && work.authoredIssues ? work.authoredIssues : []
  // An issue you filed years ago and nobody has touched is not dashboard
  // material. The daemon keeps the whole list for agents to read; this page
  // shows the ones that have actually moved.
  readonly property int openedWindowDays: 7
  readonly property var opened: {
    var out = []
    var cutoff = Date.now() - view.openedWindowDays * 86400000
    for (var i = 0; i < view.allOpened.length; i++) {
      var at = Date.parse(view.allOpened[i].updatedAt)
      if (!isNaN(at) && at >= cutoff) out.push(view.allOpened[i])
    }
    return out
  }
  readonly property var inbox: snap && snap.inbox ? snap.inbox : []
  readonly property var att: snap && snap.attention ? snap.attention : null
  readonly property var facts: snap && snap.facts ? snap.facts : []
  readonly property int urgentFacts: {
    var n = 0
    for (var i = 0; i < view.facts.length; i++) if (view.facts[i].severity === "urgent") n++
    return n
  }

  readonly property var brokenPrs: {
    var out = []
    for (var i = 0; i < view.authored.length; i++) {
      var pr = view.authored[i]
      if (pr.checksState === "FAILURE" || pr.checksState === "ERROR" || pr.reviewDecision === "CHANGES_REQUESTED")
        out.push(pr)
    }
    return out
  }

  readonly property bool showReviews: (filter === "all" || filter === "reviews") && reviews.length > 0
  readonly property bool showBroken: (filter === "all" || filter === "broken") && brokenPrs.length > 0
  readonly property bool showInbox: (filter === "all" || filter === "inbox") && inbox.length > 0
  readonly property bool showFacts: (filter === "all" || filter === "reconcile") && facts.length > 0
  readonly property bool showOpened: (filter === "all" || filter === "opened") && opened.length > 0

  spacing: Style.space(10)

  function shortRepo(full) {
    if (!full) return ""
    var parts = String(full).split("/")
    return parts.length > 1 ? parts[1] : String(full)
  }

  function ago(iso) {
    if (!iso) return ""
    var then = Date.parse(iso)
    if (isNaN(then)) return ""
    var secs = Math.max(0, Math.round((Date.now() - then) / 1000))
    if (secs < 90) return "just now"
    if (secs < 3600) return Math.round(secs / 60) + "m ago"
    if (secs < 86400) return Math.round(secs / 3600) + "h ago"
    return Math.round(secs / 86400) + "d ago"
  }

  // Badges for one of your pull requests: verdict first, then review state.
  function prBadges(pr) {
    var out = []
    if (pr.checksState === "FAILURE" || pr.checksState === "ERROR")
      out.push({ text: "CI FAILED", tone: Color.urgent, loud: true })
    else if (pr.checksState === "SUCCESS")
      out.push({ text: "CI PASS", tone: view.owner.toneOk })
    else if (pr.checksState === "PENDING")
      out.push({ text: "CI RUNNING", tone: Color.accent })

    if (pr.reviewDecision === "CHANGES_REQUESTED")
      out.push({ text: "CHANGES", tone: Color.urgent, loud: true })
    else if (pr.reviewDecision === "APPROVED")
      out.push({ text: "APPROVED", tone: view.owner.toneOk })

    if (pr.isDraft) out.push({ text: "DRAFT", tone: Qt.darker(view.foreground, 1.4) })
    return out
  }

  // One badge naming the kind of drift, so the row reads without the sentence.
  function factBadge(f) {
    var label = {
      "missing-work": "MISSING WORK",
      "ci-red-on-head": "CI RED HERE",
      "changes-requested": "CHANGES",
      "stale-branch": "MERGED",
      "fork-behind": "BEHIND"
    }[f.kind] || String(f.kind).toUpperCase()
    return [{ text: label, tone: f.severity === "urgent" ? Color.urgent : Color.accent,
              loud: f.severity === "urgent" }]
  }

  function factIcon(f) {
    if (f.kind === "ci-red-on-head") return view.owner.iconCross
    if (f.kind === "stale-branch") return view.owner.iconCheck
    if (f.kind === "fork-behind") return view.owner.iconDot
    return view.owner.iconWarn
  }

  // GitHub gives each label its own hex, and some repositories choose colours
  // that vanish against a dark panel, so anything too dark is lifted until it
  // can be read. The hue is kept: it is what makes a label recognisable.
  function labelTone(hex) {
    var c = Qt.color("#" + String(hex || "888888"))
    var luma = 0.2126 * c.r + 0.7152 * c.g + 0.0722 * c.b
    if (luma >= 0.45) return c
    return Qt.lighter(c, 1.0 + (0.45 - luma) * 2.2)
  }

  // Four bubbles is what fits beside a title on this card. Anything past that
  // is counted rather than dropped silently, and the row opens the issue.
  readonly property int maxLabels: 4

  function labelBadges(issue) {
    var out = []
    var labels = issue.labels || []
    var shown = Math.min(labels.length, view.maxLabels)
    for (var i = 0; i < shown; i++)
      out.push({ text: labels[i].name, tone: view.labelTone(labels[i].color), compact: true })
    if (labels.length > shown)
      out.push({ text: "+" + (labels.length - shown),
                 tone: Qt.darker(view.foreground, 1.3), compact: true })
    return out
  }

  function inboxBadge(n) {
    var reason = String(n.reason || "").toUpperCase().replace("_", " ")
    var tone = Color.accent
    if (n.reason === "review_requested" || n.reason === "mention" || n.reason === "assign")
      tone = Color.urgent
    if (n.reason === "ci_activity") tone = view.owner.toneOk
    return [{ text: reason, tone: tone }]
  }

  // ---------- the keyboard contract ----------
  //
  // Row zero is the chip strip; then every entry the filter left showing, in
  // the order the sections draw them; then the button at the foot. Each entry
  // carries one action, which is to open it on GitHub.
  readonly property var filters: [
    { key: "all", label: "Everything", count: view.reviews.length + view.brokenPrs.length + view.inbox.length, urgent: false },
    { key: "reviews", label: "Reviews", count: view.reviews.length, urgent: view.reviews.length > 0 },
    { key: "broken", label: "Needs fixing", count: view.brokenPrs.length, urgent: view.brokenPrs.length > 0 },
    { key: "inbox", label: "Inbox", count: view.inbox.length, urgent: false },
    { key: "reconcile", label: "Reconcile", count: view.facts.length, urgent: view.urgentFacts > 0 },
    { key: "opened", label: "Opened", count: view.opened.length, urgent: false }
  ]

  readonly property int reviewOffset: 1
  readonly property int brokenOffset: view.reviewOffset + (view.showReviews ? view.reviews.length : 0)
  readonly property int factOffset: view.brokenOffset + (view.showBroken ? view.brokenPrs.length : 0)
  readonly property int inboxOffset: view.factOffset + (view.showFacts ? view.facts.length : 0)
  readonly property int openedOffset: view.inboxOffset + (view.showInbox ? view.inbox.length : 0)
  readonly property int moreRow: view.openedOffset + (view.showOpened ? view.opened.length : 0)

  readonly property int rowCount: view.moreRow + 1
  readonly property bool formFocused: false

  function actionCount(row) { return row === 0 ? view.filters.length : 1 }

  function activateRow(row, action) {
    if (row === 0) {
      view.filter = String(view.filters[action].key)
      return
    }
    if (row === view.moreRow) { view.owner.setView("repos"); return }

    var url = ""
    if (view.showReviews && row < view.brokenOffset)
      url = view.reviews[row - view.reviewOffset].url
    else if (view.showBroken && row < view.factOffset)
      url = view.brokenPrs[row - view.brokenOffset].url
    else if (view.showFacts && row < view.inboxOffset)
      url = view.facts[row - view.factOffset].url
    else if (view.showInbox && row < view.openedOffset)
      url = view.inbox[row - view.inboxOffset].webUrl
    else if (view.showOpened)
      url = view.opened[row - view.openedOffset].url
    if (url) view.owner.openUrl(url)
  }

  // ---------- filter chips ----------
  RowLayout {
    width: parent.width
    spacing: Style.space(6)

    Repeater {
      model: view.filters
      delegate: Button {
        Layout.fillWidth: true
        text: modelData.label + " (" + modelData.count + ")"
        selected: view.filter === modelData.key
        hasCursor: view.owner.cursor === 0 && view.owner.actionIndex === index
        bordered: true
        foreground: modelData.urgent ? Color.urgent : view.foreground
        accent: modelData.urgent ? Color.urgent : Color.accent
        fontFamily: view.fontFamily
        fontSize: Style.font.caption
        horizontalPadding: Style.space(8)
        verticalPadding: Style.space(5)
        onClicked: view.filter = modelData.key
      }
    }
  }

  // ---------- the list. The whole card scrolls, so no inner viewport here:
  // nested Flickables fight each other for the wheel. ----------
  Column {
    id: listColumn
    width: parent.width
    spacing: Style.space(6)

    // ----- reviews requested of you -----
    PanelSectionHeader {
      visible: view.showReviews
      text: "WAITING ON YOU"
      foreground: Color.urgent
      fontFamily: view.fontFamily
    }

    Repeater {
      model: view.showReviews ? view.reviews : []
      delegate: ListRow {
        hasCursor: view.owner.cursor === view.reviewOffset + index
        width: listColumn.width
        icon: view.owner.iconBranch
        tone: Color.urgent
        urgent: true
        fontFamily: view.fontFamily
        title: view.shortRepo(modelData.repo) + " #" + modelData.number + "  " + modelData.title
        subtitle: "by " + modelData.author + " • " + view.ago(modelData.updatedAt)
        badges: [{ text: "REVIEW", tone: Color.urgent, loud: true }]
        onActivated: view.owner.openUrl(modelData.url)
      }
    }

    // ----- your pull requests that need you -----
    PanelSectionHeader {
      visible: view.showBroken
      text: "YOUR PULL REQUESTS NEED ATTENTION"
      foreground: Color.urgent
      fontFamily: view.fontFamily
    }

    Repeater {
      model: view.showBroken ? view.brokenPrs : []
      delegate: ListRow {
        hasCursor: view.owner.cursor === view.brokenOffset + index
        width: listColumn.width
        icon: view.owner.iconCross
        tone: Color.urgent
        urgent: true
        fontFamily: view.fontFamily
        title: view.shortRepo(modelData.repo) + " #" + modelData.number + "  " + modelData.title
        subtitle: modelData.repo + " • " + modelData.headRef + " • " + view.ago(modelData.updatedAt)
        badges: view.prBadges(modelData)
        onActivated: view.owner.openUrl(modelData.url)
      }
    }

    // ----- where the two planes disagree -----
    PanelSectionHeader {
      visible: view.showFacts
      text: "NEEDS RECONCILING"
      foreground: view.urgentFacts > 0 ? Color.urgent : view.foreground
      fontFamily: view.fontFamily
    }

    Repeater {
      model: view.showFacts ? view.facts : []
      delegate: ListRow {
        hasCursor: view.owner.cursor === view.factOffset + index
        width: listColumn.width
        icon: view.factIcon(modelData)
        tone: modelData.severity === "urgent" ? Color.urgent : Color.accent
        urgent: modelData.severity === "urgent"
        fontFamily: view.fontFamily
        title: modelData.summary
        subtitle: {
          var bits = []
          if (modelData.repo) bits.push(modelData.repo)
          if (modelData.branch) bits.push(modelData.branch)
          if (modelData.detail) bits.push(modelData.detail)
          return bits.join(" • ")
        }
        badges: view.factBadge(modelData)
        onActivated: if (modelData.url) view.owner.openUrl(modelData.url)
      }
    }

    // ----- inbox -----
    PanelSectionHeader {
      visible: view.showInbox
      text: "INBOX"
      foreground: view.foreground
      fontFamily: view.fontFamily
    }

    Repeater {
      model: view.showInbox ? view.inbox : []
      delegate: ListRow {
        hasCursor: view.owner.cursor === view.inboxOffset + index
        width: listColumn.width
        icon: modelData.type === "PullRequest" ? view.owner.iconBranch : view.owner.iconDot
        tone: Color.accent
        fontFamily: view.fontFamily
        title: modelData.title
        subtitle: modelData.repo + " • " + view.ago(modelData.updatedAt)
        badges: view.inboxBadge(modelData)
        onActivated: view.owner.openUrl(modelData.webUrl)
      }
    }

    // ----- issues you opened -----
    PanelSectionHeader {
      visible: view.showOpened
      text: "ISSUES YOU OPENED"
      foreground: view.foreground
      fontFamily: view.fontFamily
    }

    Repeater {
      model: view.showOpened ? view.opened : []
      delegate: ListRow {
        hasCursor: view.owner.cursor === view.openedOffset + index
        width: listColumn.width
        icon: view.owner.iconDot
        tone: Color.accent
        fontFamily: view.fontFamily
        title: "#" + modelData.number + "  " + modelData.title
        subtitle: modelData.repo + " • " + view.ago(modelData.updatedAt)
        badges: view.labelBadges(modelData)
        onActivated: view.owner.openUrl(modelData.url)
      }
    }

    // ----- nothing matches -----
    Rectangle {
      visible: !view.showReviews && !view.showBroken && !view.showInbox
               && !view.showFacts && !view.showOpened
      width: listColumn.width
      implicitHeight: Style.space(70)
      radius: Style.cornerRadius > 0 ? Style.space(6) : 0
      color: Qt.rgba(view.foreground.r, view.foreground.g, view.foreground.b, 0.04)

      ColumnLayout {
        anchors.centerIn: parent
        spacing: Style.space(4)

        Text {
          Layout.alignment: Qt.AlignHCenter
          textFormat: Text.PlainText
          text: view.filter === "all" ? "Nothing waiting on you" : "Nothing in this filter"
          color: view.owner.toneOk
          font.family: view.fontFamily
          font.pixelSize: Style.font.body
          font.bold: true
        }

        Text {
          Layout.alignment: Qt.AlignHCenter
          textFormat: Text.PlainText
          text: view.filter === "all"
                ? "Every check green, every review answered."
                : "Switch to Everything to see the rest."
          color: Qt.darker(view.foreground, 1.5)
          font.family: view.fontFamily
          font.pixelSize: Style.font.caption
        }
      }
    }
  }

  PanelSeparator { width: parent.width }

  // ---------- standing totals ----------
  RowLayout {
    width: parent.width
    spacing: Style.space(8)

    Text {
      Layout.fillWidth: true
      textFormat: Text.PlainText
      text: view.authored.length + " open PRs • " + view.issues.length + " issues assigned"
      color: Qt.darker(view.foreground, 1.3)
      font.family: view.fontFamily
      font.pixelSize: Style.font.caption
      elide: Text.ElideRight
    }

    Badge {
      visible: view.att && view.att.reposAtRisk > 0
      text: view.att ? (view.att.unpushedTotal + " UNPUSHED") : ""
      tone: Color.accent
      fontFamily: view.fontFamily
    }

    Badge {
      visible: view.att && view.att.interrupted > 0
      text: view.att ? (view.att.interrupted + " INTERRUPTED") : ""
      tone: Color.urgent
      loud: true
      fontFamily: view.fontFamily
    }

    Button {
      text: "Repositories"
      iconText: view.owner.iconWarn
      hasCursor: view.owner.cursor === view.moreRow
      bordered: true
      foreground: view.foreground
      accent: Color.accent
      fontFamily: view.fontFamily
      fontSize: Style.font.caption
      onClicked: view.owner.setView("repos")
    }
  }
}
