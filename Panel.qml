import QtQuick
import QtQuick.Layouts
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Ui

// Omagihu is developer and account centric: the bar answers "does anything want
// me right now", and the panel opens on the dashboard, which is the mission
// critical view. All the work happens in bin/omagihud; this reads one JSON
// document from bin/omagihu and renders it.
Panel {
  id: root
  moduleName: "karamble.omagihu"
  ipcTarget: "karamble.omagihu"

  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  // ---- settings, every fallback mirrors a manifest default
  readonly property string addr: setting("addr", "127.0.0.1:8099")
  readonly property int refreshSec: Math.max(5, Number(setting("refreshSec", 60)))
  readonly property bool countLocalRisk: setting("countLocalRisk", true) === true

  // ---- state fed by the helper
  property var snap: null
  // Which tab the card is showing. A string because there are four of them.
  property string view: "dashboard"

  // The keyboard cursor into the active view's rows; -1 means nothing is under
  // it and the panel is being driven by shortcuts or the mouse. It is a plain
  // property rather than focus, because a row is a drawing, not a control: the
  // row that holds it is found by asking who has hasCursor.
  property int cursor: -1
  // Which of the current row's actions is under the cursor. h and l walk these.
  property int actionIndex: 0
  onCursorChanged: {
    root.actionIndex = 0
    Qt.callLater(root.revealCursor)
  }
  // Every view answers the same small contract: rowCount, actionCount(row),
  // activateRow(row, action) and formFocused. The panel never learns what a
  // row is.
  readonly property var activeView: viewLoader.item

  function clampCursor() {
    if (!root.activeView) { root.cursor = -1; return }
    if (root.cursor >= root.activeView.rowCount)
      root.cursor = root.activeView.rowCount - 1
  }
  onSnapChanged: root.clampCursor()

  // cursorItem walks down for whoever is painting the cursor, so it can be
  // scrolled into view. The first hit wins, and a row is checked before the
  // buttons inside it.
  function cursorItem(item) {
    if (!item || !item.visible) return null
    if (item.hasCursor === true) return item
    var kids = item.children || []
    for (var i = 0; i < kids.length; i++) {
      var hit = root.cursorItem(kids[i])
      if (hit) return hit
    }
    return null
  }

  function revealCursor() {
    if (root.cursor < 0) return
    flick.revealItem(root.cursorItem(contentCol))
  }

  // moveCursor is the whole of the navigation: up and down walk rows, left and
  // right walk whatever that row carries.
  function moveCursor(dx, dy) {
    if (!root.activeView) return
    if (dy !== 0) {
      var n = root.activeView.rowCount
      if (n <= 0) return
      root.cursor = root.cursor < 0 ? (dy > 0 ? 0 : n - 1)
                                    : (root.cursor + dy + n) % n
      return
    }
    if (dx !== 0) {
      var actions = root.activeView.actionCount(root.cursor)
      if (actions <= 1) return
      root.actionIndex = (root.actionIndex + dx + actions) % actions
    }
  }

  function activateCursor() {
    if (!root.activeView || root.cursor < 0) return
    root.activeView.activateRow(root.cursor, root.actionIndex)
  }

  // Focus comes back to the key catcher whenever a form lets go of it, so the
  // letters start working again the moment the form is gone.
  function reclaimKeys() {
    if (keyCatcher) keyCatcher.forceActiveFocus()
  }
  // Set when the helper cannot be started at all. bin/ is deliberately not
  // committed, so a fresh clone lands here and has to say so rather than sit
  // on "connecting" for ever.
  property bool helperMissing: false
  property string lastError: ""

  readonly property color foreground: bar ? bar.foreground : Color.foreground
  readonly property string fontFamily: bar ? bar.fontFamily : Style.font.family

  readonly property var alerts: snap && snap.alerts ? snap.alerts : []
  // The catalogue and the herdr roster only matter on the alerts board, so they
  // are read when it opens rather than on every refresh.
  property var catalogue: []
  property var agents: []
  property string agentNote: ""
  property string armError: ""
  // What the bell has to answer for: watches that are still waiting.
  readonly property int armedCount: {
    var n = 0
    for (var i = 0; i < root.alerts.length; i++) {
      var st = String(root.alerts[i].status || "")
      if (st === "armed" || st === "rearming" || st === "no-sample") n++
    }
    return n
  }

  readonly property var attention: snap && snap.attention ? snap.attention : null
  readonly property var health: snap && snap.health ? snap.health : null
  // The daemon owns these; the panel only reflects and requests changes.
  readonly property bool monitoring: root.health ? root.health.monitoring === true : true
  readonly property int intervalMin: root.health && root.health.intervalMin > 0 ? root.health.intervalMin : 5
  readonly property bool mcpEnabled: root.health ? root.health.mcpEnabled === true : false
  readonly property bool fetchEnabled: root.health ? root.health.fetchEnabled === true : true
  readonly property int fetchMin: root.health && root.health.fetchMin > 0 ? root.health.fetchMin : 30
  readonly property var notifyPrefs: root.health && root.health.notify ? root.health.notify : null

  function notifyEnabled(domain) {
    if (!root.notifyPrefs) return false
    return root.notifyPrefs[domain] === true
  }

  // The freshly minted bearer token, held only until the panel closes. It is
  // shown once, at the moment it is created, because that is when it has to be
  // copied into ~/.claude.json.
  property string newToken: ""
  property string newEntry: "" 
  readonly property string level: {
    // Asleep is its own state: nothing is being watched, so nothing may shout.
    if (!root.monitoring) return "asleep"
    if (!root.attention) return "clear"
    // Local risk is a notice, and the user can silence it entirely.
    if (root.attention.level === "notice" && !root.countLocalRisk) return "clear"
    return root.attention.level
  }
  readonly property int barCount: root.level === "clear" ? 0 : (root.attention ? root.attention.count : 0)

  // Nerd Font glyphs, written as real characters because an escape sequence is
  // not decoded here. F126 is the branch mark, F021 the refresh arrow.
  readonly property string iconBranch: ""
  readonly property string iconRefresh: ""
  readonly property string iconCheck: ""
  readonly property string iconCross: ""
  readonly property string iconClock: ""
  readonly property string iconWarn: ""
  readonly property string iconDot: ""
  readonly property string iconBell: ""
  readonly property string iconCog: ""
  readonly property string iconPlus: ""
  readonly property string iconTrash: ""

  // Green is not in the theme palette, and "passing" needs to read as distinct
  // from the accent colour used for merely informational rows.
  readonly property color toneOk: "#22c55e"

  readonly property string pluginDir: Qt.resolvedUrl(".").toString().replace(/^file:\/\//, "").replace(/\/$/, "")
  readonly property string helperPath: pluginDir + "/bin/omagihu"

  // Re-read the daemon's current view, checking first that there is still a
  // helper to read it with.
  function refresh() {
    if (!helperProbe.running) helperProbe.running = true
    if (!fetchProc.running) fetchProc.running = true
  }

  // Ask the daemon to go and look now, then re-read. The button says Refresh,
  // so it has to mean the data and not just the view.
  function refreshNow() {
    if (controlProc.running) {
      root.refresh()
      return
    }
    controlProc.args = ["refresh", "--addr", root.addr]
    controlProc.running = true
  }

  function setView(name) {
    root.view = name
    root.cursor = -1
    flick.contentY = 0
    if (name === "alerts") root.loadAlertsData()
  }

  // The catalogue is fixed for the life of the daemon; the herdr roster is not,
  // so both are re-read whenever the board is opened.
  function loadAlertsData() {
    root.armError = ""
    if (!catalogueProc.running) catalogueProc.running = true
    if (!agentsProc.running) agentsProc.running = true
  }

  // Arming takes the whole trigger as one JSON document, so the panel and the
  // command line put the same thing on the wire.
  function armTrigger(doc) {
    if (alertProc.running) return
    root.armError = ""
    alertProc.args = ["arm", JSON.stringify(doc), "--addr", root.addr]
    alertProc.running = true
  }

  // Editing keeps the watch's id and its owner: it is the same question, asked
  // differently, so the daemon merges rather than replacing.
  function editTrigger(id, doc) {
    if (alertProc.running) return
    root.armError = ""
    alertProc.args = ["edit", id, JSON.stringify(doc), "--addr", root.addr]
    alertProc.running = true
  }

  function disarmTrigger(id) {
    if (alertProc.running) return
    root.armError = ""
    alertProc.args = ["disarm", id, "--addr", root.addr]
    alertProc.running = true
  }

  // The kill switch, as one key. Waking restores the rhythm the daemon already
  // holds rather than inventing a new one.
  function toggleMonitoring() {
    root.setRhythm(root.monitoring ? 0 : root.intervalMin)
  }

  // Rhythm changes go to the daemon, which owns the schedule and persists it.
  // A minutes value of zero means stop polling entirely.
  function setRhythm(minutes) {
    if (controlProc.running) return
    controlProc.args = minutes > 0
      ? ["every", String(minutes), "--addr", root.addr]
      : ["sleep", "--addr", root.addr]
    controlProc.running = true
  }

  // The MCP endpoint is served by the daemon on the same listener and behind
  // the same bearer token, so this only decides whether it answers at all.
  // One domain of desktop notification at a time, so a noisy one can be muted
  // without silencing the ones that mean somebody is waiting.
  // Copying goes through the helper, which reads the value from the 0600 store
  // and pipes it to wl-copy on stdin. Passing a token through argv would put it
  // where any process could read it.
  property string copied: ""
  function copyToClipboard(what) {
    if (copyProc.running) return
    copyProc.what = what
    copyProc.running = true
  }

  function setNotify(domain, on) {
    if (controlProc.running) return
    controlProc.args = ["notify", domain, on ? "on" : "off", "--addr", root.addr]
    controlProc.running = true
  }

  // Background fetch keeps "unpushed" and "behind" honest. Without it those
  // counts drift, because a commit looks unpushed until the local copy of the
  // remote refs catches up.
  function setFetch(on, everyMin) {
    if (controlProc.running) return
    controlProc.args = everyMin > 0
      ? ["fetch", on ? "on" : "off", String(everyMin), "--addr", root.addr]
      : ["fetch", on ? "on" : "off", "--addr", root.addr]
    controlProc.running = true
  }

  function setMCP(on) {
    if (controlProc.running) return
    controlProc.args = ["mcp", on ? "on" : "off", "--addr", root.addr]
    controlProc.running = true
  }

  // Recycling mints a new bearer token, which locks out every client holding
  // the old one. That is the point of it.
  function recycleToken() {
    if (controlProc.running) return
    root.newToken = ""
    root.newEntry = ""
    controlProc.args = ["recycle", "--addr", root.addr]
    controlProc.running = true
  }

  function forgetToken() {
    root.newToken = ""
    root.newEntry = ""
  }

  // Anything needing a prompt or a build runs in a floating terminal, the way
  // the rest of Omarchy does it. The launcher takes one shell string.
  function runSetup(command) {
    if (setupProc.running) return
    setupProc.command = ["omarchy-launch-floating-terminal-with-presentation", command]
    setupProc.running = true
  }

  // First run: build the helpers, then install the service. One click instead
  // of a wall of text to retype.
  function runBuildAndInstall() {
    root.runSetup("cd " + root.pluginDir + " && make && ./bin/omagihu-setup install")
  }

  function runSetupCommand(sub) {
    // Without the helpers this would hand the user a bare shell error, so send
    // them to the one thing that fixes it instead.
    if (root.helperMissing) {
      root.runBuildAndInstall()
      return
    }
    root.runSetup("cd " + root.pluginDir + " && ./bin/omagihu-setup " + sub)
  }

  function openUrl(url) {
    if (!url) return
    openProc.url = url
    openProc.running = true
  }

  Component.onCompleted: refresh()
  onOpenedChanged: {
    if (opened) refresh()
    // Closing the card forgets the token, so it is not sitting on screen the
    // next time the panel is opened.
    else root.forgetToken()
  }

  Timer {
    interval: root.refreshSec * 1000
    running: true
    repeat: true
    onTriggered: root.refresh()
  }

  // Whether the helper exists is asked directly, because inferring it from the
  // daemon is wrong in the case that actually bites: omarchy plugin add
  // re-clones the folder and deletes bin/ while the daemon keeps running from
  // the file it already opened. The panel then looks healthy and every button
  // that shells out fails with a bare "No such file or directory".
  Process {
    id: helperProbe
    command: ["test", "-x", root.helperPath]
    onExited: function(code, status) {
      root.helperMissing = code !== 0
      if (root.helperMissing) root.snap = null
    }
  }

  Process {
    id: fetchProc
    command: [root.helperPath, "dashboard", "--addr", root.addr]

    onExited: function(code, status) {
      // Quickshell reports a binary that will not start as a non-zero exit with
      // no output, which is exactly the unbuilt case.
      if (code !== 0 && !root.snap) root.helperMissing = true
    }

    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        var raw = String(text || "")
        if (raw.trim() === "") return
        try {
          root.snap = JSON.parse(raw)
          root.helperMissing = false
          root.lastError = ""
        } catch (e) {
          root.lastError = "unreadable helper output"
        }
      }
    }

    stderr: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        var msg = String(text || "").trim()
        if (msg !== "") root.lastError = msg.split("\n")[0]
      }
    }
  }

  Process {
    id: controlProc
    property var args: []
    command: [root.helperPath].concat(controlProc.args)
    // Re-read straight away so the chips reflect what the daemon actually did
    // rather than what was asked for.
    onExited: root.refresh()

    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        var raw = String(text || "").trim()
        if (raw === "") return
        try {
          var reply = JSON.parse(raw)
          if (reply.token) {
            root.newToken = String(reply.token)
            root.newEntry = JSON.stringify(reply.entry, null, 2)
          }
        } catch (e) {
          // Every other control command answers with something we do not need.
        }
      }
    }
  }

  // Arming and disarming answer with a reason when they refuse, and that reason
  // is the only useful thing on screen when a watch will not take.
  Process {
    id: alertProc
    property var args: []
    command: [root.helperPath].concat(alertProc.args)
    onExited: function(code, status) {
      if (code === 0) root.armError = ""
      root.refresh()
    }

    stderr: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        var msg = String(text || "").trim()
        if (msg !== "") root.armError = msg.replace(/^omagihu: /, "").split("\n")[0]
      }
    }
  }

  Process {
    id: catalogueProc
    command: [root.helperPath, "catalogue", "--addr", root.addr]
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        try {
          var reply = JSON.parse(String(text || ""))
          root.catalogue = reply.paths || []
        } catch (e) { }
      }
    }
  }

  Process {
    id: agentsProc
    command: [root.helperPath, "agents", "--addr", root.addr]
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        try {
          var reply = JSON.parse(String(text || ""))
          root.agents = reply.agents || []
          root.agentNote = String(reply.note || "")
        } catch (e) { }
      }
    }
  }

  Process {
    id: copyProc
    property string what: "entry"
    command: [root.helperPath, "clip", copyProc.what, "--addr", root.addr]
    onExited: function(code, status) {
      root.copied = code === 0 ? copyProc.what : ""
      copiedReset.restart()
    }
  }

  // The "copied" acknowledgement is transient: it confirms the action and then
  // gets out of the way.
  Timer {
    id: copiedReset
    interval: 2500
    onTriggered: root.copied = ""
  }

  Process {
    id: setupProc
    // The terminal owns the interaction; when it closes, re-read whatever it
    // changed.
    onExited: root.refresh()
  }

  Process {
    id: openProc
    property string url: ""
    command: [root.helperPath, "open", openProc.url]
  }

  BarIconButton {
    id: button
    anchors.fill: parent
    bar: root.bar
    // BarIconButton renders text through OpticalGlyph, a single glyph canvas
    // with labelVisible false, so the bar carries the mark alone. The counts
    // live in the tooltip and the panel.
    text: root.iconBranch
    active: root.level === "urgent" || root.level === "warn"
    foreground: {
      if (root.helperMissing) return Qt.darker(root.foreground, 1.6)
      if (root.level === "asleep") return Qt.darker(root.foreground, 2.0)
      if (root.level === "urgent") return Color.urgent
      if (root.level === "warn") return Color.accent
      if (root.level === "notice") return Qt.darker(root.foreground, 1.2)
      return root.foreground
    }
    tooltipText: {
      if (root.helperMissing) return "Omagihu: not built yet, run make in the plugin directory"
      if (!root.snap) return "Omagihu: connecting"
      if (!root.monitoring) return "Omagihu: asleep\nNothing is being polled and no data leaves this machine."
      var a = root.attention
      if (!a) return "Omagihu"
      var lines = [a.summary]
      if (a.reviews > 0) lines.push(a.reviews + " reviews requested")
      if (a.brokenPrs > 0) lines.push(a.brokenPrs + " of your PRs failing or sent back")
      if (a.unread > 0) lines.push(a.unread + " unread")
      if (a.reposAtRisk > 0) lines.push(a.reposAtRisk + " repos with local work, " + a.unpushedTotal + " unpushed commits")
      return "Omagihu\n" + lines.join("\n")
    }
    onPressed: function(b) {
      if (root.opened) root.close()
      else root.open()
    }
  }

  KeyboardPanel {
    id: panel
    anchorItem: button
    owner: root
    bar: root.bar
    open: root.opened
    focusTarget: keyCatcher
    contentWidth: panel.fittedContentWidth(Style.space(560))
    // The card grows with its content up to the screen; past that the content
    // scrolls rather than painting through the border.
    contentHeight: panel.fittedContentHeight(contentCol.implicitHeight, Style.space(820))

    // Every key the panel answers to arrives here first, before any focused
    // control, which is what lets a bare letter mean something. A form sets
    // blocked and the catcher stands aside so typing is just typing.
    PanelKeyCatcher {
      id: keyCatcher
      anchors.fill: parent
      blocked: !!root.activeView && root.activeView.formFocused === true

      onCloseRequested: {
        // A form is the innermost thing on screen, so Escape puts that away
        // first and only closes the card once there is nothing left inside it.
        if (root.activeView && root.activeView.formOpen === true)
          root.activeView.cancelForm()
        else root.close()
      }
      onTabRequested: function (direction) { root.switchPanel(direction) }
      onMoveRequested: function (dx, dy) { root.moveCursor(dx, dy) }
      onActivateRequested: root.activateCursor()
      onDeleteRequested: {
        // x removes, but only where there is something to remove.
        if (root.view === "alerts" && root.activeView && root.cursor >= 0)
          root.activeView.deleteRow(root.cursor)
      }

      // Omarchy is keyboard first, so everything the panel does has a key and
      // the mouse is a convenience rather than the only way in.
      onTextKey: function (t) {
        switch (String(t).toLowerCase()) {
        case "d":
          root.setView("dashboard")
          break
        case "c":
          root.setView("repos")
          break
        case "a":
          root.setView(root.view === "alerts" ? "dashboard" : "alerts")
          break
        case "s":
          root.setView(root.view === "settings" ? "dashboard" : "settings")
          break
        case "r":
          root.refreshNow()
          // On the alerts board a refresh also re-reads the catalogue and who
          // is there to be woken.
          if (root.view === "alerts") root.loadAlertsData()
          break
        case "m":
          root.toggleMonitoring()
          break
        case "n":
          if (root.view === "alerts" && root.activeView) root.activeView.beginAdd()
          break
        }
      }
      Flickable {
        id: flick
        anchors.fill: parent
        contentWidth: width
        contentHeight: contentCol.implicitHeight
        interactive: contentHeight > height
        boundsBehavior: Flickable.StopAtBounds
        clip: true

        // reveal scrolls just far enough for the band y..y+h of the column to
        // sit inside the viewport, with a little air around it.
        function reveal(y, h) {
          var pad = Style.space(8)
          var maxY = Math.max(0, contentHeight - height)
          if (maxY === 0) { contentY = 0; return }
          if (y < contentY + pad) contentY = Math.max(0, y - pad)
          else if (y + h > contentY + height - pad) contentY = Math.min(maxY, y + h - height + pad)
        }
        // revealItem does the same for anything anywhere inside the column.
        function revealItem(item) {
          if (!item) return
          var p = item
          while (p && p !== contentCol) p = p.parent
          if (!p) return
          var r = item.mapToItem(contentCol, 0, 0)
          reveal(r.y, item.height)
        }
        // A form field reached by Tab is kept in view the same way a cursor is.
        readonly property Item focused: Window.activeFocusItem
        onFocusedChanged: Qt.callLater(function () { flick.revealItem(flick.focused) })

      Column {
        id: contentCol
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.top: parent.top
        spacing: Style.space(12)

        // ---------- header ----------
        Item {
          width: parent.width
          implicitHeight: Math.max(titleCol.implicitHeight, headerActions.implicitHeight)

          Column {
            id: titleCol
            anchors.left: parent.left
            anchors.right: headerActions.left
            anchors.rightMargin: Style.space(10)
            anchors.verticalCenter: parent.verticalCenter
            spacing: Style.space(2)

            Text {
              textFormat: Text.PlainText
              text: "OMAGIHU"
              color: root.foreground
              font.family: root.fontFamily
              font.pixelSize: Style.font.title
              font.bold: true
            }

            Text {
              textFormat: Text.PlainText
              text: {
                if (root.helperMissing) return "NOT BUILT"
                if (!root.snap) return "CONNECTING"
                if (!root.monitoring) return "ASLEEP · NOTHING LEAVES THIS MACHINE"
                return String(root.attention ? root.attention.summary : "").toUpperCase()
              }
              color: root.level === "urgent" ? Color.urgent : Qt.darker(root.foreground, 1.4)
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
              font.bold: true
              font.letterSpacing: 1.2
            }
          }

          RowLayout {
            id: headerActions
            anchors.right: parent.right
            anchors.verticalCenter: parent.verticalCenter
            spacing: Style.space(6)

            Button {
              text: "Refresh"
              tooltipText: "Refresh now  (r)"
              iconText: root.iconRefresh
              foreground: root.foreground
              accent: Color.accent
              fontFamily: root.fontFamily
              fontSize: Style.font.caption
              bordered: true
              onClicked: root.refreshNow()
            }
          }
        }

        // ---------- navigation. The two places you go to read something are
        // named; the two you go to act are glyphs on the right edge, so the row
        // does not grow a tab every time the plugin does. ----------
        Item {
          width: parent.width
          visible: !root.helperMissing
          height: Math.max(tabs.height, navIcons.height)

          ButtonGroup {
            id: tabs
            anchors.left: parent.left
            anchors.verticalCenter: parent.verticalCenter
            options: [
              { value: "dashboard", label: "Dashboard", tooltip: "Dashboard  (d)" },
              { value: "repos", label: "Repositories", tooltip: "Checkouts  (c)" }
            ]
            value: root.view
            foreground: root.foreground
            accent: Color.accent
            fontFamily: root.fontFamily
            onChanged: function(v) { root.setView(v) }
          }

          Row {
            id: navIcons
            anchors.right: parent.right
            anchors.verticalCenter: parent.verticalCenter
            spacing: Style.space(14)

            // The bell turns urgent for the one state that makes a watch a lie:
            // armed, with nothing running to watch it.
            Text {
              anchors.verticalCenter: parent.verticalCenter
              textFormat: Text.PlainText
              text: root.iconBell
              color: (!root.monitoring && root.armedCount > 0) ? Color.urgent : root.foreground
              opacity: root.view === "alerts" ? 1.0 : (bellHover.hovered ? 0.8 : 0.45)
              font.family: root.fontFamily
              font.pixelSize: Style.font.icon

              HoverHandler { id: bellHover; cursorShape: Qt.PointingHandCursor }
              TapHandler {
                onTapped: root.setView(root.view === "alerts" ? "dashboard" : "alerts")
              }
              PanelToolTip {
                visible: bellHover.hovered
                text: {
                  if (root.view === "alerts") return "Back to the dashboard  (a)"
                  if (!root.monitoring && root.armedCount > 0)
                    return "Alerts: " + root.armedCount + " armed, nothing watching  (a)"
                  if (root.armedCount > 0) return "Alerts: " + root.armedCount + " armed  (a)"
                  return "Alerts  (a)"
                }
              }

              // A small mark rather than a number: the count is on the board, and
              // the glyph has no room for it.
              Rectangle {
                visible: root.armedCount > 0 && root.view !== "alerts"
                anchors.right: parent.right
                anchors.top: parent.top
                anchors.rightMargin: -Style.space(2)
                anchors.topMargin: -Style.space(1)
                width: Style.space(5)
                height: width
                radius: width / 2
                color: root.monitoring ? Color.accent : Color.urgent
              }
            }

            Text {
              anchors.verticalCenter: parent.verticalCenter
              textFormat: Text.PlainText
              text: root.iconCog
              color: root.foreground
              opacity: root.view === "settings" ? 1.0 : (cogHover.hovered ? 0.8 : 0.45)
              font.family: root.fontFamily
              font.pixelSize: Style.font.icon

              HoverHandler { id: cogHover; cursorShape: Qt.PointingHandCursor }
              TapHandler {
                onTapped: root.setView(root.view === "settings" ? "dashboard" : "settings")
              }
              PanelToolTip {
                visible: cogHover.hovered
                text: root.view === "settings" ? "Back to the dashboard  (s)" : "Settings  (s)"
              }
            }
          }
        }

        PanelSeparator { width: parent.width; visible: !root.helperMissing }

        // ---------- not built yet ----------
        Column {
          width: parent.width
          visible: root.helperMissing
          spacing: Style.space(6)

          Text {
            textFormat: Text.PlainText
            width: parent.width
            wrapMode: Text.WordWrap
            text: "The helper binaries are not built. Omagihu ships source only, so the "
                  + "plugin is compiled once after install:"
            color: root.foreground
            font.family: root.fontFamily
            font.pixelSize: Style.font.body
          }

          Button {
            text: "Build and set up"
            iconText: root.iconRefresh
            bordered: true
            foreground: root.foreground
            accent: Color.accent
            fontFamily: root.fontFamily
            fontSize: Style.font.body
            onClicked: root.runBuildAndInstall()
          }

          Text {
            textFormat: Text.PlainText
            width: parent.width
            wrapMode: Text.WordWrap
            text: "This opens a terminal, compiles the helpers, asks which directories "
                + "hold your checkouts, and starts the service. Or do it by hand:"
            color: Qt.darker(root.foreground, 1.4)
            font.family: root.fontFamily
            font.pixelSize: Style.font.caption
          }

          Text {
            textFormat: Text.PlainText
            text: "  cd " + root.pluginDir + "\n  make && ./bin/omagihu-setup install"
            color: Color.accent
            font.family: root.fontFamily
            font.pixelSize: Style.font.caption
          }
        }

        // ---------- the active tab ----------
        Loader {
          id: viewLoader
          width: parent.width
          active: !root.helperMissing
          sourceComponent: {
            if (root.view === "repos") return reposComponent
            if (root.view === "alerts") return alertsComponent
            if (root.view === "settings") return settingsComponent
            return dashboardComponent
          }
        }

        Text {
          textFormat: Text.PlainText
          width: parent.width
          wrapMode: Text.WordWrap
          visible: root.lastError !== "" && !root.helperMissing
          text: root.lastError
          color: Color.urgent
          font.family: root.fontFamily
          font.pixelSize: Style.font.caption
        }
      }
      }

      // The scroll mark, shown only while there is more than fits. It sits in the
      // card's padding just inside the border, so it never crowds the rows.
      Rectangle {
        anchors.right: parent.right
        anchors.rightMargin: -(panel.padding - Style.space(3))
        width: Style.space(2)
        radius: width / 2
        visible: flick.contentHeight > flick.height
        y: flick.visibleArea.yPosition * flick.height
        height: Math.max(Style.space(16), flick.visibleArea.heightRatio * flick.height)
        color: Color.popups.text
        opacity: 0.3
      }
    }
  }

  Component {
    id: dashboardComponent
    DashboardView {
      owner: root
      snap: root.snap
    }
  }

  Component {
    id: reposComponent
    ReposView {
      owner: root
      snap: root.snap
    }
  }

  Component {
    id: alertsComponent
    AlertsView {
      owner: root
      snap: root.snap
    }
  }

  Component {
    id: settingsComponent
    SettingsView {
      owner: root
      snap: root.snap
    }
  }
}
