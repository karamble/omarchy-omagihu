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

  implicitWidth: label.implicitWidth + Style.space(12)
  implicitHeight: Style.space(20)
  radius: Style.cornerRadius > 0 ? Style.space(3) : 0
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
    font.pixelSize: Math.round(Style.font.caption * 0.9)
    font.bold: true
  }
}
