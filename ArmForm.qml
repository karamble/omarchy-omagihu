import QtQuick
import QtQuick.Layouts
import qs.Commons
import qs.Ui

// Arming a watch, in the order the sentence is built: what to watch, what
// counts as happening, how narrow, how long the question stands, and who gets
// woken. The catalogue decides which operators a path takes, so the form can
// only produce watches the daemon will accept.
Column {
  id: form

  required property var owner
  signal armed()
  // Escape anywhere in the form means "put this away", which is the view's
  // business rather than the form's.
  signal cancelled()

  // While any of these owns the keyboard, a letter is a letter. The panel reads
  // this to stand its key catcher down, so it is spelled out rather than
  // inferred: a field missed here silently swallows the shortcut instead.
  readonly property bool formFocused:
       pathDropdown.activeFocus || pathDropdown.popupOpen
    || deliverDropdown.activeFocus || deliverDropdown.popupOpen
    || timeFieldMenu.activeFocus || timeFieldMenu.popupOpen
    || whereFieldMenu.activeFocus || whereFieldMenu.popupOpen
    || operatorGroup.activeFocus || sideGroup.activeFocus
    || whereOpGroup.activeFocus || expiryGroup.activeFocus
    || boundField.activeFocus || valueField.activeFocus
    || ageField.activeFocus || whereValueField.activeFocus
    || reasonField.activeFocus || standingToggle.activeFocus
    || submitButton.activeFocus || cancelButton.activeFocus

  // Opening the form puts the keyboard in it, so there is never a moment where
  // the form is up and the keys are still driving the list behind it.
  function focusFirst() {
    pathDropdown.forceActiveFocus()
  }

  function cancel() {
    form.cancelled()
  }

  // Every field shares one escape hatch and one submit, and every menu the same
  // keyboard opening, so each is written once.
  //
  // A menu's popup hands focus back to the window when it closes rather than to
  // the control that opened it; taking it back a beat later is what keeps Tab
  // walking the form after a pick.
  component FormField: TextField {
    foreground: form.foreground
    accent: Color.accent
    font.family: form.fontFamily
    Keys.onEscapePressed: form.cancel()
    onAccepted: if (form.complete) form.submit()
  }

  component FormMenu: Dropdown {
    foreground: form.foreground
    accent: Color.accent
    fontFamily: form.fontFamily
    hasCursor: activeFocus
    Keys.onPressed: function (event) {
      if (event.key === Qt.Key_Return || event.key === Qt.Key_Enter
          || event.key === Qt.Key_Space || event.key === Qt.Key_Down) {
        open()
        event.accepted = true
      } else if (event.key === Qt.Key_Escape) {
        form.cancel()
        event.accepted = true
      }
    }
    onPopupOpenChanged: {
      if (!popupOpen && form.visible) Qt.callLater(function () { forceActiveFocus() })
    }
  }

  component FormSearch: SearchableDropdown {
    foreground: form.foreground
    accent: Color.accent
    fontFamily: form.fontFamily
    hasCursor: activeFocus
    Keys.onEscapePressed: form.cancel()
    onPopupOpenChanged: {
      if (!popupOpen && form.visible) Qt.callLater(function () { forceActiveFocus() })
    }
  }

  // Set when the form is standing in for an existing watch. Editing keeps the
  // id, so the same form serves both and there is one place a watch is written.
  property string editingId: ""
  readonly property bool editing: form.editingId !== ""

  readonly property color foreground: owner.foreground
  readonly property string fontFamily: owner.fontFamily

  property string path: ""
  property string operator: ""
  property string side: "above"
  property string bound: ""
  property string textValue: ""
  property string olderThan: "7d"
  property string timeField: ""
  property string whereField: ""
  property string whereOp: "="
  property string whereValue: ""
  property int expiryDays: 7
  // An edit leaves the expiry alone unless a chip is actually picked, so
  // reopening a watch to change a bound does not silently restart its clock.
  property bool expiryTouched: false
  property bool standing: false
  property string reason: ""
  property string deliverTo: "you"

  readonly property var leaf: {
    for (var i = 0; i < owner.catalogue.length; i++) {
      if (owner.catalogue[i].path === form.path) return owner.catalogue[i]
    }
    return null
  }

  readonly property var operators: form.leaf && form.leaf.operators ? form.leaf.operators : []
  readonly property var fields: form.leaf && form.leaf.fields ? form.leaf.fields : []
  readonly property var timeFields: form.leaf && form.leaf.timeFields ? form.leaf.timeFields : []

  readonly property bool needsBound: form.operator === "crosses" || form.operator === "count"
  readonly property bool needsText: form.operator === "becomes"
  readonly property bool needsAge: form.operator === "ages"

  // The catalogue's own words, so the form explains itself rather than needing
  // a manual next to it.
  readonly property string describes: form.leaf ? String(form.leaf.describes) : ""

  readonly property var pathOptions: {
    var out = []
    for (var i = 0; i < owner.catalogue.length; i++) {
      var l = owner.catalogue[i]
      out.push({ value: l.path, label: l.path, description: l.describes })
    }
    return out
  }

  readonly property var deliveryOptions: {
    var out = [
      { value: "you", label: "This machine", description: "a desktop notification" },
      { value: "repo", label: "Whoever is in the checkout",
        description: "the agent working where the alert is about" }
    ]
    for (var i = 0; i < owner.agents.length; i++) {
      var a = owner.agents[i]
      var name = String(a.terminal_title_stripped || a.agent || a.pane_id)
      out.push({ value: String(a.pane_id),
                 label: name + "  [" + a.pane_id + "]",
                 description: String(a.cwd || "") })
    }
    return out
  }

  readonly property bool complete: {
    if (form.path === "" || form.operator === "") return false
    if (form.needsBound && String(form.bound).trim() === "") return false
    if (form.needsText && form.textValue === "") return false
    if (form.needsAge && form.olderThan === "") return false
    return true
  }

  // Choosing a path resets everything that only made sense for the last one.
  onPathChanged: {
    form.operator = form.operators.length > 0 ? String(form.operators[0]) : ""
    form.timeField = form.timeFields.length > 0 ? String(form.timeFields[0]) : ""
    form.whereField = ""
    // Clear the fields rather than the properties they feed, so nothing is left
    // on screen that the form no longer believes.
    whereValueField.text = ""
    valueField.text = ""
    boundField.text = ""
  }

  // load fills the form from a stored watch. Path goes first: choosing one
  // resets everything that only made sense for the last one.
  function load(t) {
    form.editingId = String(t.id)
    form.path = String(t.path)
    form.operator = String(t.operator)

    var p = t.params || {}
    if (p.above !== undefined && p.above !== null) {
      form.side = "above"
      boundField.text = String(p.above)
    } else if (p.below !== undefined && p.below !== null) {
      form.side = "below"
      boundField.text = String(p.below)
    } else {
      boundField.text = ""
    }
    valueField.text = String(p.value !== undefined ? p.value : "")
    ageField.text = String(p.olderThan !== undefined ? p.olderThan : "7d")
    if (p.field) form.timeField = String(p.field)

    var w = (t.where || [])[0]
    form.whereField = w ? String(w.field) : ""
    form.whereOp = w ? String(w.op) : "="
    whereValueField.text = w ? String(w.value) : ""

    form.standing = t.standing === true
    reasonField.text = String(t.reason !== undefined ? t.reason : "")
    form.deliverTo = String(t.deliverTo !== undefined ? t.deliverTo : "you")
    form.expiryTouched = false
  }

  // reset puts the form back to arming a new watch.
  function reset() {
    form.editingId = ""
    form.path = ""
    form.operator = ""
    form.side = "above"
    boundField.text = ""
    form.expiryDays = 7
    form.expiryTouched = false
    form.standing = false
    form.deliverTo = "you"
    form.whereField = ""
    form.whereOp = "="
    valueField.text = ""
    ageField.text = "7d"
    whereValueField.text = ""
    reasonField.text = ""
  }

  // submit is what Enter in a field and the Arm button both reach.
  function submit() {
    if (!form.complete) return
    if (form.editing) form.owner.editTrigger(form.editingId, form.trigger())
    else form.owner.armTrigger(form.trigger())
    form.armed()
  }

  function trigger() {
    var params = {}
    if (form.needsBound) {
      if (form.side === "above") params.above = Number(form.bound)
      else params.below = Number(form.bound)
    } else if (form.needsText) {
      params.value = form.textValue
    } else if (form.needsAge) {
      params.olderThan = form.olderThan
      if (form.timeField !== "") params.field = form.timeField
    }

    var where = []
    if (form.whereField !== "" && form.whereValue !== "")
      where.push({ field: form.whereField, op: form.whereOp, value: form.whereValue })

    var doc = {
      path: form.path,
      operator: form.operator,
      params: params,
      where: where,
      deliverTo: form.deliverTo,
      standing: form.standing,
      reason: form.reason
    }
    // An edit carries no owner: the daemon keeps whoever armed it.
    if (!form.editing) doc.armedBy = "you"
    if (!form.editing || form.expiryTouched)
      doc.expiresAt = new Date(Date.now() + form.expiryDays * 86400000).toISOString()
    return doc
  }

  spacing: Style.space(8)

  // ---------- what to watch ----------
  PanelSectionHeader {
    text: "WATCH"
    foreground: form.foreground
    fontFamily: form.fontFamily
  }

  FormSearch {
    id: pathDropdown
    width: parent.width
    label: "Path"
    showLabel: true
    value: form.path
    options: form.pathOptions
    triggerLabel: form.path === "" ? "Choose something to watch" : form.path
    KeyNavigation.tab: operatorGroup
    KeyNavigation.backtab: cancelButton
    onChanged: function(v) { form.path = v }
  }

  Text {
    textFormat: Text.PlainText
    width: parent.width
    wrapMode: Text.WordWrap
    visible: form.describes !== ""
    text: form.describes
    color: Qt.darker(form.foreground, 1.4)
    font.family: form.fontFamily
    font.pixelSize: Style.font.caption
  }

  // ---------- what counts as happening ----------
  ButtonGroup {
    id: operatorGroup
    width: parent.width
    visible: form.operators.length > 0
    KeyNavigation.tab: sideGroup
    KeyNavigation.backtab: pathDropdown
    options: {
      var out = []
      for (var i = 0; i < form.operators.length; i++)
        out.push({ value: String(form.operators[i]), label: String(form.operators[i]) })
      return out
    }
    value: form.operator
    foreground: form.foreground
    accent: Color.accent
    fontFamily: form.fontFamily
    fontSize: Style.font.caption
    onChanged: function(v) { form.operator = v }
  }

  // crosses and count take one bound, from one side.
  RowLayout {
    width: parent.width
    visible: form.needsBound
    spacing: Style.space(8)

    ButtonGroup {
      id: sideGroup
      visible: form.needsBound
      options: [{ value: "above", label: "above" }, { value: "below", label: "below" }]
      value: form.side
      foreground: form.foreground
      accent: Color.accent
      fontFamily: form.fontFamily
      fontSize: Style.font.caption
      KeyNavigation.tab: boundField
      KeyNavigation.backtab: operatorGroup
      onChanged: function(v) { form.side = v }
    }

    // A plain field rather than a spinner: one kind of control in the form
    // means one focus rule, and the daemon checks the number anyway.
    FormField {
      id: boundField
      Layout.fillWidth: true
      visible: form.needsBound
      placeholderText: "how many, such as 25"
      validator: IntValidator { bottom: 0 }
      KeyNavigation.tab: valueField
      KeyNavigation.backtab: sideGroup
      onTextChanged: form.bound = text
    }
  }

  FormField {
    id: valueField
    width: parent.width
    visible: form.needsText
    placeholderText: "the value to wait for, such as urgent"
    KeyNavigation.tab: ageField
    KeyNavigation.backtab: boundField
    onTextChanged: form.textValue = text
  }

  RowLayout {
    width: parent.width
    visible: form.needsAge
    spacing: Style.space(8)

    FormField {
      id: ageField
      Layout.preferredWidth: Style.space(70)
      visible: form.needsAge
      text: "7d"
      placeholderText: "7d"
      KeyNavigation.tab: timeFieldMenu
      KeyNavigation.backtab: valueField
      onTextChanged: form.olderThan = text
    }

    FormMenu {
      id: timeFieldMenu
      Layout.fillWidth: true
      visible: form.needsAge && form.timeFields.length > 0
      showLabel: false
      value: form.timeField
      KeyNavigation.tab: whereFieldMenu
      KeyNavigation.backtab: ageField
      options: {
        var out = []
        for (var i = 0; i < form.timeFields.length; i++)
          out.push({ value: String(form.timeFields[i]), label: String(form.timeFields[i]) })
        return out
      }
      onChanged: function(v) { form.timeField = v }
    }
  }

  // ---------- how narrow ----------
  PanelSectionHeader {
    text: "ONLY WHEN"
    visible: form.fields.length > 0
    foreground: form.foreground
    fontFamily: form.fontFamily
  }

  RowLayout {
    width: parent.width
    visible: form.fields.length > 0
    spacing: Style.space(6)

    FormMenu {
      id: whereFieldMenu
      Layout.preferredWidth: Style.space(120)
      visible: form.fields.length > 0
      showLabel: false
      value: form.whereField
      KeyNavigation.tab: whereOpGroup
      KeyNavigation.backtab: timeFieldMenu
      options: {
        var out = [{ value: "", label: "any" }]
        for (var i = 0; i < form.fields.length; i++)
          out.push({ value: String(form.fields[i]), label: String(form.fields[i]) })
        return out
      }
      onChanged: function(v) { form.whereField = v }
    }

    ButtonGroup {
      id: whereOpGroup
      visible: form.fields.length > 0
      options: [{ value: "=", label: "is" }, { value: "~=", label: "contains" }]
      value: form.whereOp
      foreground: form.foreground
      accent: Color.accent
      fontFamily: form.fontFamily
      fontSize: Style.font.caption
      KeyNavigation.tab: whereValueField
      KeyNavigation.backtab: whereFieldMenu
      onChanged: function(v) { form.whereOp = v }
    }

    FormField {
      id: whereValueField
      Layout.fillWidth: true
      visible: form.fields.length > 0
      placeholderText: "value"
      KeyNavigation.tab: expiryGroup
      KeyNavigation.backtab: whereOpGroup
      onTextChanged: form.whereValue = text
    }
  }

  // ---------- how long, and how often ----------
  PanelSectionHeader {
    text: "STANDS FOR"
    foreground: form.foreground
    fontFamily: form.fontFamily
  }

  // The chips are a ButtonGroup rather than hand rolled surfaces, so they come
  // with the keyboard for free: one Tab stop, h and l between them, Enter to
  // pick. An edit shows nothing selected until a chip is touched, because
  // leaving the expiry alone is the common case.
  ButtonGroup {
    id: expiryGroup
    width: parent.width
    options: [
      { value: "1", label: "1d" },
      { value: "4", label: "4d" },
      { value: "7", label: "7d" },
      { value: "30", label: "30d" }
    ]
    value: (form.editing && !form.expiryTouched) ? "" : String(form.expiryDays)
    foreground: form.foreground
    accent: Color.accent
    fontFamily: form.fontFamily
    fontSize: Style.font.caption
    KeyNavigation.tab: standingToggle
    KeyNavigation.backtab: whereValueField
    onChanged: function (v) {
      form.expiryDays = Number(v)
      form.expiryTouched = true
    }
  }

  Toggle {
    id: standingToggle
    width: parent.width
    label: "Ring every time"
    KeyNavigation.tab: deliverDropdown
    KeyNavigation.backtab: expiryGroup
    Keys.onEscapePressed: form.cancel()
    description: form.standing
      ? "Stays armed until it expires."
      : "Rings once, then disarms itself."
    checked: form.standing
    foreground: form.foreground
    accent: Color.accent
    fontFamily: form.fontFamily
    onClicked: form.standing = !form.standing
  }

  // ---------- who gets woken ----------
  PanelSectionHeader {
    text: "WAKES"
    foreground: form.foreground
    fontFamily: form.fontFamily
  }

  FormSearch {
    id: deliverDropdown
    width: parent.width
    showLabel: false
    value: form.deliverTo
    options: form.deliveryOptions
    KeyNavigation.tab: reasonField
    KeyNavigation.backtab: standingToggle
    onChanged: function(v) { form.deliverTo = v }
  }

  Text {
    textFormat: Text.PlainText
    width: parent.width
    wrapMode: Text.WordWrap
    visible: form.owner.agentNote !== ""
    text: "No agents to wake right now, so alerts land on the desktop."
    color: Qt.darker(form.foreground, 1.4)
    font.family: form.fontFamily
    font.pixelSize: Style.font.caption
  }

  FormField {
    id: reasonField
    width: parent.width
    placeholderText: "why this matters, carried into the wake-up"
    KeyNavigation.tab: submitButton
    KeyNavigation.backtab: deliverDropdown
    onTextChanged: form.reason = text
  }

  Text {
    textFormat: Text.PlainText
    width: parent.width
    wrapMode: Text.WordWrap
    visible: form.editing
    text: "Editing " + form.editingId + ". Saving clears what it has learned, so "
        + "the next sample teaches it again."
    color: Qt.darker(form.foreground, 1.4)
    font.family: form.fontFamily
    font.pixelSize: Style.font.caption
  }

  RowLayout {
    width: parent.width
    spacing: Style.space(6)

    Button {
      id: submitButton
      Layout.fillWidth: true
      text: form.editing ? "Save changes" : "Arm this watch"
      iconText: form.owner.iconBell
      bordered: true
      focusable: true
      enabled: form.complete
      opacity: form.complete ? 1.0 : 0.45
      foreground: form.foreground
      accent: Color.accent
      fontFamily: form.fontFamily
      fontSize: Style.font.body
      KeyNavigation.tab: cancelButton
      KeyNavigation.backtab: reasonField
      Keys.onEscapePressed: form.cancel()
      onClicked: form.submit()
    }

    // The ring closes on a real control, so Escape has something visible that
    // means the same thing.
    Button {
      id: cancelButton
      text: "Cancel"
      bordered: true
      focusable: true
      foreground: form.foreground
      accent: Color.accent
      fontFamily: form.fontFamily
      fontSize: Style.font.body
      KeyNavigation.tab: pathDropdown
      KeyNavigation.backtab: submitButton
      Keys.onEscapePressed: form.cancel()
      onClicked: form.cancel()
    }
  }
}
