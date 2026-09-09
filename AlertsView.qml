import QtQuick
import QtQuick.Layouts
import qs.Commons
import qs.Ui

// The alerts board. A watch here is a standing question put to the daemon:
// ring me when this happens, then stop bothering me. Every row says what is
// being waited on, who gets woken, and how long the question stands.
Column {
  id: view

  required property var owner
  property var snap: null

  readonly property color foreground: owner.foreground
  readonly property string fontFamily: owner.fontFamily

  readonly property var alerts: owner.alerts
  // The filters are declared once and drawn from, because the chip strip is
  // also row zero to the keyboard and both need the same order.
  readonly property var filters: [
    { key: "armed", label: "Waiting" },
    { key: "spent", label: "Done" },
    { key: "all", label: "All" }
  ]
  property string filter: "all"
  property bool arming: false
  // The trigger whose bin has been clicked once. Disarming is destructive, so
  // it asks; it asks in the row itself, because a modal in a narrow card hides
  // the very thing being confirmed.
  property string pendingDisarm: ""
  // Set when a row cannot be disarmed from here, which is not an error so much
  // as a boundary: a watch belongs to whoever armed it.
  property string ownershipNote: ""

  function isLive(a) {
    var st = String(a.status || "")
    return st === "armed" || st === "rearming" || st === "no-sample"
  }

  function matches(a, key) {
    if (key === "all") return true
    if (key === "armed") return view.isLive(a)
    return !view.isLive(a)
  }

  function countFor(key) {
    var n = 0
    for (var i = 0; i < view.alerts.length; i++) {
      if (view.matches(view.alerts[i], key)) n++
    }
    return n
  }

  readonly property var rows: {
    var out = []
    for (var i = 0; i < view.alerts.length; i++) {
      if (view.matches(view.alerts[i], view.filter)) out.push(view.alerts[i])
    }
    return out
  }

  function statusTone(status) {
    if (status === "delivery-failed") return Color.urgent
    if (status === "no-sample") return Qt.lighter(Color.urgent, 1.3)
    if (status === "fired") return view.owner.toneOk
    if (status === "armed") return Color.accent
    return Qt.darker(view.foreground, 1.3)
  }

  function statusLabel(status) {
    return ({
      "armed": "ARMED",
      "rearming": "REARMING",
      "fired": "FIRED",
      "expired": "EXPIRED",
      "no-sample": "NO SAMPLE",
      "delivery-failed": "DELIVERY FAILED"
    })[status] || String(status).toUpperCase()
  }

  // The condition, in the words the catalogue uses, so a row reads back as the
  // sentence that armed it.
  function conditionOf(a) {
    var p = a.params || {}
    var s = String(a.path) + " " + String(a.operator)
    if (a.operator === "crosses" || a.operator === "count") {
      if (p.above !== undefined) s += " above " + p.above
      else if (p.below !== undefined) s += " below " + p.below
    } else if (a.operator === "becomes") {
      s += " \"" + String(p.value || "") + "\""
    } else if (a.operator === "ages") {
      s += " past " + String(p.olderThan || "")
      if (p.field) s += " on " + String(p.field)
    }
    var where = a.where || []
    if (where.length > 0) {
      var bits = []
      for (var i = 0; i < where.length; i++)
        bits.push(where[i].field + where[i].op + where[i].value)
      s += " where " + bits.join(" and ")
    }
    return s
  }

  // How long the question stands. An expiry is required, so this always has
  // something to say.
  function untilOf(a) {
    var ms = new Date(a.expiresAt).getTime() - Date.now()
    if (isNaN(ms)) return ""
    if (ms <= 0) return "expired"
    var mins = Math.floor(ms / 60000)
    if (mins < 60) return "expires in " + mins + "m"
    var hours = Math.floor(mins / 60)
    if (hours < 48) return "expires in " + hours + "h"
    return "expires in " + Math.floor(hours / 24) + "d"
  }

  function deliveryOf(a) {
    var to = String(a.deliverTo || "you")
    if (to === "you") return "wakes you"
    if (to === "repo") return "wakes whoever is in the checkout"
    return "wakes " + to
  }

  function badgesOf(a) {
    var out = [{ text: view.statusLabel(a.status),
                 tone: view.statusTone(String(a.status)),
                 loud: a.status === "delivery-failed" || a.status === "armed" }]
    if (a.standing) out.push({ text: "STANDING", tone: Qt.darker(view.foreground, 1.3) })
    else out.push({ text: "ONCE", tone: Qt.darker(view.foreground, 1.3) })
    var fired = a.state && a.state.fireCount ? a.state.fireCount : 0
    if (fired > 0) out.push({ text: fired + "x", tone: view.owner.toneOk })
    return out
  }

  function rowIcon(a) {
    if (a.status === "delivery-failed" || a.status === "no-sample") return view.owner.iconWarn
    if (a.status === "fired") return view.owner.iconCheck
    if (a.status === "expired") return view.owner.iconClock
    return view.owner.iconBell
  }

  // A watch armed from this panel is yours; one armed in a herdr pane belongs to
  // that agent. The distinction governs editing, not removal: changing an
  // agent's question out from under it would leave it waiting for something it
  // never asked for, whereas the person at the panel owns the machine and must
  // always be able to clear a watch an agent left behind.
  function isYours(a) {
    return String(a.armedBy || "you") === "you"
  }

  // ---------- the keyboard contract ----------
  //
  // Row zero is the chip strip, whose actions are its chips and the arm button;
  // every row after it is one watch, whose actions are the watch itself and its
  // bin. That is the same shape the mouse sees, so a key and a click end in the
  // same function.
  readonly property int rowCount: 1 + view.rows.length
  readonly property bool formFocused: view.arming && armForm.formFocused
  // The panel asks this so Escape can close the form rather than the card.
  readonly property bool formOpen: view.arming

  function cancelForm() {
    if (view.arming) view.toggleArming()
  }

  function actionCount(row) {
    if (row === 0) return view.filters.length + 1
    return 2
  }

  function activateRow(row, action) {
    if (row === 0) {
      if (action >= view.filters.length) { view.toggleArming(); return }
      view.filter = String(view.filters[action].key)
      return
    }
    var a = view.rows[row - 1]
    if (!a) return
    if (action === 1) view.disarmAction(a)
    else view.editAction(a)
  }

  // x goes straight for the bin, and lands on the same two step confirm the
  // pointer gets: pressing it twice is what disarms.
  function deleteRow(row) {
    if (row === 0) return
    var a = view.rows[row - 1]
    if (a) view.disarmAction(a)
  }

  function beginAdd() {
    if (view.arming) { armForm.focusFirst(); return }
    view.toggleArming()
  }

  function toggleArming() {
    view.arming = !view.arming
    armForm.reset()
    if (view.arming) {
      view.owner.loadAlertsData()
      // A beat later, because an item cannot take focus until it is on screen.
      Qt.callLater(armForm.focusFirst)
    } else {
      // The form had the keys; hand them back so the letters work again.
      view.owner.reclaimKeys()
    }
  }

  // ---------- what a row does, however you reached it ----------
  function editAction(a) {
    view.pendingDisarm = ""
    if (!view.isYours(a)) {
      view.ownershipNote = a.armedBy + " armed this one, so it cannot be edited "
          + "here: changing its terms would leave that agent waiting for "
          + "something it never asked for. The bin still removes it."
      return
    }
    view.ownershipNote = ""
    view.owner.loadAlertsData()
    view.arming = true
    armForm.load(a)
    Qt.callLater(armForm.focusFirst)
  }

  function disarmAction(a) {
    view.ownershipNote = ""
    if (view.pendingDisarm === String(a.id)) {
      view.owner.disarmTrigger(String(a.id))
      view.pendingDisarm = ""
      confirmTimeout.stop()
      return
    }
    view.pendingDisarm = String(a.id)
    confirmTimeout.restart()
  }

  spacing: Style.space(10)

  // ---------- filter chips and the arm button ----------
  RowLayout {
    width: parent.width
    spacing: Style.space(6)

    Repeater {
      model: view.filters
      delegate: Button {
        Layout.fillWidth: true
        text: modelData.label + " (" + view.countFor(modelData.key) + ")"
        selected: view.filter === modelData.key
        hasCursor: view.owner.cursor === 0 && view.owner.actionIndex === index
        bordered: true
        foreground: view.foreground
        accent: Color.accent
        fontFamily: view.fontFamily
        fontSize: Style.font.caption
        horizontalPadding: Style.space(8)
        verticalPadding: Style.space(5)
        onClicked: view.filter = modelData.key
      }
    }

    Button {
      text: view.arming ? "Cancel" : "Arm a watch"
      tooltipText: view.arming ? "Close the form  (Esc)" : "Arm a watch  (n)"
      iconText: view.arming ? "" : view.owner.iconPlus
      hasCursor: view.owner.cursor === 0
                 && view.owner.actionIndex === view.filters.length
      bordered: true
      foreground: view.foreground
      accent: Color.accent
      fontFamily: view.fontFamily
      fontSize: Style.font.caption
      horizontalPadding: Style.space(8)
      verticalPadding: Style.space(5)
      onClicked: view.toggleArming()
    }
  }

  // ---------- the form. One form for both, because arming and editing are the
  // same sentence, written once. ----------
  ArmForm {
    id: armForm
    width: parent.width
    visible: view.arming
    owner: view.owner
    onArmed: {
      view.arming = false
      armForm.reset()
      view.owner.reclaimKeys()
    }
    // Escape inside the form closes the form, not the panel.
    onCancelled: view.toggleArming()
  }

  Text {
    textFormat: Text.PlainText
    width: parent.width
    wrapMode: Text.WordWrap
    visible: view.owner.armError !== ""
    text: view.owner.armError
    color: Color.urgent
    font.family: view.fontFamily
    font.pixelSize: Style.font.caption
  }

  Text {
    textFormat: Text.PlainText
    width: parent.width
    wrapMode: Text.WordWrap
    visible: view.ownershipNote !== ""
    text: view.ownershipNote
    color: Qt.darker(view.foreground, 1.3)
    font.family: view.fontFamily
    font.pixelSize: Style.font.caption
  }

  // ---------- the board ----------
  Column {
    id: alertColumn
    width: parent.width
    spacing: Style.space(6)

    Repeater {
      model: view.rows
      delegate: ListRow {
        width: alertColumn.width
        hasCursor: view.owner.cursor === index + 1
        actionIndex: view.owner.actionIndex
        icon: view.rowIcon(modelData)
        tone: view.statusTone(String(modelData.status))
        urgent: modelData.status === "delivery-failed"
        fontFamily: view.fontFamily
        title: view.conditionOf(modelData)
        subtitle: {
          var bits = [view.deliveryOf(modelData), view.untilOf(modelData)]
          if (!view.isYours(modelData)) bits.push("armed by " + modelData.armedBy)
          if (modelData.reason) bits.push(String(modelData.reason))
          if (modelData.state && modelData.state.deliveryError)
            bits.push(String(modelData.state.deliveryError))
          return bits.join("  ·  ")
        }
        badges: view.badgesOf(modelData)
        // Only your own watches get a bin. Somebody else's is theirs to take
        // down, so the row says who owns it instead of offering the button.
        readonly property bool confirming: view.pendingDisarm === String(modelData.id)
        actionIcon: confirming ? view.owner.iconCheck : view.owner.iconTrash
        actionActive: confirming
        actionTooltip: {
          var whose = view.isYours(modelData)
            ? "this watch"
            : modelData.armedBy + "'s watch"
          return confirming ? "Again to disarm " + whose + "  (x)" : "Disarm " + whose + "  (x)"
        }
        onActionTriggered: view.disarmAction(modelData)
        // Clicking your own watch reopens it in the form: moving a bound or
        // extending an expiry is an edit, not a second watch.
        onActivated: view.editAction(modelData)
      }
    }

    Rectangle {
      visible: view.rows.length === 0
      width: alertColumn.width
      implicitHeight: Style.space(70)
      radius: Style.cornerRadius > 0 ? Style.space(6) : 0
      color: Qt.rgba(view.foreground.r, view.foreground.g, view.foreground.b, 0.04)

      ColumnLayout {
        anchors.centerIn: parent
        spacing: Style.space(4)

        Text {
          Layout.alignment: Qt.AlignHCenter
          textFormat: Text.PlainText
          text: view.filter === "all" ? "Nothing is being waited on" : "Nothing in this filter"
          color: view.foreground
          font.family: view.fontFamily
          font.pixelSize: Style.font.body
          font.bold: true
        }

        Text {
          Layout.alignment: Qt.AlignHCenter
          textFormat: Text.PlainText
          text: "Arm a watch and omagihu will ring when it comes true."
          color: Qt.darker(view.foreground, 1.5)
          font.family: view.fontFamily
          font.pixelSize: Style.font.caption
        }
      }
    }
  }

  // A watch armed while nothing is polling is a promise that cannot be kept,
  // so the board says so rather than letting the bell imply otherwise.
  Text {
    textFormat: Text.PlainText
    width: parent.width
    wrapMode: Text.WordWrap
    visible: !view.owner.monitoring && view.owner.armedCount > 0
    text: "Monitoring is off, so nothing is being sampled and none of these can fire."
    color: Color.urgent
    font.family: view.fontFamily
    font.pixelSize: Style.font.caption
  }

  // An unanswered question puts itself away rather than sitting armed on a row
  // the next click lands on.
  Timer {
    id: confirmTimeout
    interval: 4000
    onTriggered: view.pendingDisarm = ""
  }
}
