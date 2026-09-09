import QtQuick
import QtQuick.Layouts
import qs.Commons
import qs.Ui

// What the daemon is doing and which identities it speaks for. Tokens never
// reach this side: the accounts endpoint redacts them before they leave the
// process.
Column {
  id: view

  required property var owner
  property var snap: null

  readonly property color foreground: owner.foreground
  readonly property string fontFamily: owner.fontFamily

  readonly property var health: snap && snap.health ? snap.health : null
  readonly property var accounts: snap && snap.accounts ? snap.accounts : []

  // ---------- the keyboard contract ----------
  //
  // Settings is a column of controls rather than a list of things, so its rows
  // are described rather than counted: a chip strip is one row whose actions
  // are its chips, and everything else is a row with a single action. Rows that
  // come and go are absent from the list rather than skipped, so an index never
  // points at something that is not on screen.
  readonly property var rhythms: [
    { label: "1m", value: 1 },
    { label: "5m", value: 5 },
    { label: "15m", value: 15 },
    { label: "60m", value: 60 },
    { label: "Off", value: 0 }
  ]
  readonly property var cadences: [
    { label: "15m", value: 15 },
    { label: "30m", value: 30 },
    { label: "2h", value: 120 },
    { label: "6h", value: 360 }
  ]
  readonly property var notifyDomains: [
    { key: "reviews", label: "A review is requested of you",
      hint: "Somebody is blocked waiting on you." },
    { key: "broken", label: "Your pull request fails or is sent back",
      hint: "Checks went red, or a reviewer asked for changes." },
    { key: "inbox", label: "Any unread notification",
      hint: "Everything GitHub would put in your inbox. The noisy one." },
    { key: "local", label: "An unfinished rebase or merge on this machine",
      hint: "A git operation left half done, which is easy to forget." },
    { key: "reconcile", label: "GitHub and this machine have drifted apart",
      hint: "A pull request missing local commits, or checks red on the commit you have out." }
  ]

  readonly property var rows: {
    var out = [{ kind: "rhythm" }]
    for (var i = 0; i < view.notifyDomains.length; i++)
      out.push({ kind: "notify", index: i })
    out.push({ kind: "setup" })
    out.push({ kind: "fetch" })
    if (view.owner.fetchEnabled) out.push({ kind: "cadence" })
    out.push({ kind: "mcp" })
    if (view.owner.newToken !== "") out.push({ kind: "token" })
    out.push({ kind: "recycle" })
    out.push({ kind: "panel" })
    return out
  }

  readonly property int rowCount: view.rows.length
  readonly property bool formFocused: false

  // rowOf lets a control ask where it ended up, rather than every control
  // counting the ones above it.
  function rowOf(kind, index) {
    for (var i = 0; i < view.rows.length; i++) {
      var r = view.rows[i]
      if (r.kind !== kind) continue
      if (index === undefined || r.index === index) return i
    }
    return -1
  }

  function actionCount(row) {
    var r = view.rows[row]
    if (!r) return 1
    if (r.kind === "rhythm") return view.rhythms.length
    if (r.kind === "cadence") return view.cadences.length
    if (r.kind === "setup") return 4
    if (r.kind === "token") return 2
    return 1
  }

  // Activation goes through the same handlers the pointer uses, so there is one
  // description of what each control does.
  function activateRow(row, action) {
    var r = view.rows[row]
    if (!r) return
    switch (r.kind) {
    case "rhythm":
      view.owner.setRhythm(view.rhythms[action].value)
      break
    case "notify":
      var key = view.notifyDomains[r.index].key
      view.owner.setNotify(key, !view.owner.notifyEnabled(key))
      break
    case "setup":
      [rootsButton, addAccountButton, registerMcpButton, statusButton][action].clicked()
      break
    case "fetch":
      view.owner.setFetch(!view.owner.fetchEnabled, 0)
      break
    case "cadence":
      view.owner.setFetch(true, view.cadences[action].value)
      break
    case "mcp":
      view.owner.setMCP(!view.owner.mcpEnabled)
      break
    case "token":
      [copyTokenButton, copyEntryButton][action].clicked()
      break
    case "recycle":
      recycleButton.clicked()
      break
    case "panel":
      view.owner.settings.countLocalRisk = !view.owner.countLocalRisk
      break
    }
  }

  spacing: Style.space(10)

  function ago(iso) {
    if (!iso) return "never"
    var then = Date.parse(iso)
    if (isNaN(then)) return "never"
    var secs = Math.max(0, Math.round((Date.now() - then) / 1000))
    if (secs < 60) return secs + "s ago"
    if (secs < 3600) return Math.round(secs / 60) + "m ago"
    return Math.round(secs / 3600) + "h ago"
  }

  // ---------- polling rhythm, and the off switch ----------
  Column {
    width: parent.width
    spacing: Style.space(6)

    PanelSectionHeader {
      text: "POLLING"
      foreground: view.foreground
      fontFamily: view.fontFamily
    }

    Row {
      width: parent.width
      spacing: Style.space(6)

      Repeater {
        model: view.rhythms
        delegate: BorderSurface {
          required property var modelData
          required property int index
          readonly property bool hasCursor: view.owner.cursor === view.rowOf("rhythm")
                                            && view.owner.actionIndex === index
          // Off is selected whenever the daemon is asleep, whatever rhythm it
          // would use once woken.
          readonly property bool isOff: modelData.value === 0
          readonly property bool isSelected: isOff
            ? !view.owner.monitoring
            : (view.owner.monitoring && view.owner.intervalMin === modelData.value)
          readonly property color tone: isOff ? Color.urgent : Color.accent

          width: Style.space(46)
          implicitHeight: Style.space(24)
          radius: Style.cornerRadius > 0 ? Style.space(4) : 0
          color: isSelected
                 ? Qt.rgba(tone.r, tone.g, tone.b, 0.25)
                 : Qt.rgba(view.foreground.r, view.foreground.g, view.foreground.b, 0.04)
          borderSpec: Border.controlSpec("normal",
                        isSelected ? tone : Qt.darker(view.foreground, 3.5), tone)

          Text {
            anchors.centerIn: parent
            textFormat: Text.PlainText
            text: modelData.label
            font.family: view.fontFamily
            font.pixelSize: Style.font.caption
            font.bold: parent.isSelected
            color: parent.isSelected ? parent.tone : Qt.darker(view.foreground, 1.5)
          }

          MouseArea {
            anchors.fill: parent
            cursorShape: Qt.PointingHandCursor
            onClicked: view.owner.setRhythm(modelData.value)
          }
        }
      }
    }

    Text {
      textFormat: Text.PlainText
      width: parent.width
      wrapMode: Text.WordWrap
      text: view.owner.monitoring
            ? ("Checking every " + view.owner.intervalMin + " minutes. Pull requests, reviews and "
               + "issues are checked five times less often, and an unchanged inbox costs nothing "
               + "against the rate limit.")
            : ("Asleep. No request leaves this machine, no repository is inspected, and no git "
               + "process runs. The last snapshot below is what was known when it stopped.")
      color: view.owner.monitoring ? Qt.darker(view.foreground, 1.5) : Color.urgent
      font.family: view.fontFamily
      font.pixelSize: Style.font.caption
    }
  }

  PanelSeparator { width: parent.width }

  // ---------- desktop notifications ----------
  Column {
    width: parent.width
    spacing: Style.space(6)

    PanelSectionHeader {
      text: "DESKTOP NOTIFICATIONS"
      foreground: view.foreground
      fontFamily: view.fontFamily
    }

    Repeater {
      model: view.notifyDomains
      delegate: Toggle {
        width: view.width
        hasCursor: view.owner.cursor === view.rowOf("notify", index)
        label: modelData.label
        description: modelData.hint
        checked: view.owner.notifyEnabled(modelData.key)
        foreground: view.foreground
        accent: Color.accent
        fontFamily: view.fontFamily
        onClicked: view.owner.setNotify(modelData.key, !view.owner.notifyEnabled(modelData.key))
      }
    }

    Text {
      textFormat: Text.PlainText
      width: parent.width
      wrapMode: Text.WordWrap
      text: "Only transitions are announced, so a standing failure is reported once "
          + "rather than repeatedly, and a restart does not replay what you already knew. "
          + "Nothing is announced while polling is off."
      color: Qt.darker(view.foreground, 1.5)
      font.family: view.fontFamily
      font.pixelSize: Style.font.caption
    }
  }

  PanelSeparator { width: parent.width }

  // ---------- the things that need a terminal ----------
  Column {
    width: parent.width
    spacing: Style.space(6)

    PanelSectionHeader {
      text: "SETUP"
      foreground: view.foreground
      fontFamily: view.fontFamily
    }

    Flow {
      width: parent.width
      spacing: Style.space(6)

      Button {
        id: rootsButton
        hasCursor: view.owner.cursor === view.rowOf("setup") && view.owner.actionIndex === 0
        text: "Watched folders"
        iconText: view.owner.iconWarn
        bordered: true
        foreground: view.foreground
        accent: Color.accent
        fontFamily: view.fontFamily
        fontSize: Style.font.caption
        onClicked: view.owner.runSetupCommand("roots")
      }

      Button {
        id: addAccountButton
        hasCursor: view.owner.cursor === view.rowOf("setup") && view.owner.actionIndex === 1
        text: "Add account"
        iconText: view.owner.iconBranch
        bordered: true
        foreground: view.foreground
        accent: Color.accent
        fontFamily: view.fontFamily
        fontSize: Style.font.caption
        onClicked: view.owner.runSetupCommand("add")
      }

      Button {
        id: registerMcpButton
        hasCursor: view.owner.cursor === view.rowOf("setup") && view.owner.actionIndex === 2
        text: "Register MCP"
        iconText: view.owner.iconDot
        bordered: true
        foreground: view.foreground
        accent: Color.accent
        fontFamily: view.fontFamily
        fontSize: Style.font.caption
        onClicked: view.owner.runSetupCommand("mcp")
      }

      Button {
        id: statusButton
        hasCursor: view.owner.cursor === view.rowOf("setup") && view.owner.actionIndex === 3
        text: "Service status"
        iconText: view.owner.iconCheck
        bordered: true
        foreground: view.foreground
        accent: Color.accent
        fontFamily: view.fontFamily
        fontSize: Style.font.caption
        onClicked: view.owner.runSetupCommand("status")
      }
    }

    Text {
      textFormat: Text.PlainText
      width: parent.width
      wrapMode: Text.WordWrap
      text: "Each opens a floating terminal. Watched folders asks where your checkouts "
          + "live and restarts the service; adding an account needs a terminal because "
          + "the token is read from stdin, never from a command line."
      color: Qt.darker(view.foreground, 1.5)
      font.family: view.fontFamily
      font.pixelSize: Style.font.caption
    }
  }

  PanelSeparator { width: parent.width }

  // ---------- background fetch ----------
  Column {
    width: parent.width
    spacing: Style.space(6)

    PanelSectionHeader {
      text: "REMOTE REFS"
      foreground: view.foreground
      fontFamily: view.fontFamily
    }

    Toggle {
      width: parent.width
      hasCursor: view.owner.cursor === view.rowOf("fetch")
      label: "Keep remote refs current in the background"
      description: "Runs git fetch on watched repositories. Writes only inside .git: "
                 + "no branch is moved, nothing is merged, the working tree is untouched."
      checked: view.owner.fetchEnabled
      foreground: view.foreground
      accent: Color.accent
      fontFamily: view.fontFamily
      onClicked: view.owner.setFetch(!view.owner.fetchEnabled, 0)
    }

    Row {
      width: parent.width
      spacing: Style.space(6)
      visible: view.owner.fetchEnabled

      Repeater {
        model: view.cadences
        delegate: BorderSurface {
          required property int index
          readonly property bool hasCursor: view.owner.cursor === view.rowOf("cadence")
                                            && view.owner.actionIndex === index
          required property var modelData
          readonly property bool isSelected: view.owner.fetchMin === modelData.value

          width: Style.space(46)
          implicitHeight: Style.space(24)
          radius: Style.cornerRadius > 0 ? Style.space(4) : 0
          color: isSelected
                 ? Qt.rgba(Color.accent.r, Color.accent.g, Color.accent.b, 0.25)
                 : Qt.rgba(view.foreground.r, view.foreground.g, view.foreground.b, 0.04)
          borderSpec: Border.controlSpec("normal",
                        isSelected ? Color.accent : Qt.darker(view.foreground, 3.5), Color.accent)

          Text {
            anchors.centerIn: parent
            textFormat: Text.PlainText
            text: modelData.label
            font.family: view.fontFamily
            font.pixelSize: Style.font.caption
            font.bold: parent.isSelected
            color: parent.isSelected ? Color.accent : Qt.darker(view.foreground, 1.5)
          }

          MouseArea {
            anchors.fill: parent
            cursorShape: Qt.PointingHandCursor
            onClicked: view.owner.setFetch(true, modelData.value)
          }
        }
      }
    }

    Text {
      textFormat: Text.PlainText
      width: parent.width
      wrapMode: Text.WordWrap
      text: view.owner.fetchEnabled
            ? ("Fetching every " + view.owner.fetchMin + " minutes. Without this, a commit keeps "
               + "looking unpushed until something else fetches, and fork divergence is a guess.")
            : "Off. Unpushed counts and fork divergence are only as fresh as your last manual fetch."
      color: Qt.darker(view.foreground, 1.5)
      font.family: view.fontFamily
      font.pixelSize: Style.font.caption
    }
  }

  PanelSeparator { width: parent.width }

  // ---------- mcp ----------
  Column {
    width: parent.width
    spacing: Style.space(6)

    PanelSectionHeader {
      text: "MCP SERVER"
      foreground: view.foreground
      fontFamily: view.fontFamily
    }

    Toggle {
      width: parent.width
      hasCursor: view.owner.cursor === view.rowOf("mcp")
      label: "Answer Model Context Protocol requests"
      description: "Lets an agent read your inbox, pull requests and repository risk. "
                 + "Served on the same loopback listener, behind the same bearer token."
      checked: view.owner.mcpEnabled
      foreground: view.foreground
      accent: Color.accent
      fontFamily: view.fontFamily
      onClicked: view.owner.setMCP(!view.owner.mcpEnabled)
    }

    // Connection details. The token itself is deliberately absent: it lives in
    // the 0600 store, and printing it into a panel puts it in every screenshot.
    Rectangle {
      width: parent.width
      implicitHeight: mcpInfo.implicitHeight + Style.space(16)
      radius: Style.cornerRadius > 0 ? Style.space(6) : 0
      color: Qt.rgba(view.foreground.r, view.foreground.g, view.foreground.b, 0.04)
      border.width: 1
      border.color: Qt.rgba(Color.accent.r, Color.accent.g, Color.accent.b, 0.25)

      Column {
        id: mcpInfo
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.verticalCenter: parent.verticalCenter
        anchors.leftMargin: Style.space(12)
        anchors.rightMargin: Style.space(12)
        spacing: Style.space(4)

        Text {
          textFormat: Text.PlainText
          width: parent.width
          wrapMode: Text.WordWrap
          text: view.owner.mcpEnabled
                ? ("Endpoint  http://" + view.owner.addr + "/mcp")
                : "Disabled. The endpoint answers 404 until this is switched on."
          color: view.owner.mcpEnabled ? Color.accent : Qt.darker(view.foreground, 1.4)
          font.family: view.fontFamily
          font.pixelSize: Style.font.caption
          font.bold: true
        }

        Text {
          textFormat: Text.PlainText
          width: parent.width
          wrapMode: Text.WordWrap
          text: "To connect, print the entry and paste it into the mcpServers object "
              + "in ~/.claude.json:"
          color: Qt.darker(view.foreground, 1.4)
          font.family: view.fontFamily
          font.pixelSize: Style.font.caption
        }

        Text {
          textFormat: Text.PlainText
          width: parent.width
          wrapMode: Text.WrapAnywhere
          text: "  " + view.owner.pluginDir + "/bin/omagihu-setup mcp"
          color: view.foreground
          font.family: view.fontFamily
          font.pixelSize: Style.font.caption
        }

        Text {
          textFormat: Text.PlainText
          width: parent.width
          wrapMode: Text.WordWrap
          text: "Recycling mints a new token and shows it here once, so it can be copied "
              + "straight into ~/.claude.json. Every client still holding the old one is "
              + "locked out until you do, Claude included. It stays otherwise in the 0600 "
              + "store, and this panel forgets it as soon as the card closes."
          color: Qt.darker(view.foreground, 1.5)
          font.family: view.fontFamily
          font.pixelSize: Style.font.caption
        }
      }
    }

    // Shown once, straight after recycling: the value has to be copied out
    // before it is any use, and this is the moment it exists.
    Rectangle {
      width: parent.width
      visible: view.owner.newToken !== ""
      implicitHeight: tokenBox.implicitHeight + Style.space(16)
      radius: Style.cornerRadius > 0 ? Style.space(6) : 0
      color: Qt.rgba(Color.urgent.r, Color.urgent.g, Color.urgent.b, 0.08)
      border.width: 1
      border.color: Qt.rgba(Color.urgent.r, Color.urgent.g, Color.urgent.b, 0.6)

      Column {
        id: tokenBox
        anchors.left: parent.left
        anchors.right: parent.right
        anchors.verticalCenter: parent.verticalCenter
        anchors.leftMargin: Style.space(12)
        anchors.rightMargin: Style.space(12)
        spacing: Style.space(4)

        Text {
          textFormat: Text.PlainText
          text: "NEW BEARER TOKEN"
          color: Color.urgent
          font.family: view.fontFamily
          font.pixelSize: Style.font.caption
          font.bold: true
        }

        // TextEdit rather than Text so the value can be selected and copied.
        TextEdit {
          width: parent.width
          readOnly: true
          selectByMouse: true
          wrapMode: TextEdit.WrapAnywhere
          text: view.owner.newToken
          color: view.foreground
          font.family: view.fontFamily
          font.pixelSize: Style.font.caption
        }

        Text {
          textFormat: Text.PlainText
          width: parent.width
          wrapMode: Text.WordWrap
          text: "Paste this entry into the mcpServers object in ~/.claude.json. "
              + "Every client still holding the old token is locked out until you do."
          color: Qt.darker(view.foreground, 1.4)
          font.family: view.fontFamily
          font.pixelSize: Style.font.caption
        }

        TextEdit {
          width: parent.width
          readOnly: true
          selectByMouse: true
          wrapMode: TextEdit.WrapAnywhere
          text: view.owner.newEntry
          color: Color.accent
          font.family: view.fontFamily
          font.pixelSize: Style.font.caption
        }

        Row {
          spacing: Style.space(6)

          Button {
            id: copyTokenButton
            hasCursor: view.owner.cursor === view.rowOf("token") && view.owner.actionIndex === 0
            text: view.owner.copied === "token" ? "Token copied" : "Copy token"
            iconText: view.owner.copied === "token" ? view.owner.iconCheck : view.owner.iconDot
            bordered: true
            foreground: view.owner.copied === "token" ? view.owner.toneOk : view.foreground
            accent: view.owner.toneOk
            fontFamily: view.fontFamily
            fontSize: Style.font.caption
            onClicked: view.owner.copyToClipboard("token")
          }

          Button {
            id: copyEntryButton
            hasCursor: view.owner.cursor === view.rowOf("token") && view.owner.actionIndex === 1
            text: view.owner.copied === "entry" ? "Entry copied" : "Copy ~/.claude.json entry"
            iconText: view.owner.copied === "entry" ? view.owner.iconCheck : view.owner.iconDot
            bordered: true
            foreground: view.owner.copied === "entry" ? view.owner.toneOk : view.foreground
            accent: view.owner.toneOk
            fontFamily: view.fontFamily
            fontSize: Style.font.caption
            onClicked: view.owner.copyToClipboard("entry")
          }
        }

        Text {
          textFormat: Text.PlainText
          width: parent.width
          wrapMode: Text.WordWrap
          text: "This disappears when the panel closes."
          color: Qt.darker(view.foreground, 1.6)
          font.family: view.fontFamily
          font.pixelSize: Style.font.caption
        }
      }
    }

    Row {
      spacing: Style.space(6)

      Button {
        id: recycleButton
        hasCursor: view.owner.cursor === view.rowOf("recycle")
        // Two steps, because this breaks every client that is already connected.
        property bool armed: false
        text: armed ? "Confirm: recycle token" : "Recycle bearer token"
        iconText: view.owner.iconWarn
        bordered: true
        foreground: armed ? Color.urgent : view.foreground
        accent: Color.urgent
        fontFamily: view.fontFamily
        fontSize: Style.font.caption
        onClicked: {
          if (!armed) {
            armed = true
            return
          }
          armed = false
          view.owner.recycleToken()
        }
      }
    }
  }

  PanelSeparator { width: parent.width }

  // ---------- accounts ----------
  Column {
    width: parent.width
    spacing: Style.space(4)

    PanelSectionHeader {
      text: "ACCOUNTS"
      foreground: view.foreground
      fontFamily: view.fontFamily
    }

    Repeater {
      model: view.accounts
      delegate: Text {
        textFormat: Text.PlainText
        width: view.width
        wrapMode: Text.WordWrap
        text: {
          var line = modelData.login || modelData.accountId
          var bits = []
          if (modelData.rate && modelData.rate.limit > 0)
            bits.push(modelData.rate.remaining + "/" + modelData.rate.limit + " rate left")
          bits.push("inbox " + view.ago(modelData.inboxAt))
          if (modelData.error) bits.push("error: " + modelData.error)
          return line + "  ·  " + bits.join(" • ")
        }
        color: modelData.error ? Color.urgent : Qt.darker(view.foreground, 1.2)
        font.family: view.fontFamily
        font.pixelSize: Style.font.caption
      }
    }

    Text {
      textFormat: Text.PlainText
      visible: view.accounts.length === 0
      text: "No accounts. Run bin/omagihu-setup init."
      color: Color.urgent
      font.family: view.fontFamily
      font.pixelSize: Style.font.caption
    }
  }

  // ---------- daemon ----------
  Column {
    width: parent.width
    spacing: Style.space(4)

    PanelSectionHeader {
      text: "DAEMON"
      foreground: view.foreground
      fontFamily: view.fontFamily
    }

    Text {
      textFormat: Text.PlainText
      width: parent.width
      wrapMode: Text.WordWrap
      text: {
        if (!view.health) return "not connected"
        return "omagihud " + view.health.version + " • up " + view.health.uptime
             + "\n" + view.health.repos + " repositories watched, " + view.health.reposAtRisk + " need attention"
             + "\nremote polled " + view.ago(view.health.polledAt)
             + " • repositories scanned " + view.ago(view.health.scannedAt)
      }
      color: Qt.darker(view.foreground, 1.2)
      font.family: view.fontFamily
      font.pixelSize: Style.font.caption
    }

    Text {
      textFormat: Text.PlainText
      width: parent.width
      wrapMode: Text.WordWrap
      // lastError is omitted from the payload when empty, so both bindings have
      // to survive it being undefined rather than "".
      visible: !!(view.health && view.health.lastError)
      text: (view.health && view.health.lastError) ? String(view.health.lastError) : ""
      color: Color.urgent
      font.family: view.fontFamily
      font.pixelSize: Style.font.caption
    }
  }

  // ---------- panel settings ----------
  Column {
    width: parent.width
    spacing: Style.space(6)

    PanelSectionHeader {
      text: "PANEL"
      foreground: view.foreground
      fontFamily: view.fontFamily
    }

    Toggle {
      hasCursor: view.owner.cursor === view.rowOf("panel")
      label: "Local repository risk can light the bar"
      checked: view.owner.countLocalRisk
      foreground: view.foreground
      accent: Color.accent
      fontFamily: view.fontFamily
      onClicked: view.owner.settings.countLocalRisk = !view.owner.countLocalRisk
    }

    Text {
      textFormat: Text.PlainText
      width: parent.width
      wrapMode: Text.WordWrap
      text: "Daemon address " + view.owner.addr + ", panel refresh every " + view.owner.refreshSec + "s. "
          + "Both are editable in the plugin settings, along with the roots the daemon watches."
      color: Qt.darker(view.foreground, 1.5)
      font.family: view.fontFamily
      font.pixelSize: Style.font.caption
    }
  }
}
