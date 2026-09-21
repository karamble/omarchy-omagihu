pragma ComponentBehavior: Bound

import QtQuick
import QtQuick.Layouts
import qs.Commons
import qs.Ui

// The landing view, ordered by who is blocked: other people first, then your
// own broken work, then the inbox. Every section shows by default; each chip
// folds its own section away and back, independently, and the daemon keeps
// the choice. A folded chip keeps its count, and turns loud when the section
// it hides holds urgent work, so the strip never disagrees with the bar
// about whether something needs you.
Column {
  id: view

  required property var owner
  property var snap: null

  // Section keys the daemon reports as hidden. Anything not listed is shown,
  // so an absent list shows everything.
  readonly property var hiddenSections: snap && snap.hiddenSections ? snap.hiddenSections : []

  function isHidden(key) {
    return view.hiddenSections.indexOf(key) >= 0
  }

  readonly property color foreground: owner.foreground
  readonly property string fontFamily: owner.fontFamily
  // Dims as alpha over the foreground, so they stay lighter than the text on
  // a light theme as well as a dark one.
  readonly property color quiet: Util.alpha(view.foreground, 0.6)
  readonly property color dim: Util.alpha(view.foreground, 0.7)

  readonly property var work: snap && snap.work ? snap.work : null
  readonly property var reviews: work && work.reviewRequests ? work.reviewRequests : []
  readonly property var authored: work && work.authoredPrs ? work.authoredPrs : []
  readonly property var issues: work && work.assignedIssues ? work.assignedIssues : []
  // Issues somebody else opened on a repository you own. They share the
  // assigned list, marked incoming, because nobody could assign them to you.
  readonly property var incoming: {
    var out = []
    for (var i = 0; i < view.issues.length; i++) if (view.issues[i].incoming) out.push(view.issues[i])
    return out
  }
  readonly property int assignedCount: view.issues.length - view.incoming.length
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

  readonly property bool showReviews: !view.isHidden("reviews") && reviews.length > 0
  readonly property bool showIncoming: !view.isHidden("issues") && incoming.length > 0
  readonly property bool showBroken: !view.isHidden("broken") && brokenPrs.length > 0
  readonly property bool showInbox: !view.isHidden("inbox") && inbox.length > 0
  readonly property bool showFacts: !view.isHidden("reconcile") && facts.length > 0
  readonly property bool showOpened: !view.isHidden("issues") && opened.length > 0

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

    if (pr.isDraft) out.push({ text: "DRAFT", tone: view.quiet })
    return out
  }

  // One badge naming the kind of drift, so the row reads without the sentence.
  function factBadge(f) {
    var label = view.owner.factLabel(f.kind)
    // Detached work is a notice so the bar stays on the local tier, but the
    // badge shouts, as it does in the repositories view.
    var loud = f.severity === "urgent" || f.kind === "detached-work"
    return [{ text: label, tone: loud ? Color.urgent : Color.accent, loud: loud }]
  }

  function factIcon(f) {
    if (f.kind === "ci-red-on-head") return view.owner.iconCross
    if (f.kind === "stale-branch") return view.owner.iconCheck
    if (f.kind === "fork-behind") return view.owner.iconDot
    return view.owner.iconWarn
  }

  // GitHub gives each label its own hex, and some repositories choose colours
  // that vanish against the panel: too dark on a dark theme, too pale on a
  // light one. Whichever way the theme reads, the label is pushed the other
  // way until it can be seen. The hue is kept: it is what makes a label
  // recognisable.
  function luma(c) {
    return 0.2126 * c.r + 0.7152 * c.g + 0.0722 * c.b
  }
  readonly property bool lightPanel: view.luma(Color.background) >= 0.5
  function labelTone(hex) {
    var c = Qt.color("#" + String(hex || "888888"))
    var l = view.luma(c)
    if (view.lightPanel) return l <= 0.55 ? c : Qt.darker(c, 1.0 + (l - 0.55) * 2.2)
    return l >= 0.45 ? c : Qt.lighter(c, 1.0 + (0.45 - l) * 2.2)
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
                 tone: view.dim, compact: true })
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
  // Row zero is the chip strip: one action per chip, which folds that section
  // away or back, plus one more for "Show all" while anything is folded. Then
  // every entry the sections left showing, in the order they draw, each with
  // one action, which is to open it on GitHub; then the button at the foot.
  // The urgent flag is what makes a folded chip loud.
  readonly property var filters: [
    { key: "reviews", label: "Reviews", count: view.reviews.length, urgent: view.reviews.length > 0 },
    { key: "broken", label: "Needs fixing", count: view.brokenPrs.length, urgent: view.brokenPrs.length > 0 },
    { key: "inbox", label: "Inbox", count: view.inbox.length, urgent: false },
    { key: "reconcile", label: "Reconcile", count: view.facts.length, urgent: view.urgentFacts > 0 },
    { key: "issues", label: "Issues", count: view.incoming.length + view.opened.length, urgent: view.incoming.length > 0 }
  ]

  // How many of the sections this view knows are folded. A key left in the
  // daemon's list by a section that no longer exists counts for nothing.
  readonly property int hiddenCount: {
    var n = 0
    for (var i = 0; i < view.filters.length; i++) if (view.isHidden(view.filters[i].key)) n++
    return n
  }
  readonly property bool anyHidden: view.hiddenCount > 0

  readonly property int reviewOffset: 1
  readonly property int incomingOffset: view.reviewOffset + (view.showReviews ? view.reviews.length : 0)
  readonly property int brokenOffset: view.incomingOffset + (view.showIncoming ? view.incoming.length : 0)
  readonly property int factOffset: view.brokenOffset + (view.showBroken ? view.brokenPrs.length : 0)
  readonly property int inboxOffset: view.factOffset + (view.showFacts ? view.facts.length : 0)
  readonly property int openedOffset: view.inboxOffset + (view.showInbox ? view.inbox.length : 0)
  readonly property int moreRow: view.openedOffset + (view.showOpened ? view.opened.length : 0)

  readonly property int rowCount: view.moreRow + 1
  readonly property bool formFocused: false

  function actionCount(row) { return row === 0 ? view.filters.length + (view.anyHidden ? 1 : 0) : 1 }

  function toggleSection(key) {
    view.owner.setSectionHidden(key, !view.isHidden(key))
  }

  function showAll() {
    view.owner.setSectionHidden("all", false)
  }

  function activateRow(row, action) {
    if (row === 0) {
      if (action < view.filters.length) view.toggleSection(String(view.filters[action].key))
      else if (view.anyHidden) view.showAll()
      return
    }
    if (row === view.moreRow) { view.owner.setView("repos"); return }

    var url = ""
    if (view.showReviews && row < view.incomingOffset)
      url = view.reviews[row - view.reviewOffset].url
    else if (view.showIncoming && row < view.brokenOffset)
      url = view.incoming[row - view.incomingOffset].url
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
        required property var modelData
        required property int index
        readonly property bool hidden: view.isHidden(modelData.key)
        // The chip's own hue: red while its section holds urgent work.
        readonly property color hue: modelData.urgent ? Color.urgent : Color.accent
        Layout.fillWidth: true
        // Lit means shown, hue means urgent, and the two never mix: a lit
        // chip fills with its own hue at the theme's selected alpha, a
        // folded one is unlit but keeps its hue and its count, so a folded
        // section with urgent work still reads red. The fill goes through
        // background rather than selected because the theme's selected
        // colour token picks one colour for every chip and would paint an
        // urgent chip in the theme's choice.
        text: modelData.label + " (" + modelData.count + ")"
        hasCursor: view.owner.cursor === 0 && view.owner.actionIndex === index
        bordered: true
        background: hidden ? "transparent" : Qt.rgba(hue.r, hue.g, hue.b, Style.selectedFillAlpha)
        foreground: hue
        accent: hue
        fontFamily: view.fontFamily
        fontSize: Style.font.caption
        horizontalPadding: Style.space(8)
        verticalPadding: Style.space(5)
        onClicked: view.toggleSection(modelData.key)
      }
    }

    // Appears only while something is folded, so it is also the sign that
    // something is. Accent rather than plain, so it reads as the way back
    // and not as one more folded chip.
    Button {
      visible: view.anyHidden
      text: "Show all"
      hasCursor: view.owner.cursor === 0 && view.owner.actionIndex === view.filters.length
      bordered: true
      foreground: Color.accent
      accent: Color.accent
      fontFamily: view.fontFamily
      fontSize: Style.font.caption
      horizontalPadding: Style.space(8)
      verticalPadding: Style.space(5)
      onClicked: view.showAll()
    }
  }

  // A section title: a rule, then the section's glyph before its word, in
  // the tone of what it holds. The colour is set here because the shell's
  // header darkens its foreground, which on a light theme makes the title
  // heavier than the rows under it.
  component SectionHead: Column {
    id: head
    property string icon: ""
    property string text: ""
    property color tone: view.quiet
    width: parent ? parent.width : 0
    spacing: Style.space(4)

    PanelSeparator {
      width: parent.width
      foreground: view.foreground
    }

    PanelSectionHeader {
      text: (head.icon !== "" ? head.icon + "  " : "") + head.text
      foreground: head.tone
      color: head.tone
      fontFamily: view.fontFamily
    }
  }

  // ---------- the list. The whole card scrolls, so no inner viewport here:
  // nested Flickables fight each other for the wheel. ----------
  Column {
    id: listColumn
    width: parent.width
    spacing: Style.space(6)

    // ----- reviews requested of you -----
    SectionHead {
      visible: view.showReviews
      icon: view.owner.iconReview
      text: "WAITING ON YOU"
      tone: Color.urgent
    }

    Repeater {
      model: view.showReviews ? view.reviews : []
      delegate: ListRow {
        required property var modelData
        required property int index
        hasCursor: view.owner.cursor === view.reviewOffset + index
        width: listColumn.width
        icon: view.owner.iconPr
        tone: Color.urgent
        urgent: true
        fontFamily: view.fontFamily
        title: view.shortRepo(modelData.repo) + " #" + modelData.number + "  " + modelData.title
        subtitle: "by " + modelData.author + " · " + view.ago(modelData.updatedAt)
        // Both wait on the same person, but only one of them was asked for.
        badges: [{ text: modelData.incoming ? "ON YOURS" : "REVIEW", tone: Color.urgent, loud: true }]
        onActivated: view.owner.openUrl(modelData.url)
      }
    }

    // ----- issues reported on your repositories -----
    SectionHead {
      visible: view.showIncoming
      icon: view.owner.iconIssue
      text: "REPORTED ON YOUR REPOSITORIES"
      tone: Color.urgent
    }

    Repeater {
      model: view.showIncoming ? view.incoming : []
      delegate: ListRow {
        required property var modelData
        required property int index
        hasCursor: view.owner.cursor === view.incomingOffset + index
        width: listColumn.width
        icon: view.owner.iconIssue
        tone: Color.urgent
        urgent: true
        fontFamily: view.fontFamily
        title: view.shortRepo(modelData.repo) + " #" + modelData.number + "  " + modelData.title
        subtitle: "by " + modelData.author + " · " + view.ago(modelData.updatedAt)
        badges: [{ text: "ON YOURS", tone: Color.urgent, loud: true }].concat(view.labelBadges(modelData))
        // Labels are compact and already capped by labelBadges.
        maxBadges: 1 + view.maxLabels + 1
        onActivated: view.owner.openUrl(modelData.url)
      }
    }

    // ----- your pull requests that need you -----
    SectionHead {
      visible: view.showBroken
      icon: view.owner.iconPr
      text: "YOUR PULL REQUESTS NEED ATTENTION"
      tone: Color.urgent
    }

    Repeater {
      model: view.showBroken ? view.brokenPrs : []
      delegate: ListRow {
        required property var modelData
        required property int index
        hasCursor: view.owner.cursor === view.brokenOffset + index
        width: listColumn.width
        icon: view.owner.iconPr
        tone: Color.urgent
        urgent: true
        fontFamily: view.fontFamily
        title: view.shortRepo(modelData.repo) + " #" + modelData.number + "  " + modelData.title
        subtitle: modelData.repo + " · " + modelData.headRef + " · " + view.ago(modelData.updatedAt)
        badges: view.prBadges(modelData)
        onActivated: view.owner.openUrl(modelData.url)
      }
    }

    // ----- where the two planes disagree -----
    SectionHead {
      visible: view.showFacts
      icon: view.owner.iconExchange
      text: "NEEDS RECONCILING"
      tone: view.urgentFacts > 0 ? Color.urgent : view.quiet
    }

    Repeater {
      model: view.showFacts ? view.facts : []
      delegate: ListRow {
        required property var modelData
        required property int index
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
          return bits.join(" · ")
        }
        badges: view.factBadge(modelData)
        onActivated: if (modelData.url) view.owner.openUrl(modelData.url)
      }
    }

    // ----- inbox -----
    SectionHead {
      visible: view.showInbox
      icon: view.owner.iconInbox
      text: "INBOX"
    }

    Repeater {
      model: view.showInbox ? view.inbox : []
      delegate: ListRow {
        required property var modelData
        required property int index
        hasCursor: view.owner.cursor === view.inboxOffset + index
        width: listColumn.width
        icon: modelData.type === "PullRequest" ? view.owner.iconPr : view.owner.iconIssue
        tone: Color.accent
        fontFamily: view.fontFamily
        title: modelData.title
        subtitle: modelData.repo + " · " + view.ago(modelData.updatedAt)
        badges: view.inboxBadge(modelData)
        onActivated: view.owner.openUrl(modelData.webUrl)
      }
    }

    // ----- issues you opened -----
    SectionHead {
      visible: view.showOpened
      icon: view.owner.iconIssue
      text: "ISSUES YOU OPENED"
    }

    Repeater {
      model: view.showOpened ? view.opened : []
      delegate: ListRow {
        required property var modelData
        required property int index
        hasCursor: view.owner.cursor === view.openedOffset + index
        width: listColumn.width
        icon: view.owner.iconIssue
        tone: Color.accent
        fontFamily: view.fontFamily
        title: "#" + modelData.number + "  " + modelData.title
        subtitle: modelData.repo + " · " + view.ago(modelData.updatedAt)
        badges: view.labelBadges(modelData)
        maxBadges: view.maxLabels + 1
        onActivated: view.owner.openUrl(modelData.url)
      }
    }

    // ----- nothing matches -----
    Rectangle {
      visible: !view.showReviews && !view.showIncoming && !view.showBroken
               && !view.showInbox && !view.showFacts && !view.showOpened
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
          text: view.anyHidden ? "Nothing in the sections shown" : "Nothing waiting on you"
          color: view.owner.toneOk
          font.family: view.fontFamily
          font.pixelSize: Style.font.body
          font.bold: true
        }

        Text {
          Layout.alignment: Qt.AlignHCenter
          textFormat: Text.PlainText
          text: view.anyHidden
                ? view.hiddenCount + (view.hiddenCount === 1 ? " section is" : " sections are") + " folded away; Show all brings them back."
                : "Every check green, every review answered."
          color: Util.alpha(view.foreground, 0.55)
          font.family: view.fontFamily
          font.pixelSize: Style.font.caption
        }
      }
    }
  }

  PanelSeparator {
    width: parent.width
    foreground: view.foreground
  }

  // ---------- standing totals: the figures, and the way to the repositories
  // they come from. Unpushed and interrupted appear only while they are
  // non-zero, in the tone the repositories view gives them.
  component Figure: HeroStat {
    foreground: view.foreground
    fontFamily: view.fontFamily
  }

  RowLayout {
    width: parent.width
    spacing: Style.space(8)

    Row {
      Layout.fillWidth: true
      spacing: Style.space(18)

      Figure {
        value: String(view.authored.length)
        label: "open PRs"
        icon: view.owner.iconPr
      }

      Figure {
        value: String(view.assignedCount)
        label: "issues assigned"
        icon: view.owner.iconIssue
      }

      Figure {
        visible: !!view.att && view.att.reposAtRisk > 0
        value: view.att ? String(view.att.unpushedTotal) : "0"
        label: "unpushed"
        icon: view.owner.iconBranch
        valueColor: Color.accent
      }

      Figure {
        visible: !!view.att && view.att.interrupted > 0
        value: view.att ? String(view.att.interrupted) : "0"
        label: "interrupted"
        icon: view.owner.iconWarn
        valueColor: Color.urgent
      }
    }

    Button {
      text: "Repositories"
      iconText: view.owner.iconGit
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
