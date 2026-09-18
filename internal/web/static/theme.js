/* vmsync-ui theme toggle: light / dark, persisted, system-aware.
 *
 * The choice lives in localStorage under "vmsync-theme" and is applied as
 * data-theme on <html>, which is what static/style.css keys off. With no
 * saved choice the live system preference is followed; the stylesheet also
 * honours prefers-color-scheme on its own so a no-JS browser still gets a
 * sane theme. Kept dependency-free and inline-handler-free so the page's
 * Content-Security-Policy (default-src 'self') stays as tight as it is.
 */
(function () {
  "use strict";

  var KEY = "vmsync-theme";

  function savedChoice() {
    try {
      var v = window.localStorage.getItem(KEY);
      if (v === "light" || v === "dark") {
        return v;
      }
    } catch (e) {
      /* Storage unavailable (private mode, disabled): fall through. */
    }
    return null;
  }

  function systemChoice() {
    if (window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches) {
      return "dark";
    }
    return "light";
  }

  function paint(theme) {
    document.documentElement.setAttribute("data-theme", theme);
    var btn = document.getElementById("theme-toggle");
    if (btn) {
      var dark = theme === "dark";
      var name = dark ? "Switch to light mode" : "Switch to dark mode";
      btn.setAttribute("aria-pressed", dark ? "true" : "false");
      btn.setAttribute("aria-label", name);
      btn.title = name;
    }
  }

  function current() {
    return document.documentElement.getAttribute("data-theme") === "dark" ? "dark" : "light";
  }

  function store(theme) {
    try {
      window.localStorage.setItem(KEY, theme);
    } catch (e) {
      /* Non-fatal: the theme still applies for this session. */
    }
  }

  /* Apply as early as this script runs (it loads synchronously in <head>)
   * so the first paint already uses the right theme. */
  paint(savedChoice() || systemChoice());

  /* Follow the OS while the operator has not picked a side. */
  if (!savedChoice() && window.matchMedia) {
    var mq = window.matchMedia("(prefers-color-scheme: dark)");
    var follow = function (e) {
      if (!savedChoice()) {
        paint(e.matches ? "dark" : "light");
      }
    };
    if (typeof mq.addEventListener === "function") {
      mq.addEventListener("change", follow);
    } else if (typeof mq.addListener === "function") {
      mq.addListener(follow);
    }
  }

  document.addEventListener("DOMContentLoaded", function () {
    var btn = document.getElementById("theme-toggle");
    if (!btn) {
      return;
    }
    /* Re-paint in case the button arrived after the head ran. */
    paint(current());
    btn.addEventListener("click", function () {
      var next = current() === "dark" ? "light" : "dark";
      store(next);
      paint(next);
    });
  });
})();
