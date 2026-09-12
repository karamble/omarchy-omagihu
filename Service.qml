import QtQuick
import Quickshell
import Quickshell.Io

// The daemon, owned by the shell.
//
// omagihu needs something long lived: fsnotify watchers over every checkout,
// the ETags that make polling cost nothing, the fetch cadence and the alerts
// engine folding each sample. It does not need systemd for that. The shell
// builds this object when the plugin is enabled and destroys it when the plugin
// is disabled or removed, which is exactly the lifetime the daemon should have,
// and it is the pattern every other service plugin here uses.
//
// The upshot is that omagihu writes nothing outside its own folder and
// ~/.config/omagihu. There is no unit to leave behind when Omarchy deletes the
// plugin, and no service to orphan when it does.
Item {
  id: root

  visible: false
  width: 0
  height: 0

  // Injected by the shell when it constructs the service.
  property var shell: null
  property var manifest: null
  property var pluginRegistry: null

  readonly property string pluginDir: Qt.resolvedUrl(".").toString()
                                        .replace(/^file:\/\//, "").replace(/\/$/, "")
  readonly property string daemonPath: pluginDir + "/bin/omagihud"

  // Restart=on-failure, reimplemented. A daemon that cannot start must not be
  // respawned in a tight loop: back off, and after enough tries give up and
  // leave the reason where the panel can find it.
  readonly property int maxRestarts: 5
  property int restarts: 0
  property string lastError: ""

  function backoffMs() {
    return Math.min(30000, 1000 * Math.pow(2, root.restarts))
  }

  // Passed to every child, built once so the two cannot drift.
  //
  // Deliberately short. PATH reaches notify-send and herdr, both in /usr/bin,
  // and git, which the watcher shells out to. HOME finds the configuration and
  // the checkouts. The session bus is what notify-send needs to reach the
  // notification daemon; without it every alert is delivered into nothing.
  // Proxy settings and trust roots are not here on purpose.
  readonly property var childEnv: ({
    "PATH": "/usr/bin:/bin",
    "HOME": Quickshell.env("HOME") || "",
    "XDG_RUNTIME_DIR": Quickshell.env("XDG_RUNTIME_DIR") || "",
    "DBUS_SESSION_BUS_ADDRESS": Quickshell.env("DBUS_SESSION_BUS_ADDRESS") || "",
    // runGit passes the daemon's environment to git, and a remote reached over
    // SSH authenticates through the agent. Without this the background fetch
    // fails on every private remote, quietly, since GIT_TERMINAL_PROMPT is 0.
    "SSH_AUTH_SOCK": Quickshell.env("SSH_AUTH_SOCK") || ""
  })

  // The helpers are compiled on the user's machine and bin/ is not shipped, so
  // a fresh install has nothing to run yet. Probing first keeps that quiet:
  // the panel already explains it and offers to build.
  Process {
    id: probe
    command: ["/usr/bin/test", "-x", root.daemonPath]
    running: true
    clearEnvironment: true
    environment: root.childEnv

    onExited: function (code, status) {
      if (code === 0) daemon.running = true
      else root.lastError = "not built yet"
    }
  }

  Process {
    id: daemon
    command: [root.daemonPath]
    clearEnvironment: true
    environment: root.childEnv


    onExited: function (code, status) {
      // A clean exit is the daemon being told to stop, which happens when this
      // object is going away. Nothing to do then.
      if (code === 0) return
      if (root.restarts >= root.maxRestarts) {
        root.lastError = "the daemon keeps exiting (" + code + "); not restarting again"
        console.warn("omagihu: " + root.lastError)
        return
      }
      root.restarts++
      root.lastError = "daemon exited with " + code + ", restarting"
      restartTimer.interval = root.backoffMs()
      restartTimer.restart()
    }
  }

  Timer {
    id: restartTimer
    repeat: false
    onTriggered: if (!daemon.running) daemon.running = true
  }

  // A daemon that has stayed up is not a daemon that is crash looping, so the
  // budget is returned rather than spent for the life of the session.
  Timer {
    interval: 120000
    repeat: true
    running: daemon.running
    onTriggered: if (root.restarts > 0) root.restarts = 0
  }
}
