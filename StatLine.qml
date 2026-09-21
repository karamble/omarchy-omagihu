import QtQuick
import qs.Commons

// One label and value line of a grid. Labels share one column so the values
// line up down the whole card. A value that does not fit wraps under itself
// rather than eliding, because the end of a path or a remote is the part
// that says which one it is. A row that carries a bar draws it beneath its
// value.
Item {
  id: line

  // { label, value, tone, bold, bar: { ahead, behind } }
  property var row: ({})
  property color foreground: Color.foreground
  property string fontFamily: Style.font.family
  property int labelColumn: Style.space(84)

  readonly property color quiet: Util.alpha(line.foreground, 0.6)
  readonly property bool hasBar: !!line.row.bar && (line.row.bar.ahead + line.row.bar.behind) > 0

  width: parent ? parent.width : 0
  implicitHeight: Math.max(lineLabel.implicitHeight, lineValue.implicitHeight)
                  + (line.hasBar ? Style.space(4) + lineBar.implicitHeight : 0)

  Text {
    id: lineLabel
    anchors.left: parent.left
    anchors.top: parent.top
    width: line.labelColumn
    textFormat: Text.PlainText
    elide: Text.ElideRight
    text: line.row.label !== undefined ? line.row.label : ""
    color: line.quiet
    font.family: line.fontFamily
    font.pixelSize: Style.font.caption
  }

  Text {
    id: lineValue
    anchors.left: lineLabel.right
    anchors.right: parent.right
    anchors.top: parent.top
    textFormat: Text.PlainText
    wrapMode: Text.WrapAnywhere
    text: line.row.value !== undefined ? line.row.value : ""
    color: line.row.tone !== undefined ? line.row.tone : line.foreground
    font.family: line.fontFamily
    font.pixelSize: Style.font.caption
    font.bold: line.row.bold === true
  }

  SplitBar {
    id: lineBar
    visible: line.hasBar
    anchors.left: lineLabel.right
    anchors.right: parent.right
    anchors.top: lineValue.bottom
    anchors.topMargin: Style.space(4)
    foreground: line.foreground
    ahead: line.hasBar ? line.row.bar.ahead : 0
    behind: line.hasBar ? line.row.bar.behind : 0
  }
}
