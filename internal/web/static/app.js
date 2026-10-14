// SPDX-License-Identifier: AGPL-3.0-or-later
// Small behaviours HTMX does not cover. No inline handlers: the CSP forbids them.
(function () {
  // Errors (4xx/5xx) are not swapped into the page; their text goes to a toast.
  htmx.config.responseHandling = [
    { code: "204", swap: false },
    { code: "[23]..", swap: true },
    { code: "[45]..", swap: false, error: true },
  ];

  function toast(message, isError) {
    const box = document.getElementById("toast");
    if (!box || !message) return;
    const el = document.createElement("div");
    el.className = "toast" + (isError ? " error" : "");
    el.textContent = message.trim().slice(0, 400);
    box.appendChild(el);
    setTimeout(() => el.remove(), isError ? 8000 : 4000);
  }

  document.addEventListener("htmx:responseError", (e) => {
    if (e.detail.xhr.status === 401) return; // HX-Redirect sends us to sign in
    toast(e.detail.xhr.responseText || "Request failed", true);
  });
  document.addEventListener("htmx:sendError", () => toast("Cannot reach Drawbridge.", true));
  // Servers send HX-Trigger: {"toast": "message"}.
  document.addEventListener("toast", (e) => toast(e.detail.value));

  // Appearance: show a new theme or mode at once; the form saves it.
  document.addEventListener("change", (e) => {
    const form = e.target.closest("#appearance-form");
    if (!form) return;
    document.documentElement.dataset.theme = form.elements.theme.value;
    document.documentElement.dataset.mode = form.elements.mode.value;
  });

  // Settings tabs scroll sideways on a narrow screen: bring the open one into view.
  document.addEventListener("DOMContentLoaded", () => {
    const tab = document.querySelector('.tabs [aria-current="page"]');
    if (tab) tab.parentElement.scrollLeft = tab.offsetLeft - (tab.parentElement.clientWidth - tab.offsetWidth) / 2;
  });

  // A select-all checkbox checks every box with the given name in its table.
  document.addEventListener("change", (e) => {
    const all = e.target.closest("[data-select-all]");
    if (all) {
      const name = all.getAttribute("data-select-all");
      all.closest("table").querySelectorAll(`input[name="${name}"]`).forEach((b) => (b.checked = all.checked));
    }
    const auto = e.target.closest("[data-autosubmit]");
    if (auto && auto.form) auto.form.requestSubmit();
  });

  // The account menu closes on a click elsewhere or on Escape.
  document.addEventListener("click", (e) => {
    document.querySelectorAll("details.usermenu[open]").forEach((d) => {
      if (!d.contains(e.target)) d.open = false;
    });
  });
  document.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    document.querySelectorAll("details.usermenu[open]").forEach((d) => {
      d.open = false;
      d.querySelector("summary").focus();
    });
  });

  // Notification channel forms show only the fields their kind uses.
  function kindFields(form) {
    const kind = form.elements.kind.value;
    form.querySelectorAll("[data-kinds]").forEach((el) => {
      el.hidden = !el.getAttribute("data-kinds").split(" ").includes(kind);
    });
  }
  document.addEventListener("change", (e) => {
    if (e.target.matches('select[name="kind"]') && e.target.form) kindFields(e.target.form);
  });
  document.addEventListener("htmx:load", (e) => {
    e.detail.elt.querySelectorAll('select[name="kind"]').forEach((s) => s.form && kindFields(s.form));
  });

  // Forms marked data-reset-on-success clear after a successful HTMX post.
  document.addEventListener("htmx:afterRequest", (e) => {
    const form = e.detail.elt.closest && e.detail.elt.closest("form[data-reset-on-success]");
    if (form && e.detail.successful) form.reset();
  });
})();
