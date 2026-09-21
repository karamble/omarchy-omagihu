import QtQuick
import QtQuick.Layouts
import qs.Commons
import qs.Ui

// One row of a list: a leading glyph, a title that elides, an optional second
// line, and any number of badges pinned right. The whole card is the hit
// target, and it tints on hover so a long list still reads as clickable.
Rectangle {
  id: row

  property string icon: ""
  property string title: ""
  property string subtitle: ""
  // Drives the leading glyph, the left rule and the hover tint.
  property color tone: Color.accent
  // An urgent row keeps a visible edge even when the pointer is elsewhere.
  property bool urgent: false
  property string fontFamily: Style.font.family
  property var badges: []
  // How many badges a row may wear on its first line before the rest fold
  // behind one "+N" pill. Disclosing the row shows them, with the detail.
  // The caller orders its badges so what matters most comes first.
  property int maxBadges: 4
  // A disclosable row opens on activation to show its detail: whatever the
  // caller puts in detailContent, plus the folded badges. It stays a look,
  // not a setting: the caller owns the state.
  property bool disclosable: false
  property bool disclosed: false
  property string discloseIcon: ""
  property string discloseOpenIcon: ""
  property alias detailContent: detailSlot.data
  readonly property bool overflowing: row.badges.length > row.maxBadges
  readonly property var shownBadges: {
    if (!row.overflowing) return row.badges
    var out = row.badges.slice(0, row.maxBadges - 1)
    out.push({ text: "+" + (row.badges.length - out.length), overflow: true })
    return out
  }
  readonly property var hiddenBadges: row.overflowing && row.disclosed ? row.badges.slice(row.maxBadges - 1) : []
  // The keyboard cursor sits on a row the way the pointer does, and lights it
  // the same way, so there is one idea of "the row you mean" however you got
  // there. The panel finds the row holding it by looking for this property.
  property bool hasCursor: false
  // Which of the row's actions the cursor is on: 0 is the row itself, 1 the
  // trailing action. Only read while hasCursor.
  property int actionIndex: 0
  readonly property bool actionHasCursor: row.hasCursor && row.actionIndex === 1
  // An optional trailing action. It is its own hit target, so a destructive
  // one has to be aimed at rather than caught by a stray click on the row.
  property string actionIcon: ""
  property string actionTooltip: ""
  property color actionTone: Color.urgent
  // Keeps the action lit without the pointer on it, for the moment between
  // asking and confirming.
  property bool actionActive: false

  signal activated()
  signal actionTriggered()

  // Content-driven: the first line, then the folded badges and the detail
  // while disclosed.
  implicitHeight: content.implicitHeight
                  + (more.visible ? Style.space(6) + more.implicitHeight : 0)
                  + (detailSlot.visible ? Style.space(6) + detailSlot.height : 0)
                  + Style.space(14)
  radius: Style.cornerRadius > 0 ? Style.space(6) : 0
  // Nothing paints past the card, whatever the badges add up to.
  clip: true

  readonly property bool hot: mouse.containsMouse || row.hasCursor

  color: {
    if (row.hot) return Qt.rgba(row.tone.r, row.tone.g, row.tone.b, 0.14)
    if (row.urgent) return Qt.rgba(row.tone.r, row.tone.g, row.tone.b, 0.07)
    return Qt.rgba(1, 1, 1, 0.03)
  }
  border.width: row.urgent || row.hot ? 1 : 0
  border.color: Qt.rgba(row.tone.r, row.tone.g, row.tone.b, row.hot ? 0.8 : 0.35)

  Behavior on color {
    ColorAnimation { duration: 120 }
  }

  MouseArea {
    id: mouse
    anchors.fill: parent
    hoverEnabled: true
    cursorShape: Qt.PointingHandCursor
    onClicked: row.activated()
  }

  RowLayout {
    id: content
    anchors.left: parent.left
    anchors.right: parent.right
    // Centred while there is one line, as it always was; pinned to the top
    // by half the card's padding once anything opens beneath it.
    anchors.verticalCenter: row.open ? undefined : parent.verticalCenter
    anchors.top: row.open ? parent.top : undefined
    anchors.topMargin: Style.space(7)
    anchors.leftMargin: Style.space(12)
    anchors.rightMargin: Style.space(12)
    spacing: Style.space(10)

    Text {
      textFormat: Text.PlainText
      text: row.icon
      color: row.tone
      font.family: row.fontFamily
      font.pixelSize: Style.font.body
      Layout.alignment: Qt.AlignVCenter
      visible: row.icon !== ""
    }

    ColumnLayout {
      Layout.fillWidth: true
      // The name always keeps this much: badges give way before it does.
      Layout.minimumWidth: Style.space(110)
      spacing: Style.space(2)

      Text {
        Layout.fillWidth: true
        textFormat: Text.PlainText
        text: row.title
        color: Color.foreground
        font.family: row.fontFamily
        font.pixelSize: Style.font.bodySmall
        elide: Text.ElideRight
      }

      Text {
        Layout.fillWidth: true
        visible: row.subtitle !== ""
        textFormat: Text.PlainText
        text: row.subtitle
        color: Qt.darker(Color.foreground, 1.5)
        font.family: row.fontFamily
        font.pixelSize: Style.font.caption
        elide: Text.ElideRight
      }
    }

    // Badges keep their natural width; past the cap they fold behind "+N".
    Repeater {
      model: row.shownBadges
      delegate: Badge {
        id: pill
        required property var modelData
        readonly property bool fold: pill.modelData.overflow === true
        Layout.alignment: Qt.AlignVCenter
        text: pill.modelData.text !== undefined ? pill.modelData.text : ""
        // The fold pill is quiet until the row is hot or open; a click on it
        // falls through to the row, which is what discloses.
        tone: pill.fold ? (row.hot || row.disclosed ? Color.accent : Qt.darker(Color.foreground, 1.3))
                        : (pill.modelData.tone !== undefined ? pill.modelData.tone : Color.accent)
        loud: pill.fold ? row.disclosed : pill.modelData.loud === true
        compact: pill.modelData.compact === true
        fontFamily: row.fontFamily
      }
    }

    // The disclosure caret. It keeps its width so nothing shifts, and shows
    // only while the row is hot or open, so a list of rows stays a list.
    Item {
      visible: row.disclosable
      Layout.alignment: Qt.AlignVCenter
      implicitWidth: Style.space(12)
      implicitHeight: Style.space(22)

      Text {
        anchors.centerIn: parent
        visible: row.hot || row.disclosed
        textFormat: Text.PlainText
        text: row.disclosed ? row.discloseOpenIcon : row.discloseIcon
        color: row.disclosed ? row.tone : Qt.darker(Color.foreground, 1.6)
        font.family: row.fontFamily
        font.pixelSize: Style.font.caption
      }
    }

    Item {
      visible: row.actionIcon !== ""
      Layout.alignment: Qt.AlignVCenter
      implicitWidth: Style.space(22)
      implicitHeight: Style.space(22)

      Text {
        anchors.centerIn: parent
        textFormat: Text.PlainText
        text: row.actionIcon
        // Quiet until aimed at, then it says what it is.
        color: (actionMouse.containsMouse || row.actionActive || row.actionHasCursor)
               ? row.actionTone : Qt.darker(Color.foreground, 1.8)
        font.family: row.fontFamily
        font.pixelSize: Style.font.body

        Behavior on color {
          ColorAnimation { duration: 120 }
        }
      }

      MouseArea {
        id: actionMouse
        anchors.fill: parent
        hoverEnabled: true
        cursorShape: Qt.PointingHandCursor
        onClicked: row.actionTriggered()
      }

      PanelToolTip {
        visible: (actionMouse.containsMouse || row.actionActive || row.actionHasCursor)
                 && row.actionTooltip !== ""
        text: row.actionTooltip
        fontFamily: row.fontFamily
      }
    }
  }

  readonly property bool open: more.visible || detailSlot.visible

  // The folded badges, wrapping under the first line while disclosed.
  Flow {
    id: more
    visible: row.hiddenBadges.length > 0
    anchors.top: content.bottom
    anchors.topMargin: Style.space(6)
    anchors.left: parent.left
    anchors.right: parent.right
    anchors.leftMargin: Style.space(12)
    anchors.rightMargin: Style.space(12)
    spacing: Style.space(6)

    Repeater {
      model: row.hiddenBadges
      delegate: Badge {
        id: hiddenPill
        required property var modelData
        text: hiddenPill.modelData.text !== undefined ? hiddenPill.modelData.text : ""
        tone: hiddenPill.modelData.tone !== undefined ? hiddenPill.modelData.tone : Color.accent
        loud: hiddenPill.modelData.loud === true
        compact: hiddenPill.modelData.compact === true
        fontFamily: row.fontFamily
      }
    }
  }

  // The caller's detail, under the badges while disclosed. Empty for the
  // views that never set it, so their rows are unchanged.
  Item {
    id: detailSlot
    visible: row.disclosed && detailSlot.children.length > 0
    anchors.top: more.visible ? more.bottom : content.bottom
    anchors.topMargin: Style.space(6)
    anchors.left: parent.left
    anchors.right: parent.right
    anchors.leftMargin: Style.space(12)
    anchors.rightMargin: Style.space(12)
    height: childrenRect.height
  }
}
