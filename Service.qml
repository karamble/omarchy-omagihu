import QtQuick
import Quickshell

// The plugin's own lifecycle, as far as Omarchy gives us one.
//
// The shell builds this object when the plugin is enabled and destroys it when
// the plugin is disabled or removed (shell.qml, "Drop services for plugins that
// have been disabled or removed"). That destruction is the only notice a plugin
// ever gets: `omarchy plugin remove` runs no script of ours, but it does disable
// the plugin before deleting the folder, so this fires while the helpers still
// exist.
//
// It only starts and stops the daemon. It deliberately does not disable the
// unit, delete it, or touch the account store: this same handler runs when you
// merely switch the widget off, and nothing that fires on a toggle has any
// business deleting your tokens. `omagihu-setup uninstall --purge` stays the one
// way to do that, on purpose.
//
// The upshot is that a removed or disabled plugin stops talking to GitHub
// immediately, which is the same promise the kill switch makes.
Item {
  id: root

  visible: false
  width: 0
  height: 0

  // Injected by the shell when it constructs the service.
  property var shell: null
  property var manifest: null
  property var pluginRegistry: null

  // Detached, because a command owned by a component that is being destroyed
  // does not outlive it, which is exactly the trap the setup terminal fell into.
  function control(verb) {
    Quickshell.execDetached(["systemctl", "--user", verb, "omagihu.service"])
  }

  // Enabling the plugin brings the daemon back up. Before it is installed there
  // is no unit and systemctl simply fails, which is the right amount of fuss.
  Component.onCompleted: root.control("start")
  Component.onDestruction: root.control("stop")
}
