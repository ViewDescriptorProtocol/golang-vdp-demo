// Light / Dark / System control for the demo shell.
//
// "System" is stored as the absence of a preference, and applied as the absence
// of the data-theme attribute, so the CSS falls back to `color-scheme: light
// dark` and keeps tracking the OS live. Storing the literal string "system"
// would work too, but then the page would have to re-resolve it on every OS
// change instead of letting CSS do it.
//
// Nothing here is VDP: it is the demo's own chrome, like the trace panel.
(function () {
  "use strict";

  var KEY = "vdp-theme";
  var root = document.documentElement;
  var buttons = document.querySelectorAll("[data-set-theme]");

  function read() {
    try {
      var saved = localStorage.getItem(KEY);
      return saved === "light" || saved === "dark" ? saved : "system";
    } catch (e) {
      return "system"; // Storage blocked; the OS preference still applies.
    }
  }

  function write(theme) {
    try {
      if (theme === "system") {
        localStorage.removeItem(KEY);
      } else {
        localStorage.setItem(KEY, theme);
      }
    } catch (e) {
      // Not persisting is survivable — the choice still applies to this page.
    }
  }

  function apply(theme) {
    if (theme === "system") {
      delete root.dataset.theme;
    } else {
      root.dataset.theme = theme;
    }
    buttons.forEach(function (button) {
      var pressed = button.dataset.setTheme === theme;
      button.setAttribute("aria-pressed", String(pressed));
    });
  }

  buttons.forEach(function (button) {
    button.addEventListener("click", function () {
      var theme = button.dataset.setTheme;
      write(theme);
      apply(theme);
    });
  });

  // The inline head script already set the attribute; this syncs the buttons.
  apply(read());
})();
