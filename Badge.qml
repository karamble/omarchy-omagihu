import QtQuick
import qs.Commons

// A pill carrying one fact about a row: a CI verdict, an unpushed count, an
// interrupted operation. Colour is the meaning, so a row can wear several and
// still be read at a glance.
Rectangle {
  id: badge

  property string text: ""
  property color tone: Color.accent
  property string fontFamily: Style.font.family
  // A quiet badge states a fact; a loud one is asking for something.
  property bool loud: false
  // A compact badge is a label rather than a verdict: smaller, rounder and
  // quieter, so a row can wear several without them shouting over the title.
  property bool compact: false

  implicitWidth: label.implicitWidth + Style.space(badge.compact ? 8 : 12)
  implicitHeight: Style.space(badge.compact ? 14 : 20)
  // A bubble, not a chip: fully rounded when the theme rounds anything.
  radius: Style.cornerRadius > 0
          ? (badge.compact ? height / 2 : Style.space(3))
          : 0
  visible: badge.text !== ""

  color: Qt.rgba(tone.r, tone.g, tone.b, badge.loud ? 0.20 : 0.10)
  border.color: Qt.rgba(tone.r, tone.g, tone.b, badge.loud ? 1.0 : 0.45)
  border.width: 1

  Text {
    id: label
    anchors.centerIn: parent
    textFormat: Text.PlainText
    text: badge.text
    color: badge.tone
    font.family: badge.fontFamily
    font.pixelSize: Math.round(Style.font.caption * (badge.compact ? 0.75 : 0.9))
    font.bold: !badge.compact
  }
}
