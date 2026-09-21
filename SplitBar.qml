import QtQuick
import qs.Commons

// Two counts that share one bar: commits only here against commits only
// there, so the distance from the upstream is seen, not only read.
Item {
  id: split

  property real ahead: 0
  property real behind: 0
  property color foreground: Color.foreground

  readonly property real total: Math.max(1e-9, split.ahead + split.behind)

  implicitHeight: Style.space(4)

  Rectangle {
    anchors.fill: parent
    radius: height / 2
    color: Util.alpha(split.foreground, 0.12)
  }

  // Both halves fade along the bar, out from their own end, so the two
  // read as distances from the meeting point and not as two blocks.
  Rectangle {
    height: parent.height
    width: Math.round(parent.width * split.ahead / split.total)
    radius: height / 2
    gradient: Gradient {
      orientation: Gradient.Horizontal
      GradientStop { position: 0.0; color: Util.alpha(Color.accent, 1.0) }
      GradientStop { position: 1.0; color: Util.alpha(Color.accent, 0.45) }
    }
  }

  Rectangle {
    anchors.right: parent.right
    height: parent.height
    width: Math.round(parent.width * split.behind / split.total)
    radius: height / 2
    gradient: Gradient {
      orientation: Gradient.Horizontal
      GradientStop { position: 0.0; color: Util.alpha(split.foreground, 0.18) }
      GradientStop { position: 1.0; color: Util.alpha(split.foreground, 0.45) }
    }
  }
}
