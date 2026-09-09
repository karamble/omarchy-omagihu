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

  implicitHeight: content.implicitHeight + Style.space(14)
  radius: Style.cornerRadius > 0 ? Style.space(6) : 0

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
    anchors.verticalCenter: parent.verticalCenter
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

    // Badges keep their natural width so the title is what gives way.
    Repeater {
      model: row.badges
      delegate: Badge {
        Layout.alignment: Qt.AlignVCenter
        text: modelData.text !== undefined ? modelData.text : ""
        tone: modelData.tone !== undefined ? modelData.tone : Color.accent
        loud: modelData.loud === true
        compact: modelData.compact === true
        fontFamily: row.fontFamily
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
}
