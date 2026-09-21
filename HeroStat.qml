import QtQuick
import qs.Commons

// One headline figure: a large value over a quiet label, with the label's
// glyph beside the word so the mark is learned and never a puzzle.
Column {
  id: stat

  property string value: ""
  property string label: ""
  property string icon: ""
  property color foreground: Color.foreground
  property color valueColor: stat.foreground
  property string fontFamily: Style.font.family

  readonly property color quiet: Util.alpha(stat.foreground, 0.6)

  spacing: 0

  Text {
    textFormat: Text.PlainText
    text: stat.value
    color: stat.valueColor
    font.family: stat.fontFamily
    font.pixelSize: Style.font.heading
    font.bold: true
  }

  Row {
    spacing: Style.space(4)

    Text {
      visible: stat.icon !== ""
      anchors.baseline: statLabel.baseline
      textFormat: Text.PlainText
      text: stat.icon
      color: stat.quiet
      font.family: stat.fontFamily
      font.pixelSize: Style.font.caption
    }

    Text {
      id: statLabel
      textFormat: Text.PlainText
      text: stat.label
      color: stat.quiet
      font.family: stat.fontFamily
      font.pixelSize: Style.font.caption
    }
  }
}
